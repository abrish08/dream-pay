package domain

import (
	"strings"
	"testing"
	"time"
)

// RFC 6238 appendix B test vectors (SHA-1, secret "12345678901234567890").
func TestTOTPVectors(t *testing.T) {
	secret := b32.EncodeToString([]byte("12345678901234567890"))
	for ts, want := range map[int64]string{59: "287082", 1111111109: "081804", 1234567890: "005924", 2000000000: "279037"} {
		got, err := TOTPCode(secret, ts/30)
		if err != nil || got != want {
			t.Errorf("t=%d: got %s want %s", ts, got, want)
		}
	}
}

func TestVerifyTOTP(t *testing.T) {
	secret, _ := NewTOTPSecret()
	now := time.Unix(1_800_000_000, 0)
	code, _ := TOTPCode(secret, now.Unix()/30)
	step, ok := VerifyTOTP(secret, code, now, 0)
	if !ok {
		t.Fatal("current code rejected")
	}
	if _, ok := VerifyTOTP(secret, code, now, step); ok {
		t.Fatal("a used code must not be accepted twice (replay)")
	}
	prev, _ := TOTPCode(secret, now.Unix()/30-1)
	if _, ok := VerifyTOTP(secret, prev, now, 0); !ok {
		t.Fatal("previous window should be accepted (clock drift)")
	}
	old, _ := TOTPCode(secret, now.Unix()/30-3)
	if _, ok := VerifyTOTP(secret, old, now, 0); ok {
		t.Fatal("old code accepted")
	}
	u := OTPAuthURL("Paygate", "a@b.co", secret)
	if !strings.HasPrefix(u, "otpauth://totp/Paygate:a@b.co?") || !strings.Contains(u, "secret="+secret) {
		t.Fatalf("otpauth url: %s", u)
	}
}

func TestCodesAndPhones(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		c, _ := NewCode()
		if len(c) != 6 || strings.Trim(c, "0123456789") != "" {
			t.Fatalf("bad code %q", c)
		}
		seen[c] = true
	}
	if len(seen) < 45 {
		t.Fatal("codes not random")
	}
	if HashCode([]byte("p"), "u1", "123456") == HashCode([]byte("p"), "u2", "123456") {
		t.Fatal("hash must be bound to the user")
	}
	for in, want := range map[string]string{"0911 22 33 44": "+251911223344", "+251911223344": "+251911223344", "": ""} {
		if got, err := NormalizePhone(in); err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	if _, err := NormalizePhone("12345"); err == nil {
		t.Fatal("bad phone accepted")
	}
	rc, _ := NewRecoveryCodes(10)
	if len(rc) != 10 || len(rc[0]) != 9 || rc[0][4] != '-' {
		t.Fatalf("recovery codes %v", rc)
	}
}
