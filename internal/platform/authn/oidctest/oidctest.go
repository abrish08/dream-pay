// Package oidctest is a fake Google / Apple identity provider for tests: it
// serves a JWKS and signs ID tokens with its own RSA key.
package oidctest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
)

type Provider struct {
	Server *httptest.Server
	key    *rsa.PrivateKey
	kid    string
}

func New() *Provider {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	p := &Provider{key: key, kid: "test-key-1"}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": p.kid, "alg": "RS256", "use": "sig",
			"n": b64(key.N.Bytes()), "e": b64(big.NewInt(int64(key.E)).Bytes()),
		}}})
	}))
	return p
}

func (p *Provider) JWKSURL() string { return p.Server.URL }
func (p *Provider) Close()          { p.Server.Close() }

// Token signs claims (iss, aud, sub, email, exp, ...) as an RS256 ID token.
func (p *Provider) Token(claims map[string]any) string {
	h, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": p.kid, "typ": "JWT"})
	c, _ := json.Marshal(claims)
	input := b64(h) + "." + b64(c)
	sum := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, p.key, crypto.SHA256, sum[:])
	if err != nil {
		panic(err)
	}
	return input + "." + b64(sig)
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
