package authn_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"paygate/internal/platform/authn"
	"paygate/internal/platform/authn/oidctest"
)

func TestOIDC(t *testing.T) {
	idp := oidctest.New()
	defer idp.Close()
	google := authn.NewGoogle([]string{"web-client.apps.googleusercontent.com"}, idp.JWKSURL())
	apple := authn.NewApple([]string{"com.example.app"}, idp.JWKSURL())
	ctx := context.Background()
	now := time.Now().Unix()
	claims := func(over map[string]any) map[string]any {
		c := map[string]any{"iss": "https://accounts.google.com", "aud": "web-client.apps.googleusercontent.com",
			"sub": "1234567890", "email": "Abebe@Gmail.com", "email_verified": true, "name": "Abebe",
			"iat": now, "exp": now + 600, "nonce": "n-1"}
		for k, v := range over {
			c[k] = v
		}
		return c
	}

	id, err := google.Verify(ctx, idp.Token(claims(nil)), "n-1")
	if err != nil || id.Subject != "1234567890" || id.Email != "abebe@gmail.com" || !id.EmailVerified || id.Provider != "google" {
		t.Fatalf("valid token: %+v %v", id, err)
	}
	// Apple: string "true" for email_verified, aud array, hashed nonce.
	appleTok := idp.Token(claims(map[string]any{"iss": "https://appleid.apple.com", "aud": []string{"com.example.app"},
		"email_verified": "true", "nonce": "8c6976e5b5410415bde908bd4dee15dfb167a9c873fc4bb8a81f6f2ab448a918"}))
	if id, err := apple.Verify(ctx, appleTok, "admin"); err != nil || !id.EmailVerified {
		t.Fatalf("apple token: %+v %v", id, err)
	}

	tampered := idp.Token(claims(nil))
	tampered = tampered[:len(tampered)-4] + "AAAA"
	for name, tc := range map[string]struct{ tok, nonce string }{
		"other app":             {idp.Token(claims(map[string]any{"aud": "someone-else"})), ""},
		"other issuer":          {idp.Token(claims(map[string]any{"iss": "https://evil.example"})), ""},
		"expired":               {idp.Token(claims(map[string]any{"exp": now - 3600})), ""},
		"bad signature":         {tampered, ""},
		"wrong nonce":           {idp.Token(claims(nil)), "n-2"},
		"no subject":            {idp.Token(claims(map[string]any{"sub": ""})), ""},
		"garbage":               {"a.b.c", ""},
		"google token at apple": {idp.Token(claims(nil)), ""},
	} {
		v := google
		if strings.HasPrefix(name, "google token at apple") {
			v = apple
		}
		if _, err := v.Verify(ctx, tc.tok, tc.nonce); !errors.Is(err, authn.ErrInvalidIDToken) {
			t.Errorf("%s: want invalid_id_token, got %v", name, err)
		}
	}
}
