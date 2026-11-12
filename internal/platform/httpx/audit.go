package httpx

import (
	"bytes"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"paygate/internal/platform/auditlog"
	"paygate/internal/platform/reqctx"
)

// AuditOptions configures the audit middleware.
type AuditOptions struct {
	Recorder   *auditlog.Recorder
	TrustProxy bool
	// AllReads records every GET as well. Off: only changes (POST, PUT,
	// PATCH, DELETE), every staff (/admin) request and sensitive reads
	// (documents, files, exports, secrets lists) are recorded.
	AllReads bool
}

// Audit records one audit entry per action. It must be the outermost
// middleware after Correlation, so rejected requests (bad tokens, rate
// limits) are recorded too.
func Audit(o AuditOptions) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if o.Recorder == nil || r.Method == http.MethodOptions || skipAudit(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			start := time.Now()
			ctx, slot := reqctx.WithAuditSlot(r.Context())
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			head := &headBuffer{limit: 512}
			ww.Tee(head) // the first bytes only: the id of a created record
			r = r.WithContext(ctx)
			next.ServeHTTP(ww, r)

			route, params := routeOf(r)
			if !shouldAudit(r.Method, route, o.AllReads) {
				return
			}
			status := ww.Status()
			if status == 0 {
				status = http.StatusOK
			}
			d := slot.Data()
			e := auditlog.Entry{
				OccurredAt: start, Method: r.Method, Route: route, Status: status, Outcome: outcomeOf(status),
				ErrorCode: d.ErrorCode, IP: ClientIP(r, o.TrustProxy), UserAgent: truncate(r.UserAgent(), 200),
				CorrelationID: reqctx.CorrelationID(ctx), DurationMS: time.Since(start).Milliseconds(),
				Details: d.Details, ActorLabel: d.ActorLabel,
			}
			setActor(&e, d.Actor, route, params)
			if e.MerchantID == "" {
				e.MerchantID = d.MerchantID
			}
			e.Action = d.Action
			if e.Action == "" {
				e.Action = ActionOf(r.Method, route)
			}
			e.ResourceType, e.ResourceID = d.ResourceType, d.ResourceID
			if e.ResourceID == "" {
				e.ResourceType, e.ResourceID = resourceOf(route, params)
			}
			if e.ResourceID == "" && status == http.StatusCreated {
				if m := createdID.FindSubmatch(head.Bytes()); m != nil {
					e.ResourceID = string(m[1])
				}
			}
			o.Recorder.Record(e)
		})
	}
}

// Paths that are never actions: health, metrics, documentation.
func skipAudit(path string) bool {
	switch {
	case path == "/healthz", path == "/metrics", path == "/favicon.ico",
		strings.HasPrefix(path, "/docs"), strings.HasPrefix(path, "/swagger"), strings.HasPrefix(path, "/openapi"),
		strings.HasPrefix(path, "/.well-known/"):
		return true
	}
	return false
}

var sensitiveRead = regexp.MustCompile(`/documents/|/files/|\.csv$|^/v1/api-keys|^/v1/webhook-endpoints|^/v1/audit-logs|^/v1/team`)

func shouldAudit(method, route string, allReads bool) bool {
	if method != http.MethodGet && method != http.MethodHead {
		return true
	}
	if allReads {
		return !strings.HasSuffix(route, "/status") // checkout polling
	}
	return strings.HasPrefix(route, "/admin/") || sensitiveRead.MatchString(route)
}

func routeOf(r *http.Request) (string, map[string]string) {
	rc := chi.RouteContext(r.Context())
	if rc == nil || rc.RoutePattern() == "" {
		return "unmatched", nil
	}
	params := map[string]string{}
	for i, k := range rc.URLParams.Keys {
		if k != "*" && i < len(rc.URLParams.Values) {
			params[k] = rc.URLParams.Values[i]
		}
	}
	return strings.TrimSuffix(rc.RoutePattern(), "/*"), params
}

func setActor(e *auditlog.Entry, p *reqctx.Principal, route string, params map[string]string) {
	switch {
	case p != nil:
		e.ActorID, e.MerchantID, e.Environment = p.Subject, p.MerchantID, p.Environment
		switch {
		case p.Kind == reqctx.KindAPIKey:
			e.ActorKind = auditlog.ActorAPIKey
		case p.IsPlatform():
			e.ActorKind = auditlog.ActorStaff
		default:
			e.ActorKind = auditlog.ActorUser
		}
	case strings.HasPrefix(route, "/providers/"):
		e.ActorKind, e.ActorID = auditlog.ActorProvider, params["provider"]
	case strings.HasPrefix(route, "/pay/"), strings.HasPrefix(route, "/l/"):
		e.ActorKind = auditlog.ActorCustomer
	default:
		e.ActorKind = auditlog.ActorAnonymous
	}
}

func outcomeOf(status int) string {
	switch {
	case status < 400:
		return auditlog.OutcomeSuccess
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return auditlog.OutcomeDenied
	case status == http.StatusTooManyRequests:
		return auditlog.OutcomeRateLimited
	case status >= 500:
		return auditlog.OutcomeError
	default:
		return auditlog.OutcomeRejected
	}
}

// Verbs that end a route and name the action themselves.
var routeVerbs = map[string]bool{
	"start": true, "submit": true, "accept": true, "roll": true, "retry": true, "deactivate": true, "approve": true,
	"reject": true, "confirm": true, "cancel": true, "close": true, "paid": true, "failed": true, "release": true,
	"rotate_secret": true, "resend": true, "verify": true, "setup": true, "disable": true, "evidence": true,
	"refresh": true, "logout": true, "login": true, "signup": true, "forgot": true, "reset": true, "callback": true,
	"sync": true, "run": true, "resolve": true,
}

// ActionOf names an action from its route: POST /v1/payments is
// "payments.create", POST /admin/payouts/{id}/paid is "admin.payouts.paid",
// DELETE /v1/api-keys/{id} is "api_keys.delete", POST /auth/login is "auth.login".
func ActionOf(method, route string) string {
	segs := strings.Split(strings.Trim(route, "/"), "/")
	prefix := ""
	if len(segs) > 0 && (segs[0] == "v1") {
		segs = segs[1:]
	} else if len(segs) > 0 && segs[0] == "admin" {
		prefix, segs = "admin.", segs[1:]
	}
	var lits []string
	lastIsParam, prevIsParam := false, false
	for i, s := range segs {
		param := strings.HasPrefix(s, "{")
		if i == len(segs)-1 {
			lastIsParam = param
			prevIsParam = i > 0 && strings.HasPrefix(segs[i-1], "{")
		}
		if !param && s != "" {
			s = strings.NewReplacer("-", "_", ".", "_").Replace(s)
			lits = append(lits, s)
		}
	}
	if len(lits) == 0 {
		return prefix + strings.ToLower(method)
	}
	name := prefix + strings.Join(lits, ".")
	last := lits[len(lits)-1]
	switch {
	case lits[0] == "auth" || lits[0] == "pay" || lits[0] == "providers":
		if method == http.MethodGet {
			return name + ".view"
		}
		return name
	case strings.HasSuffix(last, "_csv"):
		return strings.TrimSuffix(name, "_csv") + ".export"
	case lastIsParam:
		switch method {
		case http.MethodGet, http.MethodHead:
			return name + ".view"
		case http.MethodPut, http.MethodPatch:
			return name + ".update"
		case http.MethodDelete:
			return name + ".delete"
		}
		return name + "." + strings.ToLower(method)
	case method == http.MethodPost && (prevIsParam || routeVerbs[last]):
		return name
	}
	switch method {
	case http.MethodGet, http.MethodHead:
		return name + ".list"
	case http.MethodPost:
		return name + ".create"
	case http.MethodPut, http.MethodPatch:
		return name + ".update"
	case http.MethodDelete:
		return name + ".delete"
	}
	return name + "." + strings.ToLower(method)
}

// resourceOf: the first path parameter and the literal before it, e.g.
// /v1/payments/{id}/events gives ("payments", <id>).
func resourceOf(route string, params map[string]string) (string, string) {
	segs := strings.Split(strings.Trim(route, "/"), "/")
	for i, s := range segs {
		if strings.HasPrefix(s, "{") && i > 0 {
			key := strings.Trim(s, "{}")
			if j := strings.IndexByte(key, ':'); j >= 0 {
				key = key[:j]
			}
			if v := params[key]; v != "" && key != "token" { // never record one-time file tokens
				return strings.ReplaceAll(segs[i-1], "-", "_"), truncate(v, 100)
			}
		}
	}
	return "", ""
}

var createdID = regexp.MustCompile(`^\s*\{\s*"id"\s*:\s*"([A-Za-z0-9_\-]{1,64})"`)

type headBuffer struct {
	bytes.Buffer
	limit int
}

func (h *headBuffer) Write(p []byte) (int, error) {
	if room := h.limit - h.Len(); room > 0 {
		h.Buffer.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
