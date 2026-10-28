package httpx

import (
	"context"
	"crypto/sha256"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"paygate/internal/platform/apperror"
	"paygate/internal/platform/authn"
	"paygate/internal/platform/reqctx"
)

// KeyExchanger trades a raw API key for a short-lived signed access token.
// The app implements it by asking the auth service on the bus.
type KeyExchanger func(ctx context.Context, apiKey string) (token string, err error)

// Auth authenticates requests in every service:
//
//   - "Authorization: Bearer <JWT>" (dashboard users): verified locally with
//     the auth service's public key. No network call.
//   - "Authorization: Bearer sk_live_..." (merchant servers): exchanged once
//     with the auth service for a signed token, then cached briefly, so a
//     busy key costs one auth call per CacheTTL per service instance.
//
// Either way the request ends up with a verified Principal and the signed
// token in its context. The token is forwarded on the bus so the next
// service can verify it too.
type Auth struct {
	Verifier *authn.Verifier
	Exchange KeyExchanger
	CacheTTL time.Duration // 0 disables the API key cache (revocation is then immediate)
	Log      *slog.Logger

	mu    sync.Mutex
	cache map[[32]byte]cachedToken
}

type cachedToken struct {
	token string
	p     reqctx.Principal
	until time.Time
}

const maxCachedKeys = 10_000

var errMissingCredentials = apperror.Unauthorized("missing_credentials",
	"send Authorization: Bearer <API key or access token>")

// Middleware rejects requests without valid credentials.
func (a *Auth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		raw = strings.TrimSpace(raw)
		if !ok || raw == "" {
			WriteError(w, r, a.Log, errMissingCredentials)
			return
		}
		p, token, err := a.resolve(r.Context(), raw)
		if err != nil {
			WriteError(w, r, a.Log, err)
			return
		}
		ctx := reqctx.WithToken(reqctx.WithPrincipal(r.Context(), p), token)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (a *Auth) resolve(ctx context.Context, raw string) (reqctx.Principal, string, error) {
	if !strings.HasPrefix(raw, "sk_") {
		p, err := a.Verifier.Verify(raw)
		return p, raw, err
	}
	key := sha256.Sum256([]byte(raw)) // never keep raw keys in memory longer than needed
	now := time.Now()
	if a.CacheTTL > 0 {
		a.mu.Lock()
		c, hit := a.cache[key]
		a.mu.Unlock()
		if hit && now.Before(c.until) {
			return c.p, c.token, nil
		}
	}
	token, err := a.Exchange(ctx, raw)
	if err != nil {
		return reqctx.Principal{}, "", err
	}
	p, err := a.Verifier.Verify(token)
	if err != nil {
		return reqctx.Principal{}, "", err
	}
	if a.CacheTTL > 0 {
		a.mu.Lock()
		if a.cache == nil || len(a.cache) >= maxCachedKeys {
			a.cache = map[[32]byte]cachedToken{}
		}
		a.cache[key] = cachedToken{token: token, p: p, until: now.Add(a.CacheTTL)}
		a.mu.Unlock()
	}
	return p, token, nil
}

// RequireMerchant allows only callers that act for a merchant (API keys and
// merchant team members). Every /v1 route sits behind it.
func RequireMerchant(log *slog.Logger) func(http.Handler) http.Handler {
	return guard(log, func(p reqctx.Principal) error {
		if p.MerchantID == "" {
			return apperror.Forbidden("merchant_only", "this endpoint is for merchant accounts")
		}
		return nil
	})
}

// RequirePlatform allows only platform staff. Every /admin route sits behind it.
func RequirePlatform(log *slog.Logger) func(http.Handler) http.Handler {
	return guard(log, func(p reqctx.Principal) error {
		if !p.IsPlatform() {
			return apperror.Forbidden("platform_only", "this endpoint is for platform staff")
		}
		return nil
	})
}

// RequireUser allows only logged-in people, not API keys (e.g. managing keys
// and team members must not be possible with a leaked API key).
func RequireUser(log *slog.Logger) func(http.Handler) http.Handler {
	return guard(log, func(p reqctx.Principal) error {
		if p.Kind != reqctx.KindUser {
			return apperror.Forbidden("user_login_required", "log in to the dashboard to do this; API keys cannot")
		}
		return nil
	})
}

// Require checks one RBAC permission.
func Require(permission string, log *slog.Logger) func(http.Handler) http.Handler {
	return guard(log, func(p reqctx.Principal) error {
		if !p.Can(permission) {
			return apperror.Forbidden("permission_denied", "missing permission "+permission)
		}
		return nil
	})
}

func guard(log *slog.Logger, check func(reqctx.Principal) error) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := reqctx.PrincipalFrom(r.Context())
			if !ok {
				WriteError(w, r, log, errMissingCredentials)
				return
			}
			if err := check(p); err != nil {
				WriteError(w, r, log, err)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
