package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"paygate/internal/modules/auth/internal/domain"
	"paygate/internal/platform/apperror"
)

var (
	ErrRoleNotFound = apperror.NotFound("role_not_found", "role not found")
	ErrRoleTaken    = apperror.Conflict("role_name_taken", "a role with this name already exists")
)

// ---- permissions catalogue ----

func toPermission(r permissionRec) domain.Permission {
	return domain.Permission{Code: r.Code, Module: r.Module, Description: r.Description, Audience: r.Audience,
		APIKey: r.APIKey, DeprecatedAt: r.DeprecatedAt}
}

// LockCatalog serialises catalogue changes (several services and replicas
// register at the same time). Must run inside Tx.
func (s *Store) LockCatalog(ctx context.Context) error {
	return s.q(ctx).Exec("SELECT pg_advisory_xact_lock(hashtext('auth:permissions'))").Error
}

func (s *Store) Catalog(ctx context.Context) (domain.Catalog, error) {
	var recs []permissionRec
	if err := s.q(ctx).Find(&recs).Error; err != nil {
		return nil, err
	}
	c := make(domain.Catalog, len(recs))
	for _, r := range recs {
		c[r.Code] = toPermission(r)
	}
	return c, nil
}

func (s *Store) InsertPermission(ctx context.Context, p domain.Permission, now time.Time) error {
	return s.q(ctx).Create(&permissionRec{Code: p.Code, Module: p.Module, Description: p.Description,
		Audience: p.Audience, APIKey: p.APIKey, CreatedAt: now, UpdatedAt: now}).Error
}

// UpdatePermission refreshes the definition and clears deprecation.
func (s *Store) UpdatePermission(ctx context.Context, p domain.Permission, now time.Time) error {
	return s.q(ctx).Model(&permissionRec{}).Where("code = ?", p.Code).Updates(map[string]any{
		"description": p.Description, "audience": p.Audience, "api_key": p.APIKey,
		"deprecated_at": nil, "updated_at": now}).Error
}

func (s *Store) DeprecatePermissions(ctx context.Context, codes []string, now time.Time) error {
	if len(codes) == 0 {
		return nil
	}
	return s.q(ctx).Model(&permissionRec{}).Where("code IN ? AND deprecated_at IS NULL", codes).
		Updates(map[string]any{"deprecated_at": now, "updated_at": now}).Error
}

// ---- roles ----

func toRoleRec(r *domain.Role) roleRec {
	return roleRec{ID: r.ID, MerchantID: nilIfEmpty(r.MerchantID), Audience: r.Audience, Name: r.Name,
		Description: r.Description, System: r.System, AllPermissions: r.AllPermissions,
		CreatedBy: nilIfEmpty(r.CreatedBy), CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
}

func (r roleRec) domain() *domain.Role {
	return &domain.Role{ID: r.ID, MerchantID: deref(r.MerchantID), Audience: r.Audience, Name: r.Name,
		Description: r.Description, System: r.System, AllPermissions: r.AllPermissions,
		CreatedBy: deref(r.CreatedBy), CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
}

// EnsureRole inserts a role if its id does not exist yet (system roles).
func (s *Store) EnsureRole(ctx context.Context, r *domain.Role) error {
	rec := toRoleRec(r)
	return s.q(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&rec).Error
}

func (s *Store) CreateRole(ctx context.Context, r *domain.Role) error {
	rec := toRoleRec(r)
	err := s.q(ctx).Create(&rec).Error
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return ErrRoleTaken
	}
	return err
}

func (s *Store) UpdateRole(ctx context.Context, r *domain.Role) error {
	err := s.q(ctx).Model(&roleRec{}).Where("id = ?", r.ID).Updates(map[string]any{
		"name": r.Name, "description": r.Description, "updated_at": r.UpdatedAt}).Error
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return ErrRoleTaken
	}
	return err
}

// DeleteRole removes the role, its permissions and its assignments.
func (s *Store) DeleteRole(ctx context.Context, id string) error {
	q := s.q(ctx)
	if err := q.Where("role_id = ?", id).Delete(&userRoleRec{}).Error; err != nil {
		return err
	}
	if err := q.Where("role_id = ?", id).Delete(&rolePermissionRec{}).Error; err != nil {
		return err
	}
	return q.Where("id = ?", id).Delete(&roleRec{}).Error
}

// RoleByID returns the role with its permissions (row locked when lock).
func (s *Store) RoleByID(ctx context.Context, id string, lock bool) (*domain.Role, error) {
	if !validID(id) {
		return nil, ErrRoleNotFound
	}
	q := s.q(ctx)
	if lock {
		q = q.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var rec roleRec
	err := q.Where("id = ?", id).First(&rec).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrRoleNotFound
	}
	if err != nil {
		return nil, err
	}
	roles := []*domain.Role{rec.domain()}
	return roles[0], s.loadPermissions(ctx, roles)
}

// RoleNameTaken checks names where the unique index cannot (NULL merchant_id).
func (s *Store) RoleNameTaken(ctx context.Context, merchantID, audience, name, exceptID string) (bool, error) {
	q := s.q(ctx).Model(&roleRec{}).Where("lower(name) = lower(?) AND audience = ?", name, audience)
	if merchantID == "" {
		q = q.Where("merchant_id IS NULL")
	} else {
		// A custom merchant role may not reuse a template name either.
		q = q.Where("merchant_id = ? OR merchant_id IS NULL", merchantID)
	}
	if exceptID != "" {
		q = q.Where("id <> ?", exceptID)
	}
	var n int64
	return n > 0, q.Count(&n).Error
}

// ListRoles: the roles visible in a scope.
//
//	merchantID != "": the merchant templates + that merchant's custom roles
//	merchantID == "", audience platform: platform roles
//	merchantID == "", audience merchant: merchant templates only
func (s *Store) ListRoles(ctx context.Context, merchantID, audience string) ([]*domain.Role, error) {
	q := s.q(ctx).Where("audience = ?", audience).Order("is_system DESC, name")
	if merchantID == "" {
		q = q.Where("merchant_id IS NULL")
	} else {
		q = q.Where("merchant_id IS NULL OR merchant_id = ?", merchantID)
	}
	var recs []roleRec
	if err := q.Find(&recs).Error; err != nil {
		return nil, err
	}
	return s.withPermissions(ctx, recs)
}

func (s *Store) CountCustomRoles(ctx context.Context, merchantID string) (int64, error) {
	q := s.q(ctx).Model(&roleRec{}).Where("is_system = false")
	if merchantID == "" {
		q = q.Where("merchant_id IS NULL")
	} else {
		q = q.Where("merchant_id = ?", merchantID)
	}
	var n int64
	return n, q.Count(&n).Error
}

func (s *Store) withPermissions(ctx context.Context, recs []roleRec) ([]*domain.Role, error) {
	out := make([]*domain.Role, len(recs))
	for i, r := range recs {
		out[i] = r.domain()
	}
	return out, s.loadPermissions(ctx, out)
}

func (s *Store) loadPermissions(ctx context.Context, roles []*domain.Role) error {
	if len(roles) == 0 {
		return nil
	}
	byID := map[string]*domain.Role{}
	ids := make([]string, len(roles))
	for i, r := range roles {
		byID[r.ID], ids[i] = r, r.ID
	}
	var rps []rolePermissionRec
	if err := s.q(ctx).Where("role_id IN ?", ids).Order("permission_code").Find(&rps).Error; err != nil {
		return err
	}
	for _, rp := range rps {
		byID[rp.RoleID].Permissions = append(byID[rp.RoleID].Permissions, rp.PermissionCode)
	}
	return nil
}

// ---- role <-> permission ----

func (s *Store) AddRolePermissions(ctx context.Context, roleID string, codes []string, now time.Time) error {
	if len(codes) == 0 {
		return nil
	}
	recs := make([]rolePermissionRec, len(codes))
	for i, c := range codes {
		recs[i] = rolePermissionRec{RoleID: roleID, PermissionCode: c, CreatedAt: now}
	}
	return s.q(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&recs).Error
}

// AddDefaultPermission gives a new permission to the built-in roles named.
func (s *Store) AddDefaultPermission(ctx context.Context, code string, roleIDs []string, now time.Time) error {
	for _, id := range roleIDs {
		if err := s.AddRolePermissions(ctx, id, []string{code}, now); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) RemoveRolePermission(ctx context.Context, roleID, code string) (bool, error) {
	res := s.q(ctx).Where("role_id = ? AND permission_code = ?", roleID, code).Delete(&rolePermissionRec{})
	return res.RowsAffected > 0, res.Error
}

func (s *Store) TouchRole(ctx context.Context, id string, now time.Time) error {
	return s.q(ctx).Model(&roleRec{}).Where("id = ?", id).Update("updated_at", now).Error
}

// ---- user <-> role ----

// UserRoles returns the roles of one user, with permissions, oldest first.
func (s *Store) UserRoles(ctx context.Context, userID string) ([]*domain.Role, error) {
	m, err := s.UsersRoles(ctx, []string{userID})
	return m[userID], err
}

// UsersRoles returns the roles of several users (for listings).
func (s *Store) UsersRoles(ctx context.Context, userIDs []string) (map[string][]*domain.Role, error) {
	out := map[string][]*domain.Role{}
	if len(userIDs) == 0 {
		return out, nil
	}
	var links []userRoleRec
	if err := s.q(ctx).Where("user_id IN ?", userIDs).Order("created_at, role_id").Find(&links).Error; err != nil {
		return nil, err
	}
	if len(links) == 0 {
		return out, nil
	}
	roleIDs := make([]string, 0, len(links))
	for _, l := range links {
		roleIDs = append(roleIDs, l.RoleID)
	}
	var recs []roleRec
	if err := s.q(ctx).Where("id IN ?", roleIDs).Find(&recs).Error; err != nil {
		return nil, err
	}
	all, err := s.withPermissions(ctx, recs)
	if err != nil {
		return nil, err
	}
	byID := map[string]*domain.Role{}
	for _, r := range all {
		byID[r.ID] = r
	}
	for _, l := range links {
		if r, ok := byID[l.RoleID]; ok {
			out[l.UserID] = append(out[l.UserID], r)
		}
	}
	return out, nil
}

func (s *Store) AssignRole(ctx context.Context, userID, roleID, by string, now time.Time) (bool, error) {
	res := s.q(ctx).Clauses(clause.OnConflict{DoNothing: true}).
		Create(&userRoleRec{UserID: userID, RoleID: roleID, AssignedBy: nilIfEmpty(by), CreatedAt: now})
	return res.RowsAffected > 0, res.Error
}

func (s *Store) UnassignRole(ctx context.Context, userID, roleID string) (bool, error) {
	res := s.q(ctx).Where("user_id = ? AND role_id = ?", userID, roleID).Delete(&userRoleRec{})
	return res.RowsAffected > 0, res.Error
}

// CountActiveHolders: active users of a scope (merchant, or platform when "")
// that hold the role. Used to never remove the last owner / super admin.
func (s *Store) CountActiveHolders(ctx context.Context, roleID, merchantID string) (int64, error) {
	q := s.q(ctx).Table("user_roles ur").Joins("JOIN users u ON u.id = ur.user_id").
		Where("ur.role_id = ? AND u.status = ?", roleID, domain.StatusActive)
	if merchantID == "" {
		q = q.Where("u.merchant_id IS NULL")
	} else {
		q = q.Where("u.merchant_id = ?", merchantID)
	}
	var n int64
	return n, q.Count(&n).Error
}

func (s *Store) CountRoleUsers(ctx context.Context, roleID string) (int64, error) {
	var n int64
	return n, s.q(ctx).Model(&userRoleRec{}).Where("role_id = ?", roleID).Count(&n).Error
}

// SetPrimaryRole keeps users.role (display only) in step with assignments.
func (s *Store) SetPrimaryRole(ctx context.Context, userID, name string) error {
	return s.q(ctx).Model(&userRec{}).Where("id = ?", userID).UpdateColumn("role", name).Error
}

// BackfillUserRoles gives users created before dynamic RBAC (users.role
// only, no user_roles row) the built-in role of the same name. Idempotent.
func (s *Store) BackfillUserRoles(ctx context.Context, now time.Time) (int64, error) {
	res := s.q(ctx).Exec(`INSERT INTO user_roles (user_id, role_id, created_at)
		SELECT u.id, r.id, ? FROM users u
		JOIN roles r ON r.name = u.role AND r.is_system
		 AND r.audience = CASE WHEN u.merchant_id IS NULL THEN 'platform' ELSE 'merchant' END
		WHERE NOT EXISTS (SELECT 1 FROM user_roles ur WHERE ur.user_id = u.id)
		ON CONFLICT DO NOTHING`, now)
	return res.RowsAffected, res.Error
}

// ---- audit ----

type Audit struct {
	ActorID    string
	MerchantID string
	Action     string // e.g. role.created, role.permission_added, user.role_assigned
	TargetType string
	TargetID   string
	Details    map[string]any
}

func (s *Store) Audit(ctx context.Context, a Audit, now time.Time) error {
	details := []byte("{}")
	if a.Details != nil {
		var err error
		if details, err = json.Marshal(a.Details); err != nil {
			return err
		}
	}
	return s.q(ctx).Create(&auditRec{ID: uuid.NewString(), ActorID: nilIfEmpty(a.ActorID),
		MerchantID: nilIfEmpty(a.MerchantID), Action: a.Action, TargetType: a.TargetType, TargetID: a.TargetID,
		Details: string(details), CreatedAt: now}).Error
}

type AuditEntry struct {
	ID         string          `json:"id"`
	ActorID    string          `json:"actor_id,omitempty"`
	Action     string          `json:"action"`
	TargetType string          `json:"target_type"`
	TargetID   string          `json:"target_id"`
	Details    json.RawMessage `json:"details"`
	CreatedAt  time.Time       `json:"created_at"`
}

// AuditLog lists the newest entries of a scope (merchant, or platform when "").
func (s *Store) AuditLog(ctx context.Context, merchantID string, limit int) ([]AuditEntry, error) {
	q := s.q(ctx).Order("created_at DESC").Limit(limit)
	if merchantID == "" {
		q = q.Where("merchant_id IS NULL")
	} else {
		q = q.Where("merchant_id = ?", merchantID)
	}
	var recs []auditRec
	if err := q.Find(&recs).Error; err != nil {
		return nil, err
	}
	out := make([]AuditEntry, len(recs))
	for i, r := range recs {
		out[i] = AuditEntry{ID: r.ID, ActorID: deref(r.ActorID), Action: r.Action, TargetType: r.TargetType,
			TargetID: r.TargetID, Details: json.RawMessage(r.Details), CreatedAt: r.CreatedAt}
	}
	return out, nil
}
