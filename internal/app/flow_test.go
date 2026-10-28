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
	"sync"
	"testing"
	"time"

	"paygate/internal/app"
	"paygate/internal/platform/config"
	"paygate/internal/platform/database"
	"paygate/internal/platform/secrets"
)

// TestPaymentFlow runs the whole gateway in-process against a real Postgres:
//
//	TEST_DATABASE_URL=postgres://postgres:postgres@localhost:5432/paygate_test?sslmode=disable go test ./internal/app/
//
// WARNING: it drops and recreates the module schemas in that database.
func TestPaymentFlow(t *testing.T) {
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	resetSchemas(t, dbURL)

	var handler http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
	defer srv.Close()

	hooks := &receiver{}
	hookSrv := httptest.NewServer(hooks)
	defer hookSrv.Close()

	cfg := config.Config{
		DatabaseURL: dbURL, DBMaxConns: 5, Bus: "memory",
		AuthSigningKey: bytes.Repeat([]byte{7}, 32), APIKeyPepper: strings.Repeat("p", 32), APIKeyPepperVersion: 1,
		AuthKeyCacheTTL:     0, // revocation takes effect immediately in this test
		BootstrapAdminEmail: "admin@paygate.test", BootstrapAdminPassword: "admin-password-123",
		EncryptionKey: bytes.Repeat([]byte{1}, 32), PublicBaseURL: srv.URL, AllowInsecureWebhooks: true,
	}
	ctx := context.Background()
	a, err := app.Build(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer a.Stop(ctx)
	handler = a.Handler()

	c := &client{t: t, base: srv.URL}

	// Platform admin (bootstrapped) logs in and onboards a merchant and its owner.
	c.admin = c.login("admin@paygate.test", "admin-password-123", 200)["access_token"].(string)
	m := c.do("POST", "/admin/merchants", c.admin, "", map[string]any{"name": "Shop", "email": "shop@example.com", "fee_bps": 150}, 201)
	mid := m["id"].(string)
	c.do("POST", "/admin/users", c.admin, "", map[string]any{"email": "owner@shop.test", "name": "Owner",
		"password": "owner-password-123", "role": "owner", "merchant_id": mid}, 201)

	// The merchant owner logs in and creates an API key (secret shown once).
	ownerLogin := c.login("owner@shop.test", "owner-password-123", 200)
	owner := ownerLogin["access_token"].(string)
	k := c.do("POST", "/v1/api-keys", owner, "",
		map[string]any{"name": "t", "environment": "test", "scopes": []string{"payments:read", "payments:write", "refunds:write", "balance:read", "webhooks:write"}}, 201)
	key := k["secret"].(string)
	listed := c.do("GET", "/v1/api-keys", owner, "", nil, 200)["data"].([]any)[0].(map[string]any)
	if _, leaked := listed["secret"]; leaked || listed["prefix"] != key[:12] || listed["last4"] != key[len(key)-4:] {
		t.Fatalf("key listing must show only prefix/last4: %v", listed)
	}
	ep := c.do("POST", "/v1/webhook-endpoints", key, "", map[string]any{"url": hookSrv.URL, "events": []string{"*"}}, 201)
	hooks.secret = ep["secret"].(string)

	// Create payment; replay is idempotent.
	body := map[string]any{"amount": 150000, "currency": "ETB", "provider": "mock", "reference": "INV-1"}
	p := c.do("POST", "/v1/payments", key, "idem-1", body, 201)
	pid := p["id"].(string)
	if again := c.do("POST", "/v1/payments", key, "idem-1", body, 200); again["id"] != pid {
		t.Fatal("idempotent replay returned a different payment")
	}
	c.do("POST", "/v1/payments", key, "idem-1", map[string]any{"amount": 1, "currency": "ETB", "provider": "mock", "reference": "x"}, 409)
	c.do("POST", "/v1/payments", key, "idem-2", map[string]any{"amount": 1, "currency": "ETB", "provider": "chapa", "reference": "x"}, 400) // test key + real provider

	// Customer pays; events propagate.
	resp, err := http.PostForm(p["checkout_url"].(string), url.Values{"result": {"success"}})
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("checkout: %v %v", err, resp)
	}
	c.eventually("payment succeeded", func() bool {
		return c.do("GET", "/v1/payments/"+pid, key, "", nil, 200)["status"] == "succeeded"
	})
	c.eventually("balance credited", func() bool {
		return c.do("GET", "/v1/balance?currency=ETB", key, "", nil, 200)["available"] == float64(147750)
	})

	// Refunds: partial ok, over-refund rejected, provider decline recorded as failed.
	r := c.do("POST", "/v1/refunds", key, "r-1", map[string]any{"payment_id": pid, "amount": 30000}, 201)
	if r["status"] != "succeeded" {
		t.Fatalf("refund status %v", r["status"])
	}
	c.do("POST", "/v1/refunds", key, "r-2", map[string]any{"payment_id": pid, "amount": 999999}, 422)
	if d := c.do("POST", "/v1/refunds", key, "r-3", map[string]any{"payment_id": pid, "amount": 113}, 201); d["status"] != "failed" {
		t.Fatalf("declined refund status %v", d["status"])
	}
	c.eventually("payment partially refunded", func() bool {
		return c.do("GET", "/v1/payments/"+pid, key, "", nil, 200)["status"] == "partially_refunded"
	})
	c.eventually("balance debited", func() bool {
		return c.do("GET", "/v1/balance?currency=ETB", key, "", nil, 200)["available"] == float64(117750)
	})

	// Books balance.
	tb := c.do("GET", "/admin/ledger/trial-balance", c.admin, "", nil, 200)["data"].([]any)[0].(map[string]any)
	if tb["balanced"] != true {
		t.Fatalf("trial balance not balanced: %v", tb)
	}

	// Signed webhooks arrive: payment.succeeded, refund.succeeded, refund.failed.
	c.eventually("webhooks delivered", func() bool { return hooks.count() >= 3 })
	if bad := hooks.badSignatures(); bad > 0 {
		t.Fatalf("%d webhooks had invalid signatures", bad)
	}

	// Reconciliation finds no mismatch.
	run := c.do("POST", "/admin/reconciliation/runs", c.admin, "", map[string]any{
		"from": time.Now().Add(-time.Hour).UTC(), "to": time.Now().Add(time.Hour).UTC()}, 202)
	c.eventually("reconciliation completed", func() bool {
		got := c.do("GET", "/admin/reconciliation/runs/"+run["id"].(string), c.admin, "", nil, 200)
		return got["status"] == "completed" && got["checked"] == float64(1) && got["mismatched"] == float64(0)
	})

	c.authChecks(mid, owner, ownerLogin["refresh_token"].(string), key)
}

// authChecks covers authentication, RBAC and API key management.
func (c *client) authChecks(mid, owner, ownerRefresh, key string) {
	t := c.t
	// Credentials.
	c.do("GET", "/v1/payments", "", "", nil, 401)
	c.do("GET", "/v1/payments", "sk_test_invalid", "", nil, 401)
	c.do("GET", "/v1/payments", "not-a-jwt", "", nil, 401)
	c.do("GET", "/.well-known/jwks.json", "", "", nil, 200)
	c.login("owner@shop.test", "wrong-password-123", 401)

	// Tenancy: merchants cannot use /admin, staff cannot use /v1.
	c.do("GET", "/admin/merchants/"+mid, owner, "", nil, 403)
	c.do("GET", "/admin/merchants/"+mid, key, "", nil, 403)
	c.do("GET", "/v1/payments", c.admin, "", nil, 403)

	// API keys: scopes enforced; a key cannot manage keys.
	limited := c.do("POST", "/v1/api-keys", owner, "", map[string]any{"name": "ro", "environment": "test", "scopes": []string{"payments:read"}}, 201)
	c.do("POST", "/v1/payments", limited["secret"].(string), "idem-3", map[string]any{"amount": 1, "currency": "ETB", "provider": "mock", "reference": "x"}, 403)
	c.do("POST", "/v1/api-keys", key, "", map[string]any{"name": "x", "scopes": []string{"payments:read"}}, 403)
	c.do("POST", "/v1/api-keys", owner, "", map[string]any{"name": "x", "scopes": []string{"apikeys:manage"}}, 400)
	c.do("POST", "/v1/payments", owner, "idem-4", map[string]any{"amount": 1, "currency": "ETB", "provider": "mock", "reference": "x"}, 403) // users don't create payments

	// Revoke: the key stops working at once (cache disabled in this test).
	c.do("DELETE", "/v1/api-keys/"+limited["id"].(string), owner, "", nil, 204)
	c.do("GET", "/v1/payments", limited["secret"].(string), "", nil, 401)

	// Roll: new key works, old key ends after the grace period (0 here).
	c.do("POST", "/v1/api-keys/"+limited["id"].(string)+"/roll", owner, "", map[string]any{"grace_hours": 0}, 422) // revoked keys can't be rolled
	keys := c.do("GET", "/v1/api-keys", owner, "", nil, 200)["data"].([]any)
	var mainID string
	for _, x := range keys {
		if x.(map[string]any)["name"] == "t" {
			mainID = x.(map[string]any)["id"].(string)
		}
	}
	newKey := c.do("POST", "/v1/api-keys/"+mainID+"/roll", owner, "", map[string]any{"grace_hours": 0}, 201)["secret"].(string)
	c.do("GET", "/v1/payments", newKey, "", nil, 200)
	c.do("GET", "/v1/payments", key, "", nil, 401)

	// Team RBAC: a viewer can read but not manage; a developer cannot grant refunds.
	c.do("POST", "/v1/team/members", owner, "", map[string]any{"email": "viewer@shop.test", "name": "V", "password": "viewer-password-123", "role": "viewer"}, 201)
	devID := c.do("POST", "/v1/team/members", owner, "", map[string]any{"email": "dev@shop.test", "name": "D", "password": "dev-password-1234", "role": "developer"}, 201)["id"].(string)
	c.do("POST", "/v1/team/members", owner, "", map[string]any{"email": "x@shop.test", "name": "X", "password": "x-password-12345", "role": "super_admin"}, 400)
	viewer := c.login("viewer@shop.test", "viewer-password-123", 200)["access_token"].(string)
	c.do("GET", "/v1/payments", viewer, "", nil, 200)
	c.do("GET", "/v1/balance?currency=ETB&environment=test", viewer, "", nil, 200)
	c.do("POST", "/v1/api-keys", viewer, "", map[string]any{"name": "x", "scopes": []string{"payments:read"}}, 403)
	c.do("POST", "/v1/team/members", viewer, "", map[string]any{"email": "y@shop.test", "name": "Y", "password": "y-password-12345", "role": "owner"}, 403)
	dev := c.login("dev@shop.test", "dev-password-1234", 200)["access_token"].(string)
	c.do("POST", "/v1/api-keys", dev, "", map[string]any{"name": "x", "scopes": []string{"refunds:write"}}, 403)
	c.do("POST", "/v1/api-keys", dev, "", map[string]any{"name": "x", "scopes": []string{"payments:write"}}, 201)
	if me := c.do("GET", "/auth/me", dev, "", nil, 200); me["role"] != "developer" || me["merchant_id"] != mid {
		t.Fatalf("me: %v", me)
	}
	c.rbacChecks(owner, dev, devID)

	// Refresh tokens rotate; re-using an old one revokes the whole session family.
	next := c.do("POST", "/auth/refresh", "", "", map[string]any{"refresh_token": ownerRefresh}, 200)["refresh_token"].(string)
	c.do("POST", "/auth/refresh", "", "", map[string]any{"refresh_token": ownerRefresh}, 401)
	c.do("POST", "/auth/refresh", "", "", map[string]any{"refresh_token": next}, 401)

	// Brute force: the account locks after 5 wrong passwords.
	for i := 0; i < 5; i++ {
		c.login("viewer@shop.test", "wrong-password-123", 401)
	}
	if e := c.login("viewer@shop.test", "viewer-password-123", 401)["error"].(map[string]any); e["code"] != "account_locked" {
		t.Fatalf("want account_locked, got %v", e)
	}

	// Suspension reaches auth through the merchant.updated event.
	c.do("POST", "/admin/merchants/"+mid+"/status", c.admin, "", map[string]any{"status": "suspended"}, 200)
	c.eventually("suspended merchant's key rejected", func() bool {
		return c.status("GET", "/v1/payments", newKey) == 403
	})
	c.login("owner@shop.test", "owner-password-123", 403)
}

// rbacChecks covers dynamic roles: custom roles, adding/removing permissions,
// assigning/removing roles, stale tokens, and the anti-escalation rules.
func (c *client) rbacChecks(owner, dev, devID string) {
	t := c.t
	has := func(list any, code string) bool {
		for _, x := range list.([]any) {
			if s, ok := x.(string); ok && s == code {
				return true
			}
			if m, ok := x.(map[string]any); ok && (m["code"] == code || m["name"] == code) {
				return true
			}
		}
		return false
	}

	// Catalogue: every service registered its permissions at start-up.
	perms := c.do("GET", "/v1/permissions", owner, "", nil, 200)["data"]
	if !has(perms, "payments:write") || !has(perms, "refunds:write") || has(perms, "merchants:manage") {
		t.Fatalf("merchant permission catalogue: %v", perms)
	}
	roles := c.do("GET", "/v1/roles", owner, "", nil, 200)["data"]
	if !has(roles, "owner") || !has(roles, "developer") || has(roles, "super_admin") {
		t.Fatalf("merchant roles: %v", roles)
	}

	// Custom role, rules on what it may contain and who may edit what.
	desk := c.do("POST", "/v1/roles", owner, "", map[string]any{"name": "Refund Desk", "description": "refunds",
		"permissions": []string{"payments:read", "refunds:write"}}, 201)
	deskID := desk["id"].(string)
	c.do("POST", "/v1/roles", owner, "", map[string]any{"name": "Refund Desk", "permissions": []string{"payments:read"}}, 409)
	c.do("POST", "/v1/roles", owner, "", map[string]any{"name": "Bad", "permissions": []string{"merchants:manage"}}, 400)
	c.do("POST", "/v1/roles", owner, "", map[string]any{"name": "Bad", "permissions": []string{"nope:nope"}}, 400)
	var developerID, ownerRoleID string
	for _, x := range roles.([]any) {
		r := x.(map[string]any)
		switch r["name"] {
		case "developer":
			developerID = r["id"].(string)
		case "owner":
			ownerRoleID = r["id"].(string)
		}
	}
	c.do("POST", "/v1/roles/"+developerID+"/permissions", owner, "", map[string]any{"permissions": []string{"refunds:write"}}, 403) // shared template
	c.do("DELETE", "/v1/roles/"+ownerRoleID+"/permissions/payments:read", owner, "", nil, 422)                                      // locked
	c.do("POST", "/v1/roles", dev, "", map[string]any{"name": "Dev2", "permissions": []string{"payments:read"}}, 403)               // no roles:manage

	// Assign the custom role: the developer's current token goes stale, a new
	// login carries the extra permission.
	u := c.do("POST", "/v1/team/members/"+devID+"/roles", owner, "", map[string]any{"role": "Refund Desk"}, 200)
	if !has(u["roles"], "Refund Desk") || !has(u["permissions"], "refunds:write") {
		t.Fatalf("assigned: %v", u)
	}
	c.eventually("developer's old token is stale after a role change", func() bool {
		return c.status("GET", "/v1/payments", dev) == 401
	})
	dev = c.login("dev@shop.test", "dev-password-1234", 200)["access_token"].(string)
	c.do("POST", "/v1/api-keys", dev, "", map[string]any{"name": "refunds", "scopes": []string{"refunds:write"}}, 201)

	// Remove a permission from the role: applies to every holder.
	c.do("DELETE", "/v1/roles/"+deskID+"/permissions/refunds:write", owner, "", nil, 200)
	c.eventually("holders' tokens are stale after a permission is removed", func() bool {
		return c.status("GET", "/v1/payments", dev) == 401
	})
	dev = c.login("dev@shop.test", "dev-password-1234", 200)["access_token"].(string)
	c.do("POST", "/v1/api-keys", dev, "", map[string]any{"name": "refunds", "scopes": []string{"refunds:write"}}, 403)
	c.do("POST", "/v1/roles/"+deskID+"/permissions", owner, "", map[string]any{"permissions": []string{"refunds:write"}}, 200)

	// Anti-escalation: an admin cannot hand out or touch the owner role.
	adminID := c.do("POST", "/v1/team/members", owner, "", map[string]any{"email": "adm@shop.test", "name": "A",
		"password": "adm-password-12345", "roles": []string{"admin"}}, 201)["id"].(string)
	adm := c.login("adm@shop.test", "adm-password-12345", 200)["access_token"].(string)
	c.do("POST", "/v1/team/members/"+devID+"/roles", adm, "", map[string]any{"role": "owner"}, 403)
	c.do("POST", "/v1/team/members", adm, "", map[string]any{"email": "o2@shop.test", "name": "O",
		"password": "o2-password-12345", "roles": []string{"owner"}}, 403)
	c.do("POST", "/v1/team/members/"+devID+"/roles", adm, "", map[string]any{"role": "viewer"}, 200)
	ownerID := c.do("GET", "/auth/me", owner, "", nil, 200)["id"].(string)
	c.do("DELETE", "/v1/team/members/"+ownerID, adm, "", nil, 403)
	c.do("DELETE", "/v1/team/members/"+ownerID+"/roles/"+ownerRoleID, owner, "", nil, 403) // own roles
	c.do("POST", "/v1/team/members/"+adminID+"/roles", adm, "", map[string]any{"role": "viewer"}, 403)

	// Roles in use cannot be deleted; unassign first.
	c.do("DELETE", "/v1/roles/"+deskID, owner, "", nil, 409)
	c.do("DELETE", "/v1/team/members/"+devID+"/roles/"+deskID, owner, "", nil, 200)
	c.do("DELETE", "/v1/roles/"+deskID, owner, "", nil, 204)

	if log := c.do("GET", "/v1/audit-log", owner, "", nil, 200)["data"].([]any); len(log) < 6 {
		t.Fatalf("audit log too short: %d", len(log))
	}

	// Platform side: a custom staff role (e.g. an auditor) built from permissions.
	if !has(c.do("GET", "/admin/roles", c.admin, "", nil, 200)["data"], "finance") {
		t.Fatal("platform roles missing")
	}
	c.do("POST", "/admin/roles", c.admin, "", map[string]any{"name": "Auditor", "permissions": []string{"ledger:read"}}, 201)
	c.do("POST", "/admin/roles", c.admin, "", map[string]any{"name": "Bad", "permissions": []string{"payments:write"}}, 400)
	c.do("POST", "/admin/users", c.admin, "", map[string]any{"email": "auditor@paygate.test", "name": "Au",
		"password": "auditor-password-1", "roles": []string{"Auditor"}}, 201)
	aud := c.login("auditor@paygate.test", "auditor-password-1", 200)["access_token"].(string)
	c.do("GET", "/admin/ledger/trial-balance", aud, "", nil, 200)
	c.do("GET", "/admin/roles", aud, "", nil, 403)
	c.do("POST", "/admin/users", aud, "", map[string]any{"email": "z@paygate.test", "name": "Z",
		"password": "z-password-123456", "roles": []string{"super_admin"}}, 403)
}

func resetSchemas(t *testing.T, dbURL string) {
	t.Helper()
	db, err := database.Open(dbURL, 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range app.Modules {
		if err := db.Exec("DROP SCHEMA IF EXISTS " + m + " CASCADE").Error; err != nil {
			t.Fatal(err)
		}
	}
	sqlDB, _ := db.DB()
	_ = sqlDB.Close()
}

type client struct {
	t     *testing.T
	base  string
	admin string // platform admin's access token
}

func (c *client) login(email, password string, want int) map[string]any {
	c.t.Helper()
	return c.do("POST", "/auth/login", "", "", map[string]any{"email": email, "password": password}, want)
}

func (c *client) status(method, path, token string) int {
	req, _ := http.NewRequest(method, c.base+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func (c *client) do(method, path, token, idem string, body any, want int) map[string]any {
	c.t.Helper()
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
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		c.t.Fatalf("%s %s: status %d, want %d: %s", method, path, resp.StatusCode, want, raw)
	}
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return out
}

func (c *client) eventually(what string, cond func() bool) {
	c.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	c.t.Fatalf("timed out waiting for: %s", what)
}

// receiver is a merchant's webhook endpoint that verifies signatures.
type receiver struct {
	mu     sync.Mutex
	secret string
	calls  int
	bad    int
}

func (r *receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	var ts int64
	var sig string
	for _, part := range strings.Split(req.Header.Get("Paygate-Signature"), ",") {
		k, v, _ := strings.Cut(part, "=")
		switch k {
		case "t":
			fmt.Sscan(v, &ts)
		case "v1":
			sig = v
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	expected := secrets.HMACHex([]byte(r.secret), append([]byte(fmt.Sprintf("%d.", ts)), body...))
	if !secrets.EqualHex(sig, expected) || time.Since(time.Unix(ts, 0)) > 5*time.Minute {
		r.bad++
	}
	w.WriteHeader(http.StatusOK)
}

func (r *receiver) count() int         { r.mu.Lock(); defer r.mu.Unlock(); return r.calls }
func (r *receiver) badSignatures() int { r.mu.Lock(); defer r.mu.Unlock(); return r.bad }

// TestPepperRotation: keys created under the old API key pepper keep working
// after the pepper is rotated, and are re-hashed with the new one on use.
func TestPepperRotation(t *testing.T) {
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	resetSchemas(t, dbURL)
	ctx := context.Background()
	base := config.Config{
		DatabaseURL: dbURL, DBMaxConns: 3, Bus: "memory", AuthSigningKey: bytes.Repeat([]byte{7}, 32),
		BootstrapAdminEmail: "admin@paygate.test", BootstrapAdminPassword: "admin-password-123",
		EncryptionKey: bytes.Repeat([]byte{1}, 32), PublicBaseURL: "http://localhost", AllowInsecureWebhooks: true,
	}
	start := func(cfg config.Config) (*client, func()) {
		a, err := app.Build(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatal(err)
		}
		if err := a.Start(ctx); err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(a.Handler())
		return &client{t: t, base: srv.URL}, func() { srv.Close(); a.Stop(ctx) }
	}

	old := base
	old.APIKeyPepper, old.APIKeyPepperVersion = strings.Repeat("a", 32), 1
	c, stop := start(old)
	c.admin = c.login("admin@paygate.test", "admin-password-123", 200)["access_token"].(string)
	mid := c.do("POST", "/admin/merchants", c.admin, "", map[string]any{"name": "S", "email": "s@example.com", "fee_bps": 0}, 201)["id"].(string)
	c.do("POST", "/admin/users", c.admin, "", map[string]any{"email": "o@s.test", "name": "O", "password": "owner-password-123", "role": "owner", "merchant_id": mid}, 201)
	owner := c.login("o@s.test", "owner-password-123", 200)["access_token"].(string)
	key := c.do("POST", "/v1/api-keys", owner, "", map[string]any{"name": "k", "scopes": []string{"payments:read"}}, 201)["secret"].(string)
	stop()

	rotated := base
	rotated.APIKeyPepper, rotated.APIKeyPepperVersion, rotated.APIKeyPepperPrevious = strings.Repeat("b", 32), 2, old.APIKeyPepper
	c, stop = start(rotated)
	c.do("GET", "/v1/payments", key, "", nil, 200) // old key still works, and is re-hashed
	stop()

	only := base
	only.APIKeyPepper, only.APIKeyPepperVersion = rotated.APIKeyPepper, 2
	c, stop = start(only)
	defer stop()
	c.do("GET", "/v1/payments", key, "", nil, 200) // works without the old pepper now
}
