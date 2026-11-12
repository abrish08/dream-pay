// Package transport: HTTP routes of the audit log.
//
//	GET /v1/audit-logs              the caller's own business only (audit:read)
//	GET /admin/audit-logs           every action, any merchant (audit:read_all)
//	GET /admin/audit-logs/verify    recompute the hash chain (audit:read_all)
//
// Filters: actor_id, actor_kind, action (prefix), resource_id, outcome,
// from, to (RFC 3339), limit (max 200), starting_after (cursor).
package transport

import (
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"paygate/internal/contracts/audit"
	"paygate/internal/modules/audit/internal/service"
	"paygate/internal/modules/audit/internal/store"
	"paygate/internal/platform/apperror"
	"paygate/internal/platform/httpx"
)

type HTTP struct {
	svc *service.Service
	log *slog.Logger
}

func NewHTTP(svc *service.Service, log *slog.Logger) *HTTP { return &HTTP{svc: svc, log: log} }

func (h *HTTP) RegisterPublic(r chi.Router) {
	r.With(httpx.Require(audit.PermRead, h.log)).Get("/audit-logs", func(w http.ResponseWriter, r *http.Request) {
		p := httpx.Principal(r)
		if p.MerchantID == "" { // defence in depth: /v1 already requires a merchant
			httpx.WriteError(w, r, h.log, apperror.Forbidden("merchant_only", "this log belongs to a business"))
			return
		}
		f, err := filter(r)
		if err != nil {
			httpx.WriteError(w, r, h.log, err)
			return
		}
		f.MerchantID = p.MerchantID // never from the query: a merchant sees only its own activity
		h.list(w, r, f)
	})
}

func (h *HTTP) RegisterAdmin(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(httpx.Require(audit.PermReadAll, h.log))
		r.Get("/audit-logs", func(w http.ResponseWriter, r *http.Request) {
			f, err := filter(r)
			if err != nil {
				httpx.WriteError(w, r, h.log, err)
				return
			}
			f.MerchantID = r.URL.Query().Get("merchant_id") // optional for staff
			h.list(w, r, f)
		})
		r.Get("/audit-logs/verify", func(w http.ResponseWriter, r *http.Request) {
			res, err := h.svc.Verify(r.Context())
			if err != nil {
				httpx.WriteError(w, r, h.log, err)
				return
			}
			httpx.WriteJSON(w, http.StatusOK, res)
		})
	})
}

func (h *HTTP) list(w http.ResponseWriter, r *http.Request, f store.Filter) {
	page, err := h.svc.List(r.Context(), f)
	if err != nil {
		httpx.WriteError(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, page)
}

func filter(r *http.Request) (store.Filter, error) {
	q := r.URL.Query()
	f := store.Filter{ActorID: q.Get("actor_id"), ActorKind: q.Get("actor_kind"), Action: q.Get("action"),
		ResourceID: q.Get("resource_id"), Outcome: q.Get("outcome"), Limit: httpx.QueryInt(r, "limit", 50, 200)}
	var err error
	if f.From, err = httpx.QueryTime(r, "from"); err != nil {
		return f, err
	}
	if f.To, err = httpx.QueryTime(r, "to"); err != nil {
		return f, err
	}
	if v := q.Get("starting_after"); v != "" {
		if f.BeforeID, err = strconv.ParseInt(v, 10, 64); err != nil || f.BeforeID <= 0 {
			return f, apperror.Invalid("invalid_cursor", "starting_after must be the next_cursor of the previous page")
		}
	}
	return f, nil
}
