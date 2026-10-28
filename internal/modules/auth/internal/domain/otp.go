package domain

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6238 TOTP uses HMAC-SHA1; authenticator apps expect it
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math/big"
	"net/url"
	"regexp"
	"strings"
	"time"

	"paygate/internal/platform/apperror"
)

// ---- 6-digit verification codes (email / SMS) ----

const (
	CodeTTL         = 15 * time.Minute
	CodeMaxAttempts = 5
	CodeCooldown    = 60 * time.Second // between two sends
	CodesPerHour    = 5
)

// NewCode returns a uniformly random 6-digit code.
func NewCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// HashCode binds the code to the user with a server-side pepper, so a
// database leak does not let anyone test the (only 10^6) possible codes.
func HashCode(pepper []byte, userID, code string) string {
	m := hmac.New(sha256.New, pepper)
	m.Write([]byte(userID + ":" + code))
	return hex.EncodeToString(m.Sum(nil))
}

func SameHash(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

var phoneRe = regexp.MustCompile(`^\+[1-9][0-9]{7,14}$`)

// NormalizePhone accepts E.164 (+251911000000); "" means no phone.
func NormalizePhone(p string) (string, error) {
	p = strings.NewReplacer(" ", "", "-", "", "(", "", ")", "").Replace(strings.TrimSpace(p))
	if p == "" {
		return "", nil
	}
	if strings.HasPrefix(p, "09") || strings.HasPrefix(p, "07") { // local Ethiopian mobile
		p = "+251" + p[1:]
	}
	if !phoneRe.MatchString(p) {
		return "", apperror.Invalid("invalid_phone", "phone must be in international format, e.g. +251911000000")
	}
	return p, nil
}

// ---- Authenticator app (TOTP, RFC 6238): Microsoft / Google Authenticator ----

const (
	totpPeriod = 30
	totpDigits = 6
	totpSkew   = 1 // accept the previous and next 30 s window (clock drift)
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

func NewTOTPSecret() (string, error) {
	b := make([]byte, 20) // 160 bits, as RFC 4226 recommends
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return b32.EncodeToString(b), nil
}

// OTPAuthURL is what the QR code encodes; authenticator apps scan it.
func OTPAuthURL(issuer, account, secret string) string {
	label := url.PathEscape(issuer + ":" + account)
	q := url.Values{"secret": {secret}, "issuer": {issuer}, "algorithm": {"SHA1"},
		"digits": {fmt.Sprint(totpDigits)}, "period": {fmt.Sprint(totpPeriod)}}
	return "otpauth://totp/" + label + "?" + q.Encode()
}

func TOTPCode(secret string, step int64) (string, error) {
	key, err := b32.DecodeString(strings.ToUpper(secret))
	if err != nil {
		return "", err
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	m := hmac.New(sha1.New, key)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", totpDigits, v%1_000_000), nil
}

// VerifyTOTP checks a code against now ± skew. A code (time step) already
// used is refused, so an observed code cannot be replayed.
func VerifyTOTP(secret, code string, now time.Time, lastStep int64) (int64, bool) {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != totpDigits {
		return 0, false
	}
	cur := now.Unix() / totpPeriod
	for d := int64(-totpSkew); d <= totpSkew; d++ {
		step := cur + d
		if step <= lastStep {
			continue
		}
		want, err := TOTPCode(secret, step)
		if err == nil && subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			return step, true
		}
	}
	return 0, false
}

// NewRecoveryCodes returns n one-time codes like "k7m2-9xqd".
func NewRecoveryCodes(n int) ([]string, error) {
	const alphabet = "abcdefghjkmnpqrstuvwxyz23456789"
	out := make([]string, n)
	for i := range out {
		var sb strings.Builder
		for j := 0; j < 8; j++ {
			if j == 4 {
				sb.WriteByte('-')
			}
			k, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
			if err != nil {
				return nil, err
			}
			sb.WriteByte(alphabet[k.Int64()])
		}
		out[i] = sb.String()
	}
	return out, nil
}

func NormalizeRecoveryCode(c string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(c), " ", ""))
}
