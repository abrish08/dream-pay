// Package money: amounts are always int64 minor units (cents / santim).
// Conversions to decimal strings happen only at provider boundaries.
package money

import (
	"fmt"
	"strconv"
	"strings"
)

// MaxAmount keeps fee arithmetic (amount * bps) far from int64 overflow.
const MaxAmount int64 = 1_000_000_000_000

var supported = map[string]bool{"ETB": true, "USD": true}

func Supported(currency string) bool { return supported[currency] }

// FormatMajor turns 150050 into "1500.50" (2-decimal currencies).
func FormatMajor(minor int64) string {
	sign := ""
	if minor < 0 {
		sign, minor = "-", -minor
	}
	return fmt.Sprintf("%s%d.%02d", sign, minor/100, minor%100)
}

// ParseMajor turns "1500.5" or "1500.50" or "1500" into 150050.
func ParseMajor(s string) (int64, error) {
	s = strings.TrimSpace(s)
	whole, frac, _ := strings.Cut(s, ".")
	if len(frac) > 2 {
		return 0, fmt.Errorf("too many decimals in %q", s)
	}
	frac += strings.Repeat("0", 2-len(frac))
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid amount %q", s)
	}
	f, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid amount %q", s)
	}
	return w*100 + f, nil
}
