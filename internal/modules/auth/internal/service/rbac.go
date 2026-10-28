package service

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"paygate/internal/contracts/auth"
	"paygate/internal/modules/auth/internal/domain"
	"paygate/internal/modules/auth/internal/store"
	"paygate/internal/platform/apperror"
	"paygate/internal/platform/reqctx"
)

var (
	errRoleLocked    = apperror.Unprocessable("role_locked", "this built-in role always has every permission and cannot be changed")
	errTemplateRole  = apperror.Forbidden("template_role", "built-in roles are shared by all merchants; create a custom role instead")
	errRoleInUse     = apperror.Conflict("role_in_use", "remove this role from every user before deleting it")
	errSystemRole    = apperror.Unprocessable("system_role", "built-in roles cannot be deleted")
	errOwnRoles      = apperror.Forbidden("cannot_change_own_roles", "ask another administrator to change your roles")
	errTooManyRoles  = apperror.Unprocessable("too_many_roles", "custom role limit reached")
	errRoleMismatch  = apperror.Invalid("role_not_assignable", "this role cannot be given to this user")
	errNeedRole      = apperror.Invalid("role_required", "give the user at least one role")
	errHolderMissing = apperror.Forbidden("privilege_escalation", "only a holder of this role can grant or remove it")
)

// ---- Start-up: built-in roles, auth's own permissions, old data ----

// Seed runs on every start, after auto-migration, in all replicas (the
// catalogue lock serialises them). Every step is idempotent and additive.
func (s *Service) Seed(ctx context.Context) error {
	return s.store.Tx(ctx, func(st *store.Store) error {
		if err := st.LockCatalog(ctx); err != nil {
			return err
		}
		now := s.now()
		for _, sr := range domain.SystemRoles {
			if err := st.EnsureRole(ctx, sr.Role(now)); err != nil {
				return err
			}
		}
		if _, err := s.register(ctx, st, "auth", auth.Permissions); err != nil {
			return err
		}
		n, err := st.BackfillUserRoles(ctx, now)
		if n > 0 {
			s.log.Info("gave existing users their built-in roles", "users", n)
		}
		return err
	})
}

// RegisterPermissions answers auth.RegisterPermissions (sent by every service
// at start-up).
func (s *Service) RegisterPermissions(ctx context.Context, q auth.RegisterPermissions) (auth.RegisterResult, error) {
	if q.Module == "" {
		return auth.RegisterResult{}, apperror.Invalid("invalid_module", "module is required")
	}
	var res auth.RegisterResult
	err := s.store.Tx(ctx, func(st *store.Store) error {
		if err := st.LockCatalog(ctx); err != nil {
			return err
		}
		var err error
		res, err = s.register(ctx, st, q.Module, q.Permissions)
		return err
	})
	if err == nil && (len(res.Added) > 0 || len(res.Deprecated) > 0) {
		s.log.Info("permissions registered", "for_module", q.Module, "added", res.Added, "deprecated", res.Deprecated)
	}
	return res, err
}

func (s *Service) register(ctx context.Context, st *store.Store, module string, defs []auth.PermissionDef) (auth.RegisterResult, error) {
	var res auth.RegisterResult
	seen := map[string]bool{}
	for _, d := range defs {
		if err := domain.ValidatePermissionDef(d); err != nil {
			return res, err
		}
		if seen[d.Code] {
			return res, apperror.Invalid("invalid_permission", "duplicate permission "+d.Code)
		}
		seen[d.Code] = true
	}
	catalog, err := st.Catalog(ctx)
	if err != nil {
		return res, err
	}
	now := s.now()
	for _, d := range defs {
		p := domain.Permission{Code: d.Code, Module: module, Description: d.Description, Audience: d.Audience, APIKey: d.APIKey}
		old, exists := catalog[d.Code]
		switch {
		case exists && old.Module != module:
			return res, apperror.Conflict("permission_owned_elsewhere",
				fmt.Sprintf("%s belongs to module %s", d.Code, old.Module))
		case exists:
			if old.Description != p.Description || old.Audience != p.Audience || old.APIKey != p.APIKey || !old.Active() {
				if err := st.UpdatePermission(ctx, p, now); err != nil {
					return res, err
				}
			}
		default:
			if err := st.InsertPermission(ctx, p, now); err != nil {
				return res, err
			}
			if err := st.AddDefaultPermission(ctx, p.Code, defaultRoleIDs(p, d.DefaultRoles), now); err != nil {
				return res, err
			}
			res.Added = append(res.Added, p.Code)
		}
	}
	for code, p := range catalog {
		if p.Module == module && p.Active() && !seen[code] {
			res.Deprecated = append(res.Deprecated, code)
		}
	}
	slices.Sort(res.Deprecated)
	return res, st.DeprecatePermissions(ctx, res.Deprecated, now)
}

// defaultRoleIDs: built-in roles named by DefaultRoles that may hold p.
func defaultRoleIDs(p domain.Permission, names []string) []string {
	var ids []string
	for _, sr := range domain.SystemRoles {
		if slices.Contains(names, sr.Name) && !sr.All && p.AllowedFor(sr.Audience) {
			ids = append(ids, domain.SystemRoleID(sr.Name))
		}
	}
	return ids
}

// ---- Effective access (what goes into the token) ----

func (s *Service) principal(ctx context.Context, st *store.Store, u *domain.User) (reqctx.Principal, []*domain.Role, error) {
	roles, err := st.UserRoles(ctx, u.ID)
	if err != nil {
		return reqctx.Principal{}, nil, err
	}
	roles = slices.DeleteFunc(roles, func(r *domain.Role) bool { // never another merchant's role
		return r.MerchantID != "" && r.MerchantID != u.MerchantID
	})
	catalog, err := st.Catalog(ctx)
	if err != nil {
		return reqctx.Principal{}, nil, err
	}
	return u.Principal(roles, catalog), roles, nil
}

// ---- Views ----

type PermissionView struct {
	Code        string `json:"code"`
	Description string `json:"description"`
	Module      string `json:"module"`
	Audience    string `json:"audience"`
	APIKey      bool   `json:"api_key"`
}

type RoleView struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Audience    string    `json:"audience"`
	MerchantID  string    `json:"merchant_id,omitempty"`
	BuiltIn     bool      `json:"built_in"`
	Locked      bool      `json:"locked"` // holds every permission, cannot be edited
	Permissions []string  `json:"permissions"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type RoleRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func roleView(r *domain.Role, c domain.Catalog) RoleView {
	perms := c.Effective([]*domain.Role{r}, r.Audience)
	return RoleView{ID: r.ID, Name: r.Name, Description: r.Description, Audience: r.Audience, MerchantID: r.MerchantID,
		BuiltIn: r.System, Locked: r.Locked(), Permissions: perms, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
}

// ---- Scope rules ----

// actorAudience: which side the actor is on.
func actorAudience(a reqctx.Principal) string { return domain.UserAudience(a.MerchantID) }

// roleScope decides which roles an actor works with. A merchant user always
// works on its own merchant; platform staff pick the audience (platform roles
// or the merchant templates).
func roleScope(a reqctx.Principal, audience string) (merchantID, aud string, err error) {
	if a.MerchantID != "" {
		return a.MerchantID, auth.AudienceMerchant, nil
	}
	switch audience {
	case "", auth.AudiencePlatform:
		return "", auth.AudiencePlatform, nil
	case auth.AudienceMerchant:
		return "", auth.AudienceMerchant, nil
	}
	return "", "", apperror.Invalid("invalid_audience", "audience must be platform or merchant")
}

func visible(a reqctx.Principal, r *domain.Role) bool {
	if a.MerchantID == "" {
		return true // platform staff see every role
	}
	return r.Audience == auth.AudienceMerchant && (r.MerchantID == "" || r.MerchantID == a.MerchantID)
}

// canEditRole: merchants edit only their own custom roles; platform staff
// edit platform roles and the merchant templates. Locked roles: nobody.
func canEditRole(a reqctx.Principal, r *domain.Role) error {
	if !visible(a, r) {
		return store.ErrRoleNotFound
	}
	if r.Locked() {
		return errRoleLocked
	}
	if a.MerchantID != "" && r.MerchantID != a.MerchantID {
		return errTemplateRole
	}
	if a.MerchantID == "" && r.MerchantID != "" {
		return apperror.Forbidden("merchant_role", "custom merchant roles are managed by the merchant")
	}
	return nil
}

// canGrantRole: on the actor's own side, a role can only be handed out by
// someone holding all of its permissions; an all-permissions role (owner,
// super_admin) only by someone holding that role.
func canGrantRole(a reqctx.Principal, r *domain.Role, c domain.Catalog) error {
	if r.Audience != actorAudience(a) {
		return nil // platform staff assigning merchant roles (e.g. the first owner)
	}
	if r.AllPermissions {
		if !slices.Contains(a.Roles, r.ID) {
			return errHolderMissing
		}
		return nil
	}
	return domain.CanGrant(a.Permissions, c.Effective([]*domain.Role{r}, r.Audience))
}

// canManageUser: who may change a user (roles, status). Never a user of
// another merchant, and never someone with more power than the actor.
func canManageUser(a reqctx.Principal, u *domain.User, roles []*domain.Role, c domain.Catalog) error {
	switch {
	case a.IsPlatform() && a.Can(auth.UsersManage):
	case a.MerchantID != "" && a.Can(auth.TeamManage) && u.MerchantID == a.MerchantID:
	default:
		return store.ErrUserNotFound // do not reveal users of other merchants
	}
	if domain.UserAudience(u.MerchantID) != actorAudience(a) {
		return nil
	}
	for _, r := range roles {
		if r.AllPermissions && !slices.Contains(a.Roles, r.ID) {
			return apperror.Forbidden("privilege_escalation", "only a holder of "+r.Name+" can change this user")
		}
	}
	if err := domain.CanGrant(a.Permissions, c.Effective(roles, u.Audience())); err != nil {
		return apperror.Forbidden("privilege_escalation", "this user has permissions you do not hold")
	}
	return nil
}

// roleFitsUser: merchant roles for merchant users (template or own custom
// role), platform roles for platform staff.
func roleFitsUser(r *domain.Role, u *domain.User) bool {
	if r.Audience != u.Audience() {
		return false
	}
	return r.MerchantID == "" || r.MerchantID == u.MerchantID
}

// resolveRoles turns role ids or names into roles that fit the user.
func resolveRoles(ctx context.Context, st *store.Store, refs []string, u *domain.User) ([]*domain.Role, error) {
	candidates, err := st.ListRoles(ctx, u.MerchantID, u.Audience())
	if err != nil {
		return nil, err
	}
	var out []*domain.Role
	for _, ref := range domain.Uniq(refs) {
		ref = strings.TrimSpace(ref)
		i := slices.IndexFunc(candidates, func(r *domain.Role) bool {
			return r.ID == ref || strings.EqualFold(r.Name, ref)
		})
		if i < 0 {
			return nil, apperror.Invalid("unknown_role", fmt.Sprintf("role %q does not exist for this user", ref))
		}
		if !slices.Contains(out, candidates[i]) {
			out = append(out, candidates[i])
		}
	}
	return out, nil
}

// ---- Permissions & roles API ----

func (s *Service) ListPermissions(ctx context.Context, a reqctx.Principal, audience string) ([]PermissionView, error) {
	_, aud, err := roleScope(a, audience)
	if err != nil {
		return nil, err
	}
	c, err := s.store.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	out := []PermissionView{}
	for _, p := range c {
		if p.Active() && p.AllowedFor(aud) {
			out = append(out, PermissionView{Code: p.Code, Description: p.Description, Module: p.Module,
				Audience: p.Audience, APIKey: p.APIKey})
		}
	}
	slices.SortFunc(out, func(x, y PermissionView) int { return strings.Compare(x.Code, y.Code) })
	return out, nil
}

func (s *Service) ListRoles(ctx context.Context, a reqctx.Principal, audience string) ([]RoleView, error) {
	merchantID, aud, err := roleScope(a, audience)
	if err != nil {
		return nil, err
	}
	roles, err := s.store.ListRoles(ctx, merchantID, aud)
	if err != nil {
		return nil, err
	}
	c, err := s.store.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]RoleView, len(roles))
	for i, r := range roles {
		out[i] = roleView(r, c)
	}
	return out, nil
}

func (s *Service) GetRole(ctx context.Context, a reqctx.Principal, id string) (RoleView, error) {
	r, err := s.store.RoleByID(ctx, id, false)
	if err != nil {
		return RoleView{}, err
	}
	if !visible(a, r) {
		return RoleView{}, store.ErrRoleNotFound
	}
	c, err := s.store.Catalog(ctx)
	if err != nil {
		return RoleView{}, err
	}
	return roleView(r, c), nil
}

type CreateRoleInput struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Audience    string   `json:"audience"` // platform staff only: platform (default) or merchant template
	Permissions []string `json:"permissions"`
}

func (s *Service) CreateRole(ctx context.Context, a reqctx.Principal, in CreateRoleInput) (RoleView, error) {
	merchantID, aud, err := roleScope(a, in.Audience)
	if err != nil {
		return RoleView{}, err
	}
	perms := domain.Uniq(in.Permissions)
	var view RoleView
	err = s.store.Tx(ctx, func(st *store.Store) error {
		c, err := st.Catalog(ctx)
		if err != nil {
			return err
		}
		if err := c.CheckAssignable(perms, aud); err != nil {
			return err
		}
		if aud == actorAudience(a) {
			if err := domain.CanGrant(a.Permissions, perms); err != nil {
				return err
			}
		}
		n, err := st.CountCustomRoles(ctx, merchantID)
		if err != nil {
			return err
		}
		if n >= domain.MaxCustomRoles {
			return errTooManyRoles
		}
		now := s.now()
		r, err := domain.NewRole(domain.NewRoleInput{MerchantID: merchantID, Audience: aud, Name: in.Name,
			Description: in.Description, Permissions: perms, CreatedBy: a.Subject}, now)
		if err != nil {
			return err
		}
		if taken, err := st.RoleNameTaken(ctx, merchantID, aud, r.Name, ""); err != nil || taken {
			if err == nil {
				err = store.ErrRoleTaken
			}
			return err
		}
		if err := st.CreateRole(ctx, r); err != nil {
			return err
		}
		if err := st.AddRolePermissions(ctx, r.ID, perms, now); err != nil {
			return err
		}
		r.Permissions = perms
		view = roleView(r, c)
		return st.Audit(ctx, store.Audit{ActorID: a.Subject, MerchantID: merchantID, Action: "role.created",
			TargetType: "role", TargetID: r.ID, Details: map[string]any{"name": r.Name, "permissions": perms}}, now)
	})
	return view, err
}

type UpdateRoleInput struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
}

func (s *Service) UpdateRole(ctx context.Context, a reqctx.Principal, id string, in UpdateRoleInput) (RoleView, error) {
	return s.changeRole(ctx, a, id, func(st *store.Store, r *domain.Role, now time.Time) (bool, error) {
		if r.System && in.Name != nil && *in.Name != r.Name {
			return false, apperror.Unprocessable("system_role", "built-in roles cannot be renamed")
		}
		next, err := domain.NewRole(domain.NewRoleInput{Name: valueOr(in.Name, r.Name),
			Description: valueOr(in.Description, r.Description)}, now)
		if err != nil {
			return false, err
		}
		if taken, err := st.RoleNameTaken(ctx, r.MerchantID, r.Audience, next.Name, r.ID); err != nil || taken {
			if err == nil {
				err = store.ErrRoleTaken
			}
			return false, err
		}
		r.Name, r.Description, r.UpdatedAt = next.Name, next.Description, now
		if err := st.UpdateRole(ctx, r); err != nil {
			return false, err
		}
		return false, st.Audit(ctx, store.Audit{ActorID: a.Subject, MerchantID: r.MerchantID, Action: "role.updated",
			TargetType: "role", TargetID: r.ID, Details: map[string]any{"name": r.Name}}, now)
	})
}

// AddRolePermissions adds permissions to a role. Users holding the role get
// them on their next token (refresh), at most AccessTTL later.
func (s *Service) AddRolePermissions(ctx context.Context, a reqctx.Principal, id string, codes []string) (RoleView, error) {
	codes = domain.Uniq(codes)
	return s.changeRole(ctx, a, id, func(st *store.Store, r *domain.Role, now time.Time) (bool, error) {
		c, err := st.Catalog(ctx)
		if err != nil {
			return false, err
		}
		if err := c.CheckAssignable(codes, r.Audience); err != nil {
			return false, err
		}
		if r.Audience == actorAudience(a) {
			if err := domain.CanGrant(a.Permissions, codes); err != nil {
				return false, err
			}
		}
		if err := st.AddRolePermissions(ctx, r.ID, codes, now); err != nil {
			return false, err
		}
		if err := st.TouchRole(ctx, r.ID, now); err != nil {
			return false, err
		}
		r.Permissions = domain.Uniq(append(r.Permissions, codes...))
		return false, st.Audit(ctx, store.Audit{ActorID: a.Subject, MerchantID: r.MerchantID,
			Action: "role.permissions_added", TargetType: "role", TargetID: r.ID,
			Details: map[string]any{"permissions": codes}}, now)
	})
}

// RemoveRolePermission takes a permission away from a role. Tokens of every
// holder become stale at once (AccessChanged), so it applies within seconds.
func (s *Service) RemoveRolePermission(ctx context.Context, a reqctx.Principal, id, code string) (RoleView, error) {
	return s.changeRole(ctx, a, id, func(st *store.Store, r *domain.Role, now time.Time) (bool, error) {
		if r.Audience == actorAudience(a) && !a.Can(code) {
			return false, domain.ErrEscalation(code) // cannot remove what you could not grant
		}
		removed, err := st.RemoveRolePermission(ctx, r.ID, code)
		if err != nil || !removed {
			if err == nil {
				err = apperror.NotFound("permission_not_in_role", "the role does not have "+code)
			}
			return false, err
		}
		if err := st.TouchRole(ctx, r.ID, now); err != nil {
			return false, err
		}
		r.Permissions = slices.DeleteFunc(r.Permissions, func(p string) bool { return p == code })
		return true, st.Audit(ctx, store.Audit{ActorID: a.Subject, MerchantID: r.MerchantID,
			Action: "role.permission_removed", TargetType: "role", TargetID: r.ID,
			Details: map[string]any{"permission": code}}, now)
	})
}

// changeRole loads and locks an editable role, applies fn, and publishes
// AccessChanged for the role when fn reports that access was reduced.
func (s *Service) changeRole(ctx context.Context, a reqctx.Principal, id string,
	fn func(st *store.Store, r *domain.Role, now time.Time) (revoke bool, err error)) (RoleView, error) {
	var view RoleView
	err := s.store.Tx(ctx, func(st *store.Store) error {
		r, err := st.RoleByID(ctx, id, true)
		if err != nil {
			return err
		}
		if err := canEditRole(a, r); err != nil {
			return err
		}
		now := s.now()
		revoke, err := fn(st, r, now)
		if err != nil {
			return err
		}
		if revoke {
			if err := s.events.Publish(ctx, st.DB(), auth.AccessChanged{RoleIDs: []string{r.ID}, NotBefore: now}); err != nil {
				return err
			}
		}
		c, err := st.Catalog(ctx)
		if err != nil {
			return err
		}
		view = roleView(r, c)
		return nil
	})
	return view, err
}

func (s *Service) DeleteRole(ctx context.Context, a reqctx.Principal, id string) error {
	return s.store.Tx(ctx, func(st *store.Store) error {
		r, err := st.RoleByID(ctx, id, true)
		if err != nil {
			return err
		}
		if err := canEditRole(a, r); err != nil {
			return err
		}
		if r.System {
			return errSystemRole
		}
		n, err := st.CountRoleUsers(ctx, r.ID)
		if err != nil {
			return err
		}
		if n > 0 {
			return errRoleInUse
		}
		if err := st.DeleteRole(ctx, r.ID); err != nil {
			return err
		}
		return st.Audit(ctx, store.Audit{ActorID: a.Subject, MerchantID: r.MerchantID, Action: "role.deleted",
			TargetType: "role", TargetID: r.ID, Details: map[string]any{"name": r.Name}}, s.now())
	})
}

// ---- Assigning roles to users ----

func (s *Service) AssignRole(ctx context.Context, a reqctx.Principal, userID, roleRef string) (UserView, error) {
	return s.changeUserRoles(ctx, a, userID, func(st *store.Store, u *domain.User, roles []*domain.Role, c domain.Catalog, now time.Time) error {
		target, err := resolveRoles(ctx, st, []string{roleRef}, u)
		if err != nil {
			return err
		}
		r := target[0]
		if !roleFitsUser(r, u) {
			return errRoleMismatch
		}
		if err := canGrantRole(a, r, c); err != nil {
			return err
		}
		added, err := st.AssignRole(ctx, u.ID, r.ID, a.Subject, now)
		if err != nil || !added {
			return err
		}
		return st.Audit(ctx, store.Audit{ActorID: a.Subject, MerchantID: u.MerchantID, Action: "user.role_assigned",
			TargetType: "user", TargetID: u.ID, Details: map[string]any{"role_id": r.ID, "role": r.Name}}, now)
	})
}

func (s *Service) UnassignRole(ctx context.Context, a reqctx.Principal, userID, roleID string) (UserView, error) {
	return s.changeUserRoles(ctx, a, userID, func(st *store.Store, u *domain.User, roles []*domain.Role, c domain.Catalog, now time.Time) error {
		i := slices.IndexFunc(roles, func(r *domain.Role) bool { return r.ID == roleID })
		if i < 0 {
			return apperror.NotFound("role_not_assigned", "the user does not have this role")
		}
		r := roles[i]
		if err := canGrantRole(a, r, c); err != nil {
			return err
		}
		if r.AllPermissions && u.Status == domain.StatusActive {
			n, err := st.CountActiveHolders(ctx, r.ID, u.MerchantID)
			if err != nil {
				return err
			}
			if n <= 1 {
				return errLastAdmin
			}
		}
		if _, err := st.UnassignRole(ctx, u.ID, r.ID); err != nil {
			return err
		}
		return st.Audit(ctx, store.Audit{ActorID: a.Subject, MerchantID: u.MerchantID, Action: "user.role_removed",
			TargetType: "user", TargetID: u.ID, Details: map[string]any{"role_id": r.ID, "role": r.Name}}, now)
	})
}

// changeUserRoles locks the user, checks the actor may manage them, applies
// fn, refreshes users.role and makes the user's current tokens stale.
func (s *Service) changeUserRoles(ctx context.Context, a reqctx.Principal, userID string,
	fn func(st *store.Store, u *domain.User, roles []*domain.Role, c domain.Catalog, now time.Time) error) (UserView, error) {
	if userID == a.Subject {
		return UserView{}, errOwnRoles
	}
	var view UserView
	err := s.store.Tx(ctx, func(st *store.Store) error {
		u, err := st.UserByID(ctx, userID, true)
		if err != nil {
			return err
		}
		_, roles, err := s.principal(ctx, st, u)
		if err != nil {
			return err
		}
		c, err := st.Catalog(ctx)
		if err != nil {
			return err
		}
		if err := canManageUser(a, u, roles, c); err != nil {
			return err
		}
		now := s.now()
		if err := fn(st, u, roles, c, now); err != nil {
			return err
		}
		p, roles, err := s.principal(ctx, st, u)
		if err != nil {
			return err
		}
		if err := st.SetPrimaryRole(ctx, u.ID, p.Role); err != nil {
			return err
		}
		u.Role = p.Role
		view = userView(u, roles, p.Permissions)
		return s.events.Publish(ctx, st.DB(), auth.AccessChanged{UserIDs: []string{u.ID}, NotBefore: now})
	})
	return view, err
}

func (s *Service) AuditLog(ctx context.Context, a reqctx.Principal) ([]store.AuditEntry, error) {
	return s.store.AuditLog(ctx, a.MerchantID, 200)
}

func valueOr(p *string, def string) string {
	if p == nil {
		return def
	}
	return *p
}
