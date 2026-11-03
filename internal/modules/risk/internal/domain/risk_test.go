package domain

import (
	"testing"

	"paygate/internal/contracts/risk"
)

func TestDecide(t *testing.T) {
	th := DefaultThresholds
	if d := Decide(Signals{CustomerLast10Min: 4}, th); d.Action != risk.ActionAllow {
		t.Fatalf("4 attempts: %+v", d)
	}
	if d := Decide(Signals{CustomerLast10Min: 5}, th); d.Action != risk.ActionBlock {
		t.Fatalf("5 attempts before this one: %+v", d)
	}
	if d := Decide(Signals{CustomerLast10Min: 2, RiskLevel: "high"}, th); d.Action != risk.ActionBlock {
		t.Fatalf("high risk halves the limits: %+v", d)
	}
	if d := Decide(Signals{CustomerDeclinesHour: 3}, th); d.Action != risk.ActionAllow || d.Score != 60 {
		t.Fatalf("3 declines: %+v", d)
	}
	if d := Decide(Signals{CustomerDeclinesHour: 5}, th); d.Action != risk.ActionBlock {
		t.Fatalf("5 declines: %+v", d)
	}
	if d := Decide(Signals{MerchantCount: 50, MerchantAverage: 1000, Amount: 20000}, th); d.Score != 40 || d.Action != risk.ActionAllow {
		t.Fatalf("unusual amount: %+v", d)
	}
	if d := Decide(Signals{Blocklisted: []string{"ip"}}, th); d.Action != risk.ActionBlock || d.Reasons[0] != "blocklisted_ip" {
		t.Fatalf("blocklist: %+v", d)
	}
}

func TestNormalize(t *testing.T) {
	for kind, cases := range map[string][2]string{
		KindEmail: {" Abebe@Example.COM ", "abebe@example.com"},
		KindPhone: {"0911 23-45-67", "+251911234567"},
		KindIP:    {"::ffff:196.188.1.2", "196.188.1.2"},
	} {
		if got, err := Normalize(kind, cases[0]); err != nil || got != cases[1] {
			t.Errorf("%s %q: %q %v", kind, cases[0], got, err)
		}
	}
	if _, err := Normalize("card", "4111"); err == nil {
		t.Fatal("unknown kind accepted")
	}
}
