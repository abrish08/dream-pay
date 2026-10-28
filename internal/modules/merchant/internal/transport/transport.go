package transport

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"paygate/internal/contracts/merchants"
	"paygate/internal/modules/merchant/internal/service"
	"paygate/internal/platform/bus"
	"paygate/internal/platform/httpx"
)

type HTTP struct {
	svc *service.Service
	log *slog.Logger
}

func NewHTTP(svc *service.Service, log *slog.Logger) *HTTP { return &HTTP{svc: svc, log: log} }

// RegisterAdmin: platform staff only (the app puts /admin behind RequirePlatform).
func (h *HTTP) RegisterAdmin(r chi.Router) {
	r.With(httpx.Require(merchants.PermManage, h.log)).Post("/merchants", h.create)
	r.With(httpx.Require(merchants.PermRead, h.log)).Get("/merchants/{id}", h.get)
	r.With(httpx.Require(merchants.PermManage, h.log)).Post("/merchants/{id}/status", h.setStatus)
}

func (h *HTTP) create(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name   string `json:"name"`
		Email  string `json:"email"`
		FeeBPS int64  `json:"fee_bps"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.WriteError(w, r, h.log, err)
		return
	}
	m, err := h.svc.Create(r.Context(), body.Name, body.Email, body.FeeBPS)
	if err != nil {
		httpx.WriteError(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, m)
}

func (h *HTTP) get(w http.ResponseWriter, r *http.Request) {
	m, err := h.svc.GetMerchant(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, m)
}

func (h *HTTP) setStatus(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Status string `json:"status"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.WriteError(w, r, h.log, err)
		return
	}
	m, err := h.svc.SetStatus(r.Context(), chi.URLParam(r, "id"), body.Status)
	if err != nil {
		httpx.WriteError(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, m)
}

// RegisterBus answers the merchant module's requests on the central bus.
func RegisterBus(b bus.Bus, svc *service.Service) {
	bus.Handle(b, func(ctx context.Context, q merchants.GetMerchant) (merchants.Merchant, error) {
		return svc.GetMerchant(ctx, q.ID)
	})
}
