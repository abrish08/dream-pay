package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"paygate/internal/app"
	"paygate/internal/platform/authn/oidctest"
	"paygate/internal/platform/config"
	"paygate/internal/platform/mail"
	"paygate/internal/platform/sms"
)

// startApp boots the whole gateway on a clean database.
func startApp(t *testing.T, tweak func(*config.Config)) (*client, *receiver, func()) {
	t.Helper()
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	resetSchemas(t, dbURL)
	var handler http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
	hooks := &receiver{}
	hookSrv := httptest.NewServer(hooks)
	cfg := config.Config{
		DatabaseURL: dbURL, DBMaxConns: 20, Bus: "memory",
		AuthSigningKey: bytes.Repeat([]byte{7}, 32), APIKeyPepper: strings.Repeat("p", 32), APIKeyPepperVersion: 1,
		BootstrapAdminEmail: "admin@paygate.test", BootstrapAdminPassword: "admin-password-123",
		EncryptionKey: bytes.Repeat([]byte{1}, 32), PublicBaseURL: srv.URL, AllowInsecureWebhooks: true,
		PaymentTTL: 30 * time.Minute, PaymentExpiryEvery: 200 * time.Millisecond,
		Mailer: &mail.Memory{}, SMS: &sms.Memory{},
	}
	if tweak != nil {
		tweak(&cfg)
	}
	ctx := context.Background()
	a, err := app.Build(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	handler = a.Handler()
	c := &client{t: t, base: srv.URL, hookURL: hookSrv.URL, outbox: cfg.Mailer.(*mail.Memory), texts: cfg.SMS.(*sms.Memory)}
	c.admin = c.login("admin@paygate.test", "admin-password-123", 200)["access_token"].(string)
	return c, hooks, func() { a.Stop(ctx); srv.Close(); hookSrv.Close() }
}

// signup creates a merchant through self-signup and returns the owner token
// and a test API key with every key scope.
func (c *client) signup(email string) (owner, key string) {
	res := c.do("POST", "/auth/signup", "", "", map[string]any{"business_name": "Shop " + email, "name": "Owner",
		"email": email, "password": "owner-password-123", "phone": "+251911000000"}, 201)
	if res["verification_required"] != true || res["access_token"] != nil {
		c.t.Fatalf("signup must not log in before verification: %v", res)
	}
	tok := c.do("POST", "/auth/verify", "", "", map[string]any{"email": email, "code": c.code(email)}, 200)
	owner = tok["access_token"].(string)
	key = c.do("POST", "/v1/api-keys", owner, "", map[string]any{"name": "server", "environment": "test",
		"scopes": []string{"payments:read", "payments:write", "refunds:write", "balance:read", "webhooks:write"}}, 201)["secret"].(string)
	return owner, key
}

// pay creates a mock payment and completes it at the checkout page.
func (c *client) pay(key, idem string, amount int64) string {
	p := c.do("POST", "/v1/payments", key, idem, map[string]any{"amount": amount, "currency": "ETB",
		"provider": "mock", "reference": "ORD-" + idem}, 201)
	c.checkout(p["checkout_url"].(string), true)
	id := p["id"].(string)
	c.eventually("payment "+idem+" succeeded", func() bool {
		return c.do("GET", "/v1/payments/"+id, key, "", nil, 200)["status"] == "succeeded"
	})
	return id
}

func (c *client) checkout(checkoutURL string, success bool) {
	result := "fail"
	if success {
		result = "success"
	}
	// Don't follow the redirect to the merchant's return_url.
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noRedirect.PostForm(checkoutURL, url.Values{"result": {result}})
	if err != nil || resp.StatusCode >= 400 {
		c.t.Fatalf("checkout: %v %v", err, resp)
	}
	resp.Body.Close()
}

// TestOnboardingPaymentsAPIAndPayouts covers self-signup and approval, the
// integration API (metadata, expiry, filters, pagination), webhooks
// management, payouts and the API docs.
func TestOnboardingPaymentsAPIAndPayouts(t *testing.T) {
	c, hooks, stop := startApp(t, nil)
	defer stop()

	// --- Self-signup: pending merchant, test mode only.
	owner, key := c.signup("founder@shop.test")
	me := c.do("GET", "/v1/merchant", owner, "", nil, 200)
	mid := me["id"].(string)
	if me["status"] != "pending" || me["fee_bps"] != float64(150) {
		t.Fatalf("new merchant: %v", me)
	}
	c.do("POST", "/auth/signup", "", "", map[string]any{"business_name": "X", "email": "founder@shop.test",
		"password": "owner-password-123"}, 409)
	c.do("POST", "/v1/api-keys", owner, "", map[string]any{"name": "live", "environment": "live",
		"scopes": []string{"payments:write"}}, 403)
	if !strings.Contains(fmt.Sprint(c.do("GET", "/admin/merchants?status=pending", c.admin, "", nil, 200)["data"]), mid) {
		t.Fatal("pending merchant not listed for review")
	}

	ep := c.do("POST", "/v1/webhook-endpoints", key, "", map[string]any{"url": c.hookURL, "events": []string{"*"}}, 201)
	hooks.setSecret(ep["secret"].(string))

	// --- Payments API: description, metadata, customer, expiry.
	p := c.do("POST", "/v1/payments", key, "pay-1", map[string]any{"amount": 250000, "currency": "ETB", "provider": "mock",
		"reference": "ORDER-1001", "description": "2 x coffee", "metadata": map[string]string{"order_id": "1001"},
		"customer": map[string]string{"email": "c@example.com", "name": "Customer"}, "return_url": "https://shop.example/thanks",
		"expires_in_minutes": 15}, 201)
	exp, _ := time.Parse(time.RFC3339, p["expires_at"].(string))
	if p["metadata"].(map[string]any)["order_id"] != "1001" || p["description"] != "2 x coffee" ||
		time.Until(exp) < 14*time.Minute || time.Until(exp) > 16*time.Minute {
		t.Fatalf("payment fields: %v", p)
	}
	c.do("POST", "/v1/payments", key, "pay-bad", map[string]any{"amount": 1, "currency": "ETB", "provider": "mock",
		"reference": "x", "expires_in_minutes": 2}, 400)
	c.checkout(p["checkout_url"].(string), true)
	pid := p["id"].(string)
	c.eventually("payment succeeded", func() bool {
		return c.do("GET", "/v1/payments/"+pid, key, "", nil, 200)["status"] == "succeeded"
	})

	// --- Expiry: an abandoned payment expires (provider asked first); a late
	// completion at the provider is still honoured.
	ab := c.do("POST", "/v1/payments", key, "pay-abandoned", map[string]any{"amount": 5000, "currency": "ETB",
		"provider": "mock", "reference": "ORDER-1002", "expires_in_minutes": 5}, 201)
	abID := ab["id"].(string)
	c.expireNow(abID)
	c.eventually("payment expired", func() bool {
		return c.do("GET", "/v1/payments/"+abID, key, "", nil, 200)["status"] == "expired"
	})
	c.checkout(ab["checkout_url"].(string), true)
	c.eventually("late payment honoured", func() bool {
		return c.do("GET", "/v1/payments/"+abID, key, "", nil, 200)["status"] == "succeeded"
	})

	// --- Listing: filters and cursor pagination.
	for i := 0; i < 3; i++ {
		c.do("POST", "/v1/payments", key, fmt.Sprintf("list-%d", i), map[string]any{"amount": 1000 + i, "currency": "ETB",
			"provider": "mock", "reference": fmt.Sprintf("LIST-%d", i)}, 201)
	}
	byRef := c.do("GET", "/v1/payments?reference=ORDER-1001", key, "", nil, 200)["data"].([]any)
	if len(byRef) != 1 || byRef[0].(map[string]any)["id"] != pid {
		t.Fatalf("filter by reference: %v", byRef)
	}
	if n := len(c.do("GET", "/v1/payments?status=succeeded", key, "", nil, 200)["data"].([]any)); n != 2 {
		t.Fatalf("filter by status: %d", n)
	}
	seen := map[string]bool{}
	cursor := ""
	for pages := 0; ; pages++ {
		page := c.do("GET", "/v1/payments?limit=2&starting_after="+cursor, key, "", nil, 200)
		for _, x := range page["data"].([]any) {
			id := x.(map[string]any)["id"].(string)
			if seen[id] {
				t.Fatal("pagination returned a payment twice")
			}
			seen[id] = true
		}
		if page["has_more"] != true {
			break
		}
		cursor = page["next_cursor"].(string)
	}
	if len(seen) != 5 {
		t.Fatalf("pagination saw %d payments, want 5", len(seen))
	}

	// --- Refunds list.
	c.do("POST", "/v1/refunds", key, "rf-1", map[string]any{"payment_id": pid, "amount": 50000}, 201)
	if rs := c.do("GET", "/v1/refunds?payment_id="+pid, key, "", nil, 200)["data"].([]any); len(rs) != 1 {
		t.Fatalf("refunds list: %v", rs)
	}

	// --- Webhooks: every event type arrives signed; secret rotation; retry.
	c.eventually("webhooks for success, expiry, late success and refund", func() bool {
		types := hooks.types()
		return types["payment.succeeded"] >= 2 && types["payment.expired"] >= 1 && types["refund.succeeded"] >= 1
	})
	if hooks.badSignatures() > 0 {
		t.Fatal("bad webhook signature")
	}
	rot := c.do("POST", "/v1/webhook-endpoints/"+ep["id"].(string)+"/rotate-secret", key, "", nil, 200)
	if rot["secret"] == ep["secret"] || !strings.HasPrefix(rot["secret"].(string), "whsec_") {
		t.Fatal("secret not rotated")
	}
	hooks.setSecret(rot["secret"].(string))
	c.do("POST", "/v1/webhook-deliveries/00000000-0000-0000-0000-000000000000/retry", key, "", nil, 404)

	// --- Payouts (test mode): balance = 250000 + 5000 - fees - refund.
	bal := c.do("GET", "/v1/balance?currency=ETB&environment=test", owner, "", nil, 200)
	available := int64(bal["available"].(float64))
	if want := int64(250000 - 3750 + 5000 - 75 - 50000); available != want {
		t.Fatalf("available %d, want %d", available, want)
	}
	bank := map[string]any{"bank_name": "Commercial Bank of Ethiopia", "account_number": "1000123456789", "account_name": "Shop PLC"}
	c.do("POST", "/v1/payouts", key, "po-key", map[string]any{"amount": 10000, "currency": "ETB", "bank_account": bank}, 403) // never with an API key
	c.do("POST", "/v1/payouts", owner, "po-0", map[string]any{"amount": available + 1, "currency": "ETB",
		"environment": "test", "bank_account": bank}, 422)
	po := c.do("POST", "/v1/payouts", owner, "po-1", map[string]any{"amount": 100000, "currency": "ETB",
		"environment": "test", "bank_account": bank}, 201)
	if po["status"] != "requested" || po["account_number"] != "•••••••••6789" {
		t.Fatalf("payout: %v", po)
	}
	bal = c.do("GET", "/v1/balance?currency=ETB&environment=test", owner, "", nil, 200)
	if bal["available"] != float64(available-100000) || bal["pending_payouts"] != float64(100000) {
		t.Fatalf("funds not held: %v", bal)
	}
	c.do("POST", "/admin/payouts/"+po["id"].(string)+"/paid", c.admin, "", map[string]any{"bank_reference": "CBE-TRX-1"}, 200)
	c.do("POST", "/admin/payouts/"+po["id"].(string)+"/paid", c.admin, "", map[string]any{"bank_reference": "again"}, 409)
	po2 := c.do("POST", "/v1/payouts", owner, "po-2", map[string]any{"amount": 20000, "currency": "ETB",
		"environment": "test", "bank_account": bank}, 201)
	c.do("POST", "/admin/payouts/"+po2["id"].(string)+"/failed", c.admin, "", map[string]any{"reason": "account closed"}, 200)
	c.eventually("payout booked and failed payout released", func() bool {
		b := c.do("GET", "/v1/balance?currency=ETB&environment=test", owner, "", nil, 200)
		return b["available"] == float64(available-100000) && b["pending_payouts"] == float64(0)
	})
	c.eventually("payout webhooks", func() bool {
		ty := hooks.types()
		return ty["payout.paid"] == 1 && ty["payout.failed"] == 1
	})
	if tb := c.do("GET", "/admin/ledger/trial-balance", c.admin, "", nil, 200)["data"].([]any)[0].(map[string]any); tb["balanced"] != true {
		t.Fatalf("books unbalanced: %v", tb)
	}

	// --- Approval: live mode opens.
	c.do("POST", "/admin/merchants/"+mid+"/status", c.admin, "", map[string]any{"status": "active"}, 200)
	c.do("PATCH", "/admin/merchants/"+mid, c.admin, "", map[string]any{"fee_bps": 120}, 200)
	c.eventually("approved merchant can create live keys", func() bool {
		return c.status2("POST", "/v1/api-keys", owner, map[string]any{"name": "live", "environment": "live",
			"scopes": []string{"payments:write"}}) == 201
	})

	// --- API docs.
	if c.status2("GET", "/openapi.yaml", "", nil) != 200 || c.status2("GET", "/docs", "", nil) != 200 ||
		c.status2("GET", "/swagger", "", nil) != 200 {
		t.Fatal("API docs not served")
	}
}

// TestRateLimits: per-caller limit on the API, per-IP limit on login.
func TestRateLimits(t *testing.T) {
	c, _, stop := startApp(t, func(cfg *config.Config) {
		cfg.RateLimitRPS, cfg.RateLimitBurst, cfg.AuthRateLimitMin = 1, 5, 3
	})
	defer stop()
	limited := 0
	for i := 0; i < 10; i++ {
		resp := c.raw("GET", "/admin/merchants", c.admin, "", nil)
		if resp.code == 429 {
			limited++
			if resp.header.Get("Retry-After") == "" || resp.body["error"].(map[string]any)["code"] != "rate_limited" {
				t.Fatalf("429 without Retry-After / code: %v", resp.body)
			}
		}
	}
	if limited < 4 {
		t.Fatalf("only %d of 10 requests limited (burst 5)", limited)
	}
	logins := 0
	for i := 0; i < 6; i++ { // admin login already used one token of 3
		if c.raw("POST", "/auth/login", "", "", map[string]any{"email": "x@y.z", "password": "wrong-password-1"}).code == 429 {
			logins++
		}
	}
	if logins < 3 {
		t.Fatalf("login brute force not limited (%d)", logins)
	}
}

// ---- helpers ----

type rawResp struct {
	code   int
	header http.Header
	body   map[string]any
}

// raw never fails the test, so it is safe from many goroutines.
func (c *client) raw(method, path, token, idem string, body any) rawResp {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.base+path, rdr)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if idem != "" {
		req.Header.Set("Idempotency-Key", idem)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return rawResp{code: -1, body: map[string]any{"err": err.Error()}}
	}
	defer resp.Body.Close()
	out := rawResp{code: resp.StatusCode, header: resp.Header, body: map[string]any{}}
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &out.body)
	return out
}

func (c *client) status2(method, path, token string, body any) int {
	return c.raw(method, path, token, "", body).code
}

// expireNow moves a payment's deadline into the past (test shortcut for waiting).
func (c *client) expireNow(paymentID string) {
	db := openTestDB(c.t)
	if err := db.Exec("UPDATE payment.payments SET expires_at = now() - interval '1 minute' WHERE id = ?", paymentID).Error; err != nil {
		c.t.Fatal(err)
	}
}

// TestPaymentMethodsAndHostedCheckout: merchants ask for a payment METHOD;
// without one the customer picks it on the hosted page. Test mode routes every
// method to the sandbox; live mode needs a provider installed for the method.
func TestPaymentMethodsAndHostedCheckout(t *testing.T) {
	c, _, stop := startApp(t, nil)
	defer stop()
	owner, key := c.signup("methods@shop.test")

	ms := c.do("GET", "/v1/payment-methods", key, "", nil, 200)["data"].([]any)
	if len(ms) != 5 || ms[0].(map[string]any)["available"] != true || ms[0].(map[string]any)["providers"] != nil {
		t.Fatalf("test-mode methods: %v", ms)
	}

	// Method chosen by the merchant: routed (sandbox in test mode).
	p := c.do("POST", "/v1/payments", key, "m-1", map[string]any{"amount": 30000, "currency": "ETB",
		"payment_method": "telebirr", "reference": "M-1"}, 201)
	if p["payment_method"] != "telebirr" || p["provider"] != "mock" || p["status"] != "processing" {
		t.Fatalf("routed payment: %v", p)
	}
	c.do("POST", "/v1/payments", key, "m-2", map[string]any{"amount": 1, "currency": "ETB",
		"payment_method": "bitcoin", "reference": "M-2"}, 400)

	// No method: hosted checkout page, customer picks CBE Birr.
	h := c.do("POST", "/v1/payments", key, "m-3", map[string]any{"amount": 45000, "currency": "ETB",
		"reference": "M-3", "description": "Order M-3"}, 201)
	hid := h["id"].(string)
	if h["status"] != "pending" || h["checkout_url"] != c.base+"/pay/"+hid || h["provider"] != nil {
		t.Fatalf("hosted payment: %v", h)
	}
	resp, err := http.Get(c.base + "/pay/" + hid)
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(page), "CBE Birr") || !strings.Contains(string(page), "450.00") {
		t.Fatalf("checkout page %d: %s", resp.StatusCode, page)
	}
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err = noRedirect.PostForm(c.base+"/pay/"+hid, url.Values{"method": {"cbe_birr"}})
	if err != nil || resp.StatusCode != 303 {
		t.Fatalf("choose method: %v %v", err, resp)
	}
	providerURL := resp.Header.Get("Location")
	resp.Body.Close()
	// Double submit (second tab): same checkout, no second charge.
	resp, _ = noRedirect.PostForm(c.base+"/pay/"+hid, url.Values{"method": {"telebirr"}})
	if resp.Header.Get("Location") != providerURL {
		t.Fatal("second submit started another checkout")
	}
	resp.Body.Close()
	c.checkout(providerURL, true)
	c.eventually("hosted payment succeeded", func() bool {
		got := c.do("GET", "/v1/payments/"+hid, key, "", nil, 200)
		return got["status"] == "succeeded" && got["payment_method"] == "cbe_birr"
	})
	resp, _ = http.Get(c.base + "/pay/" + hid)
	page, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(page), "succeeded") || strings.Contains(string(page), `name="method"`) {
		t.Fatal("a paid payment must not offer methods again")
	}

	// Live: nothing is routed until a direct provider is installed.
	mid := c.do("GET", "/v1/merchant", owner, "", nil, 200)["id"].(string)
	c.do("POST", "/admin/merchants/"+mid+"/status", c.admin, "", map[string]any{"status": "active"}, 200)
	var live string
	c.eventually("live key after approval", func() bool {
		r := c.raw("POST", "/v1/api-keys", owner, "", map[string]any{"name": "live", "environment": "live",
			"scopes": []string{"payments:write", "payments:read"}})
		if r.code == 201 {
			live = r.body["secret"].(string)
		}
		return r.code == 201
	})
	if ms := c.do("GET", "/v1/payment-methods", live, "", nil, 200)["data"].([]any); ms[0].(map[string]any)["available"] != false {
		t.Fatalf("live methods without providers: %v", ms)
	}
	e := c.do("POST", "/v1/payments", live, "live-1", map[string]any{"amount": 1000, "currency": "ETB",
		"payment_method": "telebirr", "reference": "L-1"}, 422)
	if e["error"].(map[string]any)["code"] != "payment_method_unavailable" {
		t.Fatalf("live unrouted method: %v", e)
	}
	health := c.do("GET", "/admin/providers", c.admin, "", nil, 200)
	if len(health["providers"].([]any)) != 1 { // only the sandbox is installed
		t.Fatalf("providers: %v", health)
	}
}

// TestAccountsAndSignIn: tokens on login, the seeded admin, Google / Apple
// sign-in with one account per email, and password reset by email.
func TestAccountsAndSignIn(t *testing.T) {
	idp := oidctest.New()
	defer idp.Close()
	outbox := &mail.Memory{}
	c, _, stop := startApp(t, func(cfg *config.Config) {
		cfg.GoogleClientIDs, cfg.GoogleJWKSURL = []string{"web.apps.googleusercontent.com"}, idp.JWKSURL()
		cfg.AppleClientIDs, cfg.AppleJWKSURL = []string{"com.shop.app"}, idp.JWKSURL()
		cfg.Mailer, cfg.DashboardURL = outbox, "https://dashboard.example"
	})
	defer stop()
	c.outbox = outbox
	now := time.Now().Unix()
	google := func(sub, email string, verified bool) string {
		return idp.Token(map[string]any{"iss": "https://accounts.google.com", "aud": "web.apps.googleusercontent.com",
			"sub": sub, "email": email, "email_verified": verified, "name": "G User", "iat": now, "exp": now + 600})
	}
	apple := func(sub, email string) string {
		return idp.Token(map[string]any{"iss": "https://appleid.apple.com", "aud": "com.shop.app",
			"sub": sub, "email": email, "email_verified": "true", "iat": now, "exp": now + 600})
	}

	// --- Login: access + refresh token. Seeded admin holds every platform permission.
	login := c.login("admin@paygate.test", "admin-password-123", 200)
	if login["token_type"] != "Bearer" || login["expires_in"] != float64(300) ||
		!strings.HasPrefix(login["refresh_token"].(string), "rt_") || strings.Count(login["access_token"].(string), ".") != 2 {
		t.Fatalf("login tokens: %v", login)
	}
	perms := fmt.Sprint(c.do("GET", "/auth/me", login["access_token"].(string), "", nil, 200)["permissions"])
	for _, p := range []string{"users:manage", "roles:manage", "merchants:manage", "ledger:read", "payouts:manage",
		"providers:read", "reconciliation:run"} {
		if !strings.Contains(perms, p) {
			t.Fatalf("seeded admin lacks %s: %s", p, perms)
		}
	}

	// --- Google sign-up: needs a business name the first time; then plain sign-in.
	tok := google("g-1", "new@gmail.com", true)
	e := c.do("POST", "/auth/oauth/google", "", "", map[string]any{"id_token": tok}, 404)
	if e["error"].(map[string]any)["code"] != "account_not_found" {
		t.Fatalf("google without account: %v", e)
	}
	first := c.do("POST", "/auth/oauth/google", "", "", map[string]any{"id_token": tok, "business_name": "G Shop"}, 201)
	gUser := first["user"].(map[string]any)["id"]
	if first["refresh_token"] == nil || c.do("GET", "/v1/merchant", first["access_token"].(string), "", nil, 200)["status"] != "pending" {
		t.Fatalf("google signup: %v", first)
	}
	again := c.do("POST", "/auth/oauth/google", "", "", map[string]any{"id_token": google("g-1", "new@gmail.com", true)}, 200)
	if again["user"].(map[string]any)["id"] != gUser {
		t.Fatal("second Google sign-in created another account")
	}
	// Same email, manual signup: refused, no second account.
	c.do("POST", "/auth/signup", "", "", map[string]any{"business_name": "X", "email": "NEW@gmail.com",
		"password": "another-password-1"}, 409)
	// Same email through Apple: linked to the same account.
	if a := c.do("POST", "/auth/oauth/apple", "", "", map[string]any{"id_token": apple("a-1", "new@gmail.com")}, 200); a["user"].(map[string]any)["id"] != gUser {
		t.Fatal("Apple sign-in with the same email made a new account")
	}
	c.do("POST", "/auth/oauth/google", "", "", map[string]any{"id_token": google("g-2", "unverified@gmail.com", false),
		"business_name": "U"}, 403)
	c.do("POST", "/auth/oauth/google", "", "", map[string]any{"id_token": "not-a-token"}, 401)
	c.do("POST", "/auth/oauth/facebook", "", "", map[string]any{"id_token": tok}, 404)

	// --- Manual signup first, Google later: linked, never a second account.
	// The email was never verified, so the old password is removed
	// (anti pre-hijacking) and old sessions end.
	c.do("POST", "/auth/signup", "", "", map[string]any{"business_name": "P Shop", "email": "owner@pshop.test",
		"password": "owner-password-123"}, 201) // never verified
	c.login("owner@pshop.test", "owner-password-123", 403) // verification_required
	linked := c.do("POST", "/auth/oauth/google", "", "", map[string]any{"id_token": google("g-3", "owner@pshop.test", true)}, 200)
	pwUser := linked["user"].(map[string]any)["id"]
	if c.do("POST", "/auth/signup", "", "", map[string]any{"business_name": "P2", "email": "owner@pshop.test",
		"password": "owner-password-123"}, 409) == nil {
		t.Fatal("unreachable")
	}
	c.login("owner@pshop.test", "owner-password-123", 401) // unverified password removed
	c.eventually("link notice emailed", func() bool {
		m, _ := outbox.Last("owner@pshop.test")
		return strings.Contains(m.Subject, "New sign-in method")
	})

	// --- Password reset by email.
	c.do("POST", "/auth/password/forgot", "", "", map[string]any{"email": "nobody@nowhere.test"}, 202)
	c.do("POST", "/auth/password/forgot", "", "", map[string]any{"email": "Owner@PShop.test"}, 202)
	var resetToken string
	c.eventually("reset email", func() bool {
		m, ok := outbox.Last("owner@pshop.test")
		if !ok || !strings.Contains(m.Text, "https://dashboard.example/reset-password?token=") {
			return false
		}
		resetToken = strings.Fields(m.Text[strings.Index(m.Text, "token=")+6:])[0]
		return true
	})
	if outbox.Count("nobody@nowhere.test") != 0 {
		t.Fatal("email sent for an unknown address")
	}
	c.do("POST", "/auth/password/reset", "", "", map[string]any{"token": resetToken, "new_password": "short"}, 400)
	c.do("POST", "/auth/password/reset", "", "", map[string]any{"token": resetToken, "new_password": "brand-new-password-9"}, 204)
	c.do("POST", "/auth/password/reset", "", "", map[string]any{"token": resetToken, "new_password": "brand-new-password-9"}, 400) // single use
	c.do("POST", "/auth/password/reset", "", "", map[string]any{"token": "prt_forged", "new_password": "brand-new-password-9"}, 400)
	c.eventually("old access token rejected after reset", func() bool {
		return c.status2("GET", "/auth/me", linked["access_token"].(string), nil) == 401
	})
	relog := c.login("owner@pshop.test", "brand-new-password-9", 200)
	if relog["user"].(map[string]any)["id"] != pwUser {
		t.Fatal("reset changed the account")
	}
	c.eventually("password-changed notice", func() bool {
		m, _ := outbox.Last("owner@pshop.test")
		return strings.Contains(m.Subject, "password was changed")
	})
	// Throttle: at most 3 reset emails per hour per account.
	before := outbox.Count("owner@pshop.test")
	for i := 0; i < 5; i++ {
		c.do("POST", "/auth/password/forgot", "", "", map[string]any{"email": "owner@pshop.test"}, 202)
	}
	time.Sleep(300 * time.Millisecond)
	if sent := outbox.Count("owner@pshop.test") - before; sent != 2 { // 1 earlier + 2 = 3 this hour
		t.Fatalf("reset emails not throttled: %d sent", sent)
	}
}
