package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"paygate/internal/contracts/auth"
	"paygate/internal/contracts/merchants"
	"paygate/internal/modules/auth/internal/domain"
	"paygate/internal/modules/auth/internal/store"
	"paygate/internal/platform/apperror"
	"paygate/internal/platform/authn"
	"paygate/internal/platform/bus"
	"paygate/internal/platform/mail"
)

// ---- Sign in with Google / Apple ----
//
// One account per email, however it was created:
//
//   - The provider account (provider + subject) is already linked: log in.
//   - Not linked, but a user with this email exists: LINK and log in (no new
//     account). Only when the provider says the email is verified. If the
//     existing account's email was never verified (someone registered it
//     with a password), that password is removed and its sessions end: this
//     defeats "account pre-hijacking", where an attacker registers a victim's
//     email first and waits for the victim to sign in with Google.
//   - No account: with business_name this is a signup (pending merchant +
//     owner, like /auth/signup but without a password); without it the
//     client is told to collect the business name.

var (
	errProviderOff     = apperror.NotFound("identity_provider_not_enabled", "this sign-in method is not enabled")
	errEmailUnverified = apperror.Forbidden("email_not_verified",
		"your Google/Apple account email is not verified; verify it there first")
	errNoAccount = apperror.NotFound("account_not_found",
		"no account uses this email yet; send business_name to sign up")
)

type ExternalInput struct {
	IDToken      string `json:"id_token"`
	Nonce        string `json:"nonce"`
	BusinessName string `json:"business_name"` // only to sign up
	Name         string `json:"name"`          // Apple shares the name only on first sign-in, client side
	Phone        string `json:"phone"`
	Website      string `json:"website"`
}

// ExternalSignIn returns tokens and whether a new account was created.
func (s *Service) ExternalSignIn(ctx context.Context, provider string, in ExternalInput) (Tokens, bool, error) {
	v, ok := s.cfg.Identity[provider]
	if !ok {
		return Tokens{}, false, errProviderOff
	}
	id, err := v.Verify(ctx, in.IDToken, in.Nonce)
	if err != nil {
		return Tokens{}, false, err
	}
	u, err := s.store.UserByIdentity(ctx, id.Provider, id.Subject)
	if err != nil {
		return Tokens{}, false, err
	}
	created := false
	if u == nil {
		if id.Email == "" || !id.EmailVerified {
			return Tokens{}, false, errEmailUnverified
		}
		if u, err = s.linkByEmail(ctx, id); err != nil {
			return Tokens{}, false, err
		}
		if u == nil {
			if strings.TrimSpace(in.BusinessName) == "" {
				return Tokens{}, false, errNoAccount
			}
			if u, err = s.externalSignup(ctx, id, in); err != nil {
				return Tokens{}, false, err
			}
			created = true
		}
	}
	if u.Status != domain.StatusActive {
		return Tokens{}, false, auth.ErrInvalidCredentials
	}
	if err := s.merchantAllowed(ctx, u.MerchantID, false); err != nil {
		return Tokens{}, false, err
	}
	u.LoginSucceeded(s.now())
	if err := s.store.SaveUser(ctx, u); err != nil {
		return Tokens{}, false, err
	}
	t, err := s.complete(ctx, u) // authenticator code next, if enabled
	return t, created, err
}

// linkByEmail links the provider identity to the existing user with the same
// email. Returns nil when no such user exists.
func (s *Service) linkByEmail(ctx context.Context, id authn.Identity) (*domain.User, error) {
	var (
		u          *domain.User
		takenOver  bool
		hadAccount bool
	)
	err := s.store.Tx(ctx, func(st *store.Store) error {
		var err error
		u, err = st.UserByEmail(ctx, id.Email) // row lock
		if errors.Is(err, store.ErrUserNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		hadAccount = true
		now := s.now()
		if u.EmailVerifiedAt == nil {
			// Ownership of the email was never proven by whoever set the
			// password: the verified owner takes the account over.
			takenOver = u.HasPassword()
			u.PasswordHash, u.EmailVerifiedAt, u.UpdatedAt = "", &now, now
			if err := st.SaveUser(ctx, u); err != nil {
				return err
			}
			if err := st.RevokeUserSessions(ctx, u.ID, now); err != nil {
				return err
			}
			if err := s.events.Publish(ctx, st.DB(), auth.AccessChanged{UserIDs: []string{u.ID}, NotBefore: now}); err != nil {
				return err
			}
		}
		if err := st.LinkIdentity(ctx, id.Provider, id.Subject, u.ID, id.Email, now); err != nil {
			return err
		}
		return st.Audit(ctx, store.Audit{ActorID: u.ID, MerchantID: u.MerchantID, Action: "user.identity_linked",
			TargetType: "user", TargetID: u.ID, Details: map[string]any{"provider": id.Provider,
				"unverified_password_removed": takenOver}}, now)
	})
	if err != nil || !hadAccount {
		return nil, err
	}
	notice := fmt.Sprintf("%s sign-in was connected to your paygate account.\n", providerLabel(id.Provider))
	if takenOver {
		notice += "\nThe password previously set on this account was removed because this email had not been verified. " +
			"Use \"Forgot password\" if you want to sign in with a password too.\n"
	}
	s.notify(u.Email, "New sign-in method on your account", notice)
	return u, nil
}

func (s *Service) externalSignup(ctx context.Context, id authn.Identity, in ExternalInput) (*domain.User, error) {
	name := in.Name
	if name == "" {
		name = id.Name
	}
	u, err := domain.NewExternalUser(id.Email, name, "", s.now())
	if err != nil {
		return nil, err
	}
	m, err := bus.Ask(ctx, s.bus, merchants.CreateMerchant{Name: in.BusinessName, Email: u.Email, Phone: in.Phone, Website: in.Website})
	if err != nil {
		return nil, err
	}
	if err := s.store.UpsertMerchant(ctx, m.ID, m.Status, time.Unix(0, 0).UTC()); err != nil {
		return nil, err
	}
	u.MerchantID, u.Role = m.ID, auth.RoleOwner
	err = s.store.Tx(ctx, func(st *store.Store) error {
		if err := st.CreateUser(ctx, u); err != nil {
			return err
		}
		if _, err := st.AssignRole(ctx, u.ID, domain.SystemRoleID(auth.RoleOwner), "", u.CreatedAt); err != nil {
			return err
		}
		if err := st.LinkIdentity(ctx, id.Provider, id.Subject, u.ID, id.Email, u.CreatedAt); err != nil {
			return err
		}
		return st.Audit(ctx, store.Audit{ActorID: u.ID, MerchantID: m.ID, Action: "merchant.signed_up",
			TargetType: "merchant", TargetID: m.ID, Details: map[string]any{"business_name": m.Name, "via": id.Provider}}, u.CreatedAt)
	})
	if errors.Is(err, store.ErrEmailTaken) {
		// A concurrent signup with the same email won: link to it instead.
		return s.linkByEmail(ctx, id)
	}
	return u, err
}

// ---- Password reset by email ----
//
//   - The answer never says whether the email exists (no account enumeration),
//     and takes the same time either way: the email is sent in the background.
//   - Token: 256 random bits, only its SHA-256 is stored, valid ResetTTL
//     (default 30 min), single use; at most 3 emails per user per hour.
//   - A successful reset: sets the password, marks the email verified (the
//     user proved they read it), clears lockouts, closes every other reset
//     link, signs the user out everywhere (sessions + access tokens) and
//     sends a "your password was changed" notice.

const maxResetsPerHour = 3

func (s *Service) ForgotPassword(ctx context.Context, email, ip string) error {
	email = normEmail(email)
	user, err := s.findByEmail(ctx, email)
	if err != nil || user == nil || user.Status != domain.StatusActive {
		return err // nil for unknown / disabled: same answer as success
	}
	now := s.now()
	n, err := s.store.CountResetsSince(ctx, user.ID, now.Add(-time.Hour))
	if err != nil {
		return err
	}
	if n >= maxResetsPerHour {
		s.log.Warn("password reset throttled", "user_id", user.ID)
		return nil
	}
	raw, hash, err := domain.NewResetToken()
	if err != nil {
		return err
	}
	if err := s.store.CreateReset(ctx, user.ID, hash, ip, now.Add(s.cfg.ResetTTL), now); err != nil {
		return err
	}
	link := s.cfg.DashboardURL + "/reset-password?token=" + raw
	s.notify(user.Email, "Reset your paygate password", fmt.Sprintf(
		"Someone asked to reset the password of your paygate account.\n\n"+
			"Open this link within %d minutes to choose a new password:\n%s\n\n"+
			"If it was not you, ignore this email: your password does not change.\n",
		int(s.cfg.ResetTTL.Minutes()), link))
	return nil
}

func (s *Service) ResetPassword(ctx context.Context, rawToken, newPassword string) error {
	if !strings.HasPrefix(rawToken, "prt_") {
		return store.ErrResetInvalid
	}
	hash, err := domain.HashPassword(newPassword) // validates strength before the token is spent
	if err != nil {
		return err
	}
	var u *domain.User
	err = s.store.Tx(ctx, func(st *store.Store) error {
		now := s.now()
		userID, err := st.UseReset(ctx, domain.HashRefreshToken(rawToken), now)
		if err != nil {
			return err
		}
		if u, err = st.UserByID(ctx, userID, true); err != nil {
			return err
		}
		if u.Status != domain.StatusActive {
			return store.ErrResetInvalid
		}
		u.PasswordHash, u.FailedLogins, u.LockedUntil, u.UpdatedAt = hash, 0, nil, now
		if u.EmailVerifiedAt == nil {
			u.EmailVerifiedAt = &now
		}
		if err := st.SaveUser(ctx, u); err != nil {
			return err
		}
		if err := st.CloseResets(ctx, u.ID, now); err != nil {
			return err
		}
		if err := st.RevokeUserSessions(ctx, u.ID, now); err != nil {
			return err
		}
		if err := st.Audit(ctx, store.Audit{ActorID: u.ID, MerchantID: u.MerchantID, Action: "user.password_reset",
			TargetType: "user", TargetID: u.ID}, now); err != nil {
			return err
		}
		return s.events.Publish(ctx, st.DB(), auth.AccessChanged{UserIDs: []string{u.ID}, NotBefore: now})
	})
	if err != nil {
		return err
	}
	s.notify(u.Email, "Your paygate password was changed",
		"The password of your paygate account was just changed and you were signed out everywhere.\n\n"+
			"If this was not you, reset your password now and contact support.\n")
	return nil
}

func (s *Service) findByEmail(ctx context.Context, email string) (*domain.User, error) {
	var u *domain.User
	err := s.store.Tx(ctx, func(st *store.Store) error {
		var err error
		u, err = st.UserByEmail(ctx, email)
		if errors.Is(err, store.ErrUserNotFound) {
			u = nil
			return nil
		}
		return err
	})
	return u, err
}

// notify sends an email in the background so the API answers in the same
// time whether or not an email goes out (no account enumeration by timing).
func (s *Service) notify(to, subject, text string) {
	if s.cfg.Mailer == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.cfg.Mailer.Send(ctx, mail.Message{To: to, Subject: subject, Text: text}); err != nil {
			s.log.Error("send email", "subject", subject, "err", err)
		}
	}()
}

func providerLabel(p string) string {
	switch p {
	case "google":
		return "Google"
	case "apple":
		return "Apple"
	}
	return p
}
