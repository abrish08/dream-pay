package domain

import (
	"strings"
	"testing"
	"time"

	"paygate/internal/contracts/payments"
	"paygate/internal/contracts/providers"
)

func TestBusinessDayIsLocal(t *testing.T) {
	loc, _ := time.LoadLocation("Africa/Addis_Ababa") // UTC+3
	// 01:30 on 3 Nov in Addis Ababa is still 2 Nov in UTC.
	now := time.Date(2026, 11, 3, 1, 30, 0, 0, loc)
	from, to := BusinessDay(now, loc)
	if !from.Equal(time.Date(2026, 11, 1, 21, 0, 0, 0, time.UTC)) || !to.Equal(time.Date(2026, 11, 2, 21, 0, 0, 0, time.UTC)) {
		t.Fatalf("2 Nov in Addis Ababa: %s .. %s", from, to)
	}
}

func TestCompare(t *testing.T) {
	p := payments.Payment{Status: payments.StatusFailed, Amount: 5000, Currency: "ETB"}
	if issue, _, _ := Compare(p, providers.Transaction{Status: providers.StatusSucceeded, Amount: 5000, Currency: "ETB"}); issue != IssueStatusMismatch {
		t.Fatal("charged but failed")
	}
	p.Status = payments.StatusExpired
	if issue, _, _ := Compare(p, providers.Transaction{Status: providers.StatusSucceeded, Amount: 5000, Currency: "ETB"}); issue != IssueStatusMismatch {
		t.Fatal("charged but expired")
	}
	p.Status = payments.StatusSucceeded
	if issue, exp, act := Compare(p, providers.Transaction{Status: providers.StatusSucceeded, Amount: 4000, Currency: "ETB"}); issue != IssueAmountMismatch ||
		exp != "ETB 50.00" || act != "ETB 40.00" {
		t.Fatalf("amount: %s %s %s", issue, exp, act)
	}
	if issue, _, _ := Compare(p, providers.Transaction{Status: providers.StatusSucceeded, Amount: 5000, Currency: "ETB"}); issue != "" {
		t.Fatal("matching payment reported")
	}
}

func TestParseSettlementCSV(t *testing.T) {
	lines, err := ParseSettlementCSV([]byte("\ufeffReference, Amount ,Currency,type,fee,settled_at\n" +
		"p1,50.00,etb,payment,0.75,2026-11-02\np2,10,USD,refund,,2026-11-02T10:00:00Z\n"))
	if err != nil || len(lines) != 2 || lines[0].Amount != 5000 || lines[0].Fee != 75 || lines[0].Currency != "ETB" ||
		lines[1].Kind != "refund" || lines[1].Amount != 1000 || lines[1].SettledAt == nil {
		t.Fatalf("parse: %+v %v", lines, err)
	}
	for name, bad := range map[string]string{
		"missing column": "reference,amount\np1,1.00\n",
		"bad amount":     "reference,amount,currency\np1,-5,ETB\n",
		"bad currency":   "reference,amount,currency\np1,5.00,EUR\n",
		"bad type":       "reference,amount,currency,type\np1,5.00,ETB,chargeback\n",
		"three decimals": "reference,amount,currency\np1,5.001,ETB\n",
		"empty":          "reference,amount,currency\n",
	} {
		if _, err := ParseSettlementCSV([]byte(bad)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	_, err = ParseSettlementCSV([]byte("reference,amount,currency\n,1.00,ETB\np2,x,ETB\n"))
	if err == nil || !strings.Contains(err.Error(), "line 2") || !strings.Contains(err.Error(), "line 3") {
		t.Fatalf("every bad line must be named: %v", err)
	}
}
