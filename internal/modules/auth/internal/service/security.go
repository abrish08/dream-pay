package service

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"time"

	"paygate/internal/contracts/auth"
	"paygate/internal/modules/auth/internal/domain"
	"paygate/internal/modules/auth/internal/store"
	"paygate/internal/platform/apperror"
	"paygate/internal/platform/reqctx"
	"paygate/internal/platform/sms"
)

// ---- Account verification (6-digit code by email, or SMS through send.et) ----
//
// Self-signed-up merchants and invited team members get no token until they
// prove they own their email (or phone). Platform staff, and accounts created
// by platform staff, are exempt: an administrator vouched for them.
//
// Codes: uniformly random 6 digits, stored as HMAC(pepper, user, code),
// valid 15 minutes, 5 wrong tries kill the code, a new code replaces the old
// one, at most one send per minute and 5 per hour.

var (
	errVerifyRequired = apperror.Forbidden("verification_required",
		"verify your account first: we sent a 6-digit code to your email")
	errInvalidCode = apperror.Invalid("invalid_code", "the code is wrong or has expired; request a new one")
)

const (
	ChannelEmail = "email"
	ChannelSMS   = "sms"
)

type SignupResult struct {
	VerificationRequired bool   `json:"verification_required"`
	Email                string `json:"email"`
	Channel              string `json:"channel"`
	Message              string `json:"message"`
}

// issueCode creates and sends a code unless the send limits are reached.
func (s *Service) issueCode(ctx context.Context, u *domain.User, channel string) error {
	now := s.now()
	last, n, err := s.store.CodeStats(ctx, u.ID, now.Add(-time.Hour))
	if err != nil {
		return err
	}
	if now.Sub(last) < domain.CodeCooldown || n >= domain.CodesPerHour {
		return nil // throttled; the previous code still works
	}
	if channel == ChannelSMS && (u.Phone == "" || s.cfg.SMS == nil) {
		channel = ChannelEmail
	}
	code, err := domain.NewCode()
	if err != nil {
		return err
	}
	dest := u.Email
	if channel == ChannelSMS {
		dest = u.Phone
	}
	if err := s.store.NewCode(ctx, store.Code{UserID: u.ID, Channel: channel, Destination: dest,
		CodeHash: domain.HashCode(s.cfg.Pepper.Secret, u.ID, code), ExpiresAt: now.Add(domain.CodeTTL), CreatedAt: now}); err != nil {
		return err
	}
	if channel == ChannelSMS {
		s.text(u.Phone, fmt.Sprintf("Your paygate verification code is %s. It expires in %d minutes. Never share it.",
			code, int(domain.CodeTTL.Minutes())))
		return nil
	}
	s.notify(u.Email, "Your paygate verification code: "+code, fmt.Sprintf(
		"Your verification code is:\n\n    %s\n\nIt expires in %d minutes. Never share it: paygate staff will never ask for it.\n"+
			"If you did not create a paygate account, ignore this email.\n", code, int(domain.CodeTTL.Minutes())))
	return nil
}

// ResendVerification always answers the same way (no account enumeration).
func (s *Service) ResendVerification(ctx context.Context, email, channel string) error {
	u, err := s.findByEmail(ctx, normEmail(email))
	if err != nil || u == nil || u.Verified() || u.VerificationExempt() || u.Status != domain.StatusActive {
		return err
	}
	return s.issueCode(ctx, u, channel)
}

// VerifyAccount checks the code and logs the user in.
func (s *Service) VerifyAccount(ctx context.Context, email, code string) (Tokens, error) {
	u, err := s.findByEmail(ctx, normEmail(email))
	if err != nil {
		return Tokens{}, err
	}
	if u == nil || u.Verified() || u.Status != domain.StatusActive {
		return Tokens{}, errInvalidCode
	}
	var wrong bool
	err = s.store.Tx(ctx, func(st *store.Store) error {
		now := s.now()
		c, err := st.OpenCode(ctx, u.ID, now)
		if err != nil {
			return err
		}
		if c == nil {
			wrong = true
			return nil
		}
		if !domain.SameHash(c.CodeHash, domain.HashCode(s.cfg.Pepper.Secret, u.ID, code)) {
			wrong = true
			return st.CodeAttempt(ctx, c.ID, c.Attempts+1, domain.CodeMaxAttempts, now)
		}
		if err := st.UseCode(ctx, c.ID, now); err != nil {
			return err
		}
		if u, err = st.UserByID(ctx, u.ID, true); err != nil {
			return err
		}
		if c.Channel == ChannelSMS {
			u.PhoneVerifiedAt = &now
		} else {
			u.EmailVerifiedAt = &now
		}
		u.UpdatedAt = now
		if err := st.SaveUser(ctx, u); err != nil {
			return err
		}
		return st.Audit(ctx, store.Audit{ActorID: u.ID, MerchantID: u.MerchantID, Action: "user.verified",
			TargetType: "user", TargetID: u.ID, Details: map[string]any{"channel": c.Channel}}, now)
	})
	if err != nil {
		return Tokens{}, err
	}
	if wrong {
		return Tokens{}, errInvalidCode
	}
	if err := s.merchantAllowed(ctx, u.MerchantID, false); err != nil {
		return Tokens{}, err
	}
	return s.complete(ctx, u)
}

// ---- Authenticator app (Microsoft / Google Authenticator, TOTP) ----

var (
	errMFAUnavailable = apperror.Unavailable("mfa_unavailable", "authenticator support needs DATA_ENCRYPTION_KEY")
	errMFAEnabled     = apperror.Conflict("mfa_already_enabled", "the authenticator is already enabled; disable it first")
	errMFANotSetUp    = apperror.Unprocessable("mfa_not_set_up", "start the authenticator setup first")
	errMFARequired    = apperror.Forbidden("mfa_enrollment_required",
		"set up an authenticator app (Microsoft Authenticator) before changing your password")
	errBadMFACode = apperror.Unauthorized("invalid_mfa_code", "the authenticator code is wrong")
)

const (
	issuer             = "Paygate"
	challengeTTL       = 5 * time.Minute
	challengeAttempts  = 5
	recoveryCodeAmount = 10
)

type TOTPSetup struct {
	Secret     string `json:"secret"`      // for manual entry in the app
	OTPAuthURL string `json:"otpauth_url"` // show as a QR code
	Issuer     string `json:"issuer"`
	Account    string `json:"account"`
}

// SetupTOTP starts enrolment: a new secret, active only after ConfirmTOTP.
func (s *Service) SetupTOTP(ctx context.Context, actor reqctx.Principal) (TOTPSetup, error) {
	if s.cfg.Box == nil {
		return TOTPSetup{}, errMFAUnavailable
	}
	var out TOTPSetup
	err := s.store.Tx(ctx, func(st *store.Store) error {
		u, err := st.UserByID(ctx, actor.Subject, true)
		if err != nil {
			return err
		}
		if u.MFAEnabled() {
			return errMFAEnabled
		}
		secret, err := domain.NewTOTPSecret()
		if err != nil {
			return err
		}
		if u.TOTPSecret, err = s.cfg.Box.Encrypt(secret); err != nil {
			return err
		}
		u.UpdatedAt = s.now()
		out = TOTPSetup{Secret: secret, OTPAuthURL: domain.OTPAuthURL(issuer, u.Email, secret), Issuer: issuer, Account: u.Email}
		return st.SaveUser(ctx, u)
	})
	return out, err
}

type TOTPEnabled struct {
	Enabled       bool     `json:"enabled"`
	RecoveryCodes []string `json:"recovery_codes"` // shown once
	Warning       string   `json:"warning"`
}

// ConfirmTOTP: the first code from the app proves it was set up correctly.
func (s *Service) ConfirmTOTP(ctx context.Context, actor reqctx.Principal, code string) (TOTPEnabled, error) {
	var out TOTPEnabled
	var u *domain.User
	err := s.store.Tx(ctx, func(st *store.Store) error {
		var err error
		if u, err = st.UserByID(ctx, actor.Subject, true); err != nil {
			return err
		}
		if u.MFAEnabled() {
			return errMFAEnabled
		}
		if u.TOTPSecret == "" || s.cfg.Box == nil {
			return errMFANotSetUp
		}
		secret, err := s.cfg.Box.Decrypt(u.TOTPSecret)
		if err != nil {
			return err
		}
		now := s.now()
		step, ok := domain.VerifyTOTP(secret, code, now, u.TOTPLastStep)
		if !ok {
			return errBadMFACode
		}
		codes, err := domain.NewRecoveryCodes(recoveryCodeAmount)
		if err != nil {
			return err
		}
		hashes := make([]string, len(codes))
		for i, c := range codes {
			hashes[i] = domain.HashCode(s.cfg.Pepper.Secret, u.ID, domain.NormalizeRecoveryCode(c))
		}
		if err := st.ReplaceRecoveryCodes(ctx, u.ID, hashes, now); err != nil {
			return err
		}
		u.TOTPEnabledAt, u.TOTPLastStep, u.UpdatedAt = &now, step, now
		if err := st.SaveUser(ctx, u); err != nil {
			return err
		}
		out = TOTPEnabled{Enabled: true, RecoveryCodes: codes,
			Warning: "Save these recovery codes somewhere safe. Each works once if you lose your phone. They are not shown again."}
		return st.Audit(ctx, store.Audit{ActorID: u.ID, MerchantID: u.MerchantID, Action: "user.mfa_enabled",
			TargetType: "user", TargetID: u.ID}, now)
	})
	if err == nil {
		s.notify(u.Email, "Authenticator app enabled", "Two-step sign-in with an authenticator app is now on for your paygate account.\n"+
			"If this was not you, reset your password and contact support at once.\n")
	}
	return out, err
}

// DisableTOTP needs the password (when the account has one) and a code.
func (s *Service) DisableTOTP(ctx context.Context, actor reqctx.Principal, password, code string) error {
	var u *domain.User
	err := s.store.Tx(ctx, func(st *store.Store) error {
		var err error
		if u, err = st.UserByID(ctx, actor.Subject, true); err != nil {
			return err
		}
		if !u.MFAEnabled() {
			return errMFANotSetUp
		}
		if u.HasPassword() && !domain.CheckPassword(password, u.PasswordHash) {
			return auth.ErrInvalidCredentials
		}
		if err := s.checkSecondFactor(ctx, st, u, code, true); err != nil {
			return err
		}
		now := s.now()
		u.TOTPSecret, u.TOTPEnabledAt, u.TOTPLastStep, u.UpdatedAt = "", nil, 0, now
		if err := st.SaveUser(ctx, u); err != nil {
			return err
		}
		if err := st.ReplaceRecoveryCodes(ctx, u.ID, nil, now); err != nil {
			return err
		}
		return st.Audit(ctx, store.Audit{ActorID: u.ID, MerchantID: u.MerchantID, Action: "user.mfa_disabled",
			TargetType: "user", TargetID: u.ID}, now)
	})
	if err == nil {
		s.notify(u.Email, "Authenticator app disabled", "Two-step sign-in was turned off for your paygate account.\n"+
			"If this was not you, reset your password and contact support at once.\n")
	}
	return err
}

type MFAStatus struct {
	Enabled           bool       `json:"enabled"`
	EnabledAt         *time.Time `json:"enabled_at,omitempty"`
	RecoveryCodesLeft int64      `json:"recovery_codes_left"`
}

func (s *Service) MFAStatus(ctx context.Context, actor reqctx.Principal) (MFAStatus, error) {
	u, err := s.store.UserByID(ctx, actor.Subject, false)
	if err != nil {
		return MFAStatus{}, err
	}
	n, err := s.store.RecoveryCodesLeft(ctx, u.ID)
	return MFAStatus{Enabled: u.MFAEnabled(), EnabledAt: u.TOTPEnabledAt, RecoveryCodesLeft: n}, err
}

// checkSecondFactor accepts a current authenticator code (never twice) or,
// when allowRecovery, an unused recovery code. Wrong codes count as failed
// sign-ins (5 lock the account for 15 minutes). u must be locked by st.
func (s *Service) checkSecondFactor(ctx context.Context, st *store.Store, u *domain.User, code string, allowRecovery bool) error {
	now := s.now()
	if u.Locked(now) {
		return errLocked
	}
	if s.cfg.Box != nil && u.TOTPSecret != "" {
		if secret, err := s.cfg.Box.Decrypt(u.TOTPSecret); err == nil {
			if step, ok := domain.VerifyTOTP(secret, code, now, u.TOTPLastStep); ok {
				u.TOTPLastStep, u.FailedLogins = step, 0
				return st.SaveUser(ctx, u)
			}
		}
	}
	if allowRecovery {
		used, err := st.UseRecoveryCode(ctx, u.ID, domain.HashCode(s.cfg.Pepper.Secret, u.ID, domain.NormalizeRecoveryCode(code)), now)
		if err != nil {
			return err
		}
		if used {
			s.notify(u.Email, "A recovery code was used", "A one-time recovery code was used to sign in to your paygate account.\n")
			return nil
		}
	}
	u.LoginFailed(now)
	if err := st.SaveUser(ctx, u); err != nil {
		return err
	}
	return errBadMFACode
}

// ---- Completing a sign-in ----

// complete issues tokens, or an MFA challenge when the authenticator is on.
func (s *Service) complete(ctx context.Context, u *domain.User) (Tokens, error) {
	if !u.MFAEnabled() {
		return s.startSession(ctx, u)
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return Tokens{}, err
	}
	raw := "mfa_" + base64.RawURLEncoding.EncodeToString(b)
	now := s.now()
	if err := s.store.CreateChallenge(ctx, u.ID, domain.HashRefreshToken(raw), now.Add(challengeTTL), now); err != nil {
		return Tokens{}, err
	}
	return Tokens{MFARequired: true, MFAToken: raw, MFAExpiresIn: int(challengeTTL.Seconds())}, nil
}

// LoginMFA completes a sign-in with the authenticator (or a recovery) code.
func (s *Service) LoginMFA(ctx context.Context, mfaToken, code string) (Tokens, error) {
	var (
		u      *domain.User
		failed error
	)
	err := s.store.Tx(ctx, func(st *store.Store) error {
		now := s.now()
		ch, err := st.OpenChallenge(ctx, domain.HashRefreshToken(mfaToken), now)
		if err != nil {
			return err
		}
		if ch == nil {
			failed = apperror.Unauthorized("invalid_mfa_token", "the sign-in step expired; log in again")
			return nil
		}
		if u, err = st.UserByID(ctx, ch.UserID, true); err != nil {
			return err
		}
		if err := s.checkSecondFactor(ctx, st, u, code, true); err != nil {
			failed = err
			return st.ChallengeAttempt(ctx, ch.ID, ch.Attempts+1, challengeAttempts, now)
		}
		return st.UseChallenge(ctx, ch.ID, now)
	})
	if err != nil {
		return Tokens{}, err
	}
	if failed != nil {
		return Tokens{}, failed
	}
	if u.Status != domain.StatusActive {
		return Tokens{}, auth.ErrInvalidCredentials
	}
	if err := s.merchantAllowed(ctx, u.MerchantID, false); err != nil {
		return Tokens{}, err
	}
	return s.startSession(ctx, u)
}

// text sends an SMS in the background.
func (s *Service) text(to, body string) {
	if s.cfg.SMS == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.cfg.SMS.Send(ctx, sms.Message{To: to, Body: body}); err != nil {
			s.log.Error("send sms", "err", err)
		}
	}()
}
