package domain

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"paygate/internal/contracts/auth"
	"paygate/internal/platform/apperror"
)

// ---- Permissions (registered by services, stored by auth) ----

type Permission struct {
	Code         string
	Module       string
	Description  string
	Audience     string
	APIKey       bool
	DeprecatedAt *time.Time
}

func (p Permission) Active() bool { return p.DeprecatedAt == nil }

// AllowedFor reports whether a role of this audience may hold the permission.
func (p Permission) AllowedFor(audience string) bool {
	return p.Audience == auth.AudienceBoth || p.Audience == audience
}

var codeRe = regexp.MustCompile(`^[a-z][a-z0-9_]*:[a-z][a-z0-9_]*$`)

// ValidatePermissionDef checks a permission sent by a service.
func ValidatePermissionDef(d auth.PermissionDef) error {
	if !codeRe.MatchString(d.Code) || len(d.Code) > 64 {
		return apperror.Invalid("invalid_permission", fmt.Sprintf("permission %q must look like resource:action", d.Code))
	}
	switch d.Audience {
	case auth.AudienceMerchant, auth.AudiencePlatform, auth.AudienceBoth:
	default:
		return apperror.Invalid("invalid_permission", fmt.Sprintf("permission %s: unknown audience %q", d.Code, d.Audience))
	}
	if d.APIKey && d.Audience == auth.AudiencePlatform {
		return apperror.Invalid("invalid_permission", "platform permissions cannot be API key scopes: "+d.Code)
	}
	return nil
}

// ---- Roles ----

type Role struct {
	ID          string
	MerchantID  string // "" for platform roles and merchant templates
	Audience    string // auth.AudienceMerchant or auth.AudiencePlatform
	Name        string
	Description string
	// System roles are created by auth. AllPermissions roles (super_admin,
	// owner) hold every permission of their audience, including future ones.
	System         bool
	AllPermissions bool
	Permissions    []string // codes stored for this role (empty when AllPermissions)
	CreatedBy      string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Template: a built-in merchant role shared by every merchant.
func (r *Role) Template() bool { return r.Audience == auth.AudienceMerchant && r.MerchantID == "" }

// Locked roles cannot be edited or deleted by anyone.
func (r *Role) Locked() bool { return r.AllPermissions }

type SystemRole struct {
	Name, Audience, Description string
	All                         bool
}

// SystemRoles are created on start if missing. Their permissions come from
// each permission's DefaultRoles; AllPermissions roles need none.
var SystemRoles = []SystemRole{
	{auth.RoleSuperAdmin, auth.AudiencePlatform, "Full platform access", true},
	{auth.RoleOperations, auth.AudiencePlatform, "Merchant onboarding and operations", false},
	{auth.RoleFinance, auth.AudiencePlatform, "Ledger, settlement and reconciliation", false},
	{auth.RoleSupport, auth.AudiencePlatform, "Read-only merchant support", false},
	{auth.RoleOwner, auth.AudienceMerchant, "Full access to the merchant account", true},
	{auth.RoleAdmin, auth.AudienceMerchant, "Manage the account, team and keys", false},
	{auth.RoleDeveloper, auth.AudienceMerchant, "Integrate: payments, webhooks, API keys", false},
	{auth.RoleAnalyst, auth.AudienceMerchant, "Payments, refunds and balance", false},
	{auth.RoleViewer, auth.AudienceMerchant, "Read-only", false},
}

var roleNamespace = uuid.MustParse("6f0c6a52-0b7e-4c1e-9d1a-9a3b1c2d4e5f")

// SystemRoleID is deterministic, so every replica seeds the same rows.
func SystemRoleID(name string) string {
	return uuid.NewSHA1(roleNamespace, []byte("system-role:"+name)).String()
}

func (s SystemRole) Role(now time.Time) *Role {
	return &Role{ID: SystemRoleID(s.Name), Audience: s.Audience, Name: s.Name, Description: s.Description,
		System: true, AllPermissions: s.All, CreatedAt: now, UpdatedAt: now}
}

const MaxCustomRoles = 50

var roleNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 _.-]{1,49}$`)

type NewRoleInput struct {
	MerchantID  string
	Audience    string
	Name        string
	Description string
	Permissions []string
	CreatedBy   string
}

func NewRole(in NewRoleInput, now time.Time) (*Role, error) {
	name := strings.TrimSpace(in.Name)
	if !roleNameRe.MatchString(name) {
		return nil, apperror.Invalid("invalid_role_name", "role name must be 2-50 letters, digits, spaces, '.', '_' or '-'")
	}
	if len(in.Description) > 200 {
		return nil, apperror.Invalid("invalid_description", "description is at most 200 characters")
	}
	return &Role{ID: uuid.NewString(), MerchantID: in.MerchantID, Audience: in.Audience, Name: name,
		Description: strings.TrimSpace(in.Description), CreatedBy: in.CreatedBy, CreatedAt: now, UpdatedAt: now}, nil
}

// ---- Effective permissions ----

// Catalog is the set of registered permissions, keyed by code.
type Catalog map[string]Permission

// Effective returns the union of the roles' permissions for a user of the
// given audience: wildcards expanded, deprecated and wrong-audience
// permissions dropped, sorted.
func (c Catalog) Effective(roles []*Role, audience string) []string {
	set := map[string]bool{}
	for _, r := range roles {
		if r.Audience != audience {
			continue
		}
		if r.AllPermissions {
			for code, p := range c {
				if p.Active() && p.AllowedFor(audience) {
					set[code] = true
				}
			}
			continue
		}
		for _, code := range r.Permissions {
			if p, ok := c[code]; ok && p.Active() && p.AllowedFor(audience) {
				set[code] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for code := range set {
		out = append(out, code)
	}
	slices.Sort(out)
	return out
}

// APIKeyScopes lists the active permissions that may be given to API keys.
func (c Catalog) APIKeyScopes() []string {
	var out []string
	for code, p := range c {
		if p.Active() && p.APIKey {
			out = append(out, code)
		}
	}
	slices.Sort(out)
	return out
}

// CheckAssignable validates permission codes for a role of this audience.
func (c Catalog) CheckAssignable(codes []string, audience string) error {
	if len(codes) == 0 {
		return apperror.Invalid("invalid_permissions", "at least one permission is required")
	}
	for _, code := range codes {
		p, ok := c[code]
		if !ok || !p.Active() {
			return apperror.Invalid("unknown_permission", "unknown permission "+code)
		}
		if !p.AllowedFor(audience) {
			return apperror.Invalid("invalid_permissions", fmt.Sprintf("%s cannot be given to a %s role", code, audience))
		}
	}
	return nil
}

// ---- Anti-escalation ----

var ErrEscalation = func(code string) error {
	return apperror.Forbidden("privilege_escalation", "you cannot grant "+code+" because you do not hold it")
}

// CanGrant: an actor may only hand out permissions it holds itself.
func CanGrant(actorPerms, perms []string) error {
	for _, p := range perms {
		if !slices.Contains(actorPerms, p) {
			return ErrEscalation(p)
		}
	}
	return nil
}

// Audience of a user: merchant team member or platform staff.
func UserAudience(merchantID string) string {
	if merchantID == "" {
		return auth.AudiencePlatform
	}
	return auth.AudienceMerchant
}

// Uniq returns the sorted, de-duplicated list.
func Uniq(in []string) []string {
	return slices.Compact(slices.Sorted(slices.Values(in)))
}
