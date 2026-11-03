package transport

import (
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"paygate/internal/contracts/reconciliation"
	"paygate/internal/modules/reconciliation/internal/domain"
	"paygate/internal/modules/reconciliation/internal/service"
	"paygate/internal/modules/reconciliation/internal/store"
	"paygate/internal/platform/apperror"
	"paygate/internal/platform/httpx"
)

type HTTP struct {
	svc *service.Service
	log *slog.Logger
}

func NewHTTP(svc *service.Service, log *slog.Logger) *HTTP { return &HTTP{svc: svc, log: log} }

// RegisterAdmin (platform staff):
//
//	POST /admin/reconciliation/runs                {from, to, provider?}   check a period now
//	GET  /admin/reconciliation/runs                runs, newest first (daily runs are automatic)
//	GET  /admin/reconciliation/runs/{id}           summary + differences found
//	GET  /admin/reconciliation/items?status=open&severity=critical&issue=...   the work queue
//	POST /admin/reconciliation/items/{id}/resolve  {note}   (reconciliation:resolve)
//	POST /admin/reconciliation/settlements?provider=mpgs&date=YYYY-MM-DD[&deposit_amount=&deposit_currency=]
//	     body: the provider's settlement CSV
//	GET  /admin/reconciliation/settlements[/{id}]
func (h *HTTP) RegisterAdmin(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(httpx.Require(reconciliation.PermRun, h.log))
		r.Post("/reconciliation/runs", h.create)
		r.Get("/reconciliation/runs", h.list)
		r.Get("/reconciliation/runs/{id}", h.get)
		r.Get("/reconciliation/items", h.items)
		r.Post("/reconciliation/settlements", h.upload)
		r.Get("/reconciliation/settlements", h.settlements)
		r.Get("/reconciliation/settlements/{id}", h.settlement)
	})
	r.With(httpx.Require(reconciliation.PermResolve, h.log)).Post("/reconciliation/items/{id}/resolve", h.resolve)
}

func (h *HTTP) create(w http.ResponseWriter, r *http.Request) {
	var body struct {
		From     time.Time `json:"from"`
		To       time.Time `json:"to"`
		Provider string    `json:"provider"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.WriteError(w, r, h.log, err)
		return
	}
	run, err := h.svc.CreateRun(r.Context(), body.From, body.To, body.Provider)
	h.respond(w, r, http.StatusAccepted, run, err)
}

func (h *HTTP) list(w http.ResponseWriter, r *http.Request) {
	runs, err := h.svc.ListRuns(r.Context(), httpx.QueryInt(r, "limit", 20, 100))
	h.respond(w, r, http.StatusOK, map[string]any{"data": runs}, err)
}

func (h *HTTP) get(w http.ResponseWriter, r *http.Request) {
	run, err := h.svc.GetRun(r.Context(), chi.URLParam(r, "id"))
	h.respond(w, r, http.StatusOK, run, err)
}

func (h *HTTP) items(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	its, err := h.svc.ListItems(r.Context(), store.ItemFilter{Status: q.Get("status"), Issue: q.Get("issue"),
		Severity: q.Get("severity"), Limit: httpx.QueryInt(r, "limit", 50, 500)})
	if its == nil {
		its = []domain.Item{}
	}
	h.respond(w, r, http.StatusOK, map[string]any{"data": its}, err)
}

func (h *HTTP) resolve(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		httpx.WriteError(w, r, h.log, apperror.NotFound("reconciliation_item_not_found", "reconciliation item not found"))
		return
	}
	var body struct {
		Note string `json:"note"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.WriteError(w, r, h.log, err)
		return
	}
	it, err := h.svc.Resolve(r.Context(), id, body.Note, httpx.Principal(r).Subject)
	h.respond(w, r, http.StatusOK, it, err)
}

func (h *HTTP) upload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, domain.MaxSettlementBytes)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		httpx.WriteError(w, r, h.log, apperror.TooLarge("file_too_large", "settlement files are at most 20 MB"))
		return
	}
	q := r.URL.Query()
	v, err := h.svc.UploadSettlement(r.Context(), service.SettlementUpload{Provider: q.Get("provider"), Date: q.Get("date"),
		FileName: q.Get("file_name"), Data: data, DepositAmount: q.Get("deposit_amount"),
		DepositCurrency: q.Get("deposit_currency"), UploadedBy: httpx.Principal(r).Subject})
	h.respond(w, r, http.StatusAccepted, v, err)
}

func (h *HTTP) settlements(w http.ResponseWriter, r *http.Request) {
	out, err := h.svc.ListSettlements(r.Context(), httpx.QueryInt(r, "limit", 20, 100))
	if out == nil {
		out = []store.Settlement{}
	}
	h.respond(w, r, http.StatusOK, map[string]any{"data": out}, err)
}

func (h *HTTP) settlement(w http.ResponseWriter, r *http.Request) {
	st, err := h.svc.GetSettlement(r.Context(), chi.URLParam(r, "id"))
	h.respond(w, r, http.StatusOK, st, err)
}

func (h *HTTP) respond(w http.ResponseWriter, r *http.Request, status int, v any, err error) {
	if err != nil {
		httpx.WriteError(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, status, v)
}
