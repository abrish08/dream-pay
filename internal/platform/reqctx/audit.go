package reqctx

import (
	"context"
	"sync"
)

// AuditSlot collects what the audit log records about one request. The
// audit middleware creates it before anything else runs; later layers fill
// it in: the auth middleware sets the caller, handlers and services may name
// the action, the resource and add details (never secrets).
type AuditSlot struct {
	mu         sync.Mutex
	actor      *Principal
	actorLabel string
	merchantID string // the business concerned when the actor is unknown
	action     string
	resType    string
	resID      string
	details    map[string]string
	errorCode  string
}

// AuditData is a copy of a slot's contents.
type AuditData struct {
	Actor        *Principal
	ActorLabel   string
	MerchantID   string
	Action       string
	ResourceType string
	ResourceID   string
	Details      map[string]string
	ErrorCode    string
}

type auditKey struct{}

// WithAuditSlot returns a context carrying a new, empty slot.
func WithAuditSlot(ctx context.Context) (context.Context, *AuditSlot) {
	s := &AuditSlot{}
	return context.WithValue(ctx, auditKey{}, s), s
}

func auditSlot(ctx context.Context) *AuditSlot {
	s, _ := ctx.Value(auditKey{}).(*AuditSlot)
	return s
}

// AuditActor records who acted (e.g. after a successful sign-in, when the
// request itself carried no token). label is a readable name such as an email.
func AuditActor(ctx context.Context, p Principal, label string) {
	if s := auditSlot(ctx); s != nil {
		s.mu.Lock()
		s.actor = &p
		if label != "" {
			s.actorLabel = label
		}
		s.mu.Unlock()
	}
}

// AuditMerchant names the business an action concerned when the caller is
// not (yet) known, e.g. a failed sign-in to one of its team's accounts, so
// the business sees it in its own activity log. It does not name the actor.
func AuditMerchant(ctx context.Context, merchantID string) {
	if s := auditSlot(ctx); s != nil {
		s.mu.Lock()
		s.merchantID = merchantID
		s.mu.Unlock()
	}
}

// AuditAction names the action, overriding the one derived from the route.
func AuditAction(ctx context.Context, action string) {
	if s := auditSlot(ctx); s != nil {
		s.mu.Lock()
		s.action = action
		s.mu.Unlock()
	}
}

// AuditResource names the record the action was about.
func AuditResource(ctx context.Context, kind, id string) {
	if s := auditSlot(ctx); s != nil {
		s.mu.Lock()
		s.resType, s.resID = kind, id
		s.mu.Unlock()
	}
}

// AuditDetail adds one detail (at most 20 per request, values cut to 200
// characters). Never pass secrets, passwords, codes or full card numbers.
func AuditDetail(ctx context.Context, key, value string) {
	if s := auditSlot(ctx); s != nil && key != "" && value != "" {
		s.mu.Lock()
		if s.details == nil {
			s.details = map[string]string{}
		}
		if len(s.details) < 20 {
			if len(value) > 200 {
				value = value[:200]
			}
			s.details[key] = value
		}
		s.mu.Unlock()
	}
}

// AuditError records the API error code of a failed request.
func AuditError(ctx context.Context, code string) {
	if s := auditSlot(ctx); s != nil {
		s.mu.Lock()
		s.errorCode = code
		s.mu.Unlock()
	}
}

func (s *AuditSlot) Data() AuditData {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := AuditData{Actor: s.actor, ActorLabel: s.actorLabel, MerchantID: s.merchantID, Action: s.action, ResourceType: s.resType,
		ResourceID: s.resID, ErrorCode: s.errorCode}
	if len(s.details) > 0 {
		d.Details = make(map[string]string, len(s.details))
		for k, v := range s.details {
			d.Details[k] = v
		}
	}
	return d
}
