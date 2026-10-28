// Package reqctx carries request-scoped values (who is calling, correlation id)
// through context.Context. Business code reads them from here and never from
// HTTP headers, so it works the same for HTTP, bus requests and event handlers.
package reqctx

import (
	"context"
	"slices"
	"time"
)

// Kinds of principal.
const (
	KindUser   = "user"    // a person logged in to the dashboard (platform staff or merchant team)
	KindAPIKey = "api_key" // a merchant's server using an API key
)

// Principal is the authenticated caller. It is built only from a verified,
// signed access token, never from unverified input.
type Principal struct {
	Kind        string   `json:"kind"`
	Subject     string   `json:"sub"`                   // user id or API key id
	MerchantID  string   `json:"merchant_id,omitempty"` // empty for platform staff
	Role        string   `json:"role,omitempty"`        // users only: primary role name, for display
	Roles       []string `json:"roles,omitempty"`       // users only: ids of every assigned role
	Environment string   `json:"environment,omitempty"` // API keys only: "live" or "test"
	Permissions []string `json:"permissions"`           // union of the permissions of all roles

	IssuedAt time.Time `json:"-"` // set from the verified token
}

// Can reports whether the principal holds a permission.
func (p Principal) Can(permission string) bool { return slices.Contains(p.Permissions, permission) }

// IsPlatform reports whether the principal is platform staff (not tied to a merchant).
func (p Principal) IsPlatform() bool { return p.Kind == KindUser && p.MerchantID == "" }

type ctxKey int

const (
	principalKey ctxKey = iota
	tokenKey
	correlationKey
)

func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey, p)
}

func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey).(Principal)
	return p, ok
}

// WithToken stores the signed access token the principal came from, so it can
// be forwarded on the bus and re-verified by the receiving service.
func WithToken(ctx context.Context, token string) context.Context {
	if token == "" {
		return ctx
	}
	return context.WithValue(ctx, tokenKey, token)
}

func TokenFrom(ctx context.Context) string {
	t, _ := ctx.Value(tokenKey).(string)
	return t
}

// WithoutIdentity drops any principal and token from ctx. Bus handlers start
// from it, so only an identity they verified themselves is ever trusted.
func WithoutIdentity(ctx context.Context) context.Context {
	return context.WithValue(context.WithValue(ctx, principalKey, nil), tokenKey, nil)
}

func WithCorrelationID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, correlationKey, id)
}

func CorrelationID(ctx context.Context) string {
	id, _ := ctx.Value(correlationKey).(string)
	return id
}
