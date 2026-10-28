package store

import (
	"time"

	"paygate/internal/platform/database"
)

// GORM models: the auth service's database schema. They are auto-migrated on
// every start (module.Base.Models). AutoMigrate only ADDS: new tables, new
// columns, new indexes. It never drops a column or deletes a row, so to
// change the schema you simply edit these structs:
//
//	new table   -> add a model to Models()
//	new column  -> add a field (pointer or with a default if the table has rows)
//	new index   -> add an index tag
//
// Renaming or removing a column needs a SQL file in migrations/ (deliberate,
// reviewed, and usually done in two releases).
//
// Index names match the ones created by the original SQL migration, and
// single-column uniqueness uses `unique` (a constraint, as the SQL had), so
// databases created before the move to AutoMigrate are adopted unchanged.
// merchant_id has no foreign key: merchants live in another service.

// Models lists every table of the auth service.
func Models() []any {
	return []any{&userRec{}, &sessionRec{}, &keyRec{}, &merchantRec{},
		&permissionRec{}, &roleRec{}, &rolePermissionRec{}, &userRoleRec{}, &auditRec{}}
}

// users: people who log in, platform staff (merchant_id NULL) and merchant teams.
type userRec struct {
	ID           string     `gorm:"type:uuid;primaryKey"`
	Email        string     `gorm:"type:text;not null;unique"` // lower-case
	Name         string     `gorm:"type:text;not null"`
	PasswordHash string     `gorm:"type:text;not null"` // argon2id, PHC string
	MerchantID   *string    `gorm:"type:uuid;index:users_merchant_idx"`
	Role         string     `gorm:"type:text;not null;default:''"` // primary role name, display only
	Status       string     `gorm:"type:text;not null"`            // active | disabled
	FailedLogins int        `gorm:"type:integer;not null;default:0"`
	LockedUntil  *time.Time `gorm:"type:timestamptz"`
	LastLoginAt  *time.Time `gorm:"type:timestamptz"`
	CreatedBy    *string    `gorm:"type:uuid"`
	CreatedAt    time.Time  `gorm:"type:timestamptz;not null"`
	UpdatedAt    time.Time  `gorm:"type:timestamptz;not null"`
}

func (userRec) TableName() string { return "users" }

// sessions: one per login. Only a SHA-256 of the refresh token is stored.
type sessionRec struct {
	ID         string     `gorm:"type:uuid;primaryKey"`
	UserID     string     `gorm:"type:uuid;not null;index:sessions_user_idx"`
	TokenHash  string     `gorm:"type:text;not null;unique"`
	ExpiresAt  time.Time  `gorm:"type:timestamptz;not null"`
	RevokedAt  *time.Time `gorm:"type:timestamptz"`
	ReplacedBy *string    `gorm:"type:uuid"`
	CreatedAt  time.Time  `gorm:"type:timestamptz;not null"`
}

func (sessionRec) TableName() string { return "sessions" }

// api_keys: only HMAC-SHA256(pepper, key) is stored, plus prefix/last4 for display.
type keyRec struct {
	ID            string              `gorm:"type:uuid;primaryKey"`
	MerchantID    string              `gorm:"type:uuid;not null;index:api_keys_merchant_idx"`
	Name          string              `gorm:"type:text;not null"`
	Prefix        string              `gorm:"type:text;not null"`
	Last4         string              `gorm:"column:last4;type:text;not null"`
	Hash          string              `gorm:"type:text;not null;unique"`
	PepperVersion int                 `gorm:"type:integer;not null"`
	Environment   string              `gorm:"type:text;not null"` // live | test
	Scopes        database.StringList `gorm:"type:jsonb;not null"`
	ExpiresAt     *time.Time          `gorm:"type:timestamptz"`
	RevokedAt     *time.Time          `gorm:"type:timestamptz"`
	LastUsedAt    *time.Time          `gorm:"type:timestamptz"`
	CreatedBy     *string             `gorm:"type:uuid"`
	CreatedAt     time.Time           `gorm:"type:timestamptz;not null"`
}

func (keyRec) TableName() string { return "api_keys" }

// merchants: local copy of merchant status, fed by merchant.updated events.
type merchantRec struct {
	ID        string    `gorm:"type:uuid;primaryKey"`
	Status    string    `gorm:"type:text;not null"`
	UpdatedAt time.Time `gorm:"type:timestamptz;not null;autoUpdateTime:false"`
}

func (merchantRec) TableName() string { return "merchants" }

// ---- RBAC ----

// permissions: the catalogue, registered by each service at start-up.
type permissionRec struct {
	Code         string     `gorm:"type:text;primaryKey"`
	Module       string     `gorm:"type:text;not null;index:permissions_module_idx"`
	Description  string     `gorm:"type:text;not null"`
	Audience     string     `gorm:"type:text;not null"` // merchant | platform | both
	APIKey       bool       `gorm:"column:api_key;not null;default:false"`
	DeprecatedAt *time.Time `gorm:"type:timestamptz"`
	CreatedAt    time.Time  `gorm:"type:timestamptz;not null"`
	UpdatedAt    time.Time  `gorm:"type:timestamptz;not null"`
}

func (permissionRec) TableName() string { return "permissions" }

// roles: built-in (is_system) and custom. merchant_id is set only for a
// merchant's own custom roles; platform roles and merchant templates have NULL.
type roleRec struct {
	ID             string    `gorm:"type:uuid;primaryKey"`
	MerchantID     *string   `gorm:"type:uuid;uniqueIndex:roles_merchant_name_key,priority:1"`
	Audience       string    `gorm:"type:text;not null"`
	Name           string    `gorm:"type:text;not null;uniqueIndex:roles_merchant_name_key,priority:2"`
	Description    string    `gorm:"type:text;not null;default:''"`
	System         bool      `gorm:"column:is_system;not null;default:false"`
	AllPermissions bool      `gorm:"not null;default:false"`
	CreatedBy      *string   `gorm:"type:uuid"`
	CreatedAt      time.Time `gorm:"type:timestamptz;not null"`
	UpdatedAt      time.Time `gorm:"type:timestamptz;not null"`
}

func (roleRec) TableName() string { return "roles" }

// role_permissions: role has many permissions.
type rolePermissionRec struct {
	RoleID         string    `gorm:"type:uuid;primaryKey"`
	PermissionCode string    `gorm:"type:text;primaryKey;index:role_permissions_code_idx"`
	CreatedAt      time.Time `gorm:"type:timestamptz;not null"`
}

func (rolePermissionRec) TableName() string { return "role_permissions" }

// user_roles: user has many roles.
type userRoleRec struct {
	UserID     string    `gorm:"type:uuid;primaryKey"`
	RoleID     string    `gorm:"type:uuid;primaryKey;index:user_roles_role_idx"`
	AssignedBy *string   `gorm:"type:uuid"`
	CreatedAt  time.Time `gorm:"type:timestamptz;not null"`
}

func (userRoleRec) TableName() string { return "user_roles" }

// audit_log: every change to roles, permissions and assignments.
type auditRec struct {
	ID         string    `gorm:"type:uuid;primaryKey"`
	ActorID    *string   `gorm:"type:uuid"`
	MerchantID *string   `gorm:"type:uuid;index:audit_log_merchant_idx"`
	Action     string    `gorm:"type:text;not null"`
	TargetType string    `gorm:"type:text;not null"`
	TargetID   string    `gorm:"type:text;not null"`
	Details    string    `gorm:"type:jsonb;not null;default:'{}'"`
	CreatedAt  time.Time `gorm:"type:timestamptz;not null;index:audit_log_created_idx"`
}

func (auditRec) TableName() string { return "audit_log" }
