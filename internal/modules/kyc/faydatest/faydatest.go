// Package faydatest is a fake Fayda eSignet (OpenID Connect) server for
// tests. It is strict like the real one: client authentication with a signed
// JWT (private_key_jwt, RS256), PKCE S256, one-time codes, signed ID token and
// signed user info.
package faydatest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"time"
)

type Server struct {
	*httptest.Server
	ClientID     string
	ClientKey    *rsa.PrivateKey // the relying party's key (what Fayda issues to it)
	Person       map[string]any  // claims returned for the next sign-in
	Deny         bool            // the person refuses consent
	signer       *rsa.PrivateKey
	mu           sync.Mutex
	codes        map[string]session
	access       map[string]map[string]any
	TokenCalls   int
	LastAssertOK bool
}

type session struct {
	challenge, nonce, redirect string
	used                       bool
}

func New(clientID string) *Server {
	signer, _ := rsa.GenerateKey(rand.Reader, 2048)
	client, _ := rsa.GenerateKey(rand.Reader, 2048)
	s := &Server{ClientID: clientID, ClientKey: client, signer: signer,
		codes: map[string]session{}, access: map[string]map[string]any{},
		Person: map[string]any{"name": "Abebe Kebede Alemu", "birthdate": "1985/04/12", "gender": "Male",
			"phone_number": "+251911234567", "email": "abebe@example.com", "nationality": "ETH",
			"individual_id": "1234 5678 9012 3456",
			"address":       map[string]any{"locality": "Bole", "region": "Addis Ababa", "country": "Ethiopia"},
			"picture":       "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString([]byte("\xff\xd8\xff\xe0 fake jpeg"))}}
	mux := http.NewServeMux()
	s.Server = httptest.NewServer(mux)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": s.URL, "authorization_endpoint": s.URL + "/authorize",
			"token_endpoint": s.URL + "/v1/esignet/oauth/v2/token", "userinfo_endpoint": s.URL + "/v1/esignet/oidc/userinfo",
			"jwks_uri": s.URL + "/.well-known/jwks.json", "token_endpoint_auth_methods_supported": []string{"private_key_jwt"},
		})
	})
	mux.HandleFunc("/.well-known/jwks.json", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kty": "RSA", "kid": "fayda-1",
			"n": b64(signer.N.Bytes()), "e": b64(big.NewInt(int64(signer.E)).Bytes())}}})
	})
	mux.HandleFunc("/authorize", s.authorize)
	mux.HandleFunc("/v1/esignet/oauth/v2/token", s.token)
	mux.HandleFunc("/v1/esignet/oidc/userinfo", s.userinfo)
	return s
}

// PrivateKeyJWK returns the client key as Fayda delivers it: a base64 JWK.
func (s *Server) PrivateKeyJWK() string {
	k := s.ClientKey
	j, _ := json.Marshal(map[string]string{"kty": "RSA", "kid": "client-1", "n": b64(k.N.Bytes()),
		"e": b64(big.NewInt(int64(k.E)).Bytes()), "d": b64(k.D.Bytes()), "p": b64(k.Primes[0].Bytes()),
		"q": b64(k.Primes[1].Bytes()), "dp": b64(k.Precomputed.Dp.Bytes()), "dq": b64(k.Precomputed.Dq.Bytes()),
		"qi": b64(k.Precomputed.Qinv.Bytes())})
	return base64.StdEncoding.EncodeToString(j)
}

// authorize simulates the person signing in and consenting: it redirects to
// the client's callback with a one-time code (or an error when Deny is set).
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("client_id") != s.ClientID || q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" ||
		q.Get("code_challenge") == "" || q.Get("nonce") == "" || !strings.Contains(q.Get("scope"), "openid") {
		http.Error(w, "invalid_request", http.StatusBadRequest)
		return
	}
	back, _ := url.Parse(q.Get("redirect_uri"))
	v := url.Values{"state": {q.Get("state")}}
	if s.Deny {
		v.Set("error", "access_denied")
		v.Set("error_description", "consent rejected")
	} else {
		code := random()
		s.mu.Lock()
		s.codes[code] = session{challenge: q.Get("code_challenge"), nonce: q.Get("nonce"), redirect: q.Get("redirect_uri")}
		s.mu.Unlock()
		v.Set("code", code)
	}
	back.RawQuery = v.Encode()
	http.Redirect(w, r, back.String(), http.StatusFound)
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.TokenCalls++
	_ = r.ParseForm()
	f := r.PostForm
	fail := func(msg string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_request", "error_description": msg})
	}
	if f.Get("grant_type") != "authorization_code" || f.Get("client_id") != s.ClientID ||
		f.Get("client_assertion_type") != "urn:ietf:params:oauth:client-assertion-type:jwt-bearer" {
		fail("bad grant or client")
		return
	}
	if err := s.checkAssertion(f.Get("client_assertion")); err != nil {
		fail("client assertion: " + err.Error())
		return
	}
	s.LastAssertOK = true
	sess, ok := s.codes[f.Get("code")]
	if !ok || sess.used || sess.redirect != f.Get("redirect_uri") {
		fail("invalid or used code")
		return
	}
	sum := sha256.Sum256([]byte(f.Get("code_verifier")))
	if b64(sum[:]) != sess.challenge {
		fail("PKCE verification failed")
		return
	}
	sess.used = true
	s.codes[f.Get("code")] = sess
	sub := "fayda-psut-" + fmt.Sprint(len(s.access)+1)
	now := time.Now().Unix()
	id := s.sign(map[string]any{"iss": s.URL, "aud": s.ClientID, "sub": sub, "nonce": sess.nonce, "iat": now, "exp": now + 300})
	at := random()
	claims := map[string]any{"sub": sub}
	for k, v := range s.Person {
		claims[k] = v
	}
	s.access[at] = claims
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"access_token": at, "id_token": id, "token_type": "Bearer", "expires_in": 300})
}

func (s *Server) checkAssertion(jwt string) error {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return fmt.Errorf("not a JWT")
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(&s.ClientKey.PublicKey, crypto.SHA256, sum[:], sig) != nil {
		return fmt.Errorf("signature")
	}
	cb, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var c map[string]any
	_ = json.Unmarshal(cb, &c)
	exp, _ := c["exp"].(float64)
	if c["iss"] != s.ClientID || c["sub"] != s.ClientID || c["aud"] != s.URL+"/v1/esignet/oauth/v2/token" ||
		time.Now().After(time.Unix(int64(exp), 0)) || c["jti"] == nil {
		return fmt.Errorf("claims %v", c)
	}
	return nil
}

func (s *Server) userinfo(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	claims, ok := s.access[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
	s.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/jwt")
	_, _ = w.Write([]byte(s.sign(claims)))
}

func (s *Server) sign(claims map[string]any) string {
	h, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "fayda-1", "typ": "JWT"})
	c, _ := json.Marshal(claims)
	input := b64(h) + "." + b64(c)
	sum := sha256.Sum256([]byte(input))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, s.signer, crypto.SHA256, sum[:])
	return input + "." + b64(sig)
}

func random() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return b64(b)
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
