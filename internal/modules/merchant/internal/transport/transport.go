package transport

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"paygate/internal/contracts/merchants"
	"paygate/internal/modules/merchant/internal/domain"
	"paygate/internal/modules/merchant/internal/service"
	"paygate/internal/platform/apperror"
	"paygate/internal/platform/bus"
	"paygate/internal/platform/httpx"
)

type HTTP struct {
	svc *service.Service
	log *slog.Logger
}

func NewHTTP(svc *service.Service, log *slog.Logger) *HTTP { return &HTTP{svc: svc, log: log} }

// RegisterAdmin: platform staff only (the app puts /admin behind RequirePlatform).
//
//	POST  /admin/merchants                 create an (approved) merchant
//	GET   /admin/merchants?status=pending  list, newest first (limit, starting_after)
//	GET   /admin/merchants/{id}
//	POST  /admin/merchants/{id}/status     {status: active|suspended}  approve / suspend / reject
//	PATCH /admin/merchants/{id}            {fee_bps}
func (h *HTTP) RegisterAdmin(r chi.Router) {
	r.With(httpx.Require(merchants.PermManage, h.log)).Post("/merchants", h.create)
	r.With(httpx.Require(merchants.PermRead, h.log)).Get("/merchants", h.list)
	r.With(httpx.Require(merchants.PermRead, h.log)).Get("/merchants/{id}", h.get)
	r.With(httpx.Require(merchants.PermManage, h.log)).Post("/merchants/{id}/status", h.setStatus)
	r.With(httpx.Require(merchants.PermManage, h.log)).Patch("/merchants/{id}", h.update)
}

// RegisterPublic: GET /v1/merchant, the caller's own merchant profile.
func (h *HTTP) RegisterPublic(r chi.Router) {
	r.Get("/merchant", func(w http.ResponseWriter, r *http.Request) {
		m, err := h.svc.GetMerchant(r.Context(), httpx.Principal(r).MerchantID)
		h.respond(w, r, http.StatusOK, m, err)
	})
}

func (h *HTTP) create(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name    string `json:"name"`
		Email   string `json:"email"`
		Phone   string `json:"phone"`
		Website string `json:"website"`
		FeeBPS  *int64 `json:"fee_bps"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.WriteError(w, r, h.log, err)
		return
	}
	fee := int64(domain.DefaultFeeBPS)
	if body.FeeBPS != nil {
		fee = *body.FeeBPS
	}
	m, err := h.svc.Create(r.Context(), domain.NewMerchantInput{Name: body.Name, Email: body.Email, Phone: body.Phone,
		Website: body.Website, FeeBPS: fee, Status: merchants.StatusActive})
	h.respond(w, r, http.StatusCreated, m, err)
}

func (h *HTTP) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page, err := h.svc.List(r.Context(), q.Get("status"), q.Get("starting_after"), httpx.QueryInt(r, "limit", 20, 100))
	h.respond(w, r, http.StatusOK, page, err)
}

func (h *HTTP) update(w http.ResponseWriter, r *http.Request) {
	var body struct {
		FeeBPS *int64 `json:"fee_bps"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.WriteError(w, r, h.log, err)
		return
	}
	if body.FeeBPS == nil {
		h.respond(w, r, 0, nil, apperror.Invalid("nothing_to_update", "send fee_bps"))
		return
	}
	m, err := h.svc.SetFee(r.Context(), chi.URLParam(r, "id"), *body.FeeBPS)
	h.respond(w, r, http.StatusOK, m, err)
}

func (h *HTTP) respond(w http.ResponseWriter, r *http.Request, status int, v any, err error) {
	if err != nil {
		httpx.WriteError(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, status, v)
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
	bus.Handle(b, func(ctx context.Context, q merchants.CreateMerchant) (merchants.Merchant, error) {
		return svc.Create(ctx, domain.NewMerchantInput{Name: q.Name, Email: q.Email, Phone: q.Phone,
			Website: q.Website, FeeBPS: domain.DefaultFeeBPS, Status: merchants.StatusPending})
	})
}
