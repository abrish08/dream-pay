package service

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"paygate/internal/modules/ledger/internal/domain"
	"paygate/internal/modules/ledger/internal/store"
	"paygate/internal/platform/apperror"
	"paygate/internal/platform/money"
	"paygate/internal/platform/reqctx"
)

// AdjustmentInput: a request to correct a merchant's balance.
type AdjustmentInput struct {
	MerchantID  string `json:"merchant_id"`
	Environment string `json:"environment"`
	Currency    string `json:"currency"`
	Direction   string `json:"direction"` // credit: give money to the merchant; debit: take it
	Amount      int64  `json:"amount"`    // minor units, > 0
	Reason      string `json:"reason"`    // why (ticket, reconciliation item ...), required
}

var (
	errSameApprover = apperror.Forbidden("four_eyes_required", "a correction must be approved by someone other than the person who requested it")
	errDecided      = apperror.Conflict("adjustment_already_decided", "this correction was already approved or rejected")
)

// RequestAdjustment records a correction waiting for a second person.
func (s *Service) RequestAdjustment(ctx context.Context, actor reqctx.Principal, in AdjustmentInput) (*store.Adjustment, error) {
	in.Currency, in.Reason = strings.ToUpper(strings.TrimSpace(in.Currency)), strings.TrimSpace(in.Reason)
	switch {
	case uuid.Validate(in.MerchantID) != nil:
		return nil, apperror.Invalid("invalid_merchant", "merchant_id must be a merchant id")
	case in.Environment != "live" && in.Environment != "test":
		return nil, apperror.Invalid("invalid_environment", "environment must be live or test")
	case !money.Supported(in.Currency):
		return nil, apperror.Invalid("unsupported_currency", "currency must be ETB or USD")
	case in.Direction != domain.AdjustCredit && in.Direction != domain.AdjustDebit:
		return nil, apperror.Invalid("invalid_direction", "direction must be credit or debit")
	case in.Amount <= 0 || in.Amount > money.MaxAmount:
		return nil, apperror.Invalid("invalid_amount", "amount must be a positive number of minor units")
	case len(in.Reason) < 10 || len(in.Reason) > 500:
		return nil, apperror.Invalid("reason_required", "explain the correction in 10 to 500 characters")
	}
	a := &store.Adjustment{ID: uuid.NewString(), MerchantID: in.MerchantID, Environment: in.Environment, Currency: in.Currency,
		Direction: in.Direction, Amount: in.Amount, Reason: in.Reason, Status: store.AdjustmentPending,
		RequestedBy: actor.Subject, RequestedAt: s.now()}
	reqctx.AuditResource(ctx, "ledger_adjustment", a.ID)
	return a, s.store.InsertAdjustment(s.db.WithContext(ctx), a)
}

// DecideAdjustment approves (and posts) or rejects a correction. The
// approver must be a different person from the requester.
func (s *Service) DecideAdjustment(ctx context.Context, actor reqctx.Principal, id string, approve bool, note string) (*store.Adjustment, error) {
	if uuid.Validate(id) != nil {
		return nil, apperror.NotFound("adjustment_not_found", "no such correction")
	}
	var out *store.Adjustment
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		a, err := s.store.LockAdjustment(tx, id)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return apperror.NotFound("adjustment_not_found", "no such correction")
		}
		if err != nil {
			return err
		}
		if a.Status != store.AdjustmentPending {
			return errDecided
		}
		if a.RequestedBy == actor.Subject {
			return errSameApprover
		}
		now := s.now()
		a.DecidedBy, a.DecidedAt, a.DecisionNote = &actor.Subject, &now, strings.TrimSpace(note)
		a.Status = store.AdjustmentRejected
		if approve {
			a.Status = store.AdjustmentApproved
			p := domain.AdjustmentPosting(a.ID, a.MerchantID, a.Environment, a.Currency, a.Direction, a.Reason, a.Amount)
			if _, err := s.store.Post(tx, p, now); err != nil {
				return err
			}
		}
		if err := s.store.SaveAdjustment(tx, a); err != nil {
			return err
		}
		if approve && a.Direction == domain.AdjustDebit {
			if err := s.warnIfNegative(tx, a.MerchantID, a.Environment, a.Currency, "correction "+a.ID); err != nil {
				return err
			}
		}
		out = a
		return nil
	})
	return out, err
}

func (s *Service) ListAdjustments(ctx context.Context, status string, limit int) ([]store.Adjustment, error) {
	return s.store.ListAdjustments(s.db.WithContext(ctx), status, limit)
}

// Summary: what the platform owes merchants against the cash it holds and
// what providers still owe it, for one environment and currency.
type Summary struct {
	Environment string `json:"environment"`
	Currency    string `json:"currency"`
	// Owed to merchants (all their accounts).
	OwedToMerchants int64            `json:"owed_to_merchants"`
	MerchantParts   map[string]int64 `json:"merchant_parts"` // available, reserve, dispute_hold, payouts_pending, refunds_pending
	// Assets.
	CashInBank      int64 `json:"cash_in_bank"`      // settlement bank account
	OwedByProviders int64 `json:"owed_by_providers"` // captured, not settled yet
	// The platform's own result.
	FeesEarned            int64 `json:"fees_earned"`
	ProviderFeesPaid      int64 `json:"provider_fees_paid"`
	Corrections           int64 `json:"corrections"`            // net paid to merchants by corrections
	SettlementDifferences int64 `json:"settlement_differences"` // unexplained deposit differences
	// Checks.
	Balanced          bool               `json:"balanced"`           // assets = what is owed + platform result
	CashShortfall     int64              `json:"cash_shortfall"`     // owed to merchants minus cash in bank (> 0: providers still have it)
	NegativeMerchants []NegativeMerchant `json:"negative_merchants"` // merchants who owe the platform
}

type NegativeMerchant struct {
	MerchantID string `json:"merchant_id"`
	Balance    int64  `json:"balance"`
}

func (s *Service) Summary(ctx context.Context, env, currency string) (Summary, error) {
	currency = strings.ToUpper(currency)
	if !money.Supported(currency) || (env != "live" && env != "test") {
		return Summary{}, apperror.Invalid("invalid_summary", "environment must be live or test, currency ETB or USD")
	}
	q := s.db.WithContext(ctx)
	rows, err := s.store.Summary(q, env, currency)
	if err != nil {
		return Summary{}, err
	}
	out := Summary{Environment: env, Currency: currency, MerchantParts: map[string]int64{}, NegativeMerchants: []NegativeMerchant{}}
	var total int64
	for _, r := range rows {
		total += r.Balance
		switch {
		case strings.HasPrefix(r.Class, "merchant:"):
			out.MerchantParts[strings.TrimPrefix(r.Class, "merchant:")] = r.Balance
			out.OwedToMerchants += r.Balance
		case r.Class == "provider:clearing":
			out.OwedByProviders = -r.Balance // an asset: debit balance
		case r.Class == "platform:settlement_bank":
			out.CashInBank = -r.Balance
		case r.Class == "platform:fees":
			out.FeesEarned = r.Balance
		case r.Class == "platform:provider_fees":
			out.ProviderFeesPaid = -r.Balance
		case r.Class == "platform:adjustments":
			out.Corrections = -r.Balance
		case r.Class == "platform:settlement_differences":
			out.SettlementDifferences = r.Balance
		}
	}
	out.Balanced = total == 0
	out.CashShortfall = out.OwedToMerchants - out.CashInBank
	neg, err := s.store.NegativeMerchants(q, env, currency)
	for _, n := range neg {
		out.NegativeMerchants = append(out.NegativeMerchants, NegativeMerchant{MerchantID: n.Class, Balance: n.Balance})
	}
	return out, err
}
