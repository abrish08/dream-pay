package transport

import (
	"context"
	"html/template"
	"io"
	"log/slog"
	"net/http"

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

// RegisterBus answers the provider module's requests on the central bus.
func RegisterBus(b bus.Bus, svc *service.Service) {
	bus.Handle(b, svc.InitiateCheckout)
	bus.Handle(b, svc.Refund)
	bus.Handle(b, func(ctx context.Context, q providers.VerifyTransaction) (providers.Transaction, error) {
		return svc.VerifyTransaction(ctx, q.Provider, q.TxRef)
	})
}
