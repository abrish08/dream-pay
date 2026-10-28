package transport

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"paygate/internal/contracts/payouts"
	"paygate/internal/modules/payout/internal/service"
	"paygate/internal/platform/httpx"
)

type HTTP struct {
	svc *service.Service
	log *slog.Logger
}

func NewHTTP(svc *service.Service, log *slog.Logger) *HTTP { return &HTTP{svc: svc, log: log} }

// RegisterPublic (merchants):
//
//	POST /v1/payouts       {amount, currency, bank_account{bank_name, account_number, account_name}, description?}
//	                       + Idempotency-Key. Logged-in users only (moving money out never works with an API key).
//	GET  /v1/payouts       ?status=&limit=&starting_after=
//	GET  /v1/payouts/{id}
func (h *HTTP) RegisterPublic(r chi.Router) {
	r.With(httpx.RequireUser(h.log), httpx.Require(payouts.PermWrite, h.log)).Post("/payouts", h.request)
	r.With(httpx.Require(payouts.PermRead, h.log)).Get("/payouts", h.listOwn)
	r.With(httpx.Require(payouts.PermRead, h.log)).Get("/payouts/{id}", h.getOwn)
}

// RegisterAdmin (platform finance):
//
//	GET  /admin/payouts?status=requested
//	POST /admin/payouts/{id}/paid    {bank_reference}
//	POST /admin/payouts/{id}/failed  {reason}
func (h *HTTP) RegisterAdmin(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(httpx.Require(payouts.PermManage, h.log))
		r.Get("/payouts", h.listAll)
		r.Get("/payouts/{id}", h.getAny)
		r.Post("/payouts/{id}/paid", h.markPaid)
		r.Post("/payouts/{id}/failed", h.markFailed)
	})
}

func (h *HTTP) request(w http.ResponseWriter, r *http.Request) {
	var body service.RequestInput
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.WriteError(w, r, h.log, err)
		return
	}
	v, created, err := h.svc.Request(r.Context(), httpx.Principal(r), r.Header.Get("Idempotency-Key"), body)
	if err != nil {
		httpx.WriteError(w, r, h.log, err)
		return
	}
	status := http.StatusCreated
	if !created {
		status = http.StatusOK
		w.Header().Set("Idempotent-Replayed", "true")
	}
	httpx.WriteJSON(w, status, v)
}

func (h *HTTP) listOwn(w http.ResponseWriter, r *http.Request) {
	h.list(w, r, httpx.Principal(r).MerchantID)
}

func (h *HTTP) listAll(w http.ResponseWriter, r *http.Request) {
	h.list(w, r, r.URL.Query().Get("merchant_id"))
}

func (h *HTTP) list(w http.ResponseWriter, r *http.Request, merchantID string) {
	q := r.URL.Query()
	page, err := h.svc.List(r.Context(), merchantID, q.Get("status"), q.Get("starting_after"), httpx.QueryInt(r, "limit", 20, 100))
	h.respond(w, r, page, err)
}

func (h *HTTP) getOwn(w http.ResponseWriter, r *http.Request) {
	v, err := h.svc.Get(r.Context(), httpx.Principal(r).MerchantID, chi.URLParam(r, "id"))
	h.respond(w, r, v, err)
}

func (h *HTTP) getAny(w http.ResponseWriter, r *http.Request) {
	v, err := h.svc.Get(r.Context(), "", chi.URLParam(r, "id"))
	h.respond(w, r, v, err)
}

func (h *HTTP) markPaid(w http.ResponseWriter, r *http.Request) {
	var body struct {
		BankReference string `json:"bank_reference"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.WriteError(w, r, h.log, err)
		return
	}
	v, err := h.svc.MarkPaid(r.Context(), httpx.Principal(r), chi.URLParam(r, "id"), body.BankReference)
	h.respond(w, r, v, err)
}

func (h *HTTP) markFailed(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Reason string `json:"reason"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.WriteError(w, r, h.log, err)
		return
	}
	v, err := h.svc.MarkFailed(r.Context(), httpx.Principal(r), chi.URLParam(r, "id"), body.Reason)
	h.respond(w, r, v, err)
}

func (h *HTTP) respond(w http.ResponseWriter, r *http.Request, v any, err error) {
	if err != nil {
		httpx.WriteError(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, v)
}
