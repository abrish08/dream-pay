package app_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"paygate/internal/platform/config"
)

// uploadSettlement sends a settlement CSV.
func (c *client) uploadSettlement(query, csv string) (int, map[string]any) {
	c.t.Helper()
	req, _ := http.NewRequest("POST", c.base+"/admin/reconciliation/settlements?"+query, bytes.NewBufferString(csv))
	req.Header.Set("Authorization", "Bearer "+c.admin)
	req.Header.Set("Content-Type", "text/csv")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func (c *client) finishedRun(id string) map[string]any {
	c.t.Helper()
	var run map[string]any
	c.eventually("run "+id+" finished", func() bool {
		run = c.do("GET", "/admin/reconciliation/runs/"+id, c.admin, "", nil, 200)
		return run["status"] == "completed" || run["status"] == "failed"
	})
	if run["status"] != "completed" {
		c.t.Fatalf("run failed: %v", run)
	}
	return run
}

func issues(run map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	items, _ := run["items"].([]any)
	for _, it := range items {
		m := it.(map[string]any)
		out[m["issue"].(string)+":"+m["reference"].(string)] = m
	}
	return out
}

// TestReconciliation: our payments, the provider and the ledger must agree;
// the settlement file must match what we captured; differences become a work
// queue that is repaired automatically where safe, or resolved by finance.
func TestReconciliation(t *testing.T) {
	c, _, stop := startApp(t, func(cfg *config.Config) { cfg.ReconciliationSettleWait = time.Millisecond })
	defer stop()
	_, key := c.signup("recon@shop.test")
	a, b, cc, d := c.pay(key, "rc-a", 10000), c.pay(key, "rc-b", 20000), c.pay(key, "rc-c", 30000), c.pay(key, "rc-d", 40000)
	fee := func(id string) float64 { return c.do("GET", "/v1/payments/"+id, key, "", nil, 200)["fee"].(float64) }
	want := 100000 - fee(a) - fee(b) - fee(cc) - fee(d)
	c.eventually("ledger booked all four", func() bool {
		return c.do("GET", "/v1/balance?currency=ETB", key, "", nil, 200)["available"] == want
	})

	// Real-world faults: D's success was lost (we say failed, the customer
	// was charged); C's capture never reached the ledger.
	db := openTestDB(t)
	if err := db.Exec("UPDATE payment.payments SET status = 'failed', failure_reason = 'timeout' WHERE id = ?", d).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`DELETE FROM ledger.ledger_entries WHERE transaction_id IN
		(SELECT id FROM ledger.ledger_transactions WHERE reference = ?)`, "payment:"+cc+":captured").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("DELETE FROM ledger.ledger_transactions WHERE reference = ?", "payment:"+cc+":captured").Error; err != nil {
		t.Fatal(err)
	}

	// 1. Payments vs provider vs ledger.
	run := c.do("POST", "/admin/reconciliation/runs", c.admin, "", map[string]any{
		"from": time.Now().Add(-time.Hour).UTC(), "to": time.Now().Add(time.Hour).UTC()}, 202)
	run = c.finishedRun(run["id"].(string))
	found := issues(run)
	if it := found["status_mismatch:"+d]; it == nil || it["status"] != "auto_resolved" || it["severity"] != "critical" {
		t.Fatalf("lost success must be repaired: %v", run["items"])
	}
	if it := found["ledger_missing:"+cc]; it == nil || it["status"] != "open" || it["severity"] != "critical" {
		t.Fatalf("missing ledger posting: %v", run["items"])
	}
	if len(found) != 2 {
		t.Fatalf("unexpected differences: %v", run["items"])
	}
	sum := run["summary"].(map[string]any)
	if sum["critical"] != float64(1) || sum["auto_resolved"] != float64(1) || sum["captured"].(map[string]any)["ETB"] == nil {
		t.Fatalf("summary: %v", sum)
	}
	// The repair is in D's history, with who did it.
	if got := trail(c.events(key, "/v1/payments/"+d+"/events")); !strings.HasSuffix(got, "failed>succeeded:reconciliation") {
		t.Fatalf("repair not recorded: %s", got)
	}
	// Found again tomorrow: not reported twice.
	again := c.finishedRun(c.do("POST", "/admin/reconciliation/runs", c.admin, "", map[string]any{
		"from": time.Now().Add(-time.Hour).UTC(), "to": time.Now().Add(time.Hour).UTC()}, 202)["id"].(string))
	if len(issues(again)) != 0 || again["mismatched"] != float64(1) {
		t.Fatalf("open difference duplicated: %v", again)
	}

	// 2. Settlement file for today: A correct, B wrong amount, an unknown
	// payment, C missing, D correct, A's refund correct, a refund we never
	// made; the bank paid in less than the net.
	rf := c.do("POST", "/v1/refunds", key, "rc-refund-a", map[string]any{"payment_id": a, "amount": 2000}, 201)
	if rf["status"] != "succeeded" {
		t.Fatalf("refund: %v", rf)
	}
	today := time.Now().In(mustLoc(t)).Format("2006-01-02")
	csv := "reference,type,amount,fee,currency,settled_at\n" +
		rf["id"].(string) + ",refund,20.00,0,ETB," + today + "\n" +
		"6b7f0000-0000-4000-8000-000000000002,refund,5.00,0,ETB," + today + "\n" +
		a + ",payment,100.00,1.50,ETB," + today + "\n" +
		b + ",payment,150.00,2.25,ETB," + today + "\n" +
		"6b7f0000-0000-4000-8000-000000000001,payment,75.00,0,ETB," + today + "\n" +
		d + ",payment,400.00,6.00,ETB," + today + "\n"
	code, up := c.uploadSettlement("provider=mock&date="+today+"&deposit_amount=600.00&deposit_currency=ETB", csv)
	if code != 202 {
		t.Fatalf("upload: %d %v", code, up)
	}
	srun := c.finishedRun(up["run"].(map[string]any)["id"].(string))
	found = issues(srun)
	for _, k := range []string{"settlement_amount_mismatch:" + b, "settlement_unknown:6b7f0000-0000-4000-8000-000000000001",
		"settlement_missing:" + cc, "settlement_unknown:6b7f0000-0000-4000-8000-000000000002"} {
		if found[k] == nil {
			t.Errorf("settlement difference %s not found: %v", k, srun["items"])
		}
	}
	var deposit map[string]any
	for k, v := range found {
		if strings.HasPrefix(k, "deposit_mismatch:") {
			deposit = v
		}
	}
	if deposit == nil || deposit["actual"] != "ETB 600.00" || len(found) != 5 {
		t.Fatalf("deposit check: %v", srun["items"])
	}
	settled := srun["summary"].(map[string]any)["settled"].(map[string]any)["ETB"].(map[string]any)
	if settled["amount"] != float64(72500) || settled["refunded"] != float64(2500) || settled["fees"] != float64(975) ||
		settled["net"] != float64(69025) {
		t.Fatalf("settled totals: %v", settled)
	}
	if code, _ := c.uploadSettlement("provider=mock&date="+today, csv); code != 409 {
		t.Fatalf("same file counted twice: %d", code)
	}
	if code, e := c.uploadSettlement("provider=mock&date="+today, "reference,amount,currency\n"+a+",abc,ETB\n,10.00,XYZ\n"); code != 400 ||
		!strings.Contains(fmt.Sprint(e), "line 2") || !strings.Contains(fmt.Sprint(e), "line 3") {
		t.Fatalf("bad file must be refused with its line numbers: %d %v", code, e)
	}

	// 3. C settles in the next day's report: the open "missing" item closes itself.
	tomorrow := time.Now().In(mustLoc(t)).AddDate(0, 0, 1).Format("2006-01-02")
	_, up = c.uploadSettlement("provider=mock&date="+tomorrow, "reference,amount,currency\n"+cc+",300.00,ETB\n")
	next := c.finishedRun(up["run"].(map[string]any)["id"].(string))
	if next["summary"].(map[string]any)["carried_over_closed"] != float64(1) {
		t.Fatalf("carried-over item not closed: %v", next["summary"])
	}

	// 4. The work queue: finance resolves what needs a person.
	queue := c.do("GET", "/admin/reconciliation/items?status=open&severity=critical", c.admin, "", nil, 200)["data"].([]any)
	if len(queue) != 5 { // ledger_missing, amount mismatch, unknown payment, unknown refund, deposit
		t.Fatalf("critical queue: %v", queue)
	}
	id := fmt.Sprint(int64(found["settlement_amount_mismatch:"+b]["id"].(float64)))
	c.do("POST", "/admin/reconciliation/items/"+id+"/resolve", c.admin, "", map[string]any{"note": "ok"}, 400)
	res := c.do("POST", "/admin/reconciliation/items/"+id+"/resolve", c.admin, "", map[string]any{
		"note": "provider confirmed a partial capture; adjustment raised"}, 200)
	if res["status"] != "resolved" || res["resolved_by"] == "" {
		t.Fatalf("resolve: %v", res)
	}
	c.do("POST", "/admin/reconciliation/items/"+id+"/resolve", c.admin, "", map[string]any{"note": "again please"}, 409)
	if n := len(c.do("GET", "/admin/reconciliation/items?status=open&severity=critical", c.admin, "", nil, 200)["data"].([]any)); n != 4 {
		t.Fatalf("queue after resolving: %d", n)
	}
	if len(c.do("GET", "/admin/reconciliation/settlements", c.admin, "", nil, 200)["data"].([]any)) != 2 {
		t.Fatal("settlements list")
	}
}

func mustLoc(t *testing.T) *time.Location {
	loc, err := time.LoadLocation("Africa/Addis_Ababa")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}
