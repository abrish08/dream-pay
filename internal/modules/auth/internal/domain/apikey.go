package domain

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"hash/crc32"
	"math/big"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"paygate/internal/platform/apperror"
)

// API key format:
//
//		sk_<env>_<43 base62 chars: 256 random bits><6 base62 chars: CRC32 checksum>
//		sk_live_3kTq0Zm...9fQ2xW
//
//	  - 256 bits from crypto/rand: cannot be guessed or brute-forced.
//	  - A recognisable prefix lets secret scanners (GitHub, GitLab) and log
//	    filters spot leaked keys, and tells test and live keys apart at a glance.
//	  - The checksum rejects typos and random strings without touching the database.
//	  - Base62 only: the key selects with a double-click and needs no URL escaping.
const (
	bodyLen     = 43
	checksumLen = 6
	MaxKeys     = 25 // active keys per merchant
)

var keyRe = regexp.MustCompile(`^sk_(live|test)_[0-9A-Za-z]{49}$`)

// Pepper is a server-side secret (from a secret manager) mixed into every key
// hash. Versions allow rotating it without invalidating existing keys.
type Pepper struct {
	Version int
	Secret  []byte
}

type APIKey struct {
	ID            string
	MerchantID    string
	Name          string
	Prefix        string // "sk_live_3kTq", safe to show
	Last4         string // safe to show
	Hash          string // HMAC-SHA256(pepper, raw key); the raw key is never stored
	PepperVersion int
	Environment   string
	Scopes        []string
	ExpiresAt     *time.Time
	RevokedAt     *time.Time
	LastUsedAt    *time.Time
	CreatedBy     string
	CreatedAt     time.Time
}

// HashKey: HMAC-SHA256 with a pepper. A fast keyed hash is the right tool for
// 256-bit random keys. Slow password hashes (bcrypt, argon2) protect
// low-entropy secrets; here they would only add latency to every API call.
// Encryption would be wrong: keys never need to be read back, and anything
// that can be decrypted can leak.
func HashKey(raw string, p Pepper) string {
	m := hmac.New(sha256.New, p.Secret)
	m.Write([]byte(raw))
	return hex.EncodeToString(m.Sum(nil))
}

// WellFormed checks format and checksum (no database access).
func WellFormed(raw string) bool {
	if !keyRe.MatchString(raw) {
		return false
	}
	n := len(raw)
	return checksum(raw[:n-checksumLen]) == raw[n-checksumLen:]
}

type NewKeyInput struct {
	MerchantID    string
	Name          string
	Environment   string
	Scopes        []string
	ExpiresInDays int
	CreatedBy     string
	// CreatorPermissions: a key can never hold a permission its creator lacks.
	CreatorPermissions []string
	// AllowedScopes: permissions services marked as usable by API keys.
	AllowedScopes []string
}

// NewKey returns the key record and the raw secret, which is shown only once.
func NewKey(in NewKeyInput, p Pepper, now time.Time) (*APIKey, string, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" || len(name) > 100 {
		return nil, "", apperror.Invalid("invalid_name", "name is required (max 100 characters)")
	}
	if in.Environment != "live" && in.Environment != "test" {
		return nil, "", apperror.Invalid("invalid_environment", "environment must be live or test")
	}
	if len(in.Scopes) == 0 {
		return nil, "", apperror.Invalid("invalid_scopes", "at least one scope is required")
	}
	for _, s := range in.Scopes {
		if !slices.Contains(in.AllowedScopes, s) {
			return nil, "", apperror.Invalid("invalid_scopes", "scope "+s+" cannot be given to an API key")
		}
		if !slices.Contains(in.CreatorPermissions, s) {
			return nil, "", apperror.Forbidden("scope_not_held", "you cannot grant "+s+": your role does not have it")
		}
	}
	if in.ExpiresInDays < 0 || in.ExpiresInDays > 3650 {
		return nil, "", apperror.Invalid("invalid_expiry", "expires_in_days must be between 0 and 3650")
	}

	raw, err := generate(in.Environment)
	if err != nil {
		return nil, "", err
	}
	k := &APIKey{
		ID: uuid.NewString(), MerchantID: in.MerchantID, Name: name,
		Prefix: raw[:len("sk_"+in.Environment+"_")+4], Last4: raw[len(raw)-4:],
		Hash: HashKey(raw, p), PepperVersion: p.Version, Environment: in.Environment,
		Scopes: slices.Compact(slices.Sorted(slices.Values(in.Scopes))), CreatedBy: in.CreatedBy, CreatedAt: now,
	}
	if in.ExpiresInDays > 0 {
		exp := now.AddDate(0, 0, in.ExpiresInDays)
		k.ExpiresAt = &exp
	}
	return k, raw, nil
}

func (k *APIKey) Usable(now time.Time) bool {
	if k.RevokedAt != nil {
		return false
	}
	return k.ExpiresAt == nil || now.Before(*k.ExpiresAt)
}

func generate(env string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	body := "sk_" + env + "_" + base62(new(big.Int).SetBytes(b), bodyLen)
	return body + checksum(body), nil
}

func checksum(s string) string {
	return base62(new(big.Int).SetUint64(uint64(crc32.ChecksumIEEE([]byte(s)))), checksumLen)
}

const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// base62 encodes n as exactly width characters (left-padded with '0').
func base62(n *big.Int, width int) string {
	out := make([]byte, width)
	base, mod := big.NewInt(62), new(big.Int)
	n = new(big.Int).Set(n)
	for i := width - 1; i >= 0; i-- {
		n.DivMod(n, base, mod)
		out[i] = alphabet[mod.Int64()]
	}
	return string(out)
}
