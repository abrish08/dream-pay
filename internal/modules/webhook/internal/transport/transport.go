package transport

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"paygate/internal/contracts/webhooks"
	"paygate/internal/modules/webhook/internal/service"
	"paygate/internal/platform/httpx"
)

type HTTP struct {
	svc *service.Service
	log *slog.Logger
}

func NewHTTP(svc *service.Service, log *slog.Logger) *HTTP { return &HTTP{svc: svc, log: log} }

func (h *HTTP) RegisterPublic(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(httpx.Require(webhooks.PermManage, h.log))
		r.Post("/webhook-endpoints", h.create)
		r.Get("/webhook-endpoints", h.list)
		r.Delete("/webhook-endpoints/{id}", h.delete)
		r.Post("/webhook-endpoints/{id}/rotate-secret", h.rotate)
		r.Get("/webhook-deliveries", h.deliveries)
		r.Post("/webhook-deliveries/{id}/retry", h.retry)
	})
}

func (h *HTTP) create(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL    string   `json:"url"`
		Events []string `json:"events"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.WriteError(w, r, h.log, err)
		return
	}
	ep, err := h.svc.CreateEndpoint(r.Context(), httpx.Principal(r).MerchantID, body.URL, body.Events)
	if err != nil {
		httpx.WriteError(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, ep)
}

func (h *HTTP) rotate(w http.ResponseWriter, r *http.Request) {
	ep, err := h.svc.RotateSecret(r.Context(), httpx.Principal(r).MerchantID, chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, ep)
}

func (h *HTTP) retry(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.RetryDelivery(r.Context(), httpx.Principal(r).MerchantID, chi.URLParam(r, "id")); err != nil {
		httpx.WriteError(w, r, h.log, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (h *HTTP) list(w http.ResponseWriter, r *http.Request) {
	eps, err := h.svc.ListEndpoints(r.Context(), httpx.Principal(r).MerchantID)
	if err != nil {
		httpx.WriteError(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"data": eps})
}

func (h *HTTP) delete(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.DeleteEndpoint(r.Context(), httpx.Principal(r).MerchantID, chi.URLParam(r, "id")); err != nil {
		httpx.WriteError(w, r, h.log, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *HTTP) deliveries(w http.ResponseWriter, r *http.Request) {
	ds, err := h.svc.ListDeliveries(r.Context(), httpx.Principal(r).MerchantID, httpx.QueryInt(r, "limit", 20, 100))
	if err != nil {
		httpx.WriteError(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"data": ds})
}
