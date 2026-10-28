// Package service is the auth service: dashboard login (users, sessions),
// dynamic RBAC (roles stored here, permissions registered by each service),
// team management and API key management, plus the API key -> access token
// exchange every service uses.
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"gorm.io/gorm"

	"paygate/internal/contracts/auth"
	"paygate/internal/contracts/merchants"
	"paygate/internal/modules/auth/internal/domain"
	"paygate/internal/modules/auth/internal/store"
	"paygate/internal/platform/apperror"
	"paygate/internal/platform/authn"
	"paygate/internal/platform/bus"
	"paygate/internal/platform/events"
	"paygate/internal/platform/mail"
	"paygate/internal/platform/reqctx"
	"paygate/internal/platform/secrets"
	"paygate/internal/platform/sms"
)

var (
	errLocked      = apperror.Unauthorized("account_locked", "too many failed logins; try again in a few minutes")
	errForbidden   = apperror.Forbidden("permission_denied", "you are not allowed to do this")
	errLastAdmin   = apperror.Unprocessable("last_admin", "this is the last active user with this role; add another first")
	errSelf        = apperror.Unprocessable("cannot_disable_self", "you cannot disable your own account")
	errTooManyKey  = apperror.Unprocessable("too_many_api_keys", "revoke an unused API key first (limit reached)")
	errNotApproved = apperror.Forbidden("merchant_not_approved",
		"your account is waiting for approval: test mode works now, live mode after approval")
)

type Config struct {
	Signer         *authn.Signer
	Pepper         domain.Pepper  // current
	PreviousPepper *domain.Pepper // optional, during a pepper rotation
	AccessTTL      time.Duration  // user access tokens (short: role changes apply on refresh)
	KeyTokenTTL    time.Duration  // tokens issued for API keys
	RefreshTTL     time.Duration

	BootstrapEmail    string // first super_admin, created once if none exists
	BootstrapPassword string
	BootstrapName     string

	Identity     map[string]*authn.OIDC // "google", "apple": enabled when client ids are configured
	Box          *secrets.Box           // encrypts authenticator secrets
	SMS          sms.Sender             // verification codes by SMS (send.et); nil = email only
	Mailer       mail.Sender
	DashboardURL string        // base of links in emails (password reset page)
	ResetTTL     time.Duration // password reset link lifetime
}

type Service struct {
	store  *store.Store
	bus    bus.Bus
	events events.Publisher
	cfg    Config
	log    *slog.Logger
	now    func() time.Time
}

func New(st *store.Store, b bus.Bus, pub events.Publisher, cfg Config, log *slog.Logger) *Service {
	if cfg.AccessTTL == 0 {
		cfg.AccessTTL = 5 * time.Minute
	}
	if cfg.KeyTokenTTL == 0 {
		cfg.KeyTokenTTL = 5 * time.Minute
	}
	if cfg.RefreshTTL == 0 {
		cfg.RefreshTTL = 7 * 24 * time.Hour
	}
	if cfg.ResetTTL == 0 {
		cfg.ResetTTL = 30 * time.Minute
	}
	cfg.DashboardURL = strings.TrimRight(cfg.DashboardURL, "/")
	return &Service{store: st, bus: b, events: pub, cfg: cfg, log: log, now: func() time.Time { return time.Now().UTC() }}
}

// ---- Views ----

type UserView struct {
	ID          string     `json:"id"`
	Email       string     `json:"email"`
	Name        string     `json:"name"`
	MerchantID  string     `json:"merchant_id,omitempty"`
	Role        string     `json:"role"` // primary role, for display
	Roles       []RoleRef  `json:"roles"`
	Permissions []string   `json:"permissions"` // union of all roles' permissions
	Status      string     `json:"status"`
	LastLoginAt *time.Time `json:"last_login_at"`
	CreatedAt   time.Time  `json:"created_at"`
}

func userView(u *domain.User, roles []*domain.Role, perms []string) UserView {
	refs := make([]RoleRef, len(roles))
	for i, r := range roles {
		refs[i] = RoleRef{ID: r.ID, Name: r.Name}
	}
	if perms == nil {
		perms = []string{}
	}
	return UserView{ID: u.ID, Email: u.Email, Name: u.Name, MerchantID: u.MerchantID, Role: u.Role, Roles: refs,
		Permissions: perms, Status: u.Status, LastLoginAt: u.LastLoginAt, CreatedAt: u.CreatedAt}
}

// Tokens is the answer to every sign-in. With an authenticator enabled the
// first step returns only MFARequired + MFAToken; POST /auth/login/mfa with
// the app's code returns the access and refresh tokens.
type Tokens struct {
	AccessToken      string     `json:"access_token,omitempty"`
	TokenType        string     `json:"token_type,omitempty"`
	ExpiresIn        int        `json:"expires_in,omitempty"`
	RefreshToken     string     `json:"refresh_token,omitempty"`
	RefreshExpiresAt *time.Time `json:"refresh_expires_at,omitempty"`
	User             *UserView  `json:"user,omitempty"`

	MFARequired  bool   `json:"mfa_required,omitempty"`
	MFAToken     string `json:"mfa_token,omitempty"`
	MFAExpiresIn int    `json:"mfa_expires_in,omitempty"`
}

// ---- Login / sessions ----

func (s *Service) Login(ctx context.Context, email, password string) (Tokens, error) {
	var (
		u       *domain.User
		outcome error
	)
	// The failed-attempt counter must be saved even when login fails, so the
	// transaction commits and the outcome is returned afterwards.
	err := s.store.Tx(ctx, func(st *store.Store) error {
		var err error
		u, err = st.UserByEmail(ctx, normEmail(email))
		if errors.Is(err, store.ErrUserNotFound) {
			domain.BurnPasswordCheck(password)
			outcome = auth.ErrInvalidCredentials
			return nil
		}
		if err != nil {
			return err
		}
		now := s.now()
		if u.Locked(now) {
			outcome = errLocked
			return nil
		}
		if !domain.CheckPassword(password, u.PasswordHash) || u.Status != domain.StatusActive {
			u.LoginFailed(now)
			outcome = auth.ErrInvalidCredentials
			return st.SaveUser(ctx, u)
		}
		u.LoginSucceeded(now)
		return st.SaveUser(ctx, u)
	})
	if err != nil {
		return Tokens{}, err
	}
	if outcome != nil {
		return Tokens{}, outcome
	}
	if err := s.merchantAllowed(ctx, u.MerchantID, false); err != nil {
		return Tokens{}, err
	}
	if !u.Verified() && !u.VerificationExempt() {
		// Correct password but the account was never verified: send a code
		// (respecting the send limits) and ask for it. No token until then.
		if err := s.issueCode(ctx, u, ChannelEmail); err != nil {
			s.log.Error("send verification code", "user_id", u.ID, "err", err)
		}
		return Tokens{}, errVerifyRequired
	}
	return s.complete(ctx, u)
}

func (s *Service) startSession(ctx context.Context, u *domain.User) (Tokens, error) {
	sess, refresh, err := domain.NewSession(u.ID, s.cfg.RefreshTTL, s.now())
	if err != nil {
		return Tokens{}, err
	}
	if err := s.store.CreateSession(ctx, sess); err != nil {
		return Tokens{}, err
	}
	return s.tokens(ctx, u, refresh, sess.ExpiresAt)
}

// tokens issues the access token. Roles and permissions are read from the
// database every time (login and each refresh), never copied from an old token.
func (s *Service) tokens(ctx context.Context, u *domain.User, refresh string, refreshExp time.Time) (Tokens, error) {
	p, roles, err := s.principal(ctx, s.store, u)
	if err != nil {
		return Tokens{}, err
	}
	access, _, err := s.cfg.Signer.Issue(p, s.cfg.AccessTTL)
	if err != nil {
		return Tokens{}, err
	}
	view := userView(u, roles, p.Permissions)
	return Tokens{AccessToken: access, TokenType: "Bearer", ExpiresIn: int(s.cfg.AccessTTL.Seconds()),
		RefreshToken: refresh, RefreshExpiresAt: &refreshExp, User: &view}, nil
}

// Refresh rotates the refresh token. Presenting a token that was already
// rotated means it was copied: every session of that user is revoked.
func (s *Service) Refresh(ctx context.Context, raw string) (Tokens, error) {
	var (
		u         *domain.User
		newRaw    string
		newExp    time.Time
		outcome   error
		hashOfRaw = domain.HashRefreshToken(raw)
	)
	err := s.store.Tx(ctx, func(st *store.Store) error {
		old, err := st.SessionByHash(ctx, hashOfRaw)
		if err != nil {
			return err
		}
		now := s.now()
		if old.RevokedAt != nil {
			if old.ReplacedBy != nil {
				s.log.Warn("refresh token reuse detected, revoking all sessions", "user_id", old.UserID)
				outcome = store.ErrSessionNotFound
				return st.RevokeUserSessions(ctx, old.UserID, now)
			}
			return store.ErrSessionNotFound
		}
		if !now.Before(old.ExpiresAt) {
			return store.ErrSessionNotFound
		}
		if u, err = st.UserByID(ctx, old.UserID, false); err != nil {
			return err
		}
		if u.Status != domain.StatusActive {
			return store.ErrSessionNotFound
		}
		next, r, err := domain.NewSession(u.ID, s.cfg.RefreshTTL, now)
		if err != nil {
			return err
		}
		if err := st.CreateSession(ctx, next); err != nil {
			return err
		}
		newRaw, newExp = r, next.ExpiresAt
		return st.RevokeSession(ctx, old.ID, &next.ID, now)
	})
	if err != nil {
		return Tokens{}, err
	}
	if outcome != nil {
		return Tokens{}, outcome
	}
	if err := s.merchantAllowed(ctx, u.MerchantID, false); err != nil {
		return Tokens{}, err
	}
	return s.tokens(ctx, u, newRaw, newExp)
}

func (s *Service) Logout(ctx context.Context, raw string) error {
	return s.store.Tx(ctx, func(st *store.Store) error {
		sess, err := st.SessionByHash(ctx, domain.HashRefreshToken(raw))
		if errors.Is(err, store.ErrSessionNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		return st.RevokeSession(ctx, sess.ID, nil, s.now())
	})
}

// ChangePassword also signs the user out everywhere else.
// ChangePassword needs the current password AND a code from the user's
// authenticator app (Microsoft Authenticator). Without an authenticator set
// up, the change is refused: set one up first (/auth/mfa/totp/setup).
func (s *Service) ChangePassword(ctx context.Context, p reqctx.Principal, current, next, totpCode string) error {
	var wrong error
	err := s.store.Tx(ctx, func(st *store.Store) error {
		u, err := st.UserByID(ctx, p.Subject, true)
		if err != nil {
			return err
		}
		if !u.MFAEnabled() {
			return errMFARequired
		}
		if !domain.CheckPassword(current, u.PasswordHash) {
			u.LoginFailed(s.now())
			wrong = auth.ErrInvalidCredentials
			return st.SaveUser(ctx, u)
		}
		if err := s.checkSecondFactor(ctx, st, u, totpCode, false); err != nil {
			if err == errBadMFACode || err == errLocked {
				wrong = err
				return nil // keep the failed-attempt count
			}
			return err
		}
		if u.PasswordHash, err = domain.HashPassword(next); err != nil {
			return err
		}
		u.UpdatedAt = s.now()
		if err := st.SaveUser(ctx, u); err != nil {
			return err
		}
		if err := st.RevokeUserSessions(ctx, u.ID, s.now()); err != nil {
			return err
		}
		if err := st.Audit(ctx, store.Audit{ActorID: u.ID, MerchantID: u.MerchantID, Action: "user.password_changed",
			TargetType: "user", TargetID: u.ID}, s.now()); err != nil {
			return err
		}
		s.notify(u.Email, "Your paygate password was changed",
			"Your password was changed and you were signed out everywhere else.\nIf this was not you, contact support at once.\n")
		return s.events.Publish(ctx, st.DB(), auth.AccessChanged{UserIDs: []string{u.ID}, NotBefore: s.now()})
	})
	if err != nil {
		return err
	}
	return wrong
}

func (s *Service) Me(ctx context.Context, p reqctx.Principal) (any, error) {
	if p.Kind != reqctx.KindUser {
		return p, nil
	}
	u, err := s.store.UserByID(ctx, p.Subject, false)
	if err != nil {
		return nil, err
	}
	cur, roles, err := s.principal(ctx, s.store, u)
	if err != nil {
		return nil, err
	}
	return userView(u, roles, cur.Permissions), nil
}

// ---- Users (platform staff and merchant teams) ----

type CreateUserInput struct {
	Email      string   `json:"email"`
	Name       string   `json:"name"`
	Password   string   `json:"password"`
	MerchantID string   `json:"merchant_id"` // platform staff only; team managers always add to their own merchant
	Roles      []string `json:"roles"`       // role ids or names
	Role       string   `json:"role"`        // shorthand for one role
}

// CreateUser is used by platform staff (any merchant, or platform staff) and
// by merchant team managers (their own merchant only). Every role given must
// pass the anti-escalation rules.
func (s *Service) CreateUser(ctx context.Context, actor reqctx.Principal, in CreateUserInput) (UserView, error) {
	switch {
	case actor.IsPlatform() && actor.Can(auth.UsersManage):
		if in.MerchantID != "" {
			if _, err := s.merchantStatus(ctx, in.MerchantID); err != nil {
				return UserView{}, err
			}
		}
	case actor.MerchantID != "" && actor.Can(auth.TeamManage):
		in.MerchantID = actor.MerchantID
	default:
		return UserView{}, errForbidden
	}
	refs := in.Roles
	if in.Role != "" {
		refs = append(refs, in.Role)
	}
	if len(refs) == 0 {
		return UserView{}, errNeedRole
	}
	u, err := domain.NewUser(domain.NewUserInput{Email: in.Email, Name: in.Name, Password: in.Password,
		MerchantID: in.MerchantID, CreatedBy: actor.Subject}, s.now())
	if err != nil {
		return UserView{}, err
	}
	if actor.IsPlatform() {
		// Created by platform staff: an administrator vouched for this
		// account, so it skips email verification. Users invited by a
		// merchant verify their email on first login.
		u.EmailVerifiedAt = &u.CreatedAt
	}
	var view UserView
	err = s.store.Tx(ctx, func(st *store.Store) error {
		roles, err := resolveRoles(ctx, st, refs, u)
		if err != nil {
			return err
		}
		c, err := st.Catalog(ctx)
		if err != nil {
			return err
		}
		for _, r := range roles {
			if err := canGrantRole(actor, r, c); err != nil {
				return err
			}
		}
		u.Role = roles[0].Name
		if err := st.CreateUser(ctx, u); err != nil {
			return err
		}
		names := make([]string, len(roles))
		for i, r := range roles {
			names[i] = r.Name
			if _, err := st.AssignRole(ctx, u.ID, r.ID, actor.Subject, u.CreatedAt); err != nil {
				return err
			}
		}
		view = userView(u, roles, c.Effective(roles, u.Audience()))
		return st.Audit(ctx, store.Audit{ActorID: actor.Subject, MerchantID: u.MerchantID, Action: "user.created",
			TargetType: "user", TargetID: u.ID, Details: map[string]any{"email": u.Email, "roles": names}}, u.CreatedAt)
	})
	return view, err
}

func (s *Service) ListUsers(ctx context.Context, merchantID string) ([]UserView, error) {
	us, err := s.store.ListUsers(ctx, merchantID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(us))
	for i, u := range us {
		ids[i] = u.ID
	}
	byUser, err := s.store.UsersRoles(ctx, ids)
	if err != nil {
		return nil, err
	}
	c, err := s.store.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]UserView, len(us))
	for i, u := range us {
		roles := byUser[u.ID]
		out[i] = userView(u, roles, c.Effective(roles, u.Audience()))
	}
	return out, nil
}

// DisableUser blocks login, ends the user's sessions and makes their access
// tokens stale everywhere (AccessChanged), so it applies within seconds.
func (s *Service) DisableUser(ctx context.Context, actor reqctx.Principal, id string) error {
	if id == actor.Subject {
		return errSelf
	}
	return s.store.Tx(ctx, func(st *store.Store) error {
		u, err := st.UserByID(ctx, id, true)
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
		if err := canManageUser(actor, u, roles, c); err != nil {
			return err
		}
		if u.Status == domain.StatusDisabled {
			return nil
		}
		for _, r := range roles {
			if !r.AllPermissions {
				continue
			}
			n, err := st.CountActiveHolders(ctx, r.ID, u.MerchantID)
			if err != nil {
				return err
			}
			if n <= 1 {
				return errLastAdmin
			}
		}
		now := s.now()
		u.Status, u.UpdatedAt = domain.StatusDisabled, now
		if err := st.SaveUser(ctx, u); err != nil {
			return err
		}
		if err := st.RevokeUserSessions(ctx, u.ID, now); err != nil {
			return err
		}
		if err := st.Audit(ctx, store.Audit{ActorID: actor.Subject, MerchantID: u.MerchantID, Action: "user.disabled",
			TargetType: "user", TargetID: u.ID}, now); err != nil {
			return err
		}
		return s.events.Publish(ctx, st.DB(), auth.AccessChanged{UserIDs: []string{u.ID}, NotBefore: now})
	})
}

// Bootstrap creates the first super_admin when nobody active holds the role
// (first deploy).
// Bootstrap seeds the first super_admin from the environment
// (BOOTSTRAP_ADMIN_EMAIL / _PASSWORD / _NAME). It runs on every start but only
// creates the admin while no active super_admin exists, so changing the
// password later in the app is not undone by a restart. To reset a lost
// admin, use "forgot password" (or disable the old admin and restart).
func (s *Service) Bootstrap(ctx context.Context) error {
	if s.cfg.BootstrapEmail == "" {
		s.log.Info("no BOOTSTRAP_ADMIN_EMAIL: no admin seeded")
		return nil
	}
	superAdmin := domain.SystemRoleID(auth.RoleSuperAdmin)
	n, err := s.store.CountActiveHolders(ctx, superAdmin, "")
	if err != nil || n > 0 {
		return err
	}
	name := s.cfg.BootstrapName
	if name == "" {
		name = "Administrator"
	}
	u, err := domain.NewUser(domain.NewUserInput{Email: s.cfg.BootstrapEmail, Name: name,
		Password: s.cfg.BootstrapPassword}, s.now())
	if err != nil {
		return fmt.Errorf("BOOTSTRAP_ADMIN_*: %w", err)
	}
	u.Role = auth.RoleSuperAdmin
	u.EmailVerifiedAt = &u.CreatedAt // set by the operator in configuration
	err = s.store.Tx(ctx, func(st *store.Store) error {
		if err := st.CreateUser(ctx, u); err != nil {
			return err
		}
		_, err := st.AssignRole(ctx, u.ID, superAdmin, "", u.CreatedAt)
		return err
	})
	if errors.Is(err, store.ErrEmailTaken) {
		s.log.Warn("bootstrap admin email exists but holds no super_admin role; assign it manually", "email", u.Email)
		return nil
	}
	if err != nil {
		return err
	}
	s.log.Info("bootstrap super_admin created; change its password", "email", u.Email)
	return nil
}

// ---- API keys ----

type KeyView struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Prefix      string     `json:"prefix"`
	Last4       string     `json:"last4"`
	Environment string     `json:"environment"`
	Scopes      []string   `json:"scopes"`
	Status      string     `json:"status"` // active | expired | revoked
	ExpiresAt   *time.Time `json:"expires_at"`
	RevokedAt   *time.Time `json:"revoked_at"`
	LastUsedAt  *time.Time `json:"last_used_at"`
	CreatedBy   string     `json:"created_by,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

type CreatedKey struct {
	KeyView
	Secret  string `json:"secret"`
	Warning string `json:"warning"`
}

func (s *Service) keyView(k *domain.APIKey) KeyView {
	status := "active"
	switch {
	case k.RevokedAt != nil:
		status = "revoked"
	case !k.Usable(s.now()):
		status = "expired"
	}
	return KeyView{ID: k.ID, Name: k.Name, Prefix: k.Prefix, Last4: k.Last4, Environment: k.Environment,
		Scopes: k.Scopes, Status: status, ExpiresAt: k.ExpiresAt, RevokedAt: k.RevokedAt, LastUsedAt: k.LastUsedAt,
		CreatedBy: k.CreatedBy, CreatedAt: k.CreatedAt}
}

func created(v KeyView, secret string) CreatedKey {
	return CreatedKey{KeyView: v, Secret: secret,
		Warning: "Store this key in your secret manager now. It is shown only once and cannot be recovered."}
}

type CreateKeyInput struct {
	Name          string   `json:"name"`
	Environment   string   `json:"environment"`
	Scopes        []string `json:"scopes"`
	ExpiresInDays int      `json:"expires_in_days"`
}

// CreateKey: only a logged-in merchant user with apikeys:manage, for their
// own merchant, with at most the permissions they hold themselves.
func (s *Service) CreateKey(ctx context.Context, actor reqctx.Principal, in CreateKeyInput) (CreatedKey, error) {
	if actor.Kind != reqctx.KindUser || actor.MerchantID == "" || !actor.Can(auth.APIKeysManage) {
		return CreatedKey{}, errForbidden
	}
	if err := s.merchantAllowed(ctx, actor.MerchantID, in.Environment == "live"); err != nil {
		return CreatedKey{}, err
	}
	c, err := s.store.Catalog(ctx)
	if err != nil {
		return CreatedKey{}, err
	}
	now := s.now()
	k, raw, err := domain.NewKey(domain.NewKeyInput{MerchantID: actor.MerchantID, Name: in.Name,
		Environment: in.Environment, Scopes: in.Scopes, ExpiresInDays: in.ExpiresInDays,
		CreatedBy: actor.Subject, CreatorPermissions: actor.Permissions, AllowedScopes: c.APIKeyScopes()}, s.cfg.Pepper, now)
	if err != nil {
		return CreatedKey{}, err
	}
	n, err := s.store.CountUsableKeys(ctx, actor.MerchantID, now)
	if err != nil {
		return CreatedKey{}, err
	}
	if n >= domain.MaxKeys {
		return CreatedKey{}, errTooManyKey
	}
	if err := s.store.CreateKey(ctx, k); err != nil {
		return CreatedKey{}, err
	}
	s.log.Info("api key created", "merchant_id", k.MerchantID, "key_id", k.ID, "by", actor.Subject, "env", k.Environment)
	return created(s.keyView(k), raw), nil
}

func (s *Service) ListKeys(ctx context.Context, merchantID string) ([]KeyView, error) {
	ks, err := s.store.ListKeys(ctx, merchantID)
	if err != nil {
		return nil, err
	}
	out := make([]KeyView, len(ks))
	for i, k := range ks {
		out[i] = s.keyView(k)
	}
	return out, nil
}

func (s *Service) RevokeKey(ctx context.Context, actor reqctx.Principal, merchantID, id string) error {
	if err := s.store.RevokeKey(ctx, merchantID, id, s.now()); err != nil {
		return err
	}
	s.log.Info("api key revoked", "merchant_id", merchantID, "key_id", id, "by", actor.Subject)
	return nil
}

// RollKey replaces a key: the new key gets the same name, environment and
// scopes; the old one keeps working for graceHours so deployments can switch
// over, then expires. graceHours 0 ends the old key immediately.
func (s *Service) RollKey(ctx context.Context, actor reqctx.Principal, id string, graceHours, expiresInDays int) (CreatedKey, error) {
	if actor.Kind != reqctx.KindUser || actor.MerchantID == "" || !actor.Can(auth.APIKeysManage) {
		return CreatedKey{}, errForbidden
	}
	if graceHours < 0 || graceHours > 7*24 {
		return CreatedKey{}, apperror.Invalid("invalid_grace", "grace_hours must be between 0 and 168")
	}
	var out CreatedKey
	err := s.store.Tx(ctx, func(st *store.Store) error {
		now := s.now()
		c, err := st.Catalog(ctx)
		if err != nil {
			return err
		}
		old, err := st.KeyByID(ctx, actor.MerchantID, id)
		if err != nil {
			return err
		}
		if !old.Usable(now) {
			return apperror.Unprocessable("api_key_not_active", "only an active key can be rolled")
		}
		k, raw, err := domain.NewKey(domain.NewKeyInput{MerchantID: old.MerchantID, Name: old.Name,
			Environment: old.Environment, Scopes: old.Scopes, ExpiresInDays: expiresInDays,
			CreatedBy: actor.Subject, CreatorPermissions: actor.Permissions, AllowedScopes: c.APIKeyScopes()}, s.cfg.Pepper, now)
		if err != nil {
			return err
		}
		if err := st.CreateKey(ctx, k); err != nil {
			return err
		}
		end := now.Add(time.Duration(graceHours) * time.Hour)
		if old.ExpiresAt == nil || end.Before(*old.ExpiresAt) {
			if err := st.SetKeyExpiry(ctx, old.ID, end); err != nil {
				return err
			}
		}
		out = created(s.keyView(k), raw)
		return nil
	})
	if err == nil {
		s.log.Info("api key rolled", "merchant_id", actor.MerchantID, "old_key_id", id, "new_key_id", out.ID, "by", actor.Subject)
	}
	return out, err
}

// ExchangeAPIKey verifies a raw key and returns a short-lived signed token
// carrying the key's merchant, environment and scopes.
func (s *Service) ExchangeAPIKey(ctx context.Context, raw string) (auth.AccessToken, error) {
	if !domain.WellFormed(raw) {
		return auth.AccessToken{}, auth.ErrInvalidKey // typo or garbage: no database lookup
	}
	peppers := []domain.Pepper{s.cfg.Pepper}
	if s.cfg.PreviousPepper != nil {
		peppers = append(peppers, *s.cfg.PreviousPepper)
	}
	hashes := make([]string, len(peppers))
	for i, p := range peppers {
		hashes[i] = domain.HashKey(raw, p)
	}
	k, err := s.store.KeyByHashes(ctx, hashes)
	if errors.Is(err, store.ErrKeyNotFound) {
		return auth.AccessToken{}, auth.ErrInvalidKey
	}
	if err != nil {
		return auth.AccessToken{}, err
	}
	now := s.now()
	if !k.Usable(now) {
		return auth.AccessToken{}, auth.ErrInvalidKey
	}
	if err := s.merchantAllowed(ctx, k.MerchantID, k.Environment == "live"); err != nil {
		return auth.AccessToken{}, err
	}
	if k.PepperVersion != s.cfg.Pepper.Version {
		if err := s.store.Rehash(ctx, k.ID, hashes[0], s.cfg.Pepper.Version); err != nil {
			s.log.Warn("rehash api key", "key_id", k.ID, "err", err)
		}
	}
	if k.LastUsedAt == nil || now.Sub(*k.LastUsedAt) > time.Minute { // throttle writes
		if err := s.store.TouchKey(ctx, k.ID, now); err != nil {
			s.log.Warn("touch api key", "key_id", k.ID, "err", err)
		}
	}
	// A scope whose permission was deprecated stops working.
	c, err := s.store.Catalog(ctx)
	if err != nil {
		return auth.AccessToken{}, err
	}
	scopes := slices.DeleteFunc(slices.Clone(k.Scopes), func(code string) bool {
		p, ok := c[code]
		return !ok || !p.Active()
	})
	token, exp, err := s.cfg.Signer.Issue(reqctx.Principal{Kind: reqctx.KindAPIKey, Subject: k.ID,
		MerchantID: k.MerchantID, Environment: k.Environment, Permissions: scopes}, s.cfg.KeyTokenTTL)
	if err != nil {
		return auth.AccessToken{}, err
	}
	return auth.AccessToken{Token: token, ExpiresAt: exp}, nil
}

// ---- Merchant status (local copy, fed by merchant.updated events) ----

func (s *Service) OnMerchantUpdated(ctx context.Context, env bus.Envelope, e merchants.MerchantUpdated) error {
	return events.HandleOnce(ctx, s.store.DB(), "auth.on_merchant_updated", env, func(tx *gorm.DB) error {
		return store.New(tx).UpsertMerchant(ctx, e.ID, e.Status, e.UpdatedAt)
	})
}

// merchantStatus reads the local copy, asking the merchant service only for a
// merchant this service has not heard of yet.
func (s *Service) merchantStatus(ctx context.Context, id string) (string, error) {
	status, found, err := s.store.MerchantStatus(ctx, id)
	if err != nil || found {
		return status, err
	}
	m, err := bus.Ask(ctx, s.bus, merchants.GetMerchant{ID: id})
	if err != nil {
		return "", err
	}
	// Epoch timestamp: any merchant.updated event will overwrite this row.
	if err := s.store.UpsertMerchant(ctx, m.ID, m.Status, time.Unix(0, 0).UTC()); err != nil {
		return "", err
	}
	return m.Status, nil
}

// merchantAllowed: platform staff ("") always pass. A pending merchant
// (signed up, not approved yet) can log in and use test mode; live mode
// (live keys) needs an approved, active merchant.
func (s *Service) merchantAllowed(ctx context.Context, merchantID string, live bool) error {
	if merchantID == "" {
		return nil
	}
	status, err := s.merchantStatus(ctx, merchantID)
	if errors.Is(err, merchants.ErrNotFound) {
		return auth.ErrMerchantInactive
	}
	if err != nil {
		return err
	}
	switch {
	case status == merchants.StatusActive:
		return nil
	case status == merchants.StatusPending && !live:
		return nil
	case status == merchants.StatusPending:
		return errNotApproved
	}
	return auth.ErrMerchantInactive
}

// ---- Merchant self-signup ----

type SignupInput struct {
	BusinessName string `json:"business_name"`
	Name         string `json:"name"` // the person signing up
	Email        string `json:"email"`
	Password     string `json:"password"`
	Phone        string `json:"phone"`
	Website      string `json:"website"`
	Channel      string `json:"verification_channel"` // email (default) or sms
}

// Signup creates a pending merchant and its owner, and logs the owner in.
// Test mode works at once; live keys wait until staff approve the merchant.
// Signup creates a pending merchant and its owner, then sends a 6-digit code.
// No token is issued until the code is confirmed (POST /auth/verify).
func (s *Service) Signup(ctx context.Context, in SignupInput) (SignupResult, error) {
	if strings.TrimSpace(in.Name) == "" {
		in.Name = in.BusinessName
	}
	u, err := domain.NewUser(domain.NewUserInput{Email: in.Email, Name: in.Name, Password: in.Password}, s.now())
	if err != nil {
		return SignupResult{}, err
	}
	if u.Phone, err = domain.NormalizePhone(in.Phone); err != nil {
		return SignupResult{}, err
	}
	if taken, err := s.store.EmailTaken(ctx, u.Email); err != nil || taken {
		if err == nil {
			err = store.ErrEmailTaken
		}
		return SignupResult{}, err
	}
	m, err := bus.Ask(ctx, s.bus, merchants.CreateMerchant{Name: in.BusinessName, Email: u.Email,
		Phone: u.Phone, Website: in.Website})
	if err != nil {
		return SignupResult{}, err
	}
	// Known locally right away (the merchant.updated event may arrive later).
	if err := s.store.UpsertMerchant(ctx, m.ID, m.Status, time.Unix(0, 0).UTC()); err != nil {
		return SignupResult{}, err
	}
	u.MerchantID, u.Role = m.ID, auth.RoleOwner
	err = s.store.Tx(ctx, func(st *store.Store) error {
		if err := st.CreateUser(ctx, u); err != nil {
			return err
		}
		if _, err := st.AssignRole(ctx, u.ID, domain.SystemRoleID(auth.RoleOwner), "", u.CreatedAt); err != nil {
			return err
		}
		return st.Audit(ctx, store.Audit{ActorID: u.ID, MerchantID: m.ID, Action: "merchant.signed_up",
			TargetType: "merchant", TargetID: m.ID, Details: map[string]any{"business_name": m.Name, "email": u.Email}}, u.CreatedAt)
	})
	if err != nil {
		return SignupResult{}, err
	}
	s.log.Info("merchant signed up", "merchant_id", m.ID, "user_id", u.ID)
	channel := ChannelEmail
	if in.Channel == ChannelSMS && u.Phone != "" && s.cfg.SMS != nil {
		channel = ChannelSMS
	}
	if err := s.issueCode(ctx, u, channel); err != nil {
		return SignupResult{}, err
	}
	where := "your email"
	if channel == ChannelSMS {
		where = "your phone"
	}
	return SignupResult{VerificationRequired: true, Email: u.Email, Channel: channel,
		Message: "We sent a 6-digit code to " + where + ". Enter it to activate your account (POST /auth/verify)."}, nil
}

func normEmail(e string) string { return strings.ToLower(strings.TrimSpace(e)) }
