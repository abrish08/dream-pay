package transport

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"paygate/internal/contracts/reconciliation"
	"paygate/internal/modules/reconciliation/internal/service"
	"paygate/internal/platform/httpx"
)

type HTTP struct {
	svc *service.Service
	log *slog.Logger
}

func NewHTTP(svc *service.Service, log *slog.Logger) *HTTP { return &HTTP{svc: svc, log: log} }

func (h *HTTP) RegisterAdmin(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(httpx.Require(reconciliation.PermRun, h.log))
		r.Post("/reconciliation/runs", h.create)
		r.Get("/reconciliation/runs", h.list)
		r.Get("/reconciliation/runs/{id}", h.get)
	})
}

func (h *HTTP) create(w http.ResponseWriter, r *http.Request) {
	var body struct {
		From time.Time `json:"from"`
		To   time.Time `json:"to"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.WriteError(w, r, h.log, err)
		return
	}
	run, err := h.svc.CreateRun(r.Context(), body.From, body.To)
	if err != nil {
		httpx.WriteError(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, run)
}

func (h *HTTP) list(w http.ResponseWriter, r *http.Request) {
	runs, err := h.svc.ListRuns(r.Context(), httpx.QueryInt(r, "limit", 20, 100))
	if err != nil {
		httpx.WriteError(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"data": runs})
}

func (h *HTTP) get(w http.ResponseWriter, r *http.Request) {
	run, err := h.svc.GetRun(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, run)
}
