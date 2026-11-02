package fayda

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"paygate/internal/modules/kyc/faydatest"
)

func TestFaydaFlow(t *testing.T) {
	idp := faydatest.New("paygate-client")
	defer idp.Close()
	key, kid, err := ParsePrivateKey(idp.PrivateKeyJWK())
	if err != nil || kid != "client-1" {
		t.Fatalf("parse key: %v %q", err, kid)
	}
	c, err := New(Config{Issuer: idp.URL, ClientID: "paygate-client", RedirectURI: "https://gw.example/kyc/fayda/callback",
		PrivateKey: key, KeyID: kid})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	verifier, challenge, _ := NewPKCE()
	state, _ := RandomToken()
	nonce, _ := RandomToken()
	link, err := c.AuthorizeURL(ctx, state, nonce, challenge)
	if err != nil || !strings.HasPrefix(link, idp.URL+"/authorize?") || !strings.Contains(link, "code_challenge_method=S256") {
		t.Fatalf("authorize url: %s %v", link, err)
	}
	// The person signs in at Fayda and is redirected back with a code.
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noFollow.Get(link)
	if err != nil || resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize: %v %v", err, resp)
	}
	back, _ := url.Parse(resp.Header.Get("Location"))
	if back.Query().Get("state") != state {
		t.Fatal("state not returned")
	}
	code := back.Query().Get("code")

	if _, err := c.Verify(ctx, code, "wrong-verifier", nonce); err == nil {
		t.Fatal("PKCE mismatch accepted")
	}
	// A fresh code (the failed attempt above spent nothing: PKCE was checked first).
	id, err := c.Verify(ctx, code, verifier, nonce)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if id.Name != "Abebe Kebede Alemu" || id.BirthDate != "1985-04-12" || id.IndividualID == "" ||
		!strings.Contains(id.Address, "Bole") || len(id.Picture) == 0 || !idp.LastAssertOK {
		t.Fatalf("identity: %+v", id)
	}
	if _, err := c.Verify(ctx, code, verifier, nonce); err == nil {
		t.Fatal("a code must work only once")
	}
}

func TestNonceAndKeys(t *testing.T) {
	idp := faydatest.New("paygate-client")
	defer idp.Close()
	key, _, _ := ParsePrivateKey(idp.PrivateKeyJWK())
	c, _ := New(Config{Issuer: idp.URL, ClientID: "paygate-client", RedirectURI: "https://gw.example/cb", PrivateKey: key})
	ctx := context.Background()
	verifier, challenge, _ := NewPKCE()
	link, _ := c.AuthorizeURL(ctx, "st", "real-nonce", challenge)
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, _ := noFollow.Get(link)
	back, _ := url.Parse(resp.Header.Get("Location"))
	if _, err := c.Verify(ctx, back.Query().Get("code"), verifier, "other-nonce"); err == nil || !strings.Contains(err.Error(), "nonce") {
		t.Fatalf("nonce mismatch accepted: %v", err)
	}
	if _, _, err := ParsePrivateKey("not a key"); err == nil {
		t.Fatal("garbage key accepted")
	}
}
