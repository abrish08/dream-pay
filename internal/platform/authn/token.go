// Package authn issues and verifies access tokens.
//
// Access tokens are JWTs signed with Ed25519 (alg "EdDSA"). Only the auth
// service holds the private key and issues tokens. Every other service holds
// only the public key(s) and verifies tokens locally: no network call, and a
// service that is compromised still cannot mint tokens.
//
// Several public keys can be configured at once so the signing key can be
// rotated without downtime: add the new public key everywhere, switch the
// auth service to the new private key, remove the old public key after the
// longest token lifetime has passed.
package authn

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"paygate/internal/platform/apperror"
	"paygate/internal/platform/reqctx"
)

const (
	issuer   = "paygate-auth"
	audience = "paygate"
	leeway   = 30 * time.Second // tolerated clock skew between services
)

var (
	ErrInvalidToken = apperror.Unauthorized("invalid_token", "access token is invalid")
	ErrExpiredToken = apperror.Unauthorized("token_expired", "access token has expired")
	// ErrStaleToken: the user's roles or permissions changed after the token
	// was issued. The client should call /auth/refresh to get a fresh one.
	ErrStaleToken = apperror.Unauthorized("token_stale", "your access changed; refresh your token")
)

type header struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
	Kid string `json:"kid"`
}

type claims struct {
	reqctx.Principal
	Iss string `json:"iss"`
	Aud string `json:"aud"`
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
	// IatMs: issue time in milliseconds, so a token issued just before an
	// access change can be told apart from one issued just after.
	IatMs int64 `json:"iat_ms,omitempty"`
}

// KeyID is a short, stable identifier of a public key (used as JWT "kid").
func KeyID(pub ed25519.PublicKey) string {
	h := sha256.Sum256(pub)
	return hex.EncodeToString(h[:8])
}

// ---- Signer (auth service only) ----

type Signer struct {
	priv ed25519.PrivateKey
	kid  string
	now  func() time.Time
}

// NewSigner takes the 32-byte Ed25519 seed (from a secret manager).
func NewSigner(seed []byte) (*Signer, error) {
	if len(seed) != ed25519.SeedSize {
		return nil, errors.New("signing key must be a 32-byte Ed25519 seed")
	}
	priv := ed25519.NewKeyFromSeed(seed)
	return &Signer{priv: priv, kid: KeyID(priv.Public().(ed25519.PublicKey)), now: time.Now}, nil
}

func (s *Signer) PublicKey() ed25519.PublicKey { return s.priv.Public().(ed25519.PublicKey) }

// Issue returns a signed token for p, valid for ttl.
func (s *Signer) Issue(p reqctx.Principal, ttl time.Duration) (string, time.Time, error) {
	now := s.now().UTC()
	exp := now.Add(ttl)
	h, err := json.Marshal(header{Alg: "EdDSA", Typ: "JWT", Kid: s.kid})
	if err != nil {
		return "", time.Time{}, err
	}
	c, err := json.Marshal(claims{Principal: p, Iss: issuer, Aud: audience, Iat: now.Unix(), Exp: exp.Unix(),
		IatMs: now.UnixMilli()})
	if err != nil {
		return "", time.Time{}, err
	}
	signingInput := b64(h) + "." + b64(c)
	sig := ed25519.Sign(s.priv, []byte(signingInput))
	return signingInput + "." + b64(sig), exp, nil
}

// ---- Verifier (every service) ----

type Verifier struct {
	keys map[string]ed25519.PublicKey // kid -> key
	now  func() time.Time

	// Revocations: user and role ids whose tokens issued before a time are
	// rejected. Filled from auth.access_changed events in every process.
	mu         sync.RWMutex
	notBefore  map[string]time.Time // "u:<id>" or "r:<id>"
	lastPruned time.Time
}

// revocationTTL: an entry older than the longest token lifetime protects
// nothing (every token issued before it has expired), so it is dropped.
const revocationTTL = 2 * time.Hour

func NewVerifier(pubs ...ed25519.PublicKey) (*Verifier, error) {
	if len(pubs) == 0 {
		return nil, errors.New("at least one public key is required")
	}
	v := &Verifier{keys: map[string]ed25519.PublicKey{}, now: time.Now, notBefore: map[string]time.Time{}}
	for _, p := range pubs {
		if len(p) != ed25519.PublicKeySize {
			return nil, errors.New("public key must be 32 bytes")
		}
		v.keys[KeyID(p)] = p
	}
	return v, nil
}

// Verify checks signature, algorithm, issuer, audience and expiry, and
// returns the principal carried by the token.
func (v *Verifier) Verify(token string) (reqctx.Principal, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return reqctx.Principal{}, ErrInvalidToken
	}
	var h header
	if err := unb64JSON(parts[0], &h); err != nil || h.Alg != "EdDSA" { // never accept "none" or other algs
		return reqctx.Principal{}, ErrInvalidToken
	}
	key, ok := v.keys[h.Kid]
	if !ok {
		return reqctx.Principal{}, ErrInvalidToken
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !ed25519.Verify(key, []byte(parts[0]+"."+parts[1]), sig) {
		return reqctx.Principal{}, ErrInvalidToken
	}
	var c claims
	if err := unb64JSON(parts[1], &c); err != nil || c.Iss != issuer || c.Aud != audience {
		return reqctx.Principal{}, ErrInvalidToken
	}
	now := v.now()
	if now.After(time.Unix(c.Exp, 0).Add(leeway)) {
		return reqctx.Principal{}, ErrExpiredToken
	}
	if time.Unix(c.Iat, 0).After(now.Add(leeway)) {
		return reqctx.Principal{}, ErrInvalidToken
	}
	if c.Kind == "" || c.Subject == "" {
		return reqctx.Principal{}, ErrInvalidToken
	}
	p := c.Principal
	p.IssuedAt = time.Unix(c.Iat, 0)
	if c.IatMs > 0 {
		p.IssuedAt = time.UnixMilli(c.IatMs)
	}
	if p.Kind == reqctx.KindUser && v.stale(p) {
		return reqctx.Principal{}, ErrStaleToken
	}
	return p, nil
}

// Revoke rejects tokens of these users, and of anyone holding these roles,
// that were issued before notBefore.
func (v *Verifier) Revoke(userIDs, roleIDs []string, notBefore time.Time) {
	v.mu.Lock()
	defer v.mu.Unlock()
	set := func(k string) {
		if notBefore.After(v.notBefore[k]) {
			v.notBefore[k] = notBefore
		}
	}
	for _, id := range userIDs {
		set("u:" + id)
	}
	for _, id := range roleIDs {
		set("r:" + id)
	}
	if now := v.now(); now.Sub(v.lastPruned) > time.Minute {
		for k, t := range v.notBefore {
			if now.Sub(t) > revocationTTL {
				delete(v.notBefore, k)
			}
		}
		v.lastPruned = now
	}
}

func (v *Verifier) stale(p reqctx.Principal) bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if len(v.notBefore) == 0 {
		return false
	}
	if t, ok := v.notBefore["u:"+p.Subject]; ok && p.IssuedAt.Before(t) {
		return true
	}
	return slices.ContainsFunc(p.Roles, func(id string) bool {
		t, ok := v.notBefore["r:"+id]
		return ok && p.IssuedAt.Before(t)
	})
}

// JWKS returns the verifier's keys as a JSON Web Key Set (RFC 8037, OKP/Ed25519).
func (v *Verifier) JWKS() map[string]any {
	keys := make([]map[string]string, 0, len(v.keys))
	for kid, k := range v.keys {
		keys = append(keys, map[string]string{"kty": "OKP", "crv": "Ed25519", "alg": "EdDSA", "use": "sig", "kid": kid, "x": b64(k)})
	}
	return map[string]any{"keys": keys}
}

// ParsePublicKeys parses a comma-separated list of base64 (std or url) 32-byte keys.
func ParsePublicKeys(list string) ([]ed25519.PublicKey, error) {
	var out []ed25519.PublicKey
	for _, s := range strings.Split(list, ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		b, err := decodeB64(s)
		if err != nil || len(b) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("invalid Ed25519 public key %q", s)
		}
		out = append(out, ed25519.PublicKey(b))
	}
	return out, nil
}

// ParseSeed parses a base64 (std or url) 32-byte Ed25519 seed.
func ParseSeed(s string) ([]byte, error) {
	b, err := decodeB64(strings.TrimSpace(s))
	if err != nil || len(b) != ed25519.SeedSize {
		return nil, errors.New("must be a base64-encoded 32-byte Ed25519 seed")
	}
	return b, nil
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func unb64JSON(s string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func decodeB64(s string) ([]byte, error) {
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, errors.New("invalid base64")
}
