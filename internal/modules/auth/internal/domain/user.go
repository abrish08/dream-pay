package domain

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/argon2"

	"paygate/internal/platform/apperror"
	"paygate/internal/platform/reqctx"
)

const (
	StatusActive   = "active"
	StatusDisabled = "disabled"

	MinPasswordLen = 12
	MaxPasswordLen = 128

	MaxFailedLogins = 5
	LockoutDuration = 15 * time.Minute
)

type User struct {
	ID           string
	Email        string
	Name         string
	PasswordHash string
	MerchantID   string // empty for platform staff
	Role         string // primary role name, for display; access comes from Roles
	Status       string
	FailedLogins int
	LockedUntil  *time.Time
	LastLoginAt  *time.Time
	CreatedBy    string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

type NewUserInput struct {
	Email      string
	Name       string
	Password   string
	MerchantID string
	CreatedBy  string
}

func NewUser(in NewUserInput, now time.Time) (*User, error) {
	email := strings.ToLower(strings.TrimSpace(in.Email))
	if a, err := mail.ParseAddress(email); err != nil || a.Address != email {
		return nil, apperror.Invalid("invalid_email", "a valid email is required")
	}
	name := strings.TrimSpace(in.Name)
	if name == "" || len(name) > 100 {
		return nil, apperror.Invalid("invalid_name", "name is required (max 100 characters)")
	}
	hash, err := HashPassword(in.Password)
	if err != nil {
		return nil, err
	}
	return &User{ID: uuid.NewString(), Email: email, Name: name, PasswordHash: hash, MerchantID: in.MerchantID,
		Status: StatusActive, CreatedBy: in.CreatedBy, CreatedAt: now, UpdatedAt: now}, nil
}

// Principal is what the user's access token carries: the ids of all the
// user's roles and the union of their permissions.
func (u *User) Principal(roles []*Role, c Catalog) reqctx.Principal {
	p := reqctx.Principal{Kind: reqctx.KindUser, Subject: u.ID, MerchantID: u.MerchantID,
		Permissions: c.Effective(roles, UserAudience(u.MerchantID))}
	for _, r := range roles {
		p.Roles = append(p.Roles, r.ID)
	}
	if len(roles) > 0 {
		p.Role = roles[0].Name
	}
	return p
}

// Audience: merchant team member or platform staff.
func (u *User) Audience() string { return UserAudience(u.MerchantID) }

func (u *User) Locked(now time.Time) bool { return u.LockedUntil != nil && now.Before(*u.LockedUntil) }

// LoginFailed counts a wrong password and locks the account after too many.
func (u *User) LoginFailed(now time.Time) {
	u.FailedLogins++
	if u.FailedLogins >= MaxFailedLogins {
		until := now.Add(LockoutDuration)
		u.LockedUntil, u.FailedLogins = &until, 0
	}
	u.UpdatedAt = now
}

func (u *User) LoginSucceeded(now time.Time) {
	u.FailedLogins, u.LockedUntil, u.LastLoginAt, u.UpdatedAt = 0, nil, &now, now
}

// ---- Passwords: argon2id (RFC 9106), OWASP-recommended parameters ----

const (
	argonTime    = 2
	argonMemory  = 19 * 1024 // KiB
	argonThreads = 1
	argonKeyLen  = 32
)

func ValidatePassword(pw string) error {
	if len(pw) < MinPasswordLen || len(pw) > MaxPasswordLen {
		return apperror.Invalid("weak_password",
			fmt.Sprintf("password must be %d to %d characters", MinPasswordLen, MaxPasswordLen))
	}
	return nil
}

// HashPassword returns a PHC string: $argon2id$v=19$m=..,t=..,p=..$salt$hash.
// Parameters are stored with the hash so they can be raised later.
func HashPassword(pw string) (string, error) {
	if err := ValidatePassword(pw); err != nil {
		return "", err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// CheckPassword compares in constant time.
func CheckPassword(pw, phc string) bool {
	parts := strings.Split(phc, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[4])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[5])
	if err1 != nil || err2 != nil {
		return false
	}
	got := argon2.IDKey([]byte(pw), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// dummyHash is checked when the email is unknown, so a login for a missing
// user takes as long as one for an existing user (no account enumeration).
var dummyHash, _ = HashPassword("not-a-real-password-0000")

func BurnPasswordCheck(pw string) { CheckPassword(pw, dummyHash) }

// ---- Refresh tokens ----

type Session struct {
	ID         string
	UserID     string
	TokenHash  string
	ExpiresAt  time.Time
	RevokedAt  *time.Time
	ReplacedBy *string
	CreatedAt  time.Time
}

// NewSession returns the session and the raw refresh token (given to the client once).
func NewSession(userID string, ttl time.Duration, now time.Time) (*Session, string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, "", err
	}
	raw := "rt_" + base64.RawURLEncoding.EncodeToString(b)
	return &Session{ID: uuid.NewString(), UserID: userID, TokenHash: HashRefreshToken(raw),
		ExpiresAt: now.Add(ttl), CreatedAt: now}, raw, nil
}

// HashRefreshToken: refresh tokens are 256-bit random, so SHA-256 is enough.
func HashRefreshToken(raw string) string {
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}
