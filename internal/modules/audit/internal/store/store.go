// Package store keeps the audit log: append-only rows chained by hash.
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"paygate/internal/platform/auditlog"
)

// Genesis is the "previous hash" of the first row.
const Genesis = "0000000000000000000000000000000000000000000000000000000000000000"

type Row struct {
	ID            int64             `json:"id" gorm:"primaryKey"`
	OccurredAt    time.Time         `json:"occurred_at"`
	RecordedAt    time.Time         `json:"recorded_at"`
	ActorKind     string            `json:"actor_kind"`
	ActorID       string            `json:"actor_id,omitempty"`
	ActorLabel    string            `json:"actor_label,omitempty"`
	MerchantID    string            `json:"merchant_id,omitempty"`
	Environment   string            `json:"environment,omitempty"`
	Action        string            `json:"action"`
	Method        string            `json:"method,omitempty"`
	Route         string            `json:"route,omitempty"`
	ResourceType  string            `json:"resource_type,omitempty"`
	ResourceID    string            `json:"resource_id,omitempty"`
	Outcome       string            `json:"outcome"`
	Status        int               `json:"status,omitempty"`
	ErrorCode     string            `json:"error_code,omitempty"`
	IP            string            `json:"ip,omitempty"`
	UserAgent     string            `json:"user_agent,omitempty"`
	CorrelationID string            `json:"correlation_id,omitempty"`
	DurationMS    int64             `json:"duration_ms"`
	Details       map[string]string `json:"details,omitempty" gorm:"serializer:json"`
	Source        string            `json:"source,omitempty"`
	PrevHash      string            `json:"prev_hash"`
	Hash          string            `json:"hash"`
}

func (Row) TableName() string { return "audit_logs" }

// FromEntry normalises an entry into a row (times in UTC, microseconds, as
// Postgres stores them, so the hash can be recomputed later).
func FromEntry(e auditlog.Entry, now time.Time) Row {
	cut := func(s string, n int) string {
		if len(s) > n {
			return s[:n]
		}
		return s
	}
	return Row{
		OccurredAt: e.OccurredAt.UTC().Truncate(time.Microsecond), RecordedAt: now.UTC().Truncate(time.Microsecond),
		ActorKind: cut(e.ActorKind, 20), ActorID: cut(e.ActorID, 100), ActorLabel: cut(e.ActorLabel, 200),
		MerchantID: cut(e.MerchantID, 100), Environment: cut(e.Environment, 10), Action: cut(e.Action, 120),
		Method: cut(e.Method, 10), Route: cut(e.Route, 200), ResourceType: cut(e.ResourceType, 60), ResourceID: cut(e.ResourceID, 100),
		Outcome: cut(e.Outcome, 20), Status: e.Status, ErrorCode: cut(e.ErrorCode, 80), IP: cut(e.IP, 60),
		UserAgent: cut(e.UserAgent, 200), CorrelationID: cut(e.CorrelationID, 100), DurationMS: e.DurationMS,
		Details: details(e.Details), Source: cut(e.Source, 60),
	}
}

func details(d map[string]string) map[string]string {
	if d == nil {
		return map[string]string{}
	}
	return d
}

// ComputeHash covers every stored field and the previous row's hash.
func ComputeHash(r Row) string {
	keys := make([]string, 0, len(r.Details))
	for k := range r.Details {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var details strings.Builder
	for _, k := range keys {
		details.WriteString(strconv.Quote(k) + "=" + strconv.Quote(r.Details[k]) + ";")
	}
	fields := []string{
		r.PrevHash, strconv.FormatInt(r.ID, 10), r.OccurredAt.UTC().Format(time.RFC3339Nano), r.RecordedAt.UTC().Format(time.RFC3339Nano),
		r.ActorKind, r.ActorID, r.ActorLabel, r.MerchantID, r.Environment, r.Action, r.Method, r.Route,
		r.ResourceType, r.ResourceID, r.Outcome, strconv.Itoa(r.Status), r.ErrorCode, r.IP, r.UserAgent,
		r.CorrelationID, strconv.FormatInt(r.DurationMS, 10), details.String(), r.Source,
	}
	for i, f := range fields {
		fields[i] = strconv.Quote(f)
	}
	sum := sha256.Sum256([]byte(strings.Join(fields, "|")))
	return hex.EncodeToString(sum[:])
}

type Store struct{}

func New() *Store { return &Store{} }

// Append adds a row at the end of the chain. The chain is extended by one
// writer at a time (transaction lock), so it never forks.
func (s *Store) Append(db *gorm.DB, r Row) (int64, error) {
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SELECT pg_advisory_xact_lock(hashtext('audit_logs_chain'))").Error; err != nil {
			return err
		}
		var last Row
		err := tx.Select("id", "hash").Order("id DESC").Limit(1).Take(&last).Error
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
			r.PrevHash = Genesis
		case err != nil:
			return err
		default:
			r.PrevHash = last.Hash
		}
		if err := tx.Raw("SELECT nextval(pg_get_serial_sequence('audit_logs', 'id'))").Scan(&r.ID).Error; err != nil {
			return err
		}
		r.Hash = ComputeHash(r)
		return tx.Create(&r).Error
	})
	return r.ID, err
}

type Filter struct {
	MerchantID string // required for merchant views
	ActorID    string
	ActorKind  string
	Action     string // prefix, e.g. "payouts."
	ResourceID string
	Outcome    string
	From, To   time.Time
	BeforeID   int64
	Limit      int
}

func (s *Store) List(db *gorm.DB, f Filter) ([]Row, bool, error) {
	q := db.Model(&Row{})
	if f.MerchantID != "" {
		q = q.Where("merchant_id = ?", f.MerchantID)
	}
	if f.ActorID != "" {
		q = q.Where("actor_id = ?", f.ActorID)
	}
	if f.ActorKind != "" {
		q = q.Where("actor_kind = ?", f.ActorKind)
	}
	if f.Action != "" {
		q = q.Where("action LIKE ?", strings.NewReplacer("%", `\%`, "_", `\_`).Replace(f.Action)+"%")
	}
	if f.ResourceID != "" {
		q = q.Where("resource_id = ?", f.ResourceID)
	}
	if f.Outcome != "" {
		q = q.Where("outcome = ?", f.Outcome)
	}
	if !f.From.IsZero() {
		q = q.Where("occurred_at >= ?", f.From)
	}
	if !f.To.IsZero() {
		q = q.Where("occurred_at < ?", f.To)
	}
	if f.BeforeID > 0 {
		q = q.Where("id < ?", f.BeforeID)
	}
	var rows []Row
	if err := q.Order("id DESC").Limit(f.Limit + 1).Find(&rows).Error; err != nil {
		return nil, false, err
	}
	more := len(rows) > f.Limit
	if more {
		rows = rows[:f.Limit]
	}
	return rows, more, nil
}

// VerifyResult: Intact is false at the first row whose hash or link is wrong.
type VerifyResult struct {
	Intact   bool   `json:"intact"`
	Checked  int64  `json:"checked"`
	LastID   int64  `json:"last_id"`
	BrokenAt int64  `json:"broken_at,omitempty"`
	Problem  string `json:"problem,omitempty"`
	LastHash string `json:"last_hash,omitempty"`
}

// Verify recomputes the whole chain, oldest first, in batches.
func (s *Store) Verify(db *gorm.DB, batch int) (VerifyResult, error) {
	res := VerifyResult{Intact: true}
	prev, after := Genesis, int64(0)
	for {
		var rows []Row
		if err := db.Where("id > ?", after).Order("id").Limit(batch).Find(&rows).Error; err != nil {
			return res, err
		}
		for _, r := range rows {
			res.Checked++
			res.LastID = r.ID
			switch {
			case r.PrevHash != prev:
				res.Intact, res.BrokenAt, res.Problem = false, r.ID, "the link to the previous entry is broken (an entry was removed or inserted)"
			case ComputeHash(r) != r.Hash:
				res.Intact, res.BrokenAt, res.Problem = false, r.ID, "the entry was changed after it was recorded"
			}
			if !res.Intact {
				return res, nil
			}
			prev = r.Hash
		}
		if len(rows) < batch {
			res.LastHash = prev
			return res, nil
		}
		after = rows[len(rows)-1].ID
	}
}

// DetailsJSON is used by tests and exports.
func DetailsJSON(d map[string]string) string {
	b, _ := json.Marshal(d)
	return fmt.Sprint(string(b))
}
