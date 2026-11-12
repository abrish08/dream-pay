// Package walletmock simulates a mobile wallet (telebirr, CBE Birr, M-Pesa)
// that speaks the generic push API of the placeholder wallet adapter. Use it
// in tests, or in a browser with: go run ./cmd/wallet-sim
//
// The "phone" (/phone) lists PIN prompts waiting for the customer: approve,
// decline, or run out of balance. Each decision is sent back as a signed
// callback, like a real wallet.
package walletmock

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"html/template"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"
)

type Push struct {
	Reference, MSISDN, Amount, Currency, CallbackURL, Status, Reason string
	CreatedAt                                                        time.Time
	refunds                                                          map[string]map[string]string
}

type Sim struct {
	APIKey, WebhookSecret string
	// Down makes the wallet refuse requests (503: nothing accepted).
	Down bool

	mu     sync.Mutex
	pushes map[string]*Push
	order  []string
	Pushes int // POST /v1/push calls accepted (double-charge checks)
	client *http.Client
}

func NewSim(apiKey, secret string) *Sim {
	return &Sim{APIKey: apiKey, WebhookSecret: secret, pushes: map[string]*Push{}, client: &http.Client{Timeout: 10 * time.Second}}
}

// Server is a Sim on a test HTTP server.
type Server struct {
	*httptest.Server
	*Sim
}

func New(apiKey, secret string) *Server {
	sim := NewSim(apiKey, secret)
	return &Server{Server: httptest.NewServer(sim), Sim: sim}
}

func (s *Sim) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/phone" && r.Method == http.MethodGet:
		s.phone(w)
		return
	case r.URL.Path == "/phone" && r.Method == http.MethodPost:
		_ = r.ParseForm()
		s.Decide(r.PostForm.Get("reference"), r.PostForm.Get("decision"))
		http.Redirect(w, r, "/phone", http.StatusSeeOther)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+s.APIKey {
		apiErr(w, 401, "unauthorized", "invalid API key")
		return
	}
	if s.Down {
		apiErr(w, 503, "unavailable", "maintenance")
		return
	}
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/push":
		var req struct{ Reference, MSISDN, Amount, Currency, Description, CallbackURL string }
		var raw map[string]string
		_ = json.Unmarshal(body, &raw)
		req.Reference, req.MSISDN, req.Amount, req.Currency, req.CallbackURL = raw["reference"], raw["msisdn"], raw["amount"], raw["currency"], raw["callback_url"]
		if req.Reference == "" || !strings.HasPrefix(req.MSISDN, "+251") || req.Amount == "" {
			apiErr(w, 400, "invalid_request", "reference, msisdn (+251...) and amount are required")
			return
		}
		if p, ok := s.pushes[req.Reference]; ok { // idempotent per reference, like a real wallet should be
			writeJSON(w, map[string]string{"provider_ref": "W-" + p.Reference[:8], "status": p.Status})
			return
		}
		s.Pushes++
		s.pushes[req.Reference] = &Push{Reference: req.Reference, MSISDN: req.MSISDN, Amount: req.Amount, Currency: req.Currency,
			CallbackURL: req.CallbackURL, Status: "pending", CreatedAt: time.Now(), refunds: map[string]map[string]string{}}
		s.order = append(s.order, req.Reference)
		writeJSON(w, map[string]string{"provider_ref": "W-" + req.Reference[:8], "status": "pending"})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/transactions/"):
		p, ok := s.pushes[strings.TrimPrefix(r.URL.Path, "/v1/transactions/")]
		if !ok {
			apiErr(w, 404, "not_found", "unknown reference")
			return
		}
		writeJSON(w, map[string]string{"status": p.Status, "amount": p.Amount, "currency": p.Currency, "reason": p.Reason,
			"provider_ref": "W-" + p.Reference[:8]})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/refunds":
		var raw map[string]string
		_ = json.Unmarshal(body, &raw)
		p, ok := s.pushes[raw["reference"]]
		if !ok || p.Status != "succeeded" {
			apiErr(w, 422, "not_refundable", "transaction not refundable")
			return
		}
		if rf, ok := p.refunds[raw["refund_reference"]]; ok {
			writeJSON(w, rf)
			return
		}
		rf := map[string]string{"status": "succeeded", "refund_ref": "WR-" + raw["refund_reference"][:8]}
		p.refunds[raw["refund_reference"]] = rf
		writeJSON(w, rf)
	default:
		apiErr(w, 404, "not_found", "unknown endpoint")
	}
}

// Decide plays the customer: approve | decline | insufficient_funds | timeout.
// The wallet then sends a signed callback (except timeout: no answer at all).
func (s *Sim) Decide(reference, decision string) {
	s.mu.Lock()
	p, ok := s.pushes[reference]
	if !ok || p.Status != "pending" {
		s.mu.Unlock()
		return
	}
	switch decision {
	case "approve":
		p.Status = "succeeded"
	case "insufficient_funds":
		p.Status, p.Reason = "failed", "insufficient_funds"
	case "timeout":
		p.Status, p.Reason = "failed", "customer_did_not_respond"
	default:
		p.Status, p.Reason = "failed", "declined_by_customer"
	}
	cb := p.CallbackURL
	s.mu.Unlock()
	if cb != "" && decision != "timeout" {
		go s.callback(cb, reference)
	}
}

// Callback sends the signed notification now (tests call it directly).
func (s *Sim) Callback(url, reference string) (int, error) {
	body, _ := json.Marshal(map[string]string{"reference": reference})
	m := hmac.New(sha256.New, []byte(s.WebhookSecret))
	m.Write(body)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Signature", hex.EncodeToString(m.Sum(nil)))
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

func (s *Sim) callback(url, reference string) { _, _ = s.Callback(url, reference) }

// Pending returns the references waiting for the customer.
func (s *Sim) Pending() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, ref := range s.order {
		if s.pushes[ref].Status == "pending" {
			out = append(out, ref)
		}
	}
	return out
}

func (s *Sim) Get(reference string) (Push, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pushes[reference]
	if !ok {
		return Push{}, false
	}
	return *p, true
}

var phonePage = template.Must(template.New("phone").Parse(`<!doctype html><html><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1"><meta http-equiv="refresh" content="3">
<title>Wallet simulator: phone</title><style>body{font-family:system-ui,sans-serif;max-width:420px;margin:24px auto;padding:0 16px}
.p{border:1px solid #ccc;border-radius:12px;padding:16px;margin:12px 0}button{margin:4px 4px 0 0;padding:8px 10px}
.sim{background:#fff4d6;padding:8px;border-radius:8px;font-size:13px}</style></head><body>
<div class="sim">Wallet simulator: this page plays the customer's phone. No real money.</div>
<h3>PIN prompts</h3>{{range .}}<div class="p"><b>Pay {{.Amount}} {{.Currency}}?</b><br>to {{.MSISDN}}<br><small>{{.Reference}}</small>
<form method="post"><input type="hidden" name="reference" value="{{.Reference}}">
<button name="decision" value="approve">Enter PIN (approve)</button><button name="decision" value="decline">Decline</button>
<button name="decision" value="insufficient_funds">Insufficient balance</button><button name="decision" value="timeout">Ignore (timeout)</button></form></div>
{{else}}<p>No prompts. Pay with a wallet on the checkout page.</p>{{end}}</body></html>`))

func (s *Sim) phone(w http.ResponseWriter) {
	s.mu.Lock()
	var list []Push
	for _, ref := range s.order {
		if p := s.pushes[ref]; p.Status == "pending" {
			list = append(list, *p)
		}
	}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = phonePage.Execute(w, list)
}

func apiErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "message": msg})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
