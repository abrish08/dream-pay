package app_test

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"strings"
	"testing"
	"time"
)

// totp computes an authenticator code exactly like Microsoft Authenticator.
func totp(t *testing.T, secret string, step int64) string {
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	if err != nil {
		t.Fatal(err)
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	m := hmac.New(sha1.New, key)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	return fmt.Sprintf("%06d", (binary.BigEndian.Uint32(sum[off:off+4])&0x7fffffff)%1_000_000)
}

// TestVerificationAndAuthenticator: no access before the account is
// verified (6-digit code by email or SMS), staff are exempt, and the
// authenticator app protects login and password changes.
func TestVerificationAndAuthenticator(t *testing.T) {
	c, _, stop := startApp(t, nil)
	defer stop()

	// --- Email verification after signup.
	res := c.do("POST", "/auth/signup", "", "", map[string]any{"business_name": "V Shop", "email": "v@shop.test",
		"password": "owner-password-123"}, 201)
	if res["verification_required"] != true || res["channel"] != "email" || res["access_token"] != nil {
		t.Fatalf("signup: %v", res)
	}
	if e := c.login("v@shop.test", "owner-password-123", 403)["error"].(map[string]any); e["code"] != "verification_required" {
		t.Fatalf("unverified login: %v", e)
	}
	c.login("v@shop.test", "wrong-password-123", 401) // wrong password never reveals the state
	code := c.code("v@shop.test")
	wrong := "000000"
	if code == wrong {
		wrong = "111111"
	}
	c.do("POST", "/auth/verify", "", "", map[string]any{"email": "v@shop.test", "code": wrong}, 400)
	tok := c.do("POST", "/auth/verify", "", "", map[string]any{"email": "v@shop.test", "code": code}, 200)
	owner := tok["access_token"].(string)
	c.do("POST", "/auth/verify", "", "", map[string]any{"email": "v@shop.test", "code": code}, 400) // single use
	c.login("v@shop.test", "owner-password-123", 200)
	c.do("POST", "/auth/verify/resend", "", "", map[string]any{"email": "nobody@x.test"}, 202)

	// --- Brute force: 5 wrong codes kill the code.
	c.do("POST", "/auth/signup", "", "", map[string]any{"business_name": "B", "email": "b@shop.test",
		"password": "owner-password-123"}, 201)
	good := c.code("b@shop.test")
	for i := 0; i < 5; i++ {
		bad := fmt.Sprintf("%06d", i)
		if bad == good {
			bad = "999999"
		}
		c.do("POST", "/auth/verify", "", "", map[string]any{"email": "b@shop.test", "code": bad}, 400)
	}
	c.do("POST", "/auth/verify", "", "", map[string]any{"email": "b@shop.test", "code": good}, 400)

	// --- SMS channel (send.et): the code goes to the phone.
	r := c.do("POST", "/auth/signup", "", "", map[string]any{"business_name": "S", "email": "s@shop.test",
		"password": "owner-password-123", "phone": "0911 22 33 44", "verification_channel": "sms"}, 201)
	if r["channel"] != "sms" {
		t.Fatalf("sms signup: %v", r)
	}
	var smsCode string
	c.eventually("sms code", func() bool {
		m, ok := c.texts.Last("+251911223344")
		if ok {
			smsCode = m.Body[strings.Index(m.Body, "code is ")+8:][:6]
		}
		return ok
	})
	c.do("POST", "/auth/verify", "", "", map[string]any{"email": "s@shop.test", "code": smsCode}, 200)

	// --- Admin bypass: staff and staff-created accounts need no code.
	mid := c.do("POST", "/admin/merchants", c.admin, "", map[string]any{"name": "A", "email": "a@corp.test"}, 201)["id"]
	c.do("POST", "/admin/users", c.admin, "", map[string]any{"email": "o@corp.test", "name": "O",
		"password": "owner-password-123", "role": "owner", "merchant_id": mid}, 201)
	c.login("o@corp.test", "owner-password-123", 200)

	// --- Invited team member: verifies on first login.
	c.do("POST", "/v1/team/members", owner, "", map[string]any{"email": "dev@v.test", "name": "D",
		"password": "dev-password-1234", "roles": []string{"developer"}}, 201)
	c.loginVerified("dev@v.test", "dev-password-1234")

	// --- Authenticator (Microsoft Authenticator / TOTP).
	change := func(code string, want int) {
		c.do("POST", "/auth/password", owner, "", map[string]any{"current_password": "owner-password-123",
			"new_password": "changed-password-77", "totp_code": code}, want)
	}
	if e := c.raw("POST", "/auth/password", owner, "", map[string]any{"current_password": "owner-password-123",
		"new_password": "changed-password-77", "totp_code": "123456"}); e.code != 403 ||
		e.body["error"].(map[string]any)["code"] != "mfa_enrollment_required" {
		t.Fatalf("password change without an authenticator: %v", e)
	}
	setup := c.do("POST", "/auth/mfa/totp/setup", owner, "", nil, 200)
	secret := setup["secret"].(string)
	if !strings.HasPrefix(setup["otpauth_url"].(string), "otpauth://totp/Paygate:v@shop.test?") {
		t.Fatalf("setup: %v", setup)
	}
	if s := time.Now().Unix() % 30; s > 24 { // keep the three codes below in one window range
		time.Sleep(time.Duration(31-s) * time.Second)
	}
	step := time.Now().Unix() / 30
	c.do("POST", "/auth/mfa/totp/confirm", owner, "", map[string]any{"code": "000000"}, 401)
	conf := c.do("POST", "/auth/mfa/totp/confirm", owner, "", map[string]any{"code": totp(t, secret, step-1)}, 200)
	recovery := conf["recovery_codes"].([]any)
	if len(recovery) != 10 {
		t.Fatalf("recovery codes: %v", conf)
	}
	c.eventually("MFA enabled email", func() bool {
		m, _ := c.outbox.Last("v@shop.test")
		return strings.Contains(m.Subject, "Authenticator app enabled")
	})

	// Login now has a second step.
	first := c.login("v@shop.test", "owner-password-123", 200)
	if first["mfa_required"] != true || first["access_token"] != nil {
		t.Fatalf("login with MFA: %v", first)
	}
	c.do("POST", "/auth/login/mfa", "", "", map[string]any{"mfa_token": first["mfa_token"], "code": "000000"}, 401)
	done := c.do("POST", "/auth/login/mfa", "", "", map[string]any{"mfa_token": first["mfa_token"], "code": totp(t, secret, step)}, 200)
	if done["access_token"] == nil || done["refresh_token"] == nil {
		t.Fatalf("mfa login: %v", done)
	}
	owner = done["access_token"].(string)
	// The same code cannot be used twice (replay); a recovery code works once.
	second := c.login("v@shop.test", "owner-password-123", 200)
	c.do("POST", "/auth/login/mfa", "", "", map[string]any{"mfa_token": second["mfa_token"], "code": totp(t, secret, step)}, 401)
	c.do("POST", "/auth/login/mfa", "", "", map[string]any{"mfa_token": second["mfa_token"], "code": recovery[0]}, 200)
	third := c.login("v@shop.test", "owner-password-123", 200)
	c.do("POST", "/auth/login/mfa", "", "", map[string]any{"mfa_token": third["mfa_token"], "code": recovery[0]}, 401)

	// Password change needs the authenticator code (recovery codes are not enough).
	change("", 401)
	change(recovery[1].(string), 401)
	change(totp(t, secret, step+1), 204)
	c.eventually("old token rejected after password change", func() bool {
		return c.status2("GET", "/auth/me", owner, nil) == 401
	})
	c.login("v@shop.test", "owner-password-123", 401)
	again := c.login("v@shop.test", "changed-password-77", 200)
	owner = c.do("POST", "/auth/login/mfa", "", "", map[string]any{"mfa_token": again["mfa_token"], "code": recovery[2]}, 200)["access_token"].(string)
	if st := c.do("GET", "/auth/mfa", owner, "", nil, 200); st["enabled"] != true || st["recovery_codes_left"] != float64(8) {
		t.Fatalf("mfa status: %v", st)
	}
	c.do("POST", "/auth/mfa/totp/disable", owner, "", map[string]any{"password": "changed-password-77", "code": recovery[3]}, 204)
	if l := c.login("v@shop.test", "changed-password-77", 200); l["access_token"] == nil {
		t.Fatal("login after disabling MFA should be one step")
	}

	// --- Security headers.
	resp := c.raw("GET", "/v1/payments", "", "", nil)
	h := resp.header
	if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("X-Frame-Options") != "DENY" ||
		!strings.Contains(h.Get("Content-Security-Policy"), "default-src 'none'") {
		t.Fatalf("security headers: %v", h)
	}
}
