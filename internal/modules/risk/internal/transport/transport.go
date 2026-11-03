package transport

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"paygate/internal/contracts/risk"
	"paygate/internal/modules/risk/internal/service"
	"paygate/internal/platform/httpx"
)

type HTTP struct {
	svc *service.Service
	log *slog.Logger
}

func NewHTTP(svc *service.Service, log *slog.Logger) *HTTP { return &HTTP{svc: svc, log: log} }

// RegisterAdmin (platform staff):
//
//	GET    /admin/risk/blocklist?kind=email        (risk:read)
//	POST   /admin/risk/blocklist   {kind, value, reason}   (risk:manage)
//	DELETE /admin/risk/blocklist/{id}              (risk:manage)
//	GET    /admin/risk/decisions?merchant_id=&payment_id=&action=block   (risk:read)
func (h *HTTP) RegisterAdmin(r chi.Router) {
	r.With(httpx.Require(risk.PermRead, h.log)).Get("/risk/blocklist", func(w http.ResponseWriter, r *http.Request) {
		out, err := h.svc.Blocks(r.Context(), r.URL.Query().Get("kind"), httpx.QueryInt(r, "limit", 50, 500))
		h.respond(w, r, http.StatusOK, map[string]any{"data": out}, err)
	})
	r.With(httpx.Require(risk.PermManage, h.log)).Post("/risk/blocklist", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Kind, Value, Reason string }
		if err := httpx.DecodeJSON(r, &body); err != nil {
			httpx.WriteError(w, r, h.log, err)
			return
		}
		e, err := h.svc.Block(r.Context(), body.Kind, body.Value, body.Reason, httpx.Principal(r).Subject)
		h.respond(w, r, http.StatusCreated, e, err)
	})
	r.With(httpx.Require(risk.PermManage, h.log)).Delete("/risk/blocklist/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := h.svc.Unblock(r.Context(), chi.URLParam(r, "id")); err != nil {
			httpx.WriteError(w, r, h.log, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	r.With(httpx.Require(risk.PermRead, h.log)).Get("/risk/decisions", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		out, err := h.svc.Decisions(r.Context(), q.Get("merchant_id"), q.Get("payment_id"), q.Get("action"),
			httpx.QueryInt(r, "limit", 50, 500))
		h.respond(w, r, http.StatusOK, map[string]any{"data": out}, err)
	})
}

func (h *HTTP) respond(w http.ResponseWriter, r *http.Request, status int, v any, err error) {
	if err != nil {
		httpx.WriteError(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, status, v)
}
