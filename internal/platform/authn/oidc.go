package authn

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"paygate/internal/platform/apperror"
)

// OIDC verifies ID tokens from "Sign in with Google" and "Sign in with Apple".
//
// The app (web, Android, iOS) signs the user in with Google or Apple and sends
// us the ID token. We never see the user's Google/Apple password. We check:
//
//   - signature: RS256 with the provider's published keys (JWKS, cached,
//     refreshed when a new key id appears, i.e. key rotation)
//   - issuer: really Google / Apple
//   - audience: issued to OUR client id (a token for another app is refused)
//   - expiry (and issued-at not in the future)
//   - nonce, when the app sent one (stops a stolen token being replayed)
type OIDC struct {
	Name      string
	Issuers   []string
	JWKSURL   string
	ClientIDs []string // web client id, iOS bundle id, Android client id, Apple services id...

	hc      *http.Client
	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey
	fetched time.Time
	now     func() time.Time
}

const (
	GoogleJWKS = "https://www.googleapis.com/oauth2/v3/certs"
	AppleJWKS  = "https://appleid.apple.com/auth/keys"
)

func NewGoogle(clientIDs []string, jwksURL string) *OIDC {
	if jwksURL == "" {
		jwksURL = GoogleJWKS
	}
	return newOIDC("google", []string{"https://accounts.google.com", "accounts.google.com"}, jwksURL, clientIDs)
}

func NewApple(clientIDs []string, jwksURL string) *OIDC {
	if jwksURL == "" {
		jwksURL = AppleJWKS
	}
	return newOIDC("apple", []string{"https://appleid.apple.com"}, jwksURL, clientIDs)
}

func newOIDC(name string, issuers []string, jwks string, clientIDs []string) *OIDC {
	return &OIDC{Name: name, Issuers: issuers, JWKSURL: jwks, ClientIDs: clientIDs,
		hc: &http.Client{Timeout: 10 * time.Second}, keys: map[string]*rsa.PublicKey{}, now: time.Now}
}

// Identity is what a verified ID token tells us.
type Identity struct {
	Provider      string
	Subject       string // stable user id at the provider (never changes, unlike email)
	Email         string
	EmailVerified bool
	Name          string
}

var ErrInvalidIDToken = apperror.Unauthorized("invalid_id_token", "the sign-in token is invalid or expired; sign in again")

type idClaims struct {
	Iss           string          `json:"iss"`
	Aud           json.RawMessage `json:"aud"` // string or array
	Sub           string          `json:"sub"`
	Exp           int64           `json:"exp"`
	Iat           int64           `json:"iat"`
	Nonce         string          `json:"nonce"`
	Email         string          `json:"email"`
	EmailVerified any             `json:"email_verified"` // Google: bool, Apple: "true"
	Name          string          `json:"name"`
}

func (o *OIDC) Verify(ctx context.Context, token, nonce string) (Identity, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Identity{}, ErrInvalidIDToken
	}
	var h struct{ Alg, Kid string }
	if err := unb64JSON(parts[0], &h); err != nil || h.Alg != "RS256" || h.Kid == "" {
		return Identity{}, ErrInvalidIDToken
	}
	key, err := o.key(ctx, h.Kid)
	if err != nil {
		return Identity{}, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Identity{}, ErrInvalidIDToken
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig) != nil {
		return Identity{}, ErrInvalidIDToken
	}
	var c idClaims
	if err := unb64JSON(parts[1], &c); err != nil {
		return Identity{}, ErrInvalidIDToken
	}
	now := o.now()
	switch {
	case !slices.Contains(o.Issuers, c.Iss):
		return Identity{}, ErrInvalidIDToken.WithMessage("token was not issued by " + o.Name)
	case !o.audienceOK(c.Aud):
		return Identity{}, ErrInvalidIDToken.WithMessage("token was issued for another application")
	case c.Sub == "":
		return Identity{}, ErrInvalidIDToken
	case now.After(time.Unix(c.Exp, 0).Add(time.Minute)):
		return Identity{}, ErrInvalidIDToken.WithMessage("sign-in token expired; sign in again")
	case time.Unix(c.Iat, 0).After(now.Add(time.Minute)):
		return Identity{}, ErrInvalidIDToken
	}
	if nonce != "" { // apps may send the nonce itself or its SHA-256 (Apple's recommendation)
		hashed := sha256.Sum256([]byte(nonce))
		if c.Nonce != nonce && c.Nonce != hex.EncodeToString(hashed[:]) {
			return Identity{}, ErrInvalidIDToken.WithMessage("nonce mismatch")
		}
	}
	verified := false
	switch v := c.EmailVerified.(type) {
	case bool:
		verified = v
	case string:
		verified = v == "true"
	}
	return Identity{Provider: o.Name, Subject: c.Sub, Email: strings.ToLower(strings.TrimSpace(c.Email)),
		EmailVerified: verified, Name: c.Name}, nil
}

func (o *OIDC) audienceOK(raw json.RawMessage) bool {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return slices.Contains(o.ClientIDs, one)
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		for _, a := range many {
			if slices.Contains(o.ClientIDs, a) {
				return true
			}
		}
	}
	return false
}

// key returns the signing key, refreshing the cached JWKS when it is older
// than an hour or the key id is new (at most once a minute).
func (o *OIDC) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	k, ok := o.keys[kid]
	age := o.now().Sub(o.fetched)
	if ok && age < time.Hour {
		return k, nil
	}
	if !ok && age < time.Minute && !o.fetched.IsZero() {
		return nil, ErrInvalidIDToken // unknown key and we refreshed very recently
	}
	if err := o.fetch(ctx); err != nil {
		if ok {
			return k, nil // provider unreachable: keep using the cached key
		}
		return nil, apperror.Unavailable("identity_provider_unavailable", o.Name+" sign-in is unavailable: "+err.Error())
	}
	if k, ok = o.keys[kid]; !ok {
		return nil, ErrInvalidIDToken
	}
	return k, nil
}

func (o *OIDC) fetch(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.JWKSURL, nil)
	if err != nil {
		return err
	}
	resp, err := o.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("JWKS HTTP %d", resp.StatusCode)
	}
	var set struct {
		Keys []struct{ Kty, Kid, N, E string }
	}
	if err := json.NewDecoder(resp.Body).Decode(&set); err != nil {
		return err
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil || len(e) > 4 {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	if len(keys) == 0 {
		return errors.New("JWKS has no RSA keys")
	}
	o.keys, o.fetched = keys, o.now()
	return nil
}
