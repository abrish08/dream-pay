// Package auth: messages owned by the auth service.
//
// RBAC model:
//
//	permission  defined in CODE by the service that checks it (each service's
//	            contract has a Permissions list) and registered with the auth
//	            service at start-up (RegisterPermissions).
//	role        a named set of permissions, stored by the auth service and
//	            edited at runtime (add / remove permissions).
//	user        holds any number of roles; their permissions are the union.
//
// Every endpoint checks one permission. Services never look up roles: the
// signed access token already carries the caller's permissions.
package auth

import (
	"context"
	"time"

	"paygate/internal/platform/apperror"
	"paygate/internal/platform/bus"
)

// ---- Permission catalogue ----

// Audience says which kind of role may hold a permission.
const (
	AudienceMerchant = "merchant" // merchant team roles (scoped to one merchant)
	AudiencePlatform = "platform" // platform staff roles (system admins, finance, support…)
	AudienceBoth     = "both"
)

// PermissionDef declares one permission. Code is "resource:action".
type PermissionDef struct {
	Code        string `json:"code"`
	Description string `json:"description"`
	Audience    string `json:"audience"`
	// APIKey: the permission may be given to an API key as a scope.
	APIKey bool `json:"api_key,omitempty"`
	// DefaultRoles: built-in roles that get this permission when it is
	// registered for the first time. Later edits to those roles are kept.
	DefaultRoles []string `json:"default_roles,omitempty"`
}

// Permissions owned by the auth service itself.
const (
	APIKeysManage = "apikeys:manage"
	TeamManage    = "team:manage"  // merchant: manage own team members and their roles
	UsersManage   = "users:manage" // platform: manage any user
	RolesRead     = "roles:read"
	RolesManage   = "roles:manage" // create roles, add/remove permissions
)

var Permissions = []PermissionDef{
	{Code: APIKeysManage, Description: "Create, roll and revoke API keys", Audience: AudienceMerchant,
		DefaultRoles: []string{RoleAdmin, RoleDeveloper}},
	{Code: TeamManage, Description: "Invite team members and assign their roles", Audience: AudienceMerchant,
		DefaultRoles: []string{RoleAdmin}},
	{Code: UsersManage, Description: "Create users and assign roles for any account", Audience: AudiencePlatform},
	{Code: RolesRead, Description: "View roles and permissions", Audience: AudienceBoth,
		DefaultRoles: []string{RoleAdmin, RoleOperations}},
	{Code: RolesManage, Description: "Create roles and change their permissions", Audience: AudienceBoth,
		DefaultRoles: []string{RoleAdmin}},
}

// ---- Built-in roles (created by the auth service, always present) ----

const (
	// Platform.
	RoleSuperAdmin = "super_admin" // every platform permission, now and future; locked
	RoleOperations = "operations"
	RoleFinance    = "finance"
	RoleSupport    = "support"

	// Merchant templates, shared by every merchant.
	RoleOwner     = "owner" // every merchant permission, now and future; locked
	RoleAdmin     = "admin"
	RoleDeveloper = "developer"
	RoleAnalyst   = "analyst"
	RoleViewer    = "viewer"
)

// ---- Errors ----

var (
	ErrInvalidKey         = apperror.Unauthorized("invalid_api_key", "API key is invalid, revoked or expired")
	ErrInvalidCredentials = apperror.Unauthorized("invalid_credentials", "email or password is incorrect")
	ErrMerchantInactive   = apperror.Forbidden("merchant_inactive", "merchant account is not active")
)

// ---- Requests ----

// AccessToken is a short-lived signed token (JWT, EdDSA).
type AccessToken struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// ExchangeAPIKey trades a raw API key for a short-lived access token. Every
// service's HTTP layer calls it (and caches the result briefly).
type ExchangeAPIKey struct {
	bus.Returns[AccessToken]
	Key string `json:"key"`
}

func (ExchangeAPIKey) Route() string { return "auth.exchange_api_key" }

// RegisterPermissions: every service sends its permission list at start-up.
// Idempotent. Codes of this module that are missing from the list are marked
// deprecated (never deleted: roles may still reference them).
type RegisterPermissions struct {
	bus.Returns[RegisterResult]
	Module      string          `json:"module"`
	Permissions []PermissionDef `json:"permissions"`
}

type RegisterResult struct {
	Added      []string `json:"added"`
	Deprecated []string `json:"deprecated"`
}

func (RegisterPermissions) Route() string { return "auth.register_permissions" }

// ---- Events ----

// AccessChanged: roles or permissions changed. Every service rejects access
// tokens of these users (or of anyone holding these roles) issued before
// NotBefore, so the change applies within seconds instead of at token expiry.
type AccessChanged struct {
	UserIDs   []string  `json:"user_ids,omitempty"`
	RoleIDs   []string  `json:"role_ids,omitempty"`
	NotBefore time.Time `json:"not_before"`
}

func (AccessChanged) EventTopic() string { return "auth.access_changed.v1" }

// RegisterTask returns a start-up task that registers a module's permissions
// with the auth service (used with module.Base.AddStartTask).
func RegisterTask(b bus.Bus, module string, perms []PermissionDef) func(context.Context) error {
	return func(ctx context.Context) error {
		_, err := bus.Ask(ctx, b, RegisterPermissions{Module: module, Permissions: perms})
		return err
	}
}
