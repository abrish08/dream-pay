package app_test

import (
	"strings"
	"testing"
	"time"

	"paygate/internal/platform/config"
)

// TestStaffMFA: platform staff must sign in with an authenticator app
// (Microsoft Authenticator, Google Authenticator...) to use /admin; merchant
// users need it to request payouts. A session keeps how it was signed in:
// enrolling later does not upgrade old sessions.
func TestStaffMFA(t *testing.T) {
	c, _, stop := startApp(t, func(cfg *config.Config) {
		cfg.RequireStaffMFA, cfg.RequireMFAForPayouts = true, true
	})
	defer stop()

	if e := c.do("GET", "/admin/merchants", c.admin, "", nil, 403); code(e) != "mfa_required" {
		t.Fatalf("staff without MFA: %v", e)
	}
	setup := c.do("POST", "/auth/mfa/totp/setup", c.admin, "", nil, 200) // enrolling is allowed without MFA
	secret := setup["secret"].(string)
	if setup["issuer"] != "Paygate Admin" {
		t.Fatalf("staff label: %v", setup["issuer"])
	}
	if s := time.Now().Unix() % 30; s > 24 {
		time.Sleep(time.Duration(31-s) * time.Second)
	}
	step := time.Now().Unix() / 30
	c.do("POST", "/auth/mfa/totp/confirm", c.admin, "", map[string]any{"code": totp(t, secret, step-1)}, 200)
	if e := c.do("GET", "/admin/merchants", c.admin, "", nil, 403); code(e) != "mfa_required" {
		t.Fatalf("a session from before enrolment must not become MFA: %v", e)
	}

	first := c.login("admin@paygate.test", "admin-password-123", 200)
	if first["mfa_required"] != true {
		t.Fatalf("login: %v", first)
	}
	done := c.do("POST", "/auth/login/mfa", "", "", map[string]any{"mfa_token": first["mfa_token"], "code": totp(t, secret, step)}, 200)
	c.do("GET", "/admin/merchants", done["access_token"].(string), "", nil, 200)
	refreshed := c.do("POST", "/auth/refresh", "", "", map[string]any{"refresh_token": done["refresh_token"]}, 200)
	c.do("GET", "/admin/merchants", refreshed["access_token"].(string), "", nil, 200) // MFA kept across refresh

	// Merchants: payouts need MFA too.
	owner, _ := c.signup("mfa-payouts@shop.test")
	if e := c.do("POST", "/v1/payouts", owner, "po-1", map[string]any{"amount": 10000, "currency": "ETB"}, 403); code(e) != "mfa_required" {
		t.Fatalf("payout without MFA: %v", e)
	}
	if !strings.HasPrefix(setup["qr_code"].(string), "data:image/png;base64,iVBOR") {
		t.Fatalf("QR code to scan: %.40v", setup["qr_code"])
	}
}

// TestMerchantMFA: with REQUIRE_MERCHANT_MFA, a merchant user must enrol
// (scan the QR code or type the key) before using the dashboard API; the
// merchant's server keeps working with its API key.
func TestMerchantMFA(t *testing.T) {
	c, _, stop := startApp(t, func(cfg *config.Config) { cfg.RequireMerchantMFA = true })
	defer stop()
	c.do("POST", "/auth/signup", "", "", map[string]any{"business_name": "MFA Shop", "name": "Owner",
		"email": "mfa-merchant2@shop.test", "password": "owner-password-123", "phone": "+251911000000"}, 201)
	owner := c.do("POST", "/auth/verify", "", "", map[string]any{"email": "mfa-merchant2@shop.test",
		"code": c.code("mfa-merchant2@shop.test")}, 200)["access_token"].(string)
	if e := c.do("POST", "/v1/api-keys", owner, "", map[string]any{"name": "k", "environment": "test",
		"scopes": []string{"payments:write"}}, 403); code(e) != "mfa_required" {
		t.Fatalf("API key creation without MFA: %v", e)
	}
	if e := c.do("GET", "/v1/merchant", owner, "", nil, 403); code(e) != "mfa_required" {
		t.Fatalf("dashboard without MFA: %v", e)
	}
	setup := c.do("POST", "/auth/mfa/totp/setup", owner, "", nil, 200)
	secret := setup["secret"].(string)
	if setup["issuer"] != "Paygate - MFA Shop" || setup["account"] != "mfa-merchant2@shop.test" {
		t.Fatalf("merchant label: %v %v", setup["issuer"], setup["account"])
	}
	if s := time.Now().Unix() % 30; s > 24 {
		time.Sleep(time.Duration(31-s) * time.Second)
	}
	step := time.Now().Unix() / 30
	c.do("POST", "/auth/mfa/totp/confirm", owner, "", map[string]any{"code": totp(t, secret, step-1)}, 200)
	first := c.login("mfa-merchant2@shop.test", "owner-password-123", 200)
	tok := c.do("POST", "/auth/login/mfa", "", "", map[string]any{"mfa_token": first["mfa_token"], "code": totp(t, secret, step)}, 200)["access_token"].(string)
	c.do("GET", "/v1/merchant", tok, "", nil, 200)

	// The secret is stored encrypted, never in clear text.
	var stored string
	openTestDB(t).Raw("SELECT totp_secret FROM auth.users WHERE email = ?", "mfa-merchant2@shop.test").Scan(&stored)
	if stored == "" || strings.Contains(stored, secret) {
		t.Fatalf("TOTP secret not encrypted: %q", stored)
	}
}
