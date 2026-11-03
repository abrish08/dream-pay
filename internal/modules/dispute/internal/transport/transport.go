package transport

import (
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"paygate/internal/contracts/disputes"
	"paygate/internal/modules/dispute/internal/domain"
	"paygate/internal/modules/dispute/internal/service"
	"paygate/internal/platform/apperror"
	"paygate/internal/platform/httpx"
)

type HTTP struct {
	svc *service.Service
	log *slog.Logger
}

func NewHTTP(svc *service.Service, log *slog.Logger) *HTTP { return &HTTP{svc: svc, log: log} }

// RegisterPublic (merchant users; never API keys):
//
//	GET  /v1/disputes?status=needs_response
//	GET  /v1/disputes/{id}
//	POST /v1/disputes/{id}/evidence     multipart: text, files (PDF/JPEG/PNG, 5 MB, 5 files)
//	POST /v1/disputes/{id}/submit       send the evidence for review
//	POST /v1/disputes/{id}/accept       do not contest (lost)
//	GET  /v1/disputes/{id}/files/{fileId}
func (h *HTTP) RegisterPublic(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(httpx.RequireUser(h.log))
		r.With(httpx.Require(disputes.PermRead, h.log)).Get("/disputes", func(w http.ResponseWriter, r *http.Request) {
			out, err := h.svc.List(r.Context(), httpx.Principal(r).MerchantID, r.URL.Query().Get("status"), httpx.QueryInt(r, "limit", 20, 100))
			h.respond(w, r, http.StatusOK, map[string]any{"data": out}, err)
		})
		r.With(httpx.Require(disputes.PermRead, h.log)).Get("/disputes/{id}", func(w http.ResponseWriter, r *http.Request) {
			v, err := h.svc.Get(r.Context(), httpx.Principal(r).MerchantID, chi.URLParam(r, "id"))
			h.respond(w, r, http.StatusOK, v, err)
		})
		r.With(httpx.Require(disputes.PermRead, h.log)).Get("/disputes/{id}/files/{fileId}", h.fileFor(true))
		r.Group(func(r chi.Router) {
			r.Use(httpx.Require(disputes.PermWrite, h.log))
			r.Post("/disputes/{id}/evidence", h.evidence)
			r.Post("/disputes/{id}/submit", func(w http.ResponseWriter, r *http.Request) {
				p := httpx.Principal(r)
				v, err := h.svc.Submit(r.Context(), p.MerchantID, p.Subject, chi.URLParam(r, "id"))
				h.respond(w, r, http.StatusOK, v, err)
			})
			r.Post("/disputes/{id}/accept", func(w http.ResponseWriter, r *http.Request) {
				p := httpx.Principal(r)
				v, err := h.svc.Accept(r.Context(), p.MerchantID, p.Subject, chi.URLParam(r, "id"))
				h.respond(w, r, http.StatusOK, v, err)
			})
		})
	})
}

// RegisterAdmin (staff, disputes:manage):
//
//	POST /admin/disputes                 {payment_id, amount?, reason_code, reason, provider_case_id, evidence_due_by?}
//	GET  /admin/disputes?status=
//	GET  /admin/disputes/{id}
//	GET  /admin/disputes/{id}/files/{fileId}
//	POST /admin/disputes/{id}/close      {outcome: won|lost, fee?, note}
func (h *HTTP) RegisterAdmin(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(httpx.Require(disputes.PermManage, h.log))
		r.Post("/disputes", func(w http.ResponseWriter, r *http.Request) {
			var in service.OpenInput
			if err := httpx.DecodeJSON(r, &in); err != nil {
				httpx.WriteError(w, r, h.log, err)
				return
			}
			v, err := h.svc.Open(r.Context(), httpx.Principal(r).Subject, in)
			h.respond(w, r, http.StatusCreated, v, err)
		})
		r.Get("/disputes", func(w http.ResponseWriter, r *http.Request) {
			out, err := h.svc.List(r.Context(), "", r.URL.Query().Get("status"), httpx.QueryInt(r, "limit", 20, 100))
			h.respond(w, r, http.StatusOK, map[string]any{"data": out}, err)
		})
		r.Get("/disputes/{id}", func(w http.ResponseWriter, r *http.Request) {
			v, err := h.svc.Get(r.Context(), "", chi.URLParam(r, "id"))
			h.respond(w, r, http.StatusOK, v, err)
		})
		r.Get("/disputes/{id}/files/{fileId}", h.fileFor(false))
		r.Post("/disputes/{id}/close", func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Outcome string `json:"outcome"`
				Fee     int64  `json:"fee"`
				Note    string `json:"note"`
			}
			if err := httpx.DecodeJSON(r, &body); err != nil {
				httpx.WriteError(w, r, h.log, err)
				return
			}
			v, err := h.svc.Close(r.Context(), "", httpx.Principal(r).Subject, chi.URLParam(r, "id"), body.Outcome, body.Fee, body.Note)
			h.respond(w, r, http.StatusOK, v, err)
		})
	})
}

func (h *HTTP) evidence(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, int64(domain.MaxFiles*domain.MaxFileBytes)+(1<<20))
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		httpx.WriteError(w, r, h.log, apperror.Invalid("invalid_upload", "send multipart/form-data with text and/or files (PDF/JPEG/PNG, 5 MB each)"))
		return
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()
	var files []service.Upload
	for _, fh := range r.MultipartForm.File["files"] {
		f, err := fh.Open()
		if err != nil {
			httpx.WriteError(w, r, h.log, err)
			return
		}
		data, err := io.ReadAll(io.LimitReader(f, domain.MaxFileBytes+1))
		f.Close()
		if err != nil {
			httpx.WriteError(w, r, h.log, err)
			return
		}
		files = append(files, service.Upload{Name: fh.Filename, Data: data})
	}
	p := httpx.Principal(r)
	v, err := h.svc.AddEvidence(r.Context(), p.MerchantID, p.Subject, chi.URLParam(r, "id"), r.FormValue("text"), files)
	h.respond(w, r, http.StatusOK, v, err)
}

func (h *HTTP) fileFor(merchant bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		merchantID := ""
		if merchant {
			merchantID = httpx.Principal(r).MerchantID
		}
		f, err := h.svc.File(r.Context(), merchantID, chi.URLParam(r, "id"), chi.URLParam(r, "fileId"))
		if err != nil {
			httpx.WriteError(w, r, h.log, err)
			return
		}
		w.Header().Set("Content-Type", f.ContentType)
		w.Header().Set("Content-Disposition", `attachment; filename="`+f.Name+`"`)
		w.Header().Set("Cache-Control", "private, no-store")
		w.Header().Set("Content-Length", strconv.Itoa(len(f.Data)))
		_, _ = w.Write(f.Data)
	}
}

func (h *HTTP) respond(w http.ResponseWriter, r *http.Request, status int, v any, err error) {
	if err != nil {
		httpx.WriteError(w, r, h.log, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, status, v)
}
