// Package domain: fraud rules. Pure functions over counts; no database.
package domain

import (
	"net/mail"
	"net/netip"
	"strings"

	"paygate/internal/contracts/merchants"
	"paygate/internal/contracts/risk"
	"paygate/internal/platform/apperror"
)

const BlockScore = 100

const (
	KindEmail = "email"
	KindPhone = "phone"
	KindIP    = "ip"
)

// Thresholds of the velocity rules (normal risk merchants; high risk halves them).
type Thresholds struct {
	CustomerPer10Min int // attempts by one email or phone
	CustomerPerDay   int
	IPPer10Min       int
	DeclinesPerHour  int // failed payments of one customer (card testing)
	UnusualFactor    int // amount > factor x the merchant's 30-day average
	UnusualMinCount  int // ...once the merchant has this many payments
}

var DefaultThresholds = Thresholds{CustomerPer10Min: 5, CustomerPerDay: 20, IPPer10Min: 10, DeclinesPerHour: 3,
	UnusualFactor: 10, UnusualMinCount: 20}

func (t Thresholds) For(level string) Thresholds {
	if level != merchants.RiskHigh {
		return t
	}
	half := func(n int) int { return max(1, n/2) }
	return Thresholds{CustomerPer10Min: half(t.CustomerPer10Min), CustomerPerDay: half(t.CustomerPerDay),
		IPPer10Min: half(t.IPPer10Min), DeclinesPerHour: half(t.DeclinesPerHour), UnusualFactor: half(t.UnusualFactor),
		UnusualMinCount: t.UnusualMinCount}
}

// Signals are the facts the rules look at (counted before this attempt).
type Signals struct {
	Blocklisted          []string // kinds matched
	CustomerLast10Min    int
	CustomerLastDay      int
	IPLast10Min          int
	CustomerDeclinesHour int
	MerchantAverage      int64 // average amount of the last 30 days (same currency)
	MerchantCount        int
	Amount               int64
	RiskLevel            string
}

// Decide applies the rules. Each rule adds to the score; 100 blocks.
func Decide(s Signals, t Thresholds) risk.Decision {
	t = t.For(s.RiskLevel)
	d := risk.Decision{Action: risk.ActionAllow}
	add := func(points int, reason string) {
		d.Score += points
		d.Reasons = append(d.Reasons, reason)
	}
	for _, k := range s.Blocklisted {
		add(BlockScore, "blocklisted_"+k)
	}
	if s.CustomerLast10Min >= t.CustomerPer10Min || s.CustomerLastDay >= t.CustomerPerDay {
		add(BlockScore, "velocity_customer")
	}
	if s.IPLast10Min >= t.IPPer10Min {
		add(BlockScore, "velocity_ip")
	}
	switch {
	case s.CustomerDeclinesHour >= t.DeclinesPerHour+2:
		add(BlockScore, "repeated_declines")
	case s.CustomerDeclinesHour >= t.DeclinesPerHour:
		add(60, "repeated_declines")
	}
	if s.MerchantCount >= t.UnusualMinCount && s.MerchantAverage > 0 && s.Amount > int64(t.UnusualFactor)*s.MerchantAverage {
		add(40, "unusual_amount")
	}
	if s.RiskLevel == merchants.RiskHigh {
		add(20, "high_risk_merchant")
	}
	if d.Score >= BlockScore {
		d.Action = risk.ActionBlock
	}
	return d
}

// Normalize returns the canonical form of a blocklist value.
func Normalize(kind, value string) (string, error) {
	v := strings.TrimSpace(value)
	switch kind {
	case KindEmail:
		a, err := mail.ParseAddress(v)
		if err != nil {
			return "", apperror.Invalid("invalid_value", "not a valid email")
		}
		return strings.ToLower(a.Address), nil
	case KindPhone:
		v = strings.NewReplacer(" ", "", "-", "", "(", "", ")", "").Replace(v)
		if strings.HasPrefix(v, "0") && len(v) == 10 { // 0911... -> +251911...
			v = "+251" + v[1:]
		}
		if len(v) < 8 || len(v) > 16 || strings.Trim(v, "+0123456789") != "" {
			return "", apperror.Invalid("invalid_value", "not a valid phone number")
		}
		return v, nil
	case KindIP:
		a, err := netip.ParseAddr(v)
		if err != nil {
			return "", apperror.Invalid("invalid_value", "not a valid IP address")
		}
		return a.Unmap().String(), nil
	}
	return "", apperror.Invalid("invalid_kind", "kind must be email, phone or ip")
}

// NormalizeOrEmpty is Normalize for optional customer data (bad -> "").
func NormalizeOrEmpty(kind, value string) string {
	if value == "" {
		return ""
	}
	v, err := Normalize(kind, value)
	if err != nil {
		return ""
	}
	return v
}
