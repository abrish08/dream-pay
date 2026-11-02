// Package fayda verifies a person with Ethiopia's national digital ID (Fayda)
// through its eSignet OpenID Connect provider.
//
// Flow (authorization code + PKCE, client authenticated with private_key_jwt):
//
//  1. AuthorizeURL: the merchant's owner is sent to Fayda, authenticates (OTP
//     to the phone registered with Fayda, wallet or biometrics) and consents
//     to share their details.
//  2. Fayda redirects back with a one-time code; Exchange swaps it for tokens,
//     proving our identity with a JWT signed by our private key (no client
//     secret exists).
//  3. The ID token and the user info (a signed JWT) are verified with Fayda's
//     published keys before anything is trusted.
//
// Endpoints are discovered from <issuer>/.well-known/openid-configuration
// (production issuer: https://esignet.ida.fayda.et).
package fayda

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

type Config struct {
	Issuer      string // e.g. https://esignet.ida.fayda.et
	ClientID    string
	RedirectURI string // our callback, registered with Fayda
	PrivateKey  *rsa.PrivateKey
	KeyID       string // kid of our key, if Fayda registered one
	ACR         string // authentication methods, e.g. "mosip:idp:acr:generated-code"
	HTTP        *http.Client
}

type Client struct {
	cfg  Config
	now  func() time.Time
	mu   sync.Mutex
	meta *metadata
	keys map[string]*rsa.PublicKey
	jwks time.Time
}

type metadata struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
}

func New(cfg Config) (*Client, error) {
	if cfg.Issuer == "" || cfg.ClientID == "" || cfg.RedirectURI == "" || cfg.PrivateKey == nil {
		return nil, errors.New("fayda: issuer, client id, redirect URI and private key are required")
	}
	if cfg.ACR == "" {
		cfg.ACR = "mosip:idp:acr:generated-code"
	}
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: 15 * time.Second}
	}
	cfg.Issuer = strings.TrimRight(cfg.Issuer, "/")
	return &Client{cfg: cfg, now: time.Now, keys: map[string]*rsa.PublicKey{}}, nil
}

// Identity: what Fayda confirmed about the person.
type Identity struct {
	Subject      string
	IndividualID string
	Name         string
	BirthDate    string
	Gender       string
	Phone        string
	Email        string
	Nationality  string
	Address      string
	Picture      []byte // decoded photo, if shared
}

// ---- PKCE / state ----

// NewPKCE returns a code verifier and its S256 challenge.
func NewPKCE() (verifier, challenge string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

func RandomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// claimsRequest asks for the details KYC needs.
var claimsRequest = `{"userinfo":{"name":{"essential":true},"birthdate":{"essential":true},"gender":{"essential":false},` +
	`"phone_number":{"essential":false},"email":{"essential":false},"address":{"essential":false},` +
	`"nationality":{"essential":false},"picture":{"essential":false},"individual_id":{"essential":false}},"id_token":{}}`

// AuthorizeURL builds the link that sends the person to Fayda.
func (c *Client) AuthorizeURL(ctx context.Context, state, nonce, challenge string) (string, error) {
	m, err := c.metadata(ctx)
	if err != nil {
		return "", err
	}
	q := url.Values{
		"response_type": {"code"}, "client_id": {c.cfg.ClientID}, "redirect_uri": {c.cfg.RedirectURI},
		"scope": {"openid profile email phone"}, "acr_values": {c.cfg.ACR}, "claims": {claimsRequest},
		"state": {state}, "nonce": {nonce}, "code_challenge": {challenge}, "code_challenge_method": {"S256"},
		"ui_locales": {"en"}, "display": {"page"}, "prompt": {"login"},
	}
	return m.AuthorizationEndpoint + "?" + q.Encode(), nil
}

// Verify completes the flow: exchanges the code and returns the verified identity.
func (c *Client) Verify(ctx context.Context, code, verifier, nonce string) (Identity, error) {
	m, err := c.metadata(ctx)
	if err != nil {
		return Identity{}, err
	}
	assertion, err := c.clientAssertion(m.TokenEndpoint)
	if err != nil {
		return Identity{}, err
	}
	form := url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {c.cfg.RedirectURI},
		"client_id": {c.cfg.ClientID}, "code_verifier": {verifier},
		"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
		"client_assertion":      {assertion},
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, m.TokenEndpoint, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var tok struct {
		AccessToken string `json:"access_token"`
		IDToken     string `json:"id_token"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if err := c.do(req, &tok); err != nil {
		return Identity{}, fmt.Errorf("fayda token: %w", err)
	}
	if tok.AccessToken == "" || tok.IDToken == "" {
		return Identity{}, fmt.Errorf("fayda token: %s %s", tok.Error, tok.Description)
	}
	var idc map[string]any
	if err := c.verifyJWT(ctx, tok.IDToken, &idc); err != nil {
		return Identity{}, fmt.Errorf("fayda id_token: %w", err)
	}
	if err := c.checkStandard(idc, nonce); err != nil {
		return Identity{}, fmt.Errorf("fayda id_token: %w", err)
	}

	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, m.UserinfoEndpoint, nil)
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	resp, err := c.cfg.HTTP.Do(req)
	if err != nil {
		return Identity{}, fmt.Errorf("fayda userinfo: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return Identity{}, fmt.Errorf("fayda userinfo: HTTP %d", resp.StatusCode)
	}
	var ui map[string]any
	raw := strings.TrimSpace(string(body))
	if strings.Count(raw, ".") == 4 {
		return Identity{}, errors.New("fayda userinfo is encrypted (JWE): register the client without userinfo encryption")
	}
	if err := c.verifyJWT(ctx, raw, &ui); err != nil {
		return Identity{}, fmt.Errorf("fayda userinfo: %w", err)
	}
	if ui["sub"] != idc["sub"] {
		return Identity{}, errors.New("fayda userinfo belongs to another subject")
	}
	return identityFrom(ui), nil
}

func (c *Client) checkStandard(cl map[string]any, nonce string) error {
	if cl["iss"] != c.cfg.Issuer {
		return errors.New("wrong issuer")
	}
	if !audienceIncludes(cl["aud"], c.cfg.ClientID) {
		return errors.New("issued for another client")
	}
	if exp, _ := cl["exp"].(float64); c.now().After(time.Unix(int64(exp), 0).Add(time.Minute)) {
		return errors.New("expired")
	}
	if cl["nonce"] != nonce {
		return errors.New("nonce mismatch (replay?)")
	}
	if s, _ := cl["sub"].(string); s == "" {
		return errors.New("no subject")
	}
	return nil
}

func audienceIncludes(aud any, id string) bool {
	switch a := aud.(type) {
	case string:
		return a == id
	case []any:
		for _, x := range a {
			if x == id {
				return true
			}
		}
	}
	return false
}

func identityFrom(ui map[string]any) Identity {
	str := func(k string) string {
		switch v := ui[k].(type) {
		case string:
			return strings.TrimSpace(v)
		case map[string]any: // localized value, e.g. {"en": "...", "am": "..."}
			if en, ok := v["en"].(string); ok {
				return en
			}
			for _, x := range v {
				if s, ok := x.(string); ok {
					return s
				}
			}
		}
		return ""
	}
	id := Identity{Subject: str("sub"), IndividualID: str("individual_id"), Name: str("name"),
		BirthDate: normalizeDate(str("birthdate")), Gender: str("gender"), Phone: str("phone_number"),
		Email: str("email"), Nationality: str("nationality")}
	switch a := ui["address"].(type) {
	case string:
		id.Address = a
	case map[string]any:
		var parts []string
		for _, k := range []string{"formatted", "street_address", "locality", "region", "country"} {
			if s, ok := a[k].(string); ok && s != "" {
				parts = append(parts, s)
				if k == "formatted" {
					break
				}
			}
		}
		id.Address = strings.Join(parts, ", ")
	}
	if pic := str("picture"); pic != "" {
		if _, b64, ok := strings.Cut(pic, ";base64,"); ok {
			pic = b64
		}
		if b, err := base64.StdEncoding.DecodeString(pic); err == nil {
			id.Picture = b
		}
	}
	return id
}

// normalizeDate accepts YYYY-MM-DD or YYYY/MM/DD.
func normalizeDate(s string) string { return strings.ReplaceAll(s, "/", "-") }

// ---- client assertion (private_key_jwt, RS256) ----

func (c *Client) clientAssertion(audience string) (string, error) {
	now := c.now()
	h := map[string]string{"alg": "RS256", "typ": "JWT"}
	if c.cfg.KeyID != "" {
		h["kid"] = c.cfg.KeyID
	}
	cl := map[string]any{"iss": c.cfg.ClientID, "sub": c.cfg.ClientID, "aud": audience, "jti": uuid.NewString(),
		"iat": now.Unix(), "exp": now.Add(2 * time.Minute).Unix()}
	hb, _ := json.Marshal(h)
	cb, _ := json.Marshal(cl)
	input := b64(hb) + "." + b64(cb)
	sum := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, c.cfg.PrivateKey, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return input + "." + b64(sig), nil
}

// ---- verifying Fayda's JWTs ----

func (c *Client) verifyJWT(ctx context.Context, token string, out *map[string]any) error {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return errors.New("not a JWT")
	}
	var h struct{ Alg, Kid string }
	hb, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || json.Unmarshal(hb, &h) != nil || h.Alg != "RS256" {
		return errors.New("unexpected JWT algorithm")
	}
	key, err := c.key(ctx, h.Kid)
	if err != nil {
		return err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], sig) != nil {
		return errors.New("bad signature")
	}
	cb, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return err
	}
	return json.Unmarshal(cb, out)
}

func (c *Client) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	c.mu.Lock()
	k, ok := c.keys[kid]
	fresh := c.now().Sub(c.jwks) < time.Hour
	c.mu.Unlock()
	if ok && fresh {
		return k, nil
	}
	m, err := c.metadata(ctx)
	if err != nil {
		return nil, err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, m.JWKSURI, nil)
	var set struct {
		Keys []struct{ Kty, Kid, N, E string }
	}
	if err := c.do(req, &set); err != nil {
		return nil, fmt.Errorf("fayda keys: %w", err)
	}
	keys := map[string]*rsa.PublicKey{}
	for _, jk := range set.Keys {
		if jk.Kty != "RSA" {
			continue
		}
		n, e1 := base64.RawURLEncoding.DecodeString(jk.N)
		e, e2 := base64.RawURLEncoding.DecodeString(jk.E)
		if e1 == nil && e2 == nil {
			keys[jk.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
		}
	}
	c.mu.Lock()
	c.keys, c.jwks = keys, c.now()
	c.mu.Unlock()
	if k, ok := keys[kid]; ok {
		return k, nil
	}
	if kid == "" && len(keys) == 1 {
		for _, k := range keys {
			return k, nil
		}
	}
	return nil, errors.New("unknown signing key")
}

func (c *Client) metadata(ctx context.Context) (*metadata, error) {
	c.mu.Lock()
	m := c.meta
	c.mu.Unlock()
	if m != nil {
		return m, nil
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.Issuer+"/.well-known/openid-configuration", nil)
	var got metadata
	if err := c.do(req, &got); err != nil {
		return nil, fmt.Errorf("fayda discovery: %w", err)
	}
	if got.Issuer != c.cfg.Issuer || got.AuthorizationEndpoint == "" || got.TokenEndpoint == "" ||
		got.UserinfoEndpoint == "" || got.JWKSURI == "" {
		return nil, errors.New("fayda discovery: incomplete or wrong-issuer metadata")
	}
	c.mu.Lock()
	c.meta = &got
	c.mu.Unlock()
	return &got, nil
}

func (c *Client) do(req *http.Request, out any) error {
	resp, err := c.cfg.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.Unmarshal(body, out)
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// ParsePrivateKey accepts what Fayda issues (a base64-encoded RSA private JWK)
// or a PEM key (PKCS#1 / PKCS#8). Returns the key and its kid, if any.
func ParsePrivateKey(s string) (*rsa.PrivateKey, string, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "-----BEGIN") {
		blk, _ := pem.Decode([]byte(s))
		if blk == nil {
			return nil, "", errors.New("invalid PEM key")
		}
		if k, err := x509.ParsePKCS1PrivateKey(blk.Bytes); err == nil {
			return k, "", nil
		}
		k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
		if err != nil {
			return nil, "", err
		}
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, "", errors.New("the key is not RSA")
		}
		return rk, "", nil
	}
	raw := []byte(s)
	if !strings.HasPrefix(s, "{") {
		dec, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			if dec, err = base64.RawURLEncoding.DecodeString(s); err != nil {
				return nil, "", errors.New("FAYDA_PRIVATE_KEY must be a base64 JWK, a JWK or a PEM key")
			}
		}
		raw = dec
	}
	var j struct{ Kty, Kid, N, E, D, P, Q, Dp, Dq, Qi string }
	if err := json.Unmarshal(raw, &j); err != nil || j.Kty != "RSA" || j.D == "" {
		return nil, "", errors.New("FAYDA_PRIVATE_KEY is not an RSA private JWK")
	}
	num := func(v string) *big.Int {
		b, _ := base64.RawURLEncoding.DecodeString(v)
		return new(big.Int).SetBytes(b)
	}
	k := &rsa.PrivateKey{PublicKey: rsa.PublicKey{N: num(j.N), E: int(num(j.E).Int64())}, D: num(j.D),
		Primes: []*big.Int{num(j.P), num(j.Q)}}
	if err := k.Validate(); err != nil {
		return nil, "", fmt.Errorf("FAYDA_PRIVATE_KEY: %w", err)
	}
	k.Precompute()
	return k, j.Kid, nil
}
