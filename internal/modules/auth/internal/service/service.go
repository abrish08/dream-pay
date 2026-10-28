// Package service is the auth service: dashboard login (users, sessions),
// dynamic RBAC (roles stored here, permissions registered by each service),
// team management and API key management, plus the API key -> access token
// exchange every service uses.
package service

import (
	"context"
	"errors"
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
	"paygate/internal/platform/reqctx"
)

var (
	errLocked     = apperror.Unauthorized("account_locked", "too many failed logins; try again in a few minutes")
	errForbidden  = apperror.Forbidden("permission_denied", "you are not allowed to do this")
	errLastAdmin  = apperror.Unprocessable("last_admin", "this is the last active user with this role; add another first")
	errSelf       = apperror.Unprocessable("cannot_disable_self", "you cannot disable your own account")
	errTooManyKey = apperror.Unprocessable("too_many_api_keys", "revoke an unused API key first (limit reached)")
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

type Tokens struct {
	AccessToken      string    `json:"access_token"`
	TokenType        string    `json:"token_type"`
	ExpiresIn        int       `json:"expires_in"`
	RefreshToken     string    `json:"refresh_token"`
	RefreshExpiresAt time.Time `json:"refresh_expires_at"`
	User             UserView  `json:"user"`
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
	if err := s.merchantActive(ctx, u.MerchantID); err != nil {
		return Tokens{}, err
	}
	return s.startSession(ctx, u)
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
	return Tokens{AccessToken: access, TokenType: "Bearer", ExpiresIn: int(s.cfg.AccessTTL.Seconds()),
		RefreshToken: refresh, RefreshExpiresAt: refreshExp, User: userView(u, roles, p.Permissions)}, nil
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
	if err := s.merchantActive(ctx, u.MerchantID); err != nil {
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
func (s *Service) ChangePassword(ctx context.Context, p reqctx.Principal, current, next string) error {
	return s.store.Tx(ctx, func(st *store.Store) error {
		u, err := st.UserByID(ctx, p.Subject, true)
		if err != nil {
			return err
		}
		if !domain.CheckPassword(current, u.PasswordHash) {
			return auth.ErrInvalidCredentials
		}
		if u.PasswordHash, err = domain.HashPassword(next); err != nil {
			return err
		}
		u.UpdatedAt = s.now()
		if err := st.SaveUser(ctx, u); err != nil {
			return err
		}
		return st.RevokeUserSessions(ctx, u.ID, s.now())
	})
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
func (s *Service) Bootstrap(ctx context.Context) error {
	if s.cfg.BootstrapEmail == "" {
		return nil
	}
	superAdmin := domain.SystemRoleID(auth.RoleSuperAdmin)
	n, err := s.store.CountActiveHolders(ctx, superAdmin, "")
	if err != nil || n > 0 {
		return err
	}
	u, err := domain.NewUser(domain.NewUserInput{Email: s.cfg.BootstrapEmail, Name: "Administrator",
		Password: s.cfg.BootstrapPassword}, s.now())
	if err != nil {
		return err
	}
	u.Role = auth.RoleSuperAdmin
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
	if err := s.merchantActive(ctx, actor.MerchantID); err != nil {
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
	if err := s.merchantActive(ctx, k.MerchantID); err != nil {
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

// merchantActive is a no-op for platform staff (merchantID "").
func (s *Service) merchantActive(ctx context.Context, merchantID string) error {
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
	if status != merchants.StatusActive {
		return auth.ErrMerchantInactive
	}
	return nil
}

func normEmail(e string) string { return strings.ToLower(strings.TrimSpace(e)) }
