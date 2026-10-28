package transport

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"paygate/internal/contracts/ledger"
	"paygate/internal/modules/ledger/internal/service"
	"paygate/internal/platform/apperror"
	"paygate/internal/platform/httpx"
)

type HTTP struct {
	svc *service.Service
	log *slog.Logger
}

func NewHTTP(svc *service.Service, log *slog.Logger) *HTTP { return &HTTP{svc: svc, log: log} }

func (h *HTTP) RegisterPublic(r chi.Router) {
	r.With(httpx.Require(ledger.PermBalanceRead, h.log)).Get("/balance", h.balance)
}

func (h *HTTP) RegisterAdmin(r chi.Router) {
	r.With(httpx.Require(ledger.PermLedgerRead, h.log)).Get("/ledger/trial-balance", h.trialBalance)
}

func (h *HTTP) balance(w http.ResponseWriter, r *http.Request) {
	p := httpx.Principal(r)
	currency := r.URL.Query().Get("currency")
	if currency == "" {
		currency = "ETB"
	}
	// An API key is bound to one environment. A dashboard user picks one
	// (?environment=test|live, default live).
	env := p.Environment
	if env == "" {
		env = r.URL.Query().Get("environment")
		if env == "" {
			env = "live"
		}
		if env != "live" && env != "test" {
			httpx.WriteError(w, r, h.log, apperror.Invalid("invalid_environment", "environment must be live or test"))
			return
		}
	}
	b, err := h.svc.MerchantBalance(r.Context(), p.MerchantID, env, currency)
	if err != nil {
		httpx.WriteError(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, b)
}

func (h *HTTP) trialBalance(w http.ResponseWriter, r *http.Request) {
	rows, err := h.svc.TrialBalance(r.Context())
	if err != nil {
		httpx.WriteError(w, r, h.log, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"data": rows})
}
