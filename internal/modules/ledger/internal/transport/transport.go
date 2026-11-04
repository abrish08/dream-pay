package transport

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"paygate/internal/contracts/ledger"
	"paygate/internal/modules/ledger/internal/service"
	"paygate/internal/modules/ledger/internal/store"
	"paygate/internal/platform/apperror"
	"paygate/internal/platform/csvx"
	"paygate/internal/platform/httpx"
	"paygate/internal/platform/money"
)

type HTTP struct {
	svc *service.Service
	log *slog.Logger
}

func NewHTTP(svc *service.Service, log *slog.Logger) *HTTP { return &HTTP{svc: svc, log: log} }

func (h *HTTP) RegisterPublic(r chi.Router) {
	r.With(httpx.Require(ledger.PermBalanceRead, h.log)).Get("/balance", h.balance)
	r.With(httpx.Require(ledger.PermBalanceRead, h.log)).Get("/reports/statement.csv", h.statement)
}

// statement: GET /v1/reports/statement.csv?currency=ETB&from=&to= every
// movement of the available balance (payments, refunds, reserve, disputes,
// payouts): the opening balance first, then each line with the balance after it.
func (h *HTTP) statement(w http.ResponseWriter, r *http.Request) {
	from, to, err := csvx.Period(r, time.Now().UTC())
	if err != nil {
		httpx.WriteError(w, r, h.log, err)
		return
	}
	p := httpx.Principal(r)
	env := p.Environment
	if env == "" {
		if env = r.URL.Query().Get("environment"); env == "" {
			env = "live"
		}
	}
	currency := strings.ToUpper(r.URL.Query().Get("currency"))
	if currency == "" {
		currency = "ETB"
	}
	var out *csvx.Writer
	err = h.svc.Statement(r.Context(), p.MerchantID, env, currency, from, to, func(opening int64) {
		out = csvx.New(w, "statement-"+currency+"-"+from.Format("20060102")+"-"+to.Format("20060102")+".csv",
			[]string{"date", "reference", "description", "credit", "debit", "balance"})
		_ = out.Row(from.Format(time.RFC3339), "opening", "opening balance", "", "", money.FormatMajor(opening))
	}, func(l store.StatementLine, bal int64) error {
		return out.Row(l.CreatedAt.Format(time.RFC3339), l.Reference, l.Description, money.FormatMajor(l.Credit),
			money.FormatMajor(l.Debit), money.FormatMajor(bal))
	})
	if out == nil { // failed before anything was written
		httpx.WriteError(w, r, h.log, err)
		return
	}
	if err != nil {
		h.log.Error("statement report", "err", err) // the file ends early
	}
	out.Flush()
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
