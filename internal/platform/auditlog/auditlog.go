// Package auditlog records who did what, for the whole backend.
//
// Every process has one Recorder. The HTTP middleware (httpx.Audit) hands it
// one Entry per action; the Recorder delivers entries in the background to
// the audit service (over the bus, wherever it runs), so recording never
// slows a request down. If the audit service cannot be reached, the entry is
// retried and finally written to the process log as "audit_entry_unsent", so
// it can still be recovered from log storage. Nothing is silently dropped.
package auditlog

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"paygate/internal/platform/metrics"
)

// Who acted.
const (
	ActorUser      = "user"      // a merchant team member (dashboard)
	ActorAPIKey    = "api_key"   // a merchant's server
	ActorStaff     = "staff"     // platform staff (admin console / admin API)
	ActorCustomer  = "customer"  // a payer on the hosted checkout
	ActorProvider  = "provider"  // a payment provider calling back
	ActorSystem    = "system"    // a background job
	ActorAnonymous = "anonymous" // not signed in (sign-in attempts, bad tokens)
)

// Outcomes.
const (
	OutcomeSuccess     = "success"
	OutcomeDenied      = "denied"       // 401 / 403
	OutcomeRejected    = "rejected"     // 4xx validation, conflicts, not found
	OutcomeRateLimited = "rate_limited" // 429
	OutcomeError       = "error"        // 5xx
)

// Entry is one audit record. It never holds request bodies, headers, query
// strings, passwords, codes or tokens.
type Entry struct {
	OccurredAt    time.Time         `json:"occurred_at"`
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
	DurationMS    int64             `json:"duration_ms,omitempty"`
	Details       map[string]string `json:"details,omitempty"`
	Source        string            `json:"source,omitempty"` // the service (process) that recorded it
}

// Sink stores one entry (the audit service, reached over the bus).
type Sink func(ctx context.Context, e Entry) error

var (
	recorded = metrics.Counter("paygate_audit_entries_total", "Audit entries by delivery result", "result")
)

// Recorder queues entries and delivers them in the background.
type Recorder struct {
	sink   Sink
	log    *slog.Logger
	source string
	ch     chan Entry
	once   sync.Once
	done   chan struct{}
}

// NewRecorder: queue holds up to size entries waiting for delivery.
func NewRecorder(sink Sink, source string, size int, log *slog.Logger) *Recorder {
	if size <= 0 {
		size = 10_000
	}
	return &Recorder{sink: sink, log: log, source: source, ch: make(chan Entry, size), done: make(chan struct{})}
}

// Record queues an entry. It never blocks: when the queue is full the entry
// goes to the process log instead.
func (r *Recorder) Record(e Entry) {
	if r == nil {
		return
	}
	if e.OccurredAt.IsZero() {
		e.OccurredAt = time.Now()
	}
	e.OccurredAt = e.OccurredAt.UTC()
	if e.Source == "" {
		e.Source = r.source
	}
	select {
	case r.ch <- e:
	default:
		r.unsent(e, "queue full")
	}
}

// Run delivers queued entries until ctx is done, then delivers what is left
// (for up to 10 seconds) so a clean shutdown loses nothing.
func (r *Recorder) Run(ctx context.Context) {
	defer close(r.done)
	for {
		select {
		case e := <-r.ch:
			r.deliver(ctx, e)
		case <-ctx.Done():
			drain, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			for {
				select {
				case e := <-r.ch:
					r.deliver(drain, e)
				default:
					return
				}
			}
		}
	}
}

// Wait blocks until Run has returned.
func (r *Recorder) Wait() { <-r.done }

func (r *Recorder) deliver(ctx context.Context, e Entry) {
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		err = r.sink(cctx, e)
		cancel()
		if err == nil {
			recorded.Inc("stored")
			return
		}
		select {
		case <-time.After(time.Duration(attempt) * 200 * time.Millisecond):
		case <-ctx.Done():
			attempt = 3
		}
	}
	r.unsent(e, err.Error())
}

func (r *Recorder) unsent(e Entry, why string) {
	recorded.Inc("unsent")
	b, _ := json.Marshal(e)
	// Alert on this line: an audit entry is only in the process log.
	r.log.Error("audit_entry_unsent", "reason", why, "entry", string(b))
}
