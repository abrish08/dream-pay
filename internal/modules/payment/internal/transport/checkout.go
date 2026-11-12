package transport

import (
	"html/template"
	"net/http"

	"github.com/go-chi/chi/v5"

	"paygate/internal/contracts/providers"
	"paygate/internal/platform/apperror"
	"paygate/internal/platform/httpx"
	"paygate/internal/platform/money"
)

// RegisterOpen: the hosted checkout page. A payment created without a
// payment_method gets checkout_url = /pay/{id}; the customer picks how to pay
// here and is sent on to that provider.
//
//	GET  /pay/{id}   amount, merchant and the methods available now
//	POST /pay/{id}   method=telebirr|cbe_birr|mpesa|card|bank_transfer -> 303 to the provider
//
// The payment id is a random UUID: it is the bearer of the page, like a
// checkout session id. Nothing here can change the amount or the merchant.
func (h *HTTP) RegisterOpen(r chi.Router) {
	r.Get("/pay/{id}", h.checkoutPage)
	r.Post("/pay/{id}", h.chooseMethod)
	r.Get("/l/{id}", h.openLink) // payment links
	h.registerStatus(r)
}

// openLink: GET /l/{id} opens a payment for the link and sends the customer
// to the checkout page. The visitor's IP goes to the risk checks.
func (h *HTTP) openLink(w http.ResponseWriter, r *http.Request) {
	url, err := h.svc.OpenLink(r.Context(), chi.URLParam(r, "id"), httpx.ClientIP(r, h.TrustProxy))
	if err != nil {
		code := http.StatusNotFound
		msg := "This payment link does not exist."
		switch apperror.From(err).Code {
		case "payment_link_closed":
			code, msg = http.StatusGone, "This payment link is no longer active."
		case "risk_declined", "limit_exceeded":
			code, msg = http.StatusUnprocessableEntity, "This payment cannot be made. Please contact the merchant."
		case "payment_link_not_found":
		default:
			h.log.Error("open payment link", "err", err)
			code, msg = http.StatusServiceUnavailable, "Payments are unavailable right now. Please try again."
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(msg))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, url, http.StatusSeeOther)
}

// registerStatus adds GET /pay/{id}/status (JSON) for the waiting page.
func (h *HTTP) registerStatus(r chi.Router) {
	r.Get("/pay/{id}/status", func(w http.ResponseWriter, r *http.Request) {
		status, reason, err := h.svc.CheckoutStatus(r.Context(), chi.URLParam(r, "id"))
		if err != nil {
			httpx.WriteError(w, r, h.log, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": status, "failure_reason": reason})
	})
}

var checkoutTmpl = template.Must(template.New("checkout").Parse(`<!doctype html>
<html lang="{{.L.Lang}}"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.L.Pay}} {{.V.Merchant}}</title>
<style>
:root{--fg:#1b1f27;--muted:#5b6475;--line:#d8dee9;--bg:#fff;--accent:#1f5fbf;--ok:#1f7a4d;--bad:#b42318}
@media (prefers-color-scheme:dark){:root{--fg:#e6e9ef;--muted:#9aa3b2;--line:#2e3440;--bg:#14171c;--accent:#5b9bf5;--ok:#4cc38a;--bad:#f2766b}}
body{font-family:system-ui,"Noto Sans Ethiopic","Nyala",sans-serif;background:var(--bg);color:var(--fg);max-width:440px;margin:32px auto;padding:0 16px}
.card{border:1px solid var(--line);border-radius:14px;padding:24px}
.muted{color:var(--muted);font-size:14px}.amt{font-size:34px;font-weight:700;margin:12px 0 4px}
button,.btn{display:block;width:100%;box-sizing:border-box;text-align:left;padding:14px 16px;margin-top:10px;border:1px solid var(--line);
border-radius:10px;background:transparent;color:var(--fg);font-size:16px;cursor:pointer;text-decoration:none}
button:hover{border-color:var(--accent)}.primary{background:var(--accent);color:#fff;border-color:var(--accent);text-align:center}
input{width:100%;box-sizing:border-box;padding:12px;border:1px solid var(--line);border-radius:10px;font-size:18px;background:transparent;color:var(--fg)}
.err{color:var(--bad)}.ok{color:var(--ok)}h2{font-size:20px;margin:16px 0 8px}
.lang{text-align:right;font-size:13px;margin-bottom:8px}.lang a{color:var(--muted)}
.box{border:1px dashed var(--line);border-radius:10px;padding:12px;margin:10px 0}.big{font-size:22px;font-weight:700;letter-spacing:1px}
.spin{width:28px;height:28px;border:3px solid var(--line);border-top-color:var(--accent);border-radius:50%;animation:s 1s linear infinite;margin:12px 0}
@keyframes s{to{transform:rotate(360deg)}}
</style></head><body>
<div class="lang"><a href="?lang=en">English</a> · <a href="?lang=am">አማርኛ</a></div>
<div class="card">
<div class="muted">{{.V.Merchant}}</div>
<div class="amt">{{.Amount}} {{.V.Currency}}</div>
{{if .V.Description}}<div class="muted">{{.V.Description}}</div>{{end}}
<div class="muted">{{.L.Reference}} {{.V.Reference}}</div>
{{if .Error}}<p class="err">{{.Error}}</p>{{end}}

{{if eq .State "choose"}}
  {{if .V.Methods}}<h2>{{.L.Choose}}</h2>
  <form method="post">{{range .Methods}}<button name="method" value="{{.Method}}">{{.Label}}</button>{{end}}</form>
  {{else}}<p class="err">{{.L.NoMethods}}</p>{{end}}
{{else if eq .State "phone"}}
  <h2>{{.MethodLabel}}</h2>
  <form method="post"><input type="hidden" name="method" value="{{.V.Method}}">
  <label class="muted" for="phone">{{.L.PhoneLabel}}</label>
  <input id="phone" name="phone" type="tel" inputmode="tel" autocomplete="tel" placeholder="09XX XXX XXX" required autofocus>
  <button class="primary">{{.L.Pay}} {{.Amount}} {{.V.Currency}}</button></form>
{{else if eq .State "wallet"}}
  <h2>{{.L.CheckPhone}}</h2><div class="spin"></div>
  <p>{{.L.ApprovePIN}}</p>{{if .V.PhoneMasked}}<p class="muted">{{.V.PhoneMasked}}</p>{{end}}
  <p class="muted">{{.L.Waiting}}</p>
{{else if eq .State "bank"}}
  <h2>{{.L.BankTitle}}</h2>
  <p>{{.L.TransferExactly}}</p><div class="big">{{.Amount}} {{.V.Currency}}</div>
  <p>{{.L.WriteReference}}</p><div class="box big">{{.V.Instructions.Reference}}</div>
  {{range .V.Instructions.Accounts}}<div class="box"><b>{{.Bank}}</b><br>{{$.L.AccountNumber}}: <b>{{.AccountNumber}}</b><br>{{$.L.AccountName}}: {{.AccountName}}</div>{{end}}
  <p class="muted">{{.L.BankConfirm}}</p>
{{else if eq .State "waiting"}}
  <h2>{{.L.Confirming}}</h2><div class="spin"></div><p class="muted">{{.L.Waiting}}</p>
{{else if eq .State "succeeded"}}
  <h2 class="ok">✓ {{.L.Success}}</h2>
  {{if .V.ReturnURL}}<a class="btn primary" href="{{.V.ReturnURL}}">{{.L.Return}} {{.V.Merchant}}</a>{{end}}
{{else}}
  <h2 class="err">{{.L.Failed}}</h2><p>{{.FailureText}}</p>
  {{if .V.ReturnURL}}<a class="btn primary" href="{{.V.ReturnURL}}">{{.L.Return}} {{.V.Merchant}}</a>{{end}}
{{end}}
</div>
{{if .Poll}}<script>
(function(){var s={{.V.Status}};function check(){fetch("/pay/{{.V.ID}}/status",{cache:"no-store"}).then(function(r){return r.json()})
.then(function(d){if(d.status!==s){location.reload()}}).catch(function(){})}setInterval(check,{{.Poll}});})();
</script>{{end}}
</body></html>`))

func (h *HTTP) checkoutPage(w http.ResponseWriter, r *http.Request) {
	h.renderCheckout(w, r, "", "")
}

// renderCheckout shows the page for the payment's current state. phoneFor:
// the wallet method the customer picked, when its phone number is needed.
func (h *HTTP) renderCheckout(w http.ResponseWriter, r *http.Request, errMsg, phoneFor string) {
	v, err := h.svc.Checkout(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, r, h.log, err)
		return
	}
	l := lang(w, r)
	if phoneFor != "" {
		v.Method = phoneFor
	}
	state, poll := "failed", 0
	switch {
	case v.Open && (phoneFor != "" || (v.Method != "" && providers.IsWallet(v.Method) && !v.PhoneKnown)):
		state = "phone"
	case v.Open:
		state = "choose"
	case v.Status == "processing" && v.Instructions.Kind == "bank_transfer":
		state, poll = "bank", 15000
	case v.Status == "processing" && providers.IsWallet(v.Method):
		state, poll = "wallet", 3000
	case v.Status == "processing":
		state, poll = "waiting", 3000
	case v.Status == "succeeded" || v.Status == "partially_refunded" || v.Status == "refunded":
		state = "succeeded"
	}
	methods := make([]providers.MethodInfo, len(v.Methods))
	for i, m := range v.Methods {
		m.Label = l.methodLabel(m.Method, m.Label)
		methods[i] = m
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY") // no clickjacking of the pay buttons
	_ = checkoutTmpl.Execute(w, map[string]any{"V": v, "L": l, "State": state, "Poll": poll, "Methods": methods,
		"Amount": money.FormatMajor(v.Amount), "Error": errMsg, "MethodLabel": l.methodLabel(v.Method, v.Method),
		"FailureText": l.failure(v.Status, v.FailureReason)})
}

func (h *HTTP) chooseMethod(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		httpx.WriteError(w, r, h.log, err)
		return
	}
	l := lang(w, r)
	method, phone := r.PostForm.Get("method"), r.PostForm.Get("phone")
	url, err := h.svc.ChooseMethod(r.Context(), chi.URLParam(r, "id"), method, phone)
	switch code := apperror.From(err).Code; {
	case err == nil:
	case code == "phone_required":
		h.renderCheckout(w, r, "", method)
		return
	case code == "invalid_phone":
		h.renderCheckout(w, r, l.BadPhone, method)
		return
	default:
		h.renderCheckout(w, r, l.StartFailed, "")
		return
	}
	if url == "" { // the provider's answer is being confirmed: show the payment's status
		url = "/pay/" + chi.URLParam(r, "id")
	}
	http.Redirect(w, r, url, http.StatusSeeOther)
}
