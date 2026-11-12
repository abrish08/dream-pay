package app_test

import (
	"encoding/csv"
	"io"
	"net/http"
	"strings"
	"testing"
)

func (c *client) csv(token, path string) (int, [][]string) {
	c.t.Helper()
	req, _ := http.NewRequest("GET", c.base+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode, nil
	}
	rows, err := csv.NewReader(resp.Body).ReadAll()
	if err != nil {
		c.t.Fatal(err)
	}
	return resp.StatusCode, rows
}

// TestReports: CSV exports of payments and of the balance statement, safe
// to open in a spreadsheet.
func TestReports(t *testing.T) {
	c, _, stop := startApp(t, nil)
	defer stop()
	_, key := c.signup("reports@shop.test")
	p := c.do("POST", "/v1/payments", key, "rep-1", map[string]any{"amount": 100000, "currency": "ETB", "provider": "mock",
		"reference": "R-1", "description": `=HYPERLINK("http://evil","click")`}, 201)
	c.checkout(p["checkout_url"].(string), true)
	pid := p["id"].(string)
	c.pay(key, "rep-2", 50000)
	c.eventually("both captured", func() bool {
		return c.do("GET", "/v1/balance?currency=ETB", key, "", nil, 200)["available"] == float64(98500+49250)
	})
	c.do("POST", "/v1/refunds", key, "rep-r", map[string]any{"payment_id": pid, "amount": 10000}, 201)
	c.eventually("refund booked", func() bool {
		return c.do("GET", "/v1/balance?currency=ETB", key, "", nil, 200)["available"] == float64(98500+49250-10000) &&
			c.do("GET", "/v1/payments/"+pid, key, "", nil, 200)["refunded_amount"] == float64(10000)
	})

	code, rows := c.csv(key, "/v1/reports/payments.csv")
	if code != 200 || len(rows) != 3 || rows[0][0] != "id" {
		t.Fatalf("payments csv: %d %v", code, rows)
	}
	var mine []string
	for _, r := range rows[1:] {
		if r[0] == pid {
			mine = r
		}
	}
	if mine == nil || mine[5] != "1000.00" || mine[7] != "100.00" || !strings.HasPrefix(mine[14], `'=HYPERLINK`) {
		t.Fatalf("payment row (formula must be neutralised): %v", mine)
	}
	if code, _ := c.csv(key, "/v1/reports/payments.csv?from=2026-01-01&to=2026-12-31"); code != 400 {
		t.Fatalf("period over 93 days: %d", code)
	}

	code, rows = c.csv(key, "/v1/reports/statement.csv?currency=ETB")
	if code != 200 || rows[1][1] != "opening" || rows[1][5] != "0.00" || len(rows) != 5 {
		t.Fatalf("statement: %d %v", code, rows)
	}
	last := rows[len(rows)-1]
	if last[5] != "1377.50" || last[4] != "100.00" || !strings.HasPrefix(last[1], "refund:") {
		t.Fatalf("closing line: %v", last)
	}
}
