package app_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// TestEveryEndpointWithSwaggerSamples calls EVERY operation in openapi.yaml,
// with the sample data shown in Swagger ("Try it out"), as the right kind of
// caller. It fails when:
//   - the server errors (5xx),
//   - a sample is malformed for its endpoint (invalid_json: unknown field, wrong type),
//   - the route or the documented auth is wrong (route_not_found, 401/403 auth errors),
//   - any documented operation was not called.
//
// Business answers to sample data are fine (e.g. the sample verification
// code is "wrong", the sample payout exceeds the balance): the point is that
// every endpoint is reachable and every sample is a valid request.
func TestEveryEndpointWithSwaggerSamples(t *testing.T) {
	raw, err := os.ReadFile("apidocs/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Paths      map[string]map[string]operation `yaml:"paths"`
		Components struct {
			RequestBodies map[string]body `yaml:"requestBodies"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}

	c, _, stop := startApp(t, nil)
	defer stop()
	fx := newFixtures(c)

	type call struct {
		method, path string
		op           operation
	}
	var calls []call
	for path, ops := range spec.Paths {
		for method, op := range ops {
			calls = append(calls, call{strings.ToUpper(method), path, op})
		}
	}
	// Reads and changes first, deletions last (they remove fixtures), and the
	// session-ending calls at the very end.
	rank := func(x call) int {
		switch {
		case x.path == "/auth/logout" || x.path == "/auth/password":
			return 3
		case x.method == "DELETE":
			return 2
		}
		return 1
	}
	sort.SliceStable(calls, func(i, j int) bool {
		if rank(calls[i]) != rank(calls[j]) {
			return rank(calls[i]) < rank(calls[j])
		}
		return calls[i].path+calls[i].method < calls[j].path+calls[j].method
	})

	authErrors := map[string]bool{"missing_credentials": true, "invalid_token": true, "token_expired": true,
		"token_stale": true, "invalid_api_key": true, "permission_denied": true, "merchant_only": true,
		"platform_only": true, "user_login_required": true}
	var report []string
	for _, x := range calls {
		b := x.op.RequestBody
		if b.Ref != "" {
			b = spec.Components.RequestBodies[b.Ref[strings.LastIndex(b.Ref, "/")+1:]]
		}
		path := fx.fill(x.method, x.path)
		token := fx.callerFor(x.method, x.path, len(x.op.Security) > 0 && len(x.op.Security[0]) == 0)
		res := fx.send(x.method, path, token, b)

		code, _ := res.errCode()
		status := fmt.Sprintf("%-6s %-48s %d %s", x.method, x.path, res.status, code)
		report = append(report, status)
		switch {
		case res.status >= 500 || res.status <= 0:
			t.Errorf("server error: %s %s", status, res.body)
		case code == "invalid_json":
			t.Errorf("the Swagger sample is not a valid request: %s %s", status, res.body)
		case code == "route_not_found":
			t.Errorf("documented route does not exist: %s", status)
		case authErrors[code]:
			t.Errorf("auth problem (wrong caller in docs or test): %s %s", status, res.body)
		}
	}
	t.Logf("%d operations called with their Swagger samples:\n  %s", len(calls), strings.Join(report, "\n  "))
	if len(calls) < 90 {
		t.Fatalf("only %d operations in the spec", len(calls))
	}
}

type operation struct {
	Security    []map[string][]string `yaml:"security"`
	RequestBody body                  `yaml:"requestBody"`
}

type body struct {
	Ref     string `yaml:"$ref"`
	Content map[string]struct {
		Example any `yaml:"example"`
	} `yaml:"content"`
}

// fixtures: real records the path parameters and samples point at.
type fixtures struct {
	c                                         *client
	owner, key, refresh                       string
	merchant, payment, refund, hosted, mockTx string
	webhook, delivery, payout, run            string
	role, roleDel, member, memberDel          string
	staff, staffDel, staffRole, staffRoleDel  string
	analystRole, supportRole                  string
	keyRoll, keyDel, keyAdminDel              string
}

func newFixtures(c *client) *fixtures {
	f := &fixtures{c: c}
	f.merchant = c.do("POST", "/admin/merchants", c.admin, "", map[string]any{"name": "Fixture Shop", "email": "fixture@shop.test"}, 201)["id"].(string)
	c.do("POST", "/admin/users", c.admin, "", map[string]any{"email": "owner@fixture.test", "name": "Owner",
		"password": "fixture-owner-pass-1", "role": "owner", "merchant_id": f.merchant}, 201)
	login := c.login("owner@fixture.test", "fixture-owner-pass-1", 200)
	f.owner, f.refresh = login["access_token"].(string), login["refresh_token"].(string)
	scopes := []string{"payments:read", "payments:write", "refunds:write", "balance:read", "webhooks:write"}
	newKey := func(name string) map[string]any {
		return c.do("POST", "/v1/api-keys", f.owner, "", map[string]any{"name": name, "environment": "test", "scopes": scopes}, 201)
	}
	f.key = newKey("server")["secret"].(string)
	f.keyRoll, f.keyDel, f.keyAdminDel = newKey("roll")["id"].(string), newKey("del")["id"].(string), newKey("admin-del")["id"].(string)

	f.webhook = c.do("POST", "/v1/webhook-endpoints", f.key, "", map[string]any{"url": c.hookURL, "events": []string{"*"}}, 201)["id"].(string)
	f.payment = c.pay(f.key, "fixture-pay", 500000)
	f.refund = c.do("POST", "/v1/refunds", f.key, "fixture-refund", map[string]any{"payment_id": f.payment, "amount": 1000}, 201)["id"].(string)
	f.hosted = c.do("POST", "/v1/payments", f.key, "fixture-hosted", map[string]any{"amount": 20000, "currency": "ETB", "reference": "H-1"}, 201)["id"].(string)
	f.mockTx = c.do("POST", "/v1/payments", f.key, "fixture-mock", map[string]any{"amount": 30000, "currency": "ETB",
		"payment_method": "card", "reference": "M-1"}, 201)["id"].(string)
	c.eventually("a webhook delivery exists", func() bool {
		ds := c.do("GET", "/v1/webhook-deliveries", f.key, "", nil, 200)["data"].([]any)
		if len(ds) > 0 {
			f.delivery = ds[0].(map[string]any)["id"].(string)
		}
		return f.delivery != ""
	})
	c.eventually("balance for a payout", func() bool {
		return c.do("GET", "/v1/balance?currency=ETB", f.key, "", nil, 200)["available"].(float64) > 100000
	})
	f.payout = c.do("POST", "/v1/payouts", f.owner, "fixture-payout", map[string]any{"amount": 100000, "currency": "ETB",
		"environment": "test", "bank_account": map[string]string{"bank_name": "Awash Bank", "account_number": "01320123456700",
			"account_name": "Fixture PLC"}}, 201)["id"].(string)
	f.run = c.do("POST", "/admin/reconciliation/runs", c.admin, "", map[string]any{"from": time.Now().Add(-24 * time.Hour).UTC(), "to": time.Now().Add(24 * time.Hour).UTC()}, 202)["id"].(string)

	f.role = c.do("POST", "/v1/roles", f.owner, "", map[string]any{"name": "Fixture Role", "permissions": []string{"payments:read", "refunds:write"}}, 201)["id"].(string)
	f.roleDel = c.do("POST", "/v1/roles", f.owner, "", map[string]any{"name": "Disposable", "permissions": []string{"payments:read"}}, 201)["id"].(string)
	member := func(email string) string {
		return c.do("POST", "/v1/team/members", f.owner, "", map[string]any{"email": email, "name": "M",
			"password": "fixture-member-pass", "roles": []string{"viewer"}}, 201)["id"].(string)
	}
	f.member, f.memberDel = member("member@fixture.test"), member("member-del@fixture.test")
	staff := func(email string) string {
		return c.do("POST", "/admin/users", c.admin, "", map[string]any{"email": email, "name": "S",
			"password": "fixture-staff-pass1", "roles": []string{"support"}}, 201)["id"].(string)
	}
	f.staff, f.staffDel = staff("staff@paygate.test"), staff("staff-del@paygate.test")
	f.staffRole = c.do("POST", "/admin/roles", c.admin, "", map[string]any{"name": "Fixture Staff Role", "permissions": []string{"ledger:read"}}, 201)["id"].(string)
	f.staffRoleDel = c.do("POST", "/admin/roles", c.admin, "", map[string]any{"name": "Disposable Staff", "permissions": []string{"ledger:read"}}, 201)["id"].(string)
	for _, r := range c.do("GET", "/v1/roles", f.owner, "", nil, 200)["data"].([]any) {
		if r.(map[string]any)["name"] == "analyst" {
			f.analystRole = r.(map[string]any)["id"].(string)
		}
	}
	for _, r := range c.do("GET", "/admin/roles", c.admin, "", nil, 200)["data"].([]any) {
		if r.(map[string]any)["name"] == "support" {
			f.supportRole = r.(map[string]any)["id"].(string)
		}
	}
	return f
}

// fill replaces the path parameters with fixture ids. Deletions get
// throwaway records so they never remove what other calls use.
func (f *fixtures) fill(method, path string) string {
	if method == "DELETE" {
		switch path {
		case "/v1/roles/{id}":
			return "/v1/roles/" + f.roleDel
		case "/admin/roles/{id}":
			return "/admin/roles/" + f.staffRoleDel
		}
	}
	id := map[string]string{
		"/v1/payments/{id}": f.payment, "/v1/refunds/{id}": f.refund, "/v1/payouts/{id}": f.payout,
		"/v1/webhook-endpoints/{id}": f.webhook, "/v1/webhook-endpoints/{id}/rotate-secret": f.webhook,
		"/v1/webhook-deliveries/{id}/retry": f.delivery,
		"/v1/api-keys/{id}":                 f.keyDel, "/v1/api-keys/{id}/roll": f.keyRoll,
		"/v1/team/members/{id}": f.memberDel, "/v1/team/members/{id}/roles": f.member,
		"/v1/team/members/{id}/roles/{roleId}": f.member,
		"/v1/roles/{id}":                       f.role, "/v1/roles/{id}/permissions": f.role, "/v1/roles/{id}/permissions/{code}": f.role,
		"/admin/merchants/{id}": f.merchant, "/admin/merchants/{id}/status": f.merchant,
		"/admin/merchants/{id}/api-keys": f.merchant, "/admin/merchants/{id}/api-keys/{keyId}": f.merchant,
		"/admin/users/{id}": f.staffDel, "/admin/users/{id}/roles": f.staff, "/admin/users/{id}/roles/{roleId}": f.staff,
		"/admin/roles/{id}": f.staffRole, "/admin/roles/{id}/permissions": f.staffRole,
		"/admin/roles/{id}/permissions/{code}": f.staffRole,
		"/admin/payouts/{id}":                  f.payout, "/admin/payouts/{id}/paid": f.payout, "/admin/payouts/{id}/failed": f.payout,
		"/admin/reconciliation/runs/{id}": f.run, "/pay/{id}": f.hosted,
	}
	out := path
	if v, ok := id[path]; ok {
		out = strings.Replace(out, "{id}", v, 1)
	}
	r := strings.NewReplacer("{roleId}", map[bool]string{true: f.supportRole, false: f.analystRole}[strings.HasPrefix(path, "/admin")],
		"{keyId}", f.keyAdminDel, "{code}", map[bool]string{true: "ledger:read", false: "refunds:write"}[strings.HasPrefix(path, "/admin")],
		"{txRef}", f.mockTx, "{provider}", "google")
	out = r.Replace(out)
	if path == "/providers/{provider}/callback" {
		out = "/providers/mock/callback"
	}
	return out
}

// callerFor picks the caller the docs say the endpoint is for.
func (f *fixtures) callerFor(method, path string, open bool) string {
	switch {
	case open:
		return ""
	case strings.HasPrefix(path, "/admin"):
		return f.c.admin
	case strings.HasPrefix(path, "/auth"), strings.HasPrefix(path, "/v1/api-keys"), strings.HasPrefix(path, "/v1/team"),
		strings.HasPrefix(path, "/v1/roles"), path == "/v1/permissions", path == "/v1/audit-log",
		strings.HasPrefix(path, "/v1/payouts"), path == "/v1/merchant":
		return f.owner
	}
	return f.key
}

type sent struct {
	status int
	body   []byte
}

func (s sent) errCode() (string, bool) {
	var e struct {
		Error struct{ Code string } `json:"error"`
	}
	if json.Unmarshal(s.body, &e) != nil || e.Error.Code == "" {
		return "", false
	}
	return e.Error.Code, true
}

var idem int

func (f *fixtures) send(method, path, token string, b body) sent {
	var rdr io.Reader
	ctype := "application/json"
	for media, content := range b.Content {
		ex := content.Example
		if ex == nil {
			continue
		}
		if m, ok := ex.(map[string]any); ok {
			if _, has := m["payment_id"]; has {
				m["payment_id"] = f.payment // the sample's id → a real payment
			}
			if _, has := m["refresh_token"]; has {
				m["refresh_token"] = f.refresh
			}
		}
		if media == "application/x-www-form-urlencoded" {
			form := url.Values{}
			for k, v := range ex.(map[string]any) {
				form.Set(k, fmt.Sprint(v))
			}
			rdr, ctype = strings.NewReader(form.Encode()), media
		} else {
			raw, _ := json.Marshal(ex)
			rdr = bytes.NewReader(raw)
		}
	}
	req, _ := http.NewRequest(method, f.c.base+path, rdr)
	req.Header.Set("Content-Type", ctype)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	idem++
	req.Header.Set("Idempotency-Key", fmt.Sprintf("swagger-sample-%d", idem))
	hc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := hc.Do(req)
	if err != nil {
		return sent{status: -1, body: []byte(err.Error())}
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return sent{status: resp.StatusCode, body: out}
}
