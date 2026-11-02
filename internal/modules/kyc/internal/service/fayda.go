package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"time"

	"github.com/google/uuid"

	"paygate/internal/modules/kyc/internal/domain"
	"paygate/internal/modules/kyc/internal/fayda"
	"paygate/internal/modules/kyc/internal/store"
	"paygate/internal/platform/apperror"
	"paygate/internal/platform/reqctx"
)

// ---- Owner identity with Fayda (national digital ID) ----
//
//	POST /v1/kyc/fayda/start      → {authorize_url}: the owner opens it, signs in at Fayda
//	GET  /kyc/fayda/callback      ← Fayda redirects here with a one-time code
//	                              → identity verified, owner details taken from Fayda,
//	                                browser sent back to <DASHBOARD_URL>/kyc?fayda=verified
//
// The state (CSRF), nonce (replay) and PKCE verifier (code interception) are
// created per attempt; the session is single-use and expires in 10 minutes.

const faydaSessionTTL = 10 * time.Minute

var errFaydaOff = apperror.Unavailable("fayda_not_configured", "Fayda verification is not set up on this gateway")

type FaydaStart struct {
	AuthorizeURL string    `json:"authorize_url"`
	ExpiresAt    time.Time `json:"expires_at"`
}

func (s *Service) StartFayda(ctx context.Context, actor reqctx.Principal) (FaydaStart, error) {
	if s.fayda == nil || s.box == nil {
		return FaydaStart{}, errFaydaOff
	}
	a, err := s.load(ctx, s.store, actor.MerchantID, false)
	if err != nil {
		return FaydaStart{}, err
	}
	if !a.Editable() {
		return FaydaStart{}, domain.ErrLocked
	}
	state, err := fayda.RandomToken()
	if err != nil {
		return FaydaStart{}, err
	}
	nonce, err := fayda.RandomToken()
	if err != nil {
		return FaydaStart{}, err
	}
	verifier, challenge, err := fayda.NewPKCE()
	if err != nil {
		return FaydaStart{}, err
	}
	enc, err := s.box.Encrypt(verifier)
	if err != nil {
		return FaydaStart{}, err
	}
	link, err := s.fayda.AuthorizeURL(ctx, state, nonce, challenge)
	if err != nil {
		return FaydaStart{}, apperror.Unavailable("fayda_unavailable", "Fayda is unreachable right now: "+err.Error())
	}
	now := s.now()
	if err := s.store.CreateFaydaSession(ctx, hashState(state), store.FaydaSession{MerchantID: actor.MerchantID,
		UserID: actor.Subject, VerifierEnc: enc, Nonce: nonce}, now.Add(faydaSessionTTL), now); err != nil {
		return FaydaStart{}, err
	}
	return FaydaStart{AuthorizeURL: link, ExpiresAt: now.Add(faydaSessionTTL)}, nil
}

// FaydaCallback finishes a Fayda sign-in and returns where to send the browser.
func (s *Service) FaydaCallback(ctx context.Context, state, code, faydaError string) string {
	result := func(outcome, reason string) string {
		q := url.Values{"fayda": {outcome}}
		if reason != "" {
			q.Set("reason", reason)
		}
		return s.dashURL + "/kyc?" + q.Encode()
	}
	if s.fayda == nil || s.box == nil || state == "" {
		return result("failed", "not_configured")
	}
	sess, err := s.store.UseFaydaSession(ctx, hashState(state), s.now())
	if err != nil || sess == nil {
		return result("failed", "expired") // unknown, used or expired state: never processed twice
	}
	fail := func(reason string, err error) string {
		s.log.Warn("Fayda verification failed", "merchant_id", sess.MerchantID, "reason", reason, "err", err)
		_ = s.store.Record(ctx, sess.MerchantID, "fayda_failed", sess.UserID, reason, s.now())
		return result("failed", reason)
	}
	if faydaError != "" {
		return fail("cancelled", nil) // the person refused or Fayda reported an error
	}
	verifier, err := s.box.Decrypt(sess.VerifierEnc)
	if err != nil {
		return fail("internal", err)
	}
	id, err := s.fayda.Verify(ctx, code, verifier, sess.Nonce)
	if err != nil {
		return fail("verification_failed", err)
	}
	ident := domain.FaydaIdentity{Subject: id.Subject, IndividualID: id.IndividualID, Name: id.Name, BirthDate: id.BirthDate,
		Gender: id.Gender, Phone: id.Phone, Email: id.Email, Nationality: id.Nationality, Address: id.Address}
	if len(id.Picture) > 0 && len(id.Picture) <= domain.MaxFileBytes {
		ident.PhotoKey = "kyc/" + sess.MerchantID + "/fayda/" + uuid.NewString() + ".jpg"
		if err := s.files.Put(ctx, ident.PhotoKey, id.Picture, "image/jpeg"); err != nil {
			s.log.Warn("store Fayda photo", "merchant_id", sess.MerchantID, "err", err)
			ident.PhotoKey = ""
		}
	}
	err = s.store.Tx(ctx, func(st *store.Store) error {
		a, err := s.load(ctx, st, sess.MerchantID, true)
		if err != nil {
			return err
		}
		if err := a.VerifyWithFayda(ident, s.now()); err != nil {
			return err
		}
		if err := st.SaveApplication(ctx, a); err != nil {
			return err
		}
		return st.Record(ctx, sess.MerchantID, "fayda_verified", sess.UserID, "owner: "+id.Name, s.now())
	})
	if err != nil {
		if ae, ok := err.(*apperror.Error); ok {
			return fail(ae.Code, err)
		}
		return fail("internal", err)
	}
	return result("verified", "")
}

func hashState(state string) string {
	sum := sha256.Sum256([]byte("fayda-state:" + state))
	return hex.EncodeToString(sum[:])
}
