package httpx

import (
	"hash/fnv"
	"log/slog"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"paygate/internal/platform/apperror"
	"paygate/internal/platform/reqctx"
)

// Limiter is an in-memory token-bucket rate limiter: each key gets Rate
// requests per second with bursts up to Burst. Keys are spread over shards so
// concurrent requests rarely wait on the same lock.
//
// Limits are per process. With several replicas the effective limit is
// Rate x replicas; put a shared limiter (gateway or Redis) in front if you
// need one global number.
type Limiter struct {
	Rate  float64 // tokens per second
	Burst float64

	shards [64]shard
	now    func() time.Time
}

type shard struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	swept   time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewLimiter returns nil when rate <= 0 (limiting disabled).
func NewLimiter(rate, burst float64) *Limiter {
	if rate <= 0 {
		return nil
	}
	if burst < 1 {
		burst = math.Max(1, rate)
	}
	l := &Limiter{Rate: rate, Burst: burst, now: time.Now}
	for i := range l.shards {
		l.shards[i].buckets = map[string]*bucket{}
	}
	return l
}

// Allow takes one token for key. It returns the tokens left and, when
// refused, how long until the next token.
func (l *Limiter) Allow(key string) (ok bool, remaining int, retryAfter time.Duration) {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	s := &l.shards[h.Sum32()%uint32(len(l.shards))]
	now := l.now()

	s.mu.Lock()
	defer s.mu.Unlock()
	if now.Sub(s.swept) > time.Minute { // drop idle buckets (they are full anyway)
		for k, b := range s.buckets {
			if now.Sub(b.last) > 10*time.Minute {
				delete(s.buckets, k)
			}
		}
		s.swept = now
	}
	b, found := s.buckets[key]
	if !found {
		b = &bucket{tokens: l.Burst, last: now}
		s.buckets[key] = b
	}
	b.tokens = math.Min(l.Burst, b.tokens+now.Sub(b.last).Seconds()*l.Rate)
	b.last = now
	if b.tokens < 1 {
		wait := time.Duration((1 - b.tokens) / l.Rate * float64(time.Second))
		return false, 0, wait
	}
	b.tokens--
	return true, int(b.tokens), 0
}

// KeyFunc picks what a limit applies to.
type KeyFunc func(r *http.Request) string

// ByCaller limits per authenticated caller (API key or user), falling back to
// the client IP. Use after the Auth middleware.
func ByCaller(trustProxy bool) KeyFunc {
	return func(r *http.Request) string {
		if p, ok := reqctx.PrincipalFrom(r.Context()); ok && p.Subject != "" {
			return p.Kind + ":" + p.Subject
		}
		return "ip:" + ClientIP(r, trustProxy)
	}
}

// ByIP limits per client address.
func ByIP(trustProxy bool) KeyFunc {
	return func(r *http.Request) string { return "ip:" + ClientIP(r, trustProxy) }
}

// ClientIP: X-Forwarded-For is only trusted behind a proxy you control.
func ClientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			return strings.TrimSpace(strings.Split(xff, ",")[0])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

var errRateLimited = apperror.New(apperror.KindTooManyRequests, "rate_limited", "too many requests; slow down and retry after the Retry-After seconds")

// RateLimit rejects requests over the limit with 429 and Retry-After. A nil
// limiter lets everything through.
func RateLimit(l *Limiter, key KeyFunc, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if l == nil {
			return next
		}
		limit := strconv.Itoa(int(l.Burst))
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ok, remaining, retry := l.Allow(key(r))
			w.Header().Set("X-RateLimit-Limit", limit)
			w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
			if !ok {
				w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(retry.Seconds()))))
				WriteError(w, r, log, errRateLimited)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// CORS allows browser apps (a merchant dashboard) on the given origins to
// call the API. Server-to-server integrations do not need it.
func CORS(origins []string) func(http.Handler) http.Handler {
	allowed := map[string]bool{}
	for _, o := range origins {
		if o = strings.TrimRight(strings.TrimSpace(o), "/"); o != "" {
			allowed[o] = true
		}
	}
	return func(next http.Handler) http.Handler {
		if len(allowed) == 0 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin != "" && (allowed[origin] || allowed["*"]) {
				h := w.Header()
				h.Set("Access-Control-Allow-Origin", origin)
				h.Add("Vary", "Origin")
				h.Set("Access-Control-Expose-Headers",
					"X-Correlation-ID, Idempotent-Replayed, X-RateLimit-Limit, X-RateLimit-Remaining, Retry-After")
				if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
					h.Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
					h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key, X-Correlation-ID")
					h.Set("Access-Control-Max-Age", "600")
					w.WriteHeader(http.StatusNoContent)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}
