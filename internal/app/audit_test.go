package app_test

import (
	"encoding/json"
	"strings"
	"testing"

	"gorm.io/gorm"
)

// auditRows returns the entries a caller can see at path (all pages up to 200).
func (c *client) auditRows(token, path string) []map[string]any {
	c.t.Helper()
	var out []map[string]any
	for _, r := range c.do("GET", path, token, "", nil, 200)["data"].([]any) {
		out = append(out, r.(map[string]any))
	}
	return out
}

func hasAction(rows []map[string]any, action, actorKind string) bool {
	for _, r := range rows {
		if r["action"] == action && (actorKind == "" || r["actor_kind"] == actorKind) {
			return true
		}
	}
	return false
}

// TestAuditLog: every action is recorded; a merchant sees only its own
// business, staff see everything; secrets never reach the log; the log
// cannot be changed, and a change made behind its back is detected.
func TestAuditLog(t *testing.T) {
	c, _, stop := startApp(t, nil)
	defer stop()
	ownerA, keyA := c.signup("audit-a@shop.test")
	ownerB, keyB := c.signup("audit-b@shop.test")
	merchantA, merchantB := c.merchantID(ownerA), c.merchantID(ownerB)
	pid := c.pay(keyA, "aud-1", 50000)
	c.do("GET", "/v1/payments/"+pid, keyB, "", nil, 404) // B cannot see A's payment
	c.raw("POST", "/auth/login", "", "", map[string]any{"email": "audit-a@shop.test", "password": "a-wrong-password-1"})

	// A sees its own actions: the API key created by a user, the payment
	// created by the API key, with the payment's id.
	var mine []map[string]any
	c.eventually("A's actions recorded", func() bool {
		mine = c.auditRows(ownerA, "/v1/audit-logs?limit=200")
		return hasAction(mine, "api_keys.create", "user") && hasAction(mine, "payments.create", "api_key")
	})
	for _, r := range mine {
		if r["merchant_id"] != merchantA {
			t.Fatalf("A sees another business's entry: %v", r)
		}
		if r["action"] == "payments.create" && r["resource_id"] != pid {
			t.Fatalf("created payment id not recorded: %v", r)
		}
	}
	// Asking for B's log does not work: the merchant comes from the token.
	for _, r := range c.auditRows(ownerA, "/v1/audit-logs?merchant_id="+merchantB) {
		if r["merchant_id"] != merchantA {
			t.Fatalf("merchant_id filter must be ignored for merchants: %v", r)
		}
	}
	c.eventually("A sees the failed sign-in to its account (without the attacker named as A)", func() bool {
		for _, r := range c.auditRows(ownerA, "/v1/audit-logs?action=auth.login") {
			if r["outcome"] == "denied" && r["actor_kind"] == "anonymous" {
				return true
			}
		}
		return false
	})
	for _, r := range c.auditRows(ownerB, "/v1/audit-logs?limit=200") {
		if r["merchant_id"] != merchantB || r["resource_id"] == pid {
			t.Fatalf("B sees A's activity: %v", r)
		}
	}
	c.do("GET", "/v1/audit-logs", keyA, "", nil, 403) // API keys cannot read the log

	// Staff see everything and can filter by merchant.
	var all []map[string]any
	c.eventually("failed sign-in and B's 404 recorded", func() bool {
		all = c.auditRows(c.admin, "/admin/audit-logs?limit=200")
		seen := map[any]bool{}
		for _, r := range all {
			seen[r["merchant_id"]] = true
		}
		failed := false
		for _, r := range all {
			d, _ := r["details"].(map[string]any)
			if r["action"] == "auth.login" && r["outcome"] == "denied" && r["actor_kind"] == "anonymous" && d["email"] == "audit-a@shop.test" {
				failed = true
			}
		}
		return seen[merchantA] && seen[merchantB] && failed
	})
	for _, r := range c.auditRows(c.admin, "/admin/audit-logs?merchant_id="+merchantB) {
		if r["merchant_id"] != merchantB {
			t.Fatalf("staff merchant filter: %v", r)
		}
	}
	c.eventually("staff reading the log is itself recorded", func() bool {
		return hasAction(c.auditRows(c.admin, "/admin/audit-logs?actor_kind=staff&limit=200"), "admin.audit_logs.list", "staff")
	})
	c.do("GET", "/admin/audit-logs", ownerA, "", nil, 403) // merchants never see the platform log

	// No secrets: passwords, API keys and codes are never stored.
	db := openTestDB(t)
	var dump string
	if err := db.Raw("SELECT COALESCE(string_agg(row_to_json(a)::text, ' '), '') FROM audit.audit_logs a").Scan(&dump).Error; err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"owner-password-123", "a-wrong-password-1", keyA, keyB} {
		if strings.Contains(dump, secret) {
			t.Fatalf("a secret reached the audit log: %q", secret)
		}
	}

	// Append-only, and tampering is detected.
	v := c.do("GET", "/admin/audit-logs/verify", c.admin, "", nil, 200)
	if v["intact"] != true || v["checked"].(float64) < 5 {
		t.Fatalf("verify: %v", v)
	}
	if err := db.Exec("UPDATE audit.audit_logs SET outcome = 'success' WHERE outcome = 'denied'").Error; err == nil ||
		!strings.Contains(err.Error(), "append-only") {
		t.Fatalf("audit update must be refused: %v", err)
	}
	if err := db.Exec("DELETE FROM audit.audit_logs").Error; err == nil {
		t.Fatal("audit delete must be refused")
	}
	var victim int64
	if err := db.Raw("SELECT id FROM audit.audit_logs WHERE outcome = 'denied' ORDER BY id LIMIT 1").Scan(&victim).Error; err != nil || victim == 0 {
		t.Fatalf("no denied entry: %v", err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error { // someone with superuser rights edits a row
		if err := tx.Exec("SET LOCAL session_replication_role = replica").Error; err != nil {
			return err
		}
		return tx.Exec("UPDATE audit.audit_logs SET outcome = 'success' WHERE id = ?", victim).Error
	}); err != nil {
		t.Fatal(err)
	}
	v = c.do("GET", "/admin/audit-logs/verify", c.admin, "", nil, 200)
	if v["intact"] != false || v["broken_at"] != float64(victim) {
		b, _ := json.Marshal(v)
		t.Fatalf("tampering not detected at %d: %s", victim, b)
	}
}
