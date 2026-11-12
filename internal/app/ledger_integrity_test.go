package app_test

import (
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"paygate/internal/platform/config"
)

func (c *client) summary(env string) map[string]any {
	return c.do("GET", "/admin/ledger/summary?environment="+env+"&currency=ETB", c.admin, "", nil, 200)
}

// TestLedgerIntegrity: the books cannot be edited and every posting
// balances; refunds need the money; settlements and bank transfers reach the
// bank account; corrections need a second person.
func TestLedgerIntegrity(t *testing.T) {
	c, _, stop := startApp(t, func(cfg *config.Config) {
		cfg.BankAccounts = [][3]string{{"Commercial Bank of Ethiopia", "1000123456789", "Paygate PLC"}}
		cfg.BankCurrencies = []string{"ETB"}
		cfg.ProviderTestRoutes = map[string][]string{"bank_transfer": {"bank_transfer_test"}}
	})
	defer stop()
	owner, key := c.signup("ledger@shop.test")
	merchant := c.merchantID(owner)
	balance := func() float64 {
		return c.do("GET", "/v1/balance?currency=ETB&environment=test", owner, "", nil, 200)["available"].(float64)
	}
	pid := c.pay(key, "lg-1", 100000)
	c.eventually("captured", func() bool { return balance() == 98500 })

	// 1. The database protects the books.
	db := openTestDB(t)
	if err := db.Exec("UPDATE ledger.ledger_entries SET debit = debit WHERE id = (SELECT MIN(id) FROM ledger.ledger_entries)").Error; err == nil ||
		!strings.Contains(err.Error(), "append-only") {
		t.Fatalf("ledger update must be refused: %v", err)
	}
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`INSERT INTO ledger.ledger_transactions (id, reference, currency, description, created_at)
			VALUES ('7c1d0000-0000-4000-8000-000000000001', 'test:unbalanced', 'ETB', 'x', now())`).Error; err != nil {
			return err
		}
		return tx.Exec(`INSERT INTO ledger.ledger_entries (transaction_id, account, debit, credit, currency, created_at)
			VALUES ('7c1d0000-0000-4000-8000-000000000001', 'merchant:x:available:test', 100, 0, 'ETB', now())`).Error
	})
	if err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("an unbalanced posting must be refused at commit: %v", err)
	}

	// 2. A refund needs the money: 900.00 is held for a payout, 85.00 is left.
	bank := map[string]any{"bank_name": "Commercial Bank of Ethiopia", "account_number": "1000123456789", "account_name": "Shop PLC"}
	c.do("POST", "/v1/payouts", owner, "lg-po", map[string]any{"amount": 90000, "currency": "ETB", "environment": "test", "bank_account": bank}, 201)
	r := c.do("POST", "/v1/refunds", key, "lg-r1", map[string]any{"payment_id": pid, "amount": 50000}, 201)
	if r["status"] != "failed" || r["failure_reason"] != "insufficient_balance" {
		t.Fatalf("refund without the money: %v", r)
	}
	if balance() != 8500 {
		t.Fatalf("a refused refund must not move money: %v", balance())
	}
	// The payment's fee (15.00) is not refunded, so the balance may go that far below zero.
	r = c.do("POST", "/v1/refunds", key, "lg-r2", map[string]any{"payment_id": pid, "amount": 9000}, 201)
	if r["status"] != "succeeded" {
		t.Fatalf("refund within balance + fee: %v", r)
	}
	c.eventually("refund booked", func() bool { return balance() == -500 })
	if neg := c.summary("test")["negative_merchants"].([]any); len(neg) != 1 || neg[0].(map[string]any)["merchant_id"] != merchant {
		t.Fatalf("negative balance must be reported: %v", neg)
	}

	// 3. The provider's settlement reaches the bank: 1,000.00 settled, 20.00
	// fees, 975.00 deposited (5.00 unexplained).
	today := time.Now().In(mustLoc(t)).Format("2006-01-02")
	code, up := c.uploadSettlement("provider=mock&date="+today+"&deposit_amount=975.00&deposit_currency=ETB",
		"reference,type,amount,fee,currency\n"+pid+",payment,1000.00,20.00,ETB\n")
	if code != 202 {
		t.Fatalf("settlement upload: %d %v", code, up)
	}
	c.finishedRun(up["run"].(map[string]any)["id"].(string))
	var s map[string]any
	c.eventually("settlement booked", func() bool {
		s = c.summary("test")
		return s["cash_in_bank"] == float64(97500)
	})
	if s["provider_fees_paid"] != float64(2000) || s["settlement_differences"] != float64(-500) || s["balanced"] != true {
		t.Fatalf("settlement booking: %v", s)
	}
	if s["owed_by_providers"] != float64(-9000) { // the refund went out through the provider after it settled
		t.Fatalf("provider clearing: %v", s)
	}

	// 4. A bank transfer is cash in our bank as soon as finance confirms it.
	b := c.do("POST", "/v1/payments", key, "lg-bt", map[string]any{"amount": 125000, "currency": "ETB",
		"payment_method": "bank_transfer", "reference": "LG-BT"}, 201)
	c.do("POST", "/admin/bank-transfers/"+b["id"].(string)+"/confirm", c.admin, "", map[string]any{"amount": 125000, "bank_reference": "FT1"}, 200)
	c.eventually("bank transfer in the bank", func() bool { return c.summary("test")["cash_in_bank"] == float64(97500+125000) })

	// 5. Corrections: requested by one person, approved by another.
	adj := c.do("POST", "/admin/ledger/adjustments", c.admin, "", map[string]any{"merchant_id": merchant, "environment": "test",
		"currency": "ETB", "direction": "credit", "amount": 1500, "reason": "fee charged twice, ticket 482"}, 201)
	if adj["status"] != "pending_approval" {
		t.Fatalf("adjustment: %v", adj)
	}
	before := balance()
	c.do("POST", "/admin/ledger/adjustments/"+adj["id"].(string)+"/approve", c.admin, "", nil, 403) // not your own
	c.do("POST", "/admin/ledger/adjustments", c.admin, "", map[string]any{"merchant_id": merchant, "environment": "test",
		"currency": "ETB", "direction": "credit", "amount": 1500, "reason": "short"}, 400)
	c.do("POST", "/admin/users", c.admin, "", map[string]any{"email": "approver@paygate.test", "name": "Approver",
		"password": "approver-password-1", "roles": []string{"finance"}}, 201)
	approver := c.login("approver@paygate.test", "approver-password-1", 200)["access_token"].(string)
	done := c.do("POST", "/admin/ledger/adjustments/"+adj["id"].(string)+"/approve", approver, "", map[string]any{"note": "checked"}, 200)
	if done["status"] != "approved" || balance() != before+1500 {
		t.Fatalf("approved correction: %v, balance %v", done, balance())
	}
	c.do("POST", "/admin/ledger/adjustments/"+adj["id"].(string)+"/reject", approver, "", nil, 409)
	c.do("POST", "/admin/ledger/adjustments/not-an-id/approve", approver, "", nil, 404)

	if s := c.summary("test"); s["balanced"] != true || s["corrections"] != float64(1500) {
		t.Fatalf("summary at the end: %v", s)
	}
	for _, row := range c.do("GET", "/admin/ledger/trial-balance", c.admin, "", nil, 200)["data"].([]any) {
		if r := row.(map[string]any); r["debits"] != r["credits"] {
			t.Fatalf("trial balance: %v", r)
		}
	}
}
