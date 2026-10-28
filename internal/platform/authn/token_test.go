package authn

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"paygate/internal/platform/reqctx"
)

func pair(t *testing.T, seedByte byte) (*Signer, *Verifier) {
	t.Helper()
	s, err := NewSigner(bytes.Repeat([]byte{seedByte}, 32))
	if err != nil {
		t.Fatal(err)
	}
	v, err := NewVerifier(s.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	return s, v
}

var principal = reqctx.Principal{Kind: reqctx.KindUser, Subject: "u1", MerchantID: "m1", Role: "owner",
	Permissions: []string{"payments:read"}}

func TestIssueAndVerify(t *testing.T) {
	s, v := pair(t, 1)
	tok, _, err := s.Issue(principal, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	got, err := v.Verify(tok)
	if err != nil {
		t.Fatal(err)
	}
	if got.Subject != "u1" || got.MerchantID != "m1" || !got.Can("payments:read") || got.Can("payments:write") {
		t.Fatalf("unexpected principal %+v", got)
	}
}

func TestRejects(t *testing.T) {
	s, v := pair(t, 1)
	other, _ := pair(t, 2)
	tok, _, _ := s.Issue(principal, time.Minute)
	forged, _, _ := other.Issue(principal, time.Minute)

	// Tamper with the claims: grant a permission without re-signing.
	parts := strings.Split(tok, ".")
	evil := strings.Replace(decode(parts[1]), `"payments:read"`, `"payments:read","payments:write"`, 1)
	tampered := parts[0] + "." + base64.RawURLEncoding.EncodeToString([]byte(evil)) + "." + parts[2]

	none := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`)) + "." + parts[1] + "."

	s.now = func() time.Time { return time.Now().Add(-time.Hour) }
	expired, _, _ := s.Issue(principal, time.Minute)

	for name, tc := range map[string]struct {
		tok  string
		want error
	}{
		"garbage":   {"abc", ErrInvalidToken},
		"other key": {forged, ErrInvalidToken},
		"tampered":  {tampered, ErrInvalidToken},
		"alg none":  {none, ErrInvalidToken},
		"expired":   {expired, ErrExpiredToken},
		"empty":     {"", ErrInvalidToken},
		"two parts": {parts[0] + "." + parts[1], ErrInvalidToken},
	} {
		if _, err := v.Verify(tc.tok); !errors.Is(err, tc.want) {
			t.Errorf("%s: want %v, got %v", name, tc.want, err)
		}
	}
}

func TestKeyRotation(t *testing.T) {
	oldS, _ := pair(t, 1)
	newS, _ := pair(t, 2)
	v, _ := NewVerifier(oldS.PublicKey(), newS.PublicKey())
	for _, s := range []*Signer{oldS, newS} {
		tok, _, _ := s.Issue(principal, time.Minute)
		if _, err := v.Verify(tok); err != nil {
			t.Fatal(err)
		}
	}
}

func TestParseKeys(t *testing.T) {
	s, _ := pair(t, 3)
	enc := base64.StdEncoding.EncodeToString(s.PublicKey())
	keys, err := ParsePublicKeys(" " + enc + " ,")
	if err != nil || len(keys) != 1 || !keys[0].Equal(ed25519.PublicKey(s.PublicKey())) {
		t.Fatalf("parse: %v %v", keys, err)
	}
	if _, err := ParsePublicKeys("short"); err == nil {
		t.Fatal("want error")
	}
}

func decode(s string) string {
	b, _ := base64.RawURLEncoding.DecodeString(s)
	return string(b)
}

func TestRevocation(t *testing.T) {
	s, v := pair(t, 1)
	p := principal
	p.Roles = []string{"r1", "r2"}
	tok, _, _ := s.Issue(p, time.Minute)
	if _, err := v.Verify(tok); err != nil {
		t.Fatal(err)
	}
	cut := time.Now().Add(time.Second)
	v.Revoke(nil, []string{"r2"}, cut)
	if _, err := v.Verify(tok); !errors.Is(err, ErrStaleToken) {
		t.Fatalf("role change: want stale, got %v", err)
	}
	s.now = func() time.Time { return cut.Add(time.Millisecond) }
	fresh, _, _ := s.Issue(p, time.Minute)
	if _, err := v.Verify(fresh); err != nil {
		t.Fatalf("token issued after the change must work: %v", err)
	}

	other := principal
	other.Subject = "u2"
	tok2, _, _ := s.Issue(other, time.Minute)
	v.Revoke([]string{"u2"}, nil, cut.Add(time.Second))
	if _, err := v.Verify(tok2); !errors.Is(err, ErrStaleToken) {
		t.Fatalf("user change: want stale, got %v", err)
	}
	keyP := reqctx.Principal{Kind: reqctx.KindAPIKey, Subject: "u2", MerchantID: "m1"}
	keyTok, _, _ := s.Issue(keyP, time.Minute)
	if _, err := v.Verify(keyTok); err != nil {
		t.Fatalf("API key tokens are not affected: %v", err)
	}
}
