package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"paygate/internal/modules/auth/internal/domain"
	"paygate/internal/platform/apperror"
)

var (
	ErrKeyNotFound     = apperror.NotFound("api_key_not_found", "API key not found")
	ErrUserNotFound    = apperror.NotFound("user_not_found", "user not found")
	ErrSessionNotFound = apperror.Unauthorized("invalid_refresh_token", "refresh token is invalid or expired")
	ErrEmailTaken      = apperror.Conflict("email_taken", "a user with this email already exists")
)

type Store struct{ db *gorm.DB }

func New(db *gorm.DB) *Store { return &Store{db: db} }

// Tx runs fn in a transaction with a store bound to it.
func (s *Store) Tx(ctx context.Context, fn func(*Store) error) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error { return fn(&Store{db: tx}) })
}

func (s *Store) q(ctx context.Context) *gorm.DB { return s.db.WithContext(ctx) }

func validID(id string) bool { _, err := uuid.Parse(id); return err == nil }

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ---- users ----

func toUserRec(u *domain.User) userRec {
	return userRec{u.ID, u.Email, u.Name, u.PasswordHash, nilIfEmpty(u.MerchantID), u.Role, u.Status, u.FailedLogins,
		u.LockedUntil, u.LastLoginAt, nilIfEmpty(u.CreatedBy), u.CreatedAt, u.UpdatedAt}
}

func (r userRec) domain() *domain.User {
	return &domain.User{ID: r.ID, Email: r.Email, Name: r.Name, PasswordHash: r.PasswordHash, MerchantID: deref(r.MerchantID),
		Role: r.Role, Status: r.Status, FailedLogins: r.FailedLogins, LockedUntil: r.LockedUntil, LastLoginAt: r.LastLoginAt,
		CreatedBy: deref(r.CreatedBy), CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
}

func (s *Store) CreateUser(ctx context.Context, u *domain.User) error {
	rec := toUserRec(u)
	err := s.q(ctx).Create(&rec).Error
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return ErrEmailTaken
	}
	return err
}

func (s *Store) SaveUser(ctx context.Context, u *domain.User) error {
	rec := toUserRec(u)
	return s.q(ctx).Save(&rec).Error
}

func (s *Store) findUser(ctx context.Context, lock bool, where string, args ...any) (*domain.User, error) {
	q := s.q(ctx)
	if lock {
		q = q.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var rec userRec
	err := q.Where(where, args...).First(&rec).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, err
	}
	return rec.domain(), nil
}

// UserByEmail locks the row (login updates failed-attempt counters).
func (s *Store) UserByEmail(ctx context.Context, email string) (*domain.User, error) {
	return s.findUser(ctx, true, "email = ?", email)
}

func (s *Store) UserByID(ctx context.Context, id string, lock bool) (*domain.User, error) {
	if !validID(id) {
		return nil, ErrUserNotFound
	}
	return s.findUser(ctx, lock, "id = ?", id)
}

// ListUsers: merchantID "" lists platform staff.
func (s *Store) ListUsers(ctx context.Context, merchantID string) ([]*domain.User, error) {
	q := s.q(ctx).Order("created_at")
	if merchantID == "" {
		q = q.Where("merchant_id IS NULL")
	} else {
		q = q.Where("merchant_id = ?", merchantID)
	}
	var recs []userRec
	if err := q.Find(&recs).Error; err != nil {
		return nil, err
	}
	out := make([]*domain.User, len(recs))
	for i, r := range recs {
		out[i] = r.domain()
	}
	return out, nil
}

// ---- sessions ----

func (s *Store) CreateSession(ctx context.Context, x *domain.Session) error {
	rec := sessionRec(*x)
	return s.q(ctx).Create(&rec).Error
}

func (s *Store) SessionByHash(ctx context.Context, hash string) (*domain.Session, error) {
	var rec sessionRec
	err := s.q(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).Where("token_hash = ?", hash).First(&rec).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrSessionNotFound
	}
	if err != nil {
		return nil, err
	}
	x := domain.Session(rec)
	return &x, nil
}

func (s *Store) RevokeSession(ctx context.Context, id string, replacedBy *string, now time.Time) error {
	return s.q(ctx).Model(&sessionRec{}).Where("id = ? AND revoked_at IS NULL", id).
		Updates(map[string]any{"revoked_at": now, "replaced_by": replacedBy}).Error
}

func (s *Store) RevokeUserSessions(ctx context.Context, userID string, now time.Time) error {
	return s.q(ctx).Model(&sessionRec{}).Where("user_id = ? AND revoked_at IS NULL", userID).
		Update("revoked_at", now).Error
}

// ---- API keys ----

func toKeyRec(k *domain.APIKey) keyRec {
	return keyRec{k.ID, k.MerchantID, k.Name, k.Prefix, k.Last4, k.Hash, k.PepperVersion, k.Environment, k.Scopes,
		k.ExpiresAt, k.RevokedAt, k.LastUsedAt, nilIfEmpty(k.CreatedBy), k.CreatedAt}
}

func (r keyRec) domain() *domain.APIKey {
	return &domain.APIKey{ID: r.ID, MerchantID: r.MerchantID, Name: r.Name, Prefix: r.Prefix, Last4: r.Last4, Hash: r.Hash,
		PepperVersion: r.PepperVersion, Environment: r.Environment, Scopes: r.Scopes, ExpiresAt: r.ExpiresAt,
		RevokedAt: r.RevokedAt, LastUsedAt: r.LastUsedAt, CreatedBy: deref(r.CreatedBy), CreatedAt: r.CreatedAt}
}

func (s *Store) CreateKey(ctx context.Context, k *domain.APIKey) error {
	rec := toKeyRec(k)
	return s.q(ctx).Create(&rec).Error
}

// KeyByHashes finds the key matching any of the hashes (one per pepper version).
func (s *Store) KeyByHashes(ctx context.Context, hashes []string) (*domain.APIKey, error) {
	var rec keyRec
	err := s.q(ctx).Where("hash IN ?", hashes).First(&rec).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrKeyNotFound
	}
	if err != nil {
		return nil, err
	}
	return rec.domain(), nil
}

func (s *Store) KeyByID(ctx context.Context, merchantID, id string) (*domain.APIKey, error) {
	if !validID(id) {
		return nil, ErrKeyNotFound
	}
	var rec keyRec
	err := s.q(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND merchant_id = ?", id, merchantID).First(&rec).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrKeyNotFound
	}
	if err != nil {
		return nil, err
	}
	return rec.domain(), nil
}

func (s *Store) ListKeys(ctx context.Context, merchantID string) ([]*domain.APIKey, error) {
	var recs []keyRec
	if err := s.q(ctx).Where("merchant_id = ?", merchantID).Order("created_at DESC").Find(&recs).Error; err != nil {
		return nil, err
	}
	out := make([]*domain.APIKey, len(recs))
	for i, r := range recs {
		out[i] = r.domain()
	}
	return out, nil
}

func (s *Store) CountUsableKeys(ctx context.Context, merchantID string, now time.Time) (int64, error) {
	var n int64
	err := s.q(ctx).Model(&keyRec{}).
		Where("merchant_id = ? AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?)", merchantID, now).
		Count(&n).Error
	return n, err
}

func (s *Store) RevokeKey(ctx context.Context, merchantID, id string, now time.Time) error {
	if !validID(id) {
		return ErrKeyNotFound
	}
	res := s.q(ctx).Model(&keyRec{}).
		Where("id = ? AND merchant_id = ? AND revoked_at IS NULL", id, merchantID).Update("revoked_at", now)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrKeyNotFound
	}
	return nil
}

func (s *Store) SetKeyExpiry(ctx context.Context, id string, at time.Time) error {
	return s.q(ctx).Model(&keyRec{}).Where("id = ?", id).Update("expires_at", at).Error
}

func (s *Store) TouchKey(ctx context.Context, id string, now time.Time) error {
	return s.q(ctx).Model(&keyRec{}).Where("id = ?", id).Update("last_used_at", now).Error
}

// Rehash moves a key to the current pepper after a pepper rotation.
func (s *Store) Rehash(ctx context.Context, id, hash string, version int) error {
	return s.q(ctx).Model(&keyRec{}).Where("id = ?", id).
		Updates(map[string]any{"hash": hash, "pepper_version": version}).Error
}

// ---- local copy of merchant status ----

func (s *Store) MerchantStatus(ctx context.Context, id string) (string, bool, error) {
	if !validID(id) {
		return "", false, nil
	}
	var rec merchantRec
	err := s.q(ctx).Where("id = ?", id).First(&rec).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", false, nil
	}
	return rec.Status, err == nil, err
}

// UpsertMerchant keeps the newest status (events may arrive out of order).
func (s *Store) UpsertMerchant(ctx context.Context, id, status string, updatedAt time.Time) error {
	return s.q(ctx).Exec(`INSERT INTO merchants (id, status, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET status = EXCLUDED.status, updated_at = EXCLUDED.updated_at
		WHERE merchants.updated_at <= EXCLUDED.updated_at`, id, status, updatedAt).Error
}

// DB exposes the handle for events.HandleOnce.
func (s *Store) DB() *gorm.DB { return s.db }
