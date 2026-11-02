package transport

import (
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"errors"
	"github.com/go-chi/chi/v5"

	"paygate/internal/contracts/kyc"
	"paygate/internal/modules/kyc/internal/domain"
	"paygate/internal/modules/kyc/internal/service"
	"paygate/internal/modules/kyc/internal/storage"
	"paygate/internal/platform/apperror"
	"paygate/internal/platform/httpx"
)

type HTTP struct {
	svc   *service.Service
	local *storage.Local // nil with MinIO (links point straight at MinIO)
	log   *slog.Logger
}

const maxBytes = domain.MaxFileBytes // 5 MB per file

func NewHTTP(svc *service.Service, local *storage.Local, log *slog.Logger) *HTTP {
	return &HTTP{svc: svc, local: local, log: log}
}

// RegisterPublic (merchant users; never API keys):
//
//	GET    /v1/kyc                    status, profile, documents, what is missing
//	PUT    /v1/kyc/profile            business + owner details (JSON)
//	POST   /v1/kyc/documents          multipart: type, file (PDF/JPEG/PNG)
//	GET    /v1/kyc/documents/{id}     {url, expires_at}: short-lived download link
//	DELETE /v1/kyc/documents/{id}     while draft / rejected
//	POST   /v1/kyc/fayda/start        {authorize_url}: verify the owner with Fayda
//	POST   /v1/kyc/submit             send for review
func (h *HTTP) RegisterPublic(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(httpx.RequireUser(h.log))
		r.With(httpx.Require(kyc.PermRead, h.log)).Get("/kyc", h.get)
		r.With(httpx.Require(kyc.PermRead, h.log)).Get("/kyc/documents/{id}", h.linkOwn)
		r.Group(func(r chi.Router) {
			r.Use(httpx.Require(kyc.PermWrite, h.log))
			r.Put("/kyc/profile", h.saveProfile)
			r.Post("/kyc/documents", h.upload)
			r.Delete("/kyc/documents/{id}", h.deleteDoc)
			r.Post("/kyc/fayda/start", h.faydaStart)
			r.Post("/kyc/submit", h.submit)
		})
	})
}

// RegisterAdmin (compliance staff, kyc:review):
//
//	GET  /admin/kyc?status=submitted                review queue
//	GET  /admin/kyc/{merchantID}                    full application + history
//	GET  /admin/kyc/{merchantID}/documents/{id}     {url, expires_at} (viewing is recorded)
//	POST /admin/kyc/{merchantID}/approve            {note?}
//	POST /admin/kyc/{merchantID}/reject             {reason}
func (h *HTTP) RegisterAdmin(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(httpx.Require(kyc.PermReview, h.log))
		r.Get("/kyc", h.list)
		r.Get("/kyc/{merchantID}", h.review)
		r.Get("/kyc/{merchantID}/documents/{id}", h.linkAny)
		r.Post("/kyc/{merchantID}/approve", h.approve)
		r.Post("/kyc/{merchantID}/reject", h.reject)
	})
}

// RegisterOpen (no credentials; each request carries its own proof):
//
//	GET /kyc/fayda/callback   Fayda returns here (state + one-time code)
//	GET /kyc/files/{token}    local storage only: signed, expiring file link
func (h *HTTP) RegisterOpen(r chi.Router) {
	r.Get("/kyc/fayda/callback", h.faydaCallback)
	if h.local != nil {
		r.Get("/kyc/files/{token}", h.serveFile)
	}
}

func (h *HTTP) faydaStart(w http.ResponseWriter, r *http.Request) {
	v, err := h.svc.StartFayda(r.Context(), httpx.Principal(r))
	h.respond(w, r, http.StatusOK, v, err)
}

func (h *HTTP) faydaCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	to := h.svc.FaydaCallback(r.Context(), q.Get("state"), q.Get("code"), q.Get("error"))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer") // never leak the code
	http.Redirect(w, r, to, http.StatusSeeOther)
}

func (h *HTTP) serveFile(w http.ResponseWriter, r *http.Request) {
	data, name, err := h.local.Open(chi.URLParam(r, "token"))
	if err != nil {
		if !errors.Is(err, storage.ErrBadLink) {
			h.log.Error("open KYC file", "err", err)
		}
		httpx.WriteError(w, r, h.log, apperror.NotFound("link_expired", storage.ErrBadLink.Error()))
		return
	}
	ct, _ := domain.SniffFile(data)
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Disposition", `attachment; filename="`+domain.CleanFileName(name, ct)+`"`)
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = w.Write(data)
}

func (h *HTTP) get(w http.ResponseWriter, r *http.Request) {
	v, err := h.svc.Get(r.Context(), httpx.Principal(r).MerchantID)
	h.respond(w, r, http.StatusOK, v, err)
}

func (h *HTTP) saveProfile(w http.ResponseWriter, r *http.Request) {
	var p domain.Profile
	if err := httpx.DecodeJSON(r, &p); err != nil {
		httpx.WriteError(w, r, h.log, err)
		return
	}
	v, err := h.svc.SaveProfile(r.Context(), httpx.Principal(r), p)
	h.respond(w, r, http.StatusOK, v, err)
}

func (h *HTTP) upload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes+64<<10) // file + small form overhead
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			httpx.WriteError(w, r, h.log, apperror.TooLarge("file_too_large", "files are at most 5 MB"))
			return
		}
		httpx.WriteError(w, r, h.log, apperror.Invalid("invalid_upload", "send multipart/form-data with fields type and file (max 5 MB)"))
		return
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()
	f, hdr, err := r.FormFile("file")
	if err != nil {
		httpx.WriteError(w, r, h.log, apperror.Invalid("invalid_upload", "the file field is required"))
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		httpx.WriteError(w, r, h.log, err)
		return
	}
	d, err := h.svc.Upload(r.Context(), httpx.Principal(r), r.FormValue("type"), hdr.Filename, data)
	h.respond(w, r, http.StatusCreated, d, err)
}

func (h *HTTP) deleteDoc(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.DeleteDocument(r.Context(), httpx.Principal(r), chi.URLParam(r, "id")); err != nil {
		httpx.WriteError(w, r, h.log, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *HTTP) submit(w http.ResponseWriter, r *http.Request) {
	v, err := h.svc.Submit(r.Context(), httpx.Principal(r))
	h.respond(w, r, http.StatusOK, v, err)
}

func (h *HTTP) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page, err := h.svc.List(r.Context(), q.Get("status"), q.Get("starting_after"), httpx.QueryInt(r, "limit", 20, 100))
	h.respond(w, r, http.StatusOK, page, err)
}

func (h *HTTP) review(w http.ResponseWriter, r *http.Request) {
	v, err := h.svc.Review(r.Context(), chi.URLParam(r, "merchantID"))
	h.respond(w, r, http.StatusOK, v, err)
}

func (h *HTTP) approve(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Note string `json:"note"`
	}
	if r.ContentLength != 0 {
		if err := httpx.DecodeJSON(r, &body); err != nil {
			httpx.WriteError(w, r, h.log, err)
			return
		}
	}
	v, err := h.svc.Approve(r.Context(), httpx.Principal(r), chi.URLParam(r, "merchantID"), body.Note)
	h.respond(w, r, http.StatusOK, v, err)
}

func (h *HTTP) reject(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Reason string `json:"reason"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.WriteError(w, r, h.log, err)
		return
	}
	v, err := h.svc.Reject(r.Context(), httpx.Principal(r), chi.URLParam(r, "merchantID"), body.Reason)
	h.respond(w, r, http.StatusOK, v, err)
}

func (h *HTTP) linkOwn(w http.ResponseWriter, r *http.Request) {
	h.link(w, r, httpx.Principal(r).MerchantID)
}

func (h *HTTP) linkAny(w http.ResponseWriter, r *http.Request) {
	h.link(w, r, chi.URLParam(r, "merchantID"))
}

// link returns a fresh, short-lived URL built from the stored object key.
func (h *HTTP) link(w http.ResponseWriter, r *http.Request, merchantID string) {
	l, err := h.svc.DocumentLink(r.Context(), httpx.Principal(r), merchantID, chi.URLParam(r, "id"))
	h.respond(w, r, http.StatusOK, l, err)
}

func (h *HTTP) respond(w http.ResponseWriter, r *http.Request, status int, v any, err error) {
	if err != nil {
		httpx.WriteError(w, r, h.log, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store") // personal data
	httpx.WriteJSON(w, status, v)
}
