package transport

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"paygate/internal/contracts/payments"
	"paygate/internal/contracts/refunds"
	"paygate/internal/modules/refund/internal/service"
	"paygate/internal/platform/httpx"
)

type HTTP struct {
	svc *service.Service
	log *slog.Logger
}

func NewHTTP(svc *service.Service, log *slog.Logger) *HTTP { return &HTTP{svc: svc, log: log} }

func (h *HTTP) RegisterPublic(r chi.Router) {
	r.With(httpx.Require(refunds.PermWrite, h.log)).Post("/refunds", h.create)
	r.With(httpx.Require(payments.PermRead, h.log)).Get("/refunds/{id}", h.get)
}

func (h *HTTP) create(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PaymentID string `json:"payment_id"`
		Amount    int64  `json:"amount"`
		Reason    string `json:"reason"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.WriteError(w, r, h.log, err)
		return
	}
	v, created, err := h.svc.Create(r.Context(), service.CreateInput{
		MerchantID: httpx.Principal(r).MerchantID, IdempotencyKey: r.Header.Get("Idempotency-Key"),
		PaymentID: body.PaymentID, Amount: body.Amount, Reason: body.Reason,
	})
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

func (h *HTTP) get(w http.ResponseWriter, r *http.Request) {
	v, err := h.svc.Get(r.Context(), httpx.Principal(r).MerchantID, chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, v)
}
