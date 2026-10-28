package transport

import (
	"html/template"
	"net/http"

	"github.com/go-chi/chi/v5"

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
}

var checkoutTmpl = template.Must(template.New("checkout").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Pay {{.Merchant}}</title>
<style>
:root{--fg:#1b1f27;--muted:#5b6475;--line:#d8dee9;--bg:#fff;--accent:#1f5fbf}
@media (prefers-color-scheme:dark){:root{--fg:#e6e9ef;--muted:#9aa3b2;--line:#2e3440;--bg:#14171c;--accent:#5b9bf5}}
body{font-family:system-ui,sans-serif;background:var(--bg);color:var(--fg);max-width:440px;margin:40px auto;padding:0 16px}
.card{border:1px solid var(--line);border-radius:14px;padding:24px}
.muted{color:var(--muted);font-size:14px}.amt{font-size:34px;font-weight:700;margin:12px 0 4px}
button{display:block;width:100%;text-align:left;padding:14px 16px;margin-top:10px;border:1px solid var(--line);
border-radius:10px;background:transparent;color:var(--fg);font-size:16px;cursor:pointer}
button:hover{border-color:var(--accent)}.err{color:#c0392b}
</style></head><body><div class="card">
<div class="muted">{{.Merchant}}</div>
<div class="amt">{{.Amount}} {{.Currency}}</div>
{{if .Description}}<div class="muted">{{.Description}}</div>{{end}}
<div class="muted">Reference {{.Reference}}</div>
{{if .Error}}<p class="err">{{.Error}}</p>{{end}}
{{if .Open}}
  {{if .Methods}}<p>Choose how to pay</p>
  <form method="post">{{range .Methods}}<button name="method" value="{{.Method}}">{{.Label}}</button>{{end}}</form>
  {{else}}<p class="err">No payment method is available right now. Please try again shortly.</p>{{end}}
{{else}}<p>This payment is {{.Status}}.</p>
  {{if .ReturnURL}}<p><a href="{{.ReturnURL}}">Return to {{.Merchant}}</a></p>{{end}}
{{end}}
</div></body></html>`))

func (h *HTTP) checkoutPage(w http.ResponseWriter, r *http.Request) {
	h.renderCheckout(w, r, "")
}

func (h *HTTP) renderCheckout(w http.ResponseWriter, r *http.Request, errMsg string) {
	v, err := h.svc.Checkout(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, r, h.log, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY") // no clickjacking of the pay buttons
	_ = checkoutTmpl.Execute(w, map[string]any{
		"Merchant": v.Merchant, "Amount": money.FormatMajor(v.Amount), "Currency": v.Currency,
		"Description": v.Description, "Reference": v.Reference, "Open": v.Open, "Status": v.Status,
		"Methods": v.Methods, "ReturnURL": v.ReturnURL, "Error": errMsg,
	})
}

func (h *HTTP) chooseMethod(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		httpx.WriteError(w, r, h.log, err)
		return
	}
	url, err := h.svc.ChooseMethod(r.Context(), chi.URLParam(r, "id"), r.PostForm.Get("method"))
	if err != nil {
		h.renderCheckout(w, r, "This payment method could not be started. Please choose another or try again.")
		return
	}
	http.Redirect(w, r, url, http.StatusSeeOther)
}
