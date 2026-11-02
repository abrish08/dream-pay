// Package mpgstest simulates the Mastercard Gateway (MPGS) for tests and
// local development:
//
//   - the REST API the adapter uses (sessions, orders, refunds) with Basic auth;
//   - checkout.min.js and a hosted card page in the browser: card number
//     checks (Luhn, brand, expiry, CVV), a 3-D Secure OTP step, and issuer
//     answers chosen by the expiry date, like Mastercard's own test system;
//   - helpers for tests that play the customer (Pay) or send webhooks (Notify).
//
// Run it in a browser with: go run ./cmd/mpgs-sim
package mpgstest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Outcomes by expiry date (any valid Mastercard / Visa number). Any other
// future expiry is approved.
var Outcomes = []struct{ Expiry, GatewayCode, Meaning string }{
	{"01/39", "APPROVED", "Approved"},
	{"05/39", "DECLINED", "Declined by the bank"},
	{"02/39", "INSUFFICIENT_FUNDS", "Insufficient funds"},
	{"04/27", "EXPIRED_CARD", "Bank says the card is expired"},
	{"08/28", "TIMED_OUT", "Bank did not answer in time"},
}

// TestCards are numbers that pass the checks (from Mastercard's test set).
var TestCards = []struct{ Brand, Number string }{
	{"Mastercard", "5123 4500 0000 0008"},
	{"Mastercard", "2223 0000 0000 0007"},
	{"Visa", "4508 7500 1574 1019"},
}

// OTP is the 3-D Secure code the simulated bank accepts.
const OTP = "123456"

// Sim is the simulated gateway (an http.Handler).
type Sim struct {
	MerchantID, Password string

	mu       sync.Mutex
	sessions map[string]*session
	orders   map[string]*order
	Refunds  map[string]json.RawMessage // "order/txn" -> request body
	Declines map[string]bool            // refund amounts the bank declines ("10.00")
	Calls    []string
}

type session struct {
	ID, OrderID, Amount, Currency, ReturnURL, NotificationURL, MerchantName, CancelURL string
	card                                                                               *pendingCard
}

type pendingCard struct{ Brand, Last4, Expiry string }

type order struct {
	ID, Amount, Currency, Status string
	Captured, Refunded           float64
	attempts                     []map[string]any // payment attempts, then refunds
	txns                         map[string]map[string]any
}

func NewSim(merchantID, password string) *Sim {
	return &Sim{MerchantID: merchantID, Password: password, sessions: map[string]*session{},
		orders: map[string]*order{}, Refunds: map[string]json.RawMessage{}, Declines: map[string]bool{}}
}

// Server is a Sim on a local HTTPS test server (like the real gateway, TLS
// only). Use Client() for requests that must trust its certificate.
type Server struct {
	*httptest.Server
	*Sim
}

func New(merchantID, password string) *Server {
	sim := NewSim(merchantID, password)
	return &Server{Server: httptest.NewTLSServer(sim), Sim: sim}
}

func (s *Sim) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/api/rest/version/100/information":
		writeJSON(w, map[string]any{"status": "OPERATING", "gatewayVersion": "simulator"})
	case r.URL.Path == "/static/checkout/checkout.min.js":
		s.script(w, r)
	case strings.HasPrefix(r.URL.Path, "/checkout/pay/"):
		s.cardPage(w, r, strings.TrimPrefix(r.URL.Path, "/checkout/pay/"))
	case strings.HasPrefix(r.URL.Path, "/checkout/3ds/"):
		s.otpPage(w, r, strings.TrimPrefix(r.URL.Path, "/checkout/3ds/"))
	default:
		s.api(w, r)
	}
}

// ---------------- REST API ----------------

func (s *Sim) api(w http.ResponseWriter, r *http.Request) {
	prefix := "/api/rest/version/100/merchant/" + s.MerchantID
	if !strings.HasPrefix(r.URL.Path, prefix+"/") {
		http.NotFound(w, r)
		return
	}
	if u, p, ok := r.BasicAuth(); !ok || u != "merchant."+s.MerchantID || p != s.Password {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"result":"ERROR","error":{"cause":"INVALID_REQUEST","explanation":"Invalid credentials."}}`)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, prefix+"/"), "/")
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Calls = append(s.Calls, r.Method+" "+strings.Join(parts, "/"))
	switch {
	case r.Method == http.MethodGet && len(parts) == 1 && parts[0] == "paymentOptionsInquiry":
		writeJSON(w, map[string]any{"result": "SUCCESS", "merchant": s.MerchantID})
	case r.Method == http.MethodPost && len(parts) == 1 && parts[0] == "session":
		s.initiate(w, body)
	case r.Method == http.MethodGet && len(parts) == 2 && parts[0] == "order":
		o := s.orders[parts[1]]
		if o == nil {
			apiError(w, 400, "Unable to find order")
			return
		}
		writeJSON(w, map[string]any{"id": o.ID, "status": o.Status, "amount": json.Number(o.Amount), "currency": o.Currency,
			"totalCapturedAmount": o.Captured, "totalRefundedAmount": o.Refunded, "result": "SUCCESS", "transaction": o.attempts})
	case len(parts) == 4 && parts[0] == "order" && parts[2] == "transaction":
		s.transaction(w, r.Method, parts[1], parts[3], body)
	default:
		apiError(w, 400, "unsupported request")
	}
}

func (s *Sim) initiate(w http.ResponseWriter, body []byte) {
	var req struct {
		APIOperation string `json:"apiOperation"`
		Order        struct {
			ID, Amount, Currency, NotificationURL string
		} `json:"order"`
		Interaction struct {
			Operation string `json:"operation"`
			ReturnURL string `json:"returnUrl"`
			Merchant  struct {
				Name string `json:"name"`
			} `json:"merchant"`
		} `json:"interaction"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.APIOperation != "INITIATE_CHECKOUT" ||
		req.Interaction.Operation != "PURCHASE" || len(req.Order.ID) < 8 || req.Order.Currency == "" ||
		!strings.Contains(req.Order.Amount, ".") || req.Interaction.ReturnURL == "" {
		apiError(w, 400, "invalid INITIATE_CHECKOUT request: "+string(body))
		return
	}
	id := fmt.Sprintf("SESSION%04d%s", len(s.sessions)+1, strings.ReplaceAll(req.Order.ID[:8], "-", ""))
	s.sessions[id] = &session{ID: id, OrderID: req.Order.ID, Amount: req.Order.Amount, Currency: req.Order.Currency,
		ReturnURL: req.Interaction.ReturnURL, NotificationURL: req.Order.NotificationURL, MerchantName: req.Interaction.Merchant.Name}
	writeJSON(w, map[string]any{"result": "SUCCESS", "merchant": s.MerchantID,
		"session": map[string]any{"id": id, "updateStatus": "SUCCESS", "version": "1"}, "successIndicator": "abc123"})
}

func (s *Sim) transaction(w http.ResponseWriter, method, orderID, txnID string, body []byte) {
	o := s.orders[orderID]
	if o == nil {
		apiError(w, 400, "Unable to find order")
		return
	}
	if method == http.MethodGet {
		t := o.txns[txnID]
		if t == nil {
			apiError(w, 400, "Unable to find transaction")
			return
		}
		writeJSON(w, t)
		return
	}
	if method != http.MethodPut {
		apiError(w, 400, "unsupported")
		return
	}
	if o.txns[txnID] != nil {
		apiError(w, 400, "Transaction ID already exists") // the real gateway refuses duplicates
		return
	}
	var req struct {
		APIOperation string                            `json:"apiOperation"`
		Transaction  struct{ Amount, Currency string } `json:"transaction"`
	}
	_ = json.Unmarshal(body, &req)
	if req.APIOperation != "REFUND" || req.Transaction.Currency != o.Currency {
		apiError(w, 400, "invalid refund")
		return
	}
	s.Refunds[orderID+"/"+txnID] = body
	var amt float64
	fmt.Sscanf(req.Transaction.Amount, "%f", &amt)
	result, code := "SUCCESS", "APPROVED"
	if s.Declines[req.Transaction.Amount] || o.Refunded+amt > o.Captured+0.001 {
		result, code = "FAILURE", "DECLINED"
	} else {
		o.Refunded += amt
		o.Status = "PARTIALLY_REFUNDED"
		if o.Refunded >= o.Captured-0.001 {
			o.Status = "REFUNDED"
		}
	}
	t := map[string]any{"result": result, "response": map[string]any{"gatewayCode": code},
		"transaction": map[string]any{"id": txnID, "type": "REFUND", "amount": amt, "currency": o.Currency}}
	o.txns[txnID] = t
	writeJSON(w, t)
}

// attempt records one payment attempt on the order (call with mu held).
func (s *Sim) attempt(ss *session, gatewayCode string) {
	o := s.orders[ss.OrderID]
	if o == nil {
		o = &order{ID: ss.OrderID, Amount: ss.Amount, Currency: ss.Currency, txns: map[string]map[string]any{}}
		s.orders[ss.OrderID] = o
	}
	result := "SUCCESS"
	switch gatewayCode {
	case "APPROVED":
		o.Status = "CAPTURED"
		fmt.Sscanf(ss.Amount, "%f", &o.Captured)
	case "AUTHENTICATION_FAILED":
		o.Status, result = "AUTHENTICATION_UNSUCCESSFUL", "FAILURE"
	default:
		o.Status, result = "FAILED", "FAILURE"
	}
	o.attempts = append(o.attempts, map[string]any{"result": result, "response": map[string]any{"gatewayCode": gatewayCode},
		"transaction": map[string]any{"id": fmt.Sprintf("%d", len(o.attempts)+1), "type": "PAYMENT"}})
}

// ---------------- Test helpers ----------------

// Pay plays the customer (card + 3-D Secure) and returns the URL the browser
// is sent back to after an approved payment.
func (s *Sim) Pay(sessionID string, approved bool) (returnURL string) {
	code := "APPROVED"
	if !approved {
		code = "DECLINED"
	}
	return s.PayWith(sessionID, code)
}

// PayWith records an attempt with the issuer's answer (e.g. INSUFFICIENT_FUNDS).
func (s *Sim) PayWith(sessionID, gatewayCode string) (returnURL string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss, ok := s.sessions[sessionID]
	if !ok {
		return ""
	}
	s.attempt(ss, gatewayCode)
	return ss.ReturnURL + "?resultIndicator=abc123&sessionVersion=2"
}

// Notify sends the webhook MPGS posts after a payment, with the given secret.
func (s *Sim) Notify(url, secret, orderID string) (int, error) {
	b, _ := json.Marshal(map[string]any{"order": map[string]any{"id": orderID}, "result": "SUCCESS"})
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Notification-Secret", secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

// ---------------- Browser: checkout.min.js and hosted pages ----------------

// script mimics Mastercard's checkout.min.js: configure + showPaymentPage
// sends the browser to the hosted card page.
func (s *Sim) script(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/javascript")
	base := "http://" + r.Host
	if r.TLS != nil {
		base = "https://" + r.Host
	}
	fmt.Fprintf(w, `(function(){var tag=document.currentScript,sid=null;
window.Checkout={configure:function(c){sid=c&&c.session&&c.session.id;},
showPaymentPage:function(){if(!sid){var f=tag.getAttribute("data-error");if(f&&window[f])window[f]({cause:"INVALID_SESSION"});return;}
var cancel=tag.getAttribute("data-cancel")||"";
window.location.href=%q+"/checkout/pay/"+encodeURIComponent(sid)+"?cancel="+encodeURIComponent(cancel);}};})();`, base)
}

var pages = template.Must(template.New("card").Parse(`{{define "head"}}<!doctype html><html><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1"><title>Mastercard Gateway (simulator)</title>
<style>body{font-family:system-ui,sans-serif;background:#f4f5f7;margin:0;color:#141413}
.wrap{max-width:440px;margin:32px auto;padding:0 16px}.card{background:#fff;border-radius:12px;padding:24px;box-shadow:0 1px 3px #0002}
.sim{background:#fff4d6;border:1px solid #f0d58a;border-radius:8px;padding:8px 12px;font-size:13px;margin-bottom:12px}
h1{font-size:18px;margin:0}.amt{font-size:30px;font-weight:700;margin:8px 0 16px}label{display:block;font-size:13px;margin:12px 0 4px;color:#555}
input{width:100%;box-sizing:border-box;padding:10px;border:1px solid #ccc;border-radius:8px;font-size:16px}.row{display:flex;gap:12px}.row>div{flex:1}
.err{color:#b42318;font-size:13px;margin-top:4px}.alert{background:#fde8e7;color:#7a1a12;border-radius:8px;padding:10px 12px;margin-bottom:12px}
button{width:100%;margin-top:18px;padding:12px;border:0;border-radius:8px;background:#eb001b;color:#fff;font-size:16px;cursor:pointer}
a.cancel{display:block;text-align:center;margin-top:12px;color:#555;font-size:14px}table{width:100%;font-size:12px;border-collapse:collapse;margin-top:16px}
td,th{text-align:left;padding:3px 4px;border-bottom:1px solid #eee}.muted{color:#666;font-size:13px}</style></head><body><div class="wrap">
<div class="sim">Simulator of the Mastercard Gateway — no real card is charged.</div>{{end}}

{{define "card"}}{{template "head"}}<div class="card"><h1>{{.MerchantName}}</h1><div class="amt">{{.Amount}} {{.Currency}}</div>
{{if .Alert}}<div class="alert">{{.Alert}}</div>{{end}}
{{if .Paid}}<p>This order is already paid.</p>{{else}}
<form method="post"><label>Card number</label><input name="number" inputmode="numeric" autocomplete="cc-number" value="{{.Number}}" placeholder="5123 4500 0000 0008">
{{with .Errs.number}}<div class="err">{{.}}</div>{{end}}
<div class="row"><div><label>Expiry (MM/YY)</label><input name="expiry" value="{{.Expiry}}" placeholder="01/39">{{with .Errs.expiry}}<div class="err">{{.}}</div>{{end}}</div>
<div><label>Security code</label><input name="cvv" inputmode="numeric" placeholder="123">{{with .Errs.cvv}}<div class="err">{{.}}</div>{{end}}</div></div>
<label>Name on card</label><input name="name" value="{{.Name}}" placeholder="ABEBE KEBEDE">{{with .Errs.name}}<div class="err">{{.}}</div>{{end}}
<button>Pay {{.Amount}} {{.Currency}}</button></form>{{end}}
{{if .CancelURL}}<a class="cancel" href="{{.CancelURL}}">Cancel and return to the shop</a>{{end}}
<table><tr><th>Test cards</th><th></th></tr>{{range .Cards}}<tr><td>{{.Brand}}</td><td>{{.Number}}</td></tr>{{end}}</table>
<table><tr><th>Expiry</th><th>Bank answer</th></tr>{{range .Outcomes}}<tr><td>{{.Expiry}}</td><td>{{.Meaning}}</td></tr>{{end}}
<tr><td>other future date</td><td>Approved</td></tr></table><p class="muted">3-D Secure code: {{.OTP}}</p></div></div></body></html>{{end}}

{{define "otp"}}{{template "head"}}<div class="card"><h1>Verify it's you</h1>
<p class="muted">Your bank sent a one-time code to the phone number of the {{.Brand}} card ending {{.Last4}}.</p>
<div class="amt">{{.Amount}} {{.Currency}}</div>
<form method="post"><label>One-time code</label><input name="otp" inputmode="numeric" autofocus placeholder="{{.OTP}}"><button>Confirm</button></form>
</div></div></body></html>{{end}}`))

var friendly = map[string]string{
	"DECLINED":              "Your bank declined this payment. Try another card.",
	"INSUFFICIENT_FUNDS":    "Payment declined: insufficient funds on this card. Try another card or add funds.",
	"EXPIRED_CARD":          "Your bank reports this card as expired. Use another card.",
	"TIMED_OUT":             "Your bank did not respond. Please try again.",
	"AUTHENTICATION_FAILED": "Card verification (3-D Secure) failed. Try again or use another card.",
}

func (s *Sim) cardPage(w http.ResponseWriter, r *http.Request, sid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss := s.sessions[sid]
	if ss == nil {
		http.Error(w, "This payment session is invalid or has expired.", http.StatusNotFound)
		return
	}
	if c := r.URL.Query().Get("cancel"); c != "" {
		if u, err := url.Parse(c); err == nil && (u.Scheme == "http" || u.Scheme == "https") {
			ss.CancelURL = c
		}
	}
	data := map[string]any{"MerchantName": orDefault(ss.MerchantName, "Merchant"), "Amount": ss.Amount, "Currency": ss.Currency,
		"CancelURL": ss.CancelURL, "Cards": TestCards, "Outcomes": Outcomes, "OTP": OTP, "Errs": map[string]string{},
		"Alert": friendly[r.URL.Query().Get("declined")]}
	if o := s.orders[ss.OrderID]; o != nil && o.Status == "CAPTURED" {
		data["Paid"] = true
	}
	if r.Method == http.MethodPost && data["Paid"] == nil {
		_ = r.ParseForm()
		number := strings.NewReplacer(" ", "", "-", "").Replace(r.PostForm.Get("number"))
		expiry, cvv, name := strings.TrimSpace(r.PostForm.Get("expiry")), strings.TrimSpace(r.PostForm.Get("cvv")), strings.TrimSpace(r.PostForm.Get("name"))
		errs := ValidateCard(number, expiry, cvv, name, time.Now())
		if len(errs) == 0 {
			ss.card = &pendingCard{Brand: Brand(number), Last4: number[len(number)-4:], Expiry: expiry}
			http.Redirect(w, r, "/checkout/3ds/"+url.PathEscape(sid), http.StatusSeeOther)
			return
		}
		data["Errs"], data["Number"], data["Expiry"], data["Name"] = errs, r.PostForm.Get("number"), expiry, name
		w.WriteHeader(http.StatusUnprocessableEntity)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = pages.ExecuteTemplate(w, "card", data)
}

func (s *Sim) otpPage(w http.ResponseWriter, r *http.Request, sid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss := s.sessions[sid]
	if ss == nil || ss.card == nil {
		http.Error(w, "This payment session is invalid or has expired.", http.StatusNotFound)
		return
	}
	if r.Method == http.MethodPost {
		_ = r.ParseForm()
		code := "AUTHENTICATION_FAILED"
		if r.PostForm.Get("otp") == OTP {
			code = "APPROVED"
			for _, o := range Outcomes {
				if o.Expiry == ss.card.Expiry {
					code = o.GatewayCode
				}
			}
		}
		ss.card = nil
		s.attempt(ss, code)
		if code == "APPROVED" {
			http.Redirect(w, r, ss.ReturnURL+"?resultIndicator=abc123&sessionVersion=2", http.StatusSeeOther)
			return
		}
		// Like the real page: the customer stays, sees why, and may try again.
		http.Redirect(w, r, "/checkout/pay/"+url.PathEscape(sid)+"?declined="+code, http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = pages.ExecuteTemplate(w, "otp", map[string]any{"Brand": ss.card.Brand, "Last4": ss.card.Last4,
		"Amount": ss.Amount, "Currency": ss.Currency, "OTP": OTP})
}

// ValidateCard checks what the hosted page checks before contacting the bank.
func ValidateCard(number, expiry, cvv, name string, now time.Time) map[string]string {
	errs := map[string]string{}
	switch {
	case number == "":
		errs["number"] = "Enter the card number."
	case len(number) < 13 || len(number) > 19 || strings.Trim(number, "0123456789") != "":
		errs["number"] = "Card number must be 13 to 19 digits."
	case !Luhn(number):
		errs["number"] = "Card number is not valid. Check the digits."
	case Brand(number) == "":
		errs["number"] = "Only Mastercard and Visa are accepted."
	}
	var mm, yy int
	if n, _ := fmt.Sscanf(expiry, "%2d/%2d", &mm, &yy); n != 2 || mm < 1 || mm > 12 {
		errs["expiry"] = "Use MM/YY."
	} else if end := time.Date(2000+yy, time.Month(mm)+1, 1, 0, 0, 0, 0, time.UTC); !now.Before(end) {
		errs["expiry"] = "This card has expired."
	}
	if len(cvv) != 3 || strings.Trim(cvv, "0123456789") != "" {
		errs["cvv"] = "Enter the 3-digit security code."
	}
	if name == "" {
		errs["name"] = "Enter the name on the card."
	}
	return errs
}

// Luhn: the check digit every card number carries.
func Luhn(number string) bool {
	sum, double := 0, false
	for i := len(number) - 1; i >= 0; i-- {
		d := int(number[i] - '0')
		if double {
			if d *= 2; d > 9 {
				d -= 9
			}
		}
		sum, double = sum+d, !double
	}
	return sum%10 == 0
}

func Brand(number string) string {
	switch {
	case strings.HasPrefix(number, "4"):
		return "Visa"
	case len(number) >= 2 && number[0] == '5' && number[1] >= '1' && number[1] <= '5':
		return "Mastercard"
	case len(number) >= 4 && number[:4] >= "2221" && number[:4] <= "2720":
		return "Mastercard"
	}
	return ""
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

func apiError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"result": "ERROR",
		"error": map[string]any{"cause": "INVALID_REQUEST", "explanation": msg}})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
