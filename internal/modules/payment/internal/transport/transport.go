package transport

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"paygate/internal/contracts/payments"
	"paygate/internal/modules/payment/internal/service"
	"paygate/internal/modules/payment/internal/store"
	"paygate/internal/platform/bus"
	"paygate/internal/platform/httpx"
)

type HTTP struct {
	svc *service.Service
	log *slog.Logger
}

func NewHTTP(svc *service.Service, log *slog.Logger) *HTTP { return &HTTP{svc: svc, log: log} }

func (h *HTTP) RegisterPublic(r chi.Router) {
	r.With(httpx.Require(payments.PermWrite, h.log)).Post("/payments", h.create)
	r.With(httpx.Require(payments.PermRead, h.log)).Get("/payments", h.list)
	r.With(httpx.Require(payments.PermRead, h.log)).Get("/payments/{id}", h.get)
	r.With(httpx.Require(payments.PermRead, h.log)).Get("/payments/{id}/events", h.history)
}

// RegisterAdmin: platform staff (support, operations, finance) look up any
// payment and its full history.
//
//	GET /admin/payments/{id}
//	GET /admin/payments/{id}/events
func (h *HTTP) RegisterAdmin(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(httpx.Require(payments.PermReadAll, h.log))
		r.Get("/payments/{id}", func(w http.ResponseWriter, r *http.Request) {
			p, err := h.svc.GetAny(r.Context(), chi.URLParam(r, "id"))
			if err != nil {
				httpx.WriteError(w, r, h.log, err)
				return
			}
			httpx.WriteJSON(w, http.StatusOK, p)
		})
		r.Get("/payments/{id}/events", func(w http.ResponseWriter, r *http.Request) {
			h.writeHistory(w, r, "")
		})
	})
}

// history: GET /v1/payments/{id}/events, every status change of the payment,
// oldest first: who caused it (source), why, and what the provider answered.
func (h *HTTP) history(w http.ResponseWriter, r *http.Request) {
	h.writeHistory(w, r, httpx.Principal(r).MerchantID)
}

func (h *HTTP) writeHistory(w http.ResponseWriter, r *http.Request, merchantID string) {
	evs, err := h.svc.History(r.Context(), merchantID, chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, r, h.log, err)
		return
	}
	type view struct {
		store.Event
		Details json.RawMessage `json:"details,omitempty"`
	}
	out := make([]view, len(evs))
	for i, e := range evs {
		out[i] = view{Event: e}
		if e.Details != "" && e.Details != "{}" {
			out[i].Details = json.RawMessage(e.Details)
		}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"data": out})
}

func (h *HTTP) create(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Amount      int64             `json:"amount"`
		Currency    string            `json:"currency"`
		Method      string            `json:"payment_method"`
		Provider    string            `json:"provider"`
		Reference   string            `json:"reference"`
		Description string            `json:"description"`
		Metadata    map[string]string `json:"metadata"`
		Customer    struct {
			Email string `json:"email"`
			Name  string `json:"name"`
			Phone string `json:"phone"`
			IP    string `json:"ip"` // the customer's IP as your server saw it (risk checks)
		} `json:"customer"`
		ReturnURL        string `json:"return_url"`
		ExpiresInMinutes int    `json:"expires_in_minutes"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.WriteError(w, r, h.log, err)
		return
	}
	p := httpx.Principal(r)
	pay, created, err := h.svc.Create(r.Context(), service.CreateInput{
		MerchantID: p.MerchantID, Environment: p.Environment, IdempotencyKey: r.Header.Get("Idempotency-Key"),
		Amount: body.Amount, Currency: body.Currency, Method: body.Method, Provider: body.Provider, Reference: body.Reference,
		Description: body.Description, Metadata: body.Metadata,
		CustomerEmail: body.Customer.Email, CustomerName: body.Customer.Name, ReturnURL: body.ReturnURL,
		CustomerPhone: body.Customer.Phone, CustomerIP: body.Customer.IP,
		ExpiresInMin: body.ExpiresInMinutes,
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
	httpx.WriteJSON(w, status, pay)
}

func (h *HTTP) get(w http.ResponseWriter, r *http.Request) {
	pay, err := h.svc.Get(r.Context(), httpx.Principal(r).MerchantID, chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, pay)
}

// list: GET /v1/payments?status=succeeded,failed&reference=INV-1
//
//	&created_from=2026-01-01T00:00:00Z&created_to=...&limit=20&starting_after=<id>
//
// An API key sees only its own environment; a dashboard user may pass
// ?environment=live|test (default: both).
func (h *HTTP) list(w http.ResponseWriter, r *http.Request) {
	p, q := httpx.Principal(r), r.URL.Query()
	f := service.ListFilter{MerchantID: p.MerchantID, Environment: p.Environment, Reference: q.Get("reference"),
		StartingAfter: q.Get("starting_after"), Limit: httpx.QueryInt(r, "limit", 20, 100)}
	if f.Environment == "" {
		f.Environment = q.Get("environment")
	}
	if s := q.Get("status"); s != "" {
		f.Statuses = strings.Split(s, ",")
	}
	var err error
	if f.CreatedFrom, err = httpx.QueryTime(r, "created_from"); err == nil {
		f.CreatedTo, err = httpx.QueryTime(r, "created_to")
	}
	if err != nil {
		httpx.WriteError(w, r, h.log, err)
		return
	}
	page, err := h.svc.ListForMerchant(r.Context(), f)
	if err != nil {
		httpx.WriteError(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, page)
}

// RegisterBus answers the payment module's requests on the central bus.
func RegisterBus(b bus.Bus, svc *service.Service) {
	bus.Handle(b, func(ctx context.Context, q payments.GetPayment) (payments.Payment, error) {
		return svc.Get(ctx, q.MerchantID, q.PaymentID)
	})
	bus.Handle(b, svc.List)
	bus.Handle(b, svc.GetByIDs)
	bus.Handle(b, svc.Sync)
}
