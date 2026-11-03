package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"
	_ "time/tzdata" // business days in Africa/Addis_Ababa even on minimal container images

	"github.com/google/uuid"
	"gorm.io/gorm"

	"paygate/internal/contracts/ledger"
	"paygate/internal/contracts/payments"
	"paygate/internal/contracts/providers"
	"paygate/internal/contracts/refunds"
	"paygate/internal/modules/reconciliation/internal/domain"
	"paygate/internal/modules/reconciliation/internal/store"
	"paygate/internal/platform/apperror"
	"paygate/internal/platform/bus"
	"paygate/internal/platform/money"
)

type RunView struct {
	ID           string          `json:"id"`
	Kind         string          `json:"kind"`
	Provider     string          `json:"provider,omitempty"`
	SettlementID *string         `json:"settlement_id,omitempty"`
	PeriodStart  time.Time       `json:"period_start"`
	PeriodEnd    time.Time       `json:"period_end"`
	Status       string          `json:"status"`
	Checked      int             `json:"checked"`
	Mismatched   int             `json:"mismatched"`
	Summary      json.RawMessage `json:"summary,omitempty"`
	Error        string          `json:"error,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
	CompletedAt  *time.Time      `json:"completed_at"`
	Items        []domain.Item   `json:"items,omitempty"`
}

type Options struct {
	Location   *time.Location // business day (default Africa/Addis_Ababa)
	DailyAfter time.Duration  // run the daily check this long after local midnight (default 1h: late callbacks arrive first)
	StuckAfter time.Duration  // a payment still processing after this is reported (default 1h)
	SettleWait time.Duration  // ledger postings younger than this are not checked yet (default 2m)
}

type Service struct {
	db    *gorm.DB
	store *store.Store
	bus   bus.Bus // the only way to reach other modules
	opt   Options
	log   *slog.Logger
	now   func() time.Time
}

func New(db *gorm.DB, b bus.Bus, opt Options, log *slog.Logger) *Service {
	if opt.Location == nil {
		opt.Location, _ = time.LoadLocation("Africa/Addis_Ababa")
	}
	if opt.DailyAfter == 0 {
		opt.DailyAfter = time.Hour
	}
	if opt.StuckAfter == 0 {
		opt.StuckAfter = time.Hour
	}
	if opt.SettleWait == 0 {
		opt.SettleWait = 2 * time.Minute
	}
	return &Service{db: db, store: store.New(), bus: b, opt: opt, log: log,
		now: func() time.Time { return time.Now().UTC() }}
}

// ---- Runs ----

func (s *Service) CreateRun(ctx context.Context, from, to time.Time, provider string) (RunView, error) {
	r, err := domain.NewRun(domain.KindManual, from.UTC(), to.UTC(), s.now())
	if err != nil {
		return RunView{}, err
	}
	r.Provider = provider
	if _, err := s.store.InsertRun(s.db.WithContext(ctx), r); err != nil {
		return RunView{}, err
	}
	return view(r, nil), nil
}

func (s *Service) GetRun(ctx context.Context, id string) (RunView, error) {
	r, err := s.store.GetRun(s.db.WithContext(ctx), id)
	if err != nil {
		return RunView{}, err
	}
	its, err := s.store.Items(s.db.WithContext(ctx), id)
	return view(r, its), err
}

func (s *Service) ListRuns(ctx context.Context, limit int) ([]RunView, error) {
	rs, err := s.store.ListRuns(s.db.WithContext(ctx), limit)
	out := make([]RunView, len(rs))
	for i := range rs {
		out[i] = view(&rs[i], nil)
	}
	return out, err
}

// ---- Items: the work queue of differences ----

func (s *Service) ListItems(ctx context.Context, f store.ItemFilter) ([]domain.Item, error) {
	return s.store.ListItems(s.db.WithContext(ctx), f)
}

// Resolve closes a difference after a person investigated it; the note says
// what was found and done (kept for audit).
func (s *Service) Resolve(ctx context.Context, id int64, note, by string) (*domain.Item, error) {
	if len(strings.TrimSpace(note)) < 5 {
		return nil, apperror.Invalid("note_required", "explain what was found and done (at least 5 characters)")
	}
	return s.store.Resolve(s.db.WithContext(ctx), id, domain.ItemResolved, strings.TrimSpace(note), by, s.now())
}

// ---- Settlement files ----

type SettlementUpload struct {
	Provider        string
	Date            string // YYYY-MM-DD, the business day the report covers
	FileName        string
	Data            []byte
	DepositAmount   string // optional: what the bank paid in, e.g. "12345.67"
	DepositCurrency string
	UploadedBy      string
}

type SettlementView struct {
	Settlement store.Settlement `json:"settlement"`
	Run        RunView          `json:"run"`
}

// UploadSettlement stores a provider's settlement report and queues its
// matching. The same file is never counted twice.
func (s *Service) UploadSettlement(ctx context.Context, u SettlementUpload) (SettlementView, error) {
	if u.Provider == "" {
		return SettlementView{}, apperror.Invalid("provider_required", "provider is required (e.g. mpgs)")
	}
	day, err := time.ParseInLocation("2006-01-02", u.Date, s.opt.Location)
	if err != nil {
		return SettlementView{}, apperror.Invalid("invalid_date", "date must be YYYY-MM-DD")
	}
	if len(u.Data) > domain.MaxSettlementBytes {
		return SettlementView{}, apperror.TooLarge("file_too_large", "settlement files are at most 20 MB")
	}
	lines, err := domain.ParseSettlementCSV(u.Data)
	if err != nil {
		return SettlementView{}, err
	}
	sum := sha256.Sum256(u.Data)
	st := &store.Settlement{ID: uuid.NewString(), Provider: u.Provider, SettlementDate: day, FileName: u.FileName,
		SHA256: hex.EncodeToString(sum[:]), LineCount: len(lines), UploadedBy: u.UploadedBy, CreatedAt: s.now()}
	if u.DepositAmount != "" {
		amt, err := money.ParseMajor(u.DepositAmount)
		cur := strings.ToUpper(u.DepositCurrency)
		if err != nil || !money.Supported(cur) {
			return SettlementView{}, apperror.Invalid("invalid_deposit", "deposit_amount needs a valid amount and deposit_currency")
		}
		st.DepositAmount, st.DepositCurrency = &amt, &cur
	}
	r, _ := domain.NewRun(domain.KindSettlement, day.UTC(), day.AddDate(0, 0, 1).UTC(), s.now())
	r.Provider, r.SettlementID = u.Provider, &st.ID
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := s.store.InsertSettlement(tx, st, lines); err != nil {
			return err
		}
		_, err := s.store.InsertRun(tx, r)
		return err
	})
	if err != nil {
		return SettlementView{}, err
	}
	return SettlementView{Settlement: *st, Run: view(r, nil)}, nil
}

func (s *Service) ListSettlements(ctx context.Context, limit int) ([]store.Settlement, error) {
	return s.store.ListSettlements(s.db.WithContext(ctx), limit)
}

func (s *Service) GetSettlement(ctx context.Context, id string) (*store.Settlement, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, apperror.NotFound("settlement_not_found", "settlement not found")
	}
	return s.store.GetSettlement(s.db.WithContext(ctx), id)
}

// ---- Worker ----

// Worker processes queued runs and schedules the daily run for the previous
// business day (local time) once it has ended plus DailyAfter.
func (s *Service) Worker(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	var lastSchedule time.Time
	for {
		if s.now().Sub(lastSchedule) > 10*time.Minute {
			s.scheduleDaily(ctx)
			lastSchedule = s.now()
		}
		for {
			r, err := s.store.ClaimQueued(s.db.WithContext(ctx), s.now())
			if err != nil {
				if ctx.Err() == nil {
					s.log.Error("claim reconciliation run", "err", err)
				}
				break
			}
			if r == nil {
				break
			}
			s.process(ctx, r)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Service) scheduleDaily(ctx context.Context) {
	from, to := domain.BusinessDay(s.now(), s.opt.Location)
	if s.now().Before(to.Add(s.opt.DailyAfter)) {
		from, to = from.AddDate(0, 0, -1), to.AddDate(0, 0, -1) // today's cutoff not reached: make sure the day before ran
	}
	r, _ := domain.NewRun(domain.KindDaily, from, to, s.now())
	if _, err := s.store.InsertRun(s.db.WithContext(ctx), r); err != nil && ctx.Err() == nil {
		s.log.Error("schedule daily reconciliation", "err", err)
	}
}

// RunNow processes one run synchronously (tests and tools).
func (s *Service) RunNow(ctx context.Context, id string) (RunView, error) {
	r, err := s.store.GetRun(s.db.WithContext(ctx), id)
	if err != nil {
		return RunView{}, err
	}
	s.process(ctx, r)
	return s.GetRun(ctx, id)
}

func (s *Service) process(ctx context.Context, r *domain.Run) {
	c := &checker{s: s, r: r, sum: newSummary()}
	var err error
	if r.Kind == domain.KindSettlement {
		err = c.settlement(ctx)
	} else {
		err = c.payments(ctx)
	}
	now := s.now()
	r.CompletedAt = &now
	r.Status = domain.RunCompleted
	if err != nil {
		r.Status, r.Error = domain.RunFailed, err.Error()
	}
	b, _ := json.Marshal(c.sum)
	r.Summary = string(b)
	if err := s.store.SaveRun(s.db.WithContext(ctx), r); err != nil {
		s.log.Error("save reconciliation run", "run_id", r.ID, "err", err)
	}
	log := s.log.Info
	if c.sum.Critical > 0 {
		log = s.log.Error // alert on this line
	}
	log("reconciliation finished", "alert", c.sum.Critical > 0, "run_id", r.ID, "kind", r.Kind, "status", r.Status,
		"checked", r.Checked, "mismatched", r.Mismatched, "critical", c.sum.Critical, "auto_resolved", c.sum.AutoResolved)
}

// ---- The checks ----

type summary struct {
	ByStatus     map[string]int            `json:"by_status,omitempty"`
	Captured     map[string]*domain.Totals `json:"captured,omitempty"`  // per currency, from our payments
	Settled      map[string]*domain.Totals `json:"settled,omitempty"`   // per currency, from the settlement file
	Issues       map[string]int            `json:"issues"`              // new differences by issue
	Critical     int                       `json:"critical"`            // new critical differences
	AutoResolved int                       `json:"auto_resolved"`       // repaired automatically
	CarriedOver  int                       `json:"carried_over_closed"` // earlier "not settled yet" closed by this file
	Deposit      map[string]map[string]any `json:"deposit,omitempty"`   // bank deposit vs net
}

func newSummary() *summary {
	return &summary{ByStatus: map[string]int{}, Captured: map[string]*domain.Totals{}, Settled: map[string]*domain.Totals{},
		Issues: map[string]int{}}
}

func totals(m map[string]*domain.Totals, cur string) *domain.Totals {
	if m[cur] == nil {
		m[cur] = &domain.Totals{}
	}
	return m[cur]
}

type checker struct {
	s   *Service
	r   *domain.Run
	sum *summary
}

// report records a difference once (an open item for the same issue and
// reference is not duplicated) and tries to repair what can be repaired.
func (c *checker) report(ctx context.Context, it *domain.Item, repair bool) error {
	c.r.Mismatched++
	if open, err := c.s.store.HasOpen(c.s.db.WithContext(ctx), it.Issue, it.Reference); err != nil || open {
		return err
	}
	if repair && it.PaymentID != nil {
		// The provider says the customer paid: let the payment module verify
		// and record it (with "reconciliation" in the payment's history).
		p, err := bus.Ask(ctx, c.s.bus, payments.SyncPayment{PaymentID: *it.PaymentID, Source: "reconciliation",
			Note: "reconciliation run " + c.r.ID + ": " + it.Issue})
		if err == nil && domain.Captured(p.Status) {
			now := c.s.now()
			it.Status, it.Resolution, it.ResolvedBy, it.ResolvedAt = domain.ItemAutoResolved,
				"repaired: the provider confirmed the capture and the payment is now "+p.Status, "reconciliation", &now
			c.sum.AutoResolved++
		}
	}
	c.sum.Issues[it.Issue]++
	if it.Severity == domain.SeverityCritical && it.Status == domain.ItemOpen {
		c.sum.Critical++
	}
	return c.s.store.InsertItem(c.s.db.WithContext(ctx), it)
}

func (c *checker) item(p payments.Payment, issue, expected, actual string) *domain.Item {
	return domain.NewItem(c.r.ID, p.ID, p.ID, p.Provider, issue, expected, actual, c.s.now())
}

// payments: every payment of the period, against the provider's live API
// and against the ledger.
func (c *checker) payments(ctx context.Context) error {
	after := ""
	for {
		page, err := bus.Ask(ctx, c.s.bus, payments.ListPayments{
			Statuses: []string{payments.StatusSucceeded, payments.StatusPartiallyRefunded, payments.StatusRefunded,
				payments.StatusFailed, payments.StatusExpired, payments.StatusProcessing},
			Provider: c.r.Provider, CreatedFrom: c.r.PeriodStart, CreatedTo: c.r.PeriodEnd, AfterID: after, Limit: 200,
		})
		if err != nil {
			return err
		}
		var booked []payments.Payment
		for _, p := range page {
			c.sum.ByStatus[p.Status]++
			if domain.Captured(p.Status) {
				t := totals(c.sum.Captured, p.Currency)
				t.Count++
				t.Amount += p.Amount
				t.Fees += p.Fee
				t.Refunded += p.RefundedAmount
				t.Net = t.Amount - t.Fees - t.Refunded
				if c.s.now().Sub(p.UpdatedAt) >= c.s.opt.SettleWait {
					booked = append(booked, p)
				}
			}
			if p.Provider == "" {
				continue // never reached a provider (refused before routing)
			}
			c.r.Checked++
			if err := c.provider(ctx, p); err != nil {
				return err
			}
		}
		if err := c.ledger(ctx, booked); err != nil {
			return err
		}
		if len(page) < 200 {
			return nil
		}
		after = page[len(page)-1].ID
	}
}

func (c *checker) provider(ctx context.Context, p payments.Payment) error {
	if p.Status == payments.StatusProcessing {
		if c.s.now().Sub(p.CreatedAt) < c.s.opt.StuckAfter {
			return nil // still within its normal time
		}
		return c.report(ctx, c.item(p, domain.IssueStuckProcessing, "a final status",
			"processing since "+p.CreatedAt.Format(time.RFC3339)), true)
	}
	t, err := bus.Ask(ctx, c.s.bus, providers.VerifyTransaction{Provider: p.Provider, TxRef: p.ID})
	switch {
	case errors.Is(err, providers.ErrTransactionNotFound):
		if domain.Captured(p.Status) {
			return c.report(ctx, c.item(p, domain.IssueMissingAtProvider, p.Status, "not_found"), false)
		}
		return nil // failed / expired and the provider never had it: consistent
	case err != nil:
		return c.report(ctx, c.item(p, domain.IssueProviderError, p.Status, apperror.From(err).Message), false)
	}
	issue, expected, actual := domain.Compare(p, t)
	if issue == "" {
		return nil
	}
	repair := issue == domain.IssueStatusMismatch && t.Status == providers.StatusSucceeded
	return c.report(ctx, c.item(p, issue, expected, actual), repair)
}

// ledger: every captured payment must be booked once, for its amount.
func (c *checker) ledger(ctx context.Context, ps []payments.Payment) error {
	if len(ps) == 0 {
		return nil
	}
	refs := make([]string, len(ps))
	for i, p := range ps {
		refs[i] = ledger.PaymentReference(p.ID)
	}
	posted, err := bus.Ask(ctx, c.s.bus, ledger.GetPostings{References: refs})
	if err != nil {
		return err
	}
	byRef := map[string]ledger.PostingInfo{}
	for _, x := range posted {
		byRef[x.Reference] = x
	}
	for _, p := range ps {
		x, ok := byRef[ledger.PaymentReference(p.ID)]
		switch {
		case !ok:
			if err := c.report(ctx, c.item(p, domain.IssueLedgerMissing, domain.FormatAmount(p.Amount, p.Currency), "no posting"), false); err != nil {
				return err
			}
		case x.Amount != p.Amount || x.Currency != p.Currency:
			if err := c.report(ctx, c.item(p, domain.IssueLedgerAmountMismatch, domain.FormatAmount(p.Amount, p.Currency),
				domain.FormatAmount(x.Amount, x.Currency)), false); err != nil {
				return err
			}
		}
	}
	return nil
}

// settlement: the provider's report for a day against our payments.
func (c *checker) settlement(ctx context.Context) error {
	db := c.s.db.WithContext(ctx)
	st, err := c.s.store.GetSettlement(db, *c.r.SettlementID)
	if err != nil {
		return err
	}
	lines, err := c.s.store.SettlementLines(db, st.ID)
	if err != nil {
		return err
	}
	var refs, refundRefs []string
	seen := map[string]int{}
	refundSeen := map[string]int{}
	for _, l := range lines {
		t := totals(c.sum.Settled, l.Currency)
		if l.Kind == "refund" {
			t.Refunded += l.Amount
			if refundSeen[l.Reference] == 0 {
				refundRefs = append(refundRefs, l.Reference)
			}
			refundSeen[l.Reference]++
		} else {
			t.Count++
			t.Amount += l.Amount
			if seen[l.Reference] == 0 {
				refs = append(refs, l.Reference)
			}
			seen[l.Reference]++
		}
		t.Fees += l.Fee
		t.Net = t.Amount - t.Refunded - t.Fees
	}
	c.r.Checked = len(lines)

	ours := map[string]payments.Payment{}
	for i := 0; i < len(refs); i += 500 {
		batch, err := bus.Ask(ctx, c.s.bus, payments.GetPaymentsByID{IDs: refs[i:min(i+500, len(refs))]})
		if err != nil {
			return err
		}
		for _, p := range batch {
			ours[p.ID] = p
		}
	}
	elsewhere, err := c.s.store.SettledElsewhere(db, "payment", refs, st.ID)
	if err != nil {
		return err
	}
	now := c.s.now()
	newItem := func(paymentID, ref, issue, expected, actual string) *domain.Item {
		return domain.NewItem(c.r.ID, paymentID, ref, st.Provider, issue, expected, actual, now)
	}
	for _, l := range lines {
		if l.Kind != "payment" {
			continue
		}
		p, known := ours[l.Reference]
		settled := domain.FormatAmount(l.Amount, l.Currency)
		switch {
		case seen[l.Reference] > 1 || elsewhere[l.Reference]:
			seen[l.Reference] = 1 // report a duplicate once
			err = c.report(ctx, newItem(p.ID, l.Reference, domain.IssueSettlementDuplicate, "settled once", "settled again"), false)
		case !known:
			err = c.report(ctx, newItem("", l.Reference, domain.IssueSettlementUnknown, "no payment", settled), false)
		case p.Provider != st.Provider:
			err = c.report(ctx, newItem(p.ID, l.Reference, domain.IssueSettlementUnknown, "provider "+p.Provider, "provider "+st.Provider), false)
		case !domain.Captured(p.Status):
			// The provider paid us for it, so the customer was charged.
			err = c.report(ctx, newItem(p.ID, l.Reference, domain.IssueSettlementUnknown, p.Status, "settled "+settled), true)
		case l.Amount != p.Amount || l.Currency != p.Currency:
			err = c.report(ctx, newItem(p.ID, l.Reference, domain.IssueSettlementAmountMismatch,
				domain.FormatAmount(p.Amount, p.Currency), settled), false)
		}
		if err != nil {
			return err
		}
	}

	// Earlier "not settled yet" items closed by this file (timing differences).
	n, err := c.s.store.AutoResolveOpen(db, domain.IssueSettlementMissing, refs,
		"settled in the "+st.SettlementDate.Format("2006-01-02")+" report", now)
	if err != nil {
		return err
	}
	c.sum.CarriedOver = int(n)

	// Captured that day on this provider, but in no settlement yet.
	inFile := map[string]bool{}
	for _, r := range refs {
		inFile[r] = true
	}
	after := ""
	for {
		page, err := bus.Ask(ctx, c.s.bus, payments.ListPayments{Provider: st.Provider,
			Statuses:    []string{payments.StatusSucceeded, payments.StatusPartiallyRefunded, payments.StatusRefunded},
			CreatedFrom: c.r.PeriodStart, CreatedTo: c.r.PeriodEnd, AfterID: after, Limit: 200})
		if err != nil {
			return err
		}
		var check []string
		for _, p := range page {
			if !inFile[p.ID] {
				check = append(check, p.ID)
			}
		}
		earlier, err := c.s.store.SettledElsewhere(db, "payment", check, st.ID)
		if err != nil {
			return err
		}
		for _, p := range page {
			if inFile[p.ID] || earlier[p.ID] {
				continue
			}
			if err := c.report(ctx, newItem(p.ID, p.ID, domain.IssueSettlementMissing, "settled "+domain.FormatAmount(p.Amount, p.Currency),
				"not in this report"), false); err != nil {
				return err
			}
		}
		if len(page) < 200 {
			break
		}
		after = page[len(page)-1].ID
	}

	if err := c.refunds(ctx, st, lines, refundRefs, refundSeen); err != nil {
		return err
	}

	// The bank deposit must equal the report's net for that currency.
	if st.DepositAmount != nil {
		net := totals(c.sum.Settled, *st.DepositCurrency).Net
		c.sum.Deposit = map[string]map[string]any{*st.DepositCurrency: {"deposit": *st.DepositAmount, "net": net,
			"difference": *st.DepositAmount - net}}
		if *st.DepositAmount != net {
			if err := c.report(ctx, newItem("", "deposit:"+st.ID, domain.IssueDepositMismatch,
				domain.FormatAmount(net, *st.DepositCurrency), domain.FormatAmount(*st.DepositAmount, *st.DepositCurrency)), false); err != nil {
				return err
			}
		}
	}
	return nil
}

// refunds: refund lines of the report against our refunds, and our
// succeeded refunds of that day that the provider has not reported.
func (c *checker) refunds(ctx context.Context, st *store.Settlement, lines []store.SettlementLine, refs []string, seen map[string]int) error {
	db := c.s.db.WithContext(ctx)
	ours := map[string]refunds.Refund{}
	for i := 0; i < len(refs); i += 500 {
		batch, err := bus.Ask(ctx, c.s.bus, refunds.GetRefundsByID{IDs: refs[i:min(i+500, len(refs))]})
		if err != nil {
			return err
		}
		for _, r := range batch {
			ours[r.ID] = r
		}
	}
	elsewhere, err := c.s.store.SettledElsewhere(db, "refund", refs, st.ID)
	if err != nil {
		return err
	}
	now := c.s.now()
	item := func(paymentID, ref, issue, expected, actual string) *domain.Item {
		return domain.NewItem(c.r.ID, paymentID, ref, st.Provider, issue, expected, actual, now)
	}
	for _, l := range lines {
		if l.Kind != "refund" {
			continue
		}
		r, known := ours[l.Reference]
		settled := "refunded " + domain.FormatAmount(l.Amount, l.Currency)
		switch {
		case seen[l.Reference] > 1 || elsewhere[l.Reference]:
			seen[l.Reference] = 1
			err = c.report(ctx, item(r.PaymentID, l.Reference, domain.IssueSettlementDuplicate, "refunded once", "refunded again"), false)
		case !known:
			// Money went back to a customer for a refund we never made.
			err = c.report(ctx, item("", l.Reference, domain.IssueSettlementUnknown, "no refund", settled), false)
		case r.Status != refunds.StatusSucceeded:
			err = c.report(ctx, item(r.PaymentID, l.Reference, domain.IssueSettlementUnknown, "refund "+r.Status, settled), false)
		case l.Amount != r.Amount || l.Currency != r.Currency:
			err = c.report(ctx, item(r.PaymentID, l.Reference, domain.IssueSettlementAmountMismatch,
				"refunded "+domain.FormatAmount(r.Amount, r.Currency), settled), false)
		}
		if err != nil {
			return err
		}
	}
	n, err := c.s.store.AutoResolveOpen(db, domain.IssueSettlementMissing, refs,
		"refund settled in the "+st.SettlementDate.Format("2006-01-02")+" report", now)
	if err != nil {
		return err
	}
	c.sum.CarriedOver += int(n)

	inFile := map[string]bool{}
	for _, r := range refs {
		inFile[r] = true
	}
	after := ""
	for {
		page, err := bus.Ask(ctx, c.s.bus, refunds.ListRefunds{Statuses: []string{refunds.StatusSucceeded}, Provider: st.Provider,
			CreatedFrom: c.r.PeriodStart, CreatedTo: c.r.PeriodEnd, AfterID: after, Limit: 200})
		if err != nil {
			return err
		}
		var check []string
		for _, r := range page {
			if !inFile[r.ID] {
				check = append(check, r.ID)
			}
		}
		earlier, err := c.s.store.SettledElsewhere(db, "refund", check, st.ID)
		if err != nil {
			return err
		}
		for _, r := range page {
			if inFile[r.ID] || earlier[r.ID] {
				continue
			}
			if err := c.report(ctx, item(r.PaymentID, r.ID, domain.IssueSettlementMissing,
				"refund of "+domain.FormatAmount(r.Amount, r.Currency), "not in this report"), false); err != nil {
				return err
			}
		}
		if len(page) < 200 {
			return nil
		}
		after = page[len(page)-1].ID
	}
}

func view(r *domain.Run, items []domain.Item) RunView {
	v := RunView{ID: r.ID, Kind: r.Kind, Provider: r.Provider, SettlementID: r.SettlementID, PeriodStart: r.PeriodStart,
		PeriodEnd: r.PeriodEnd, Status: r.Status, Checked: r.Checked, Mismatched: r.Mismatched, Error: r.Error,
		CreatedAt: r.CreatedAt, CompletedAt: r.CompletedAt, Items: items}
	if r.Summary != "" && r.Summary != "{}" {
		v.Summary = json.RawMessage(r.Summary)
	}
	return v
}
