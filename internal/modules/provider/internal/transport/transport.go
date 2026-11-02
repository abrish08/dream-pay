package transport

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"regexp"

	"github.com/go-chi/chi/v5"

	"paygate/internal/contracts/providers"
	"paygate/internal/modules/provider/internal/service"
	"paygate/internal/platform/bus"
	"paygate/internal/platform/httpx"
	"paygate/internal/platform/money"
)

type HTTP struct {
	svc *service.Service
	log *slog.Logger
}

func NewHTTP(svc *service.Service, log *slog.Logger) *HTTP { return &HTTP{svc: svc, log: log} }

// RegisterOpen: endpoints called by providers and customers (no API key).
func (h *HTTP) RegisterOpen(r chi.Router) {
	r.Post("/providers/{provider}/callback", h.callback)
	r.Get("/providers/{provider}/callback", h.callback)
	r.Get("/providers/mock/checkout/{txRef}", h.mockPage)
	r.Post("/providers/mock/checkout/{txRef}", h.mockComplete)
	r.Get("/providers/{provider}/pay/{txRef}", h.cardPage)    // opens the card provider's payment page
	r.Get("/providers/{provider}/return/{txRef}", h.returned) // the customer comes back
}

var sessionRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// cardPage loads Mastercard's checkout script and sends the customer to the
// card form hosted by Mastercard. Card numbers never reach this server.
var cardPage = template.Must(template.New("card").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Card payment</title>
<style nonce="{{.Nonce}}">body{font-family:system-ui,sans-serif;max-width:420px;margin:48px auto;padding:0 16px;color:#1b1f27}
.amt{font-size:28px;font-weight:700;margin:12px 0}.muted{color:#5b6475;font-size:14px}#err{color:#b42318;display:none}</style>
<script nonce="{{.Nonce}}">function paygateCardError(e){var el=document.getElementById("err");el.style.display="block";
el.textContent="The card page could not be opened. Please go back and try again.";}</script>
<script nonce="{{.Nonce}}" src="{{.ScriptURL}}" data-error="paygateCardError" data-cancel="{{.CancelURL}}"></script>
</head><body>
<p class="muted">Redirecting to the secure card page…</p>
<div class="amt">{{.Amount}} {{.Currency}}</div>
<p id="err"></p>
<noscript>JavaScript is needed to pay by card.</noscript>
<script nonce="{{.Nonce}}">Checkout.configure({session:{id:{{.Session}}}});Checkout.showPaymentPage();</script>
</body></html>`))

func (h *HTTP) cardPage(w http.ResponseWriter, r *http.Request) {
	provider, txRef := chi.URLParam(r, "provider"), chi.URLParam(r, "txRef")
	session := r.URL.Query().Get("session")
	if !sessionRe.MatchString(session) {
		httpx.WriteError(w, r, h.log, providers.ErrTransactionNotFound)
		return
	}
	p, err := h.svc.HostedCardPage(r.Context(), provider, txRef)
	if err != nil {
		httpx.WriteError(w, r, h.log, err)
		return
	}
	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce)
	n := base64.RawStdEncoding.EncodeToString(nonce)
	// Only our inline code (nonce) and the provider's host may run or be contacted.
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'nonce-"+n+"' "+p.Origin+
		"; style-src 'unsafe-inline' "+p.Origin+"; img-src "+p.Origin+" data:; connect-src "+p.Origin+
		"; frame-src "+p.Origin+"; form-action "+p.Origin+"; base-uri 'none'; frame-ancestors 'none'")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = cardPage.Execute(w, map[string]any{"Nonce": n, "ScriptURL": p.ScriptURL, "Session": session,
		"Amount": p.Amount, "Currency": p.Currency,
		"CancelURL": p.CancelURL})
}

// returned: the customer is back from the provider; the status is read from
// the provider, then the customer goes to the merchant's return_url (or the
// payment's status page).
func (h *HTTP) returned(w http.ResponseWriter, r *http.Request) {
	t, err := h.svc.Return(r.Context(), chi.URLParam(r, "provider"), chi.URLParam(r, "txRef"))
	if err != nil {
		httpx.WriteError(w, r, h.log, err)
		return
	}
	to := t.ReturnURL
	if to == "" {
		to = "/pay/" + t.TxRef
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, to, http.StatusSeeOther)
}

func (h *HTTP) callback(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		httpx.WriteError(w, r, h.log, err)
		return
	}
	txRef, err := h.svc.HandleCallback(r.Context(), chi.URLParam(r, "provider"), r, body)
	if err != nil {
		httpx.WriteError(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"received": true, "tx_ref": txRef})
}

var page = template.Must(template.New("mock").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Mock checkout</title>
<style>body{font-family:system-ui,sans-serif;max-width:420px;margin:48px auto;padding:0 16px;color:#1b1f27}
.card{border:1px solid #d8dee9;border-radius:12px;padding:24px}h1{font-size:20px;margin:0 0 4px}
.amt{font-size:32px;font-weight:700;margin:16px 0}button{width:100%;padding:12px;border-radius:8px;border:0;
font-size:16px;margin-top:8px;cursor:pointer}.ok{background:#1f7a4d;color:#fff}.no{background:#eceff4;color:#1b1f27}
.muted{color:#5b6475;font-size:13px}</style></head><body><div class="card">
<h1>Mock payment provider</h1><div class="muted">Test mode — no real money moves</div>
{{if .Done}}<p class="amt">{{.Status}}</p><p class="muted">Reference {{.TxRef}}</p>
{{else}}<div class="amt">{{.Amount}} {{.Currency}}</div>
<form method="post"><input type="hidden" name="result" value="success"><button class="ok">Pay</button></form>
<form method="post"><input type="hidden" name="result" value="failed"><button class="no">Decline</button></form>
{{end}}</div></body></html>`))

func (h *HTTP) mockPage(w http.ResponseWriter, r *http.Request) {
	t, err := h.svc.GetTransaction(r.Context(), chi.URLParam(r, "txRef"))
	if err != nil {
		httpx.WriteError(w, r, h.log, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = page.Execute(w, map[string]any{
		"TxRef": t.TxRef, "Amount": money.FormatMajor(t.Amount), "Currency": t.Currency,
		"Done": t.Status != providers.StatusPending, "Status": t.Status,
	})
}

func (h *HTTP) mockComplete(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		httpx.WriteError(w, r, h.log, err)
		return
	}
	t, err := h.svc.CompleteMockPayment(r.Context(), chi.URLParam(r, "txRef"), r.PostForm.Get("result") == "success")
	if err != nil {
		httpx.WriteError(w, r, h.log, err)
		return
	}
	if t.ReturnURL != "" {
		http.Redirect(w, r, t.ReturnURL, http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = page.Execute(w, map[string]any{"TxRef": t.TxRef, "Done": true, "Status": t.Status})
}

// RegisterPublic: GET /v1/payment-methods?currency=ETB, what a merchant can
// offer right now (API keys see their own environment).
func (h *HTTP) RegisterPublic(r chi.Router) {
	r.Get("/payment-methods", func(w http.ResponseWriter, r *http.Request) {
		p := httpx.Principal(r)
		env := p.Environment
		if env == "" {
			env = r.URL.Query().Get("environment")
		}
		if env == "" {
			env = "live"
		}
		ms, err := h.svc.ListMethods(r.Context(), providers.ListMethods{Environment: env, Currency: r.URL.Query().Get("currency")})
		if err != nil {
			httpx.WriteError(w, r, h.log, err)
			return
		}
		for i := range ms {
			ms[i].Providers = nil // routing is internal
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"data": ms})
	})
}

// RegisterAdmin: GET /admin/providers, adapters, routing and health.
func (h *HTTP) RegisterAdmin(r chi.Router) {
	r.With(httpx.Require(providers.PermRead, h.log)).Get("/providers", func(w http.ResponseWriter, r *http.Request) {
		live, _ := h.svc.ListMethods(r.Context(), providers.ListMethods{Environment: "live", Currency: "ETB"})
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"providers": h.svc.Health(), "live_routes_etb": live})
	})
}

// RegisterBus answers the provider module's requests on the central bus.
func RegisterBus(b bus.Bus, svc *service.Service) {
	bus.Handle(b, svc.InitiateCheckout)
	bus.Handle(b, svc.ListMethods)
	bus.Handle(b, svc.Refund)
	bus.Handle(b, func(ctx context.Context, q providers.VerifyTransaction) (providers.Transaction, error) {
		return svc.VerifyTransaction(ctx, q.Provider, q.TxRef)
	})
}
