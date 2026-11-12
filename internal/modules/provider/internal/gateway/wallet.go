package gateway

// ============================================================================
// PLACEHOLDER WALLET ADAPTER (telebirr, CBE Birr, M-Pesa)
//
// This adapter speaks a generic "push payment" API, the one implemented by
// the mock wallet (internal/modules/provider/walletmock, go run ./cmd/wallet-sim).
// It is NOT the official API of any wallet. When a provider's API documents
// and credentials arrive, keep this file's structure and replace only the
// parts marked "PROVIDER-SPECIFIC": request/response formats, authentication
// (tokens, signed requests, certificates) and callback verification. The
// rules around them must stay:
//
//   - Initiate is idempotent per TxRef (our payment id is the provider reference);
//   - a refused connection => ErrProviderUnavailable (safe to fail over);
//     anything after the request may have arrived => ErrOutcomeUnknown (never
//     re-sent; the status checks ask Verify);
//   - Verify is the only source of truth; callbacks are authenticated, then verified;
//   - Refund is idempotent per RefundRef.
//
// Generic API (mock):
//
//	POST /v1/push                 {reference, msisdn, amount:"50.00", currency, description, callback_url}
//	                              -> {provider_ref, status:"pending"}   (the customer gets a PIN prompt)
//	GET  /v1/transactions/{ref}   -> {status: pending|succeeded|failed, amount, currency, reason}
//	POST /v1/refunds              {reference, refund_reference, amount, currency} -> {status, refund_ref, reason}
//	callback POST                 {reference}  header X-Signature: hex(HMAC-SHA256(webhook_secret, body))
// ============================================================================

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	"paygate/internal/contracts/providers"
	"paygate/internal/platform/apperror"
	"paygate/internal/platform/money"
)

type WalletConfig struct {
	Name          string // telebirr, cbebirr, mpesa (or *_test for the provider's sandbox)
	Method        string // providers.MethodTelebirr ...
	BaseURL       string
	APIKey        string // PROVIDER-SPECIFIC: real wallets use OAuth tokens, signed requests or certificates
	WebhookSecret string
	Currencies    []string
	Sandbox       bool
	PublicBaseURL string // our checkout: the customer waits there for the PIN prompt
	HTTP          *http.Client
}

type Wallet struct{ cfg WalletConfig }

func NewWallet(cfg WalletConfig) (*Wallet, error) {
	if cfg.Name == "" || cfg.Method == "" || cfg.BaseURL == "" || cfg.APIKey == "" || cfg.WebhookSecret == "" {
		return nil, fmt.Errorf("wallet %s needs base URL, API key and webhook secret", cfg.Name)
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && !(cfg.Sandbox && isLoopback(u.Hostname()))) {
		return nil, fmt.Errorf("wallet %s: base URL must be https (plain http only for a local simulator in sandbox)", cfg.Name)
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	cfg.PublicBaseURL = strings.TrimRight(cfg.PublicBaseURL, "/")
	if len(cfg.Currencies) == 0 {
		cfg.Currencies = []string{"ETB"}
	}
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: 30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	return &Wallet{cfg: cfg}, nil
}

func isLoopback(h string) bool { return h == "127.0.0.1" || h == "localhost" || h == "::1" }

func (w *Wallet) Name() string         { return w.cfg.Name }
func (w *Wallet) IsSandbox() bool      { return w.cfg.Sandbox }
func (w *Wallet) Methods() []string    { return []string{w.cfg.Method} }
func (w *Wallet) Currencies() []string { return w.cfg.Currencies }

// call sends one request (PROVIDER-SPECIFIC: authentication and format).
// Errors: connection refused or a 503 before acceptance => unavailable (not
// sent); timeouts and dropped connections => outcome unknown; 4xx => business.
func (w *Wallet) call(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, w.cfg.BaseURL+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+w.cfg.APIKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := w.cfg.HTTP.Do(req)
	if err != nil {
		if notConnected(err) {
			return providers.ErrProviderUnavailable.WithMessage(w.cfg.Name + " unreachable: " + err.Error())
		}
		return providers.ErrOutcomeUnknown.WithMessage(w.cfg.Name + ": " + err.Error())
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusUnauthorized:
		return providers.ErrProviderUnavailable.WithMessage(fmt.Sprintf("%s answered %d", w.cfg.Name, resp.StatusCode))
	case resp.StatusCode >= 500:
		return providers.ErrOutcomeUnknown.WithMessage(fmt.Sprintf("%s answered %d", w.cfg.Name, resp.StatusCode))
	case resp.StatusCode == http.StatusNotFound:
		return providers.ErrTransactionNotFound
	case resp.StatusCode >= 400:
		var e struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &e)
		return apperror.Unprocessable("wallet_rejected", strings.TrimSpace(e.Code+" "+e.Message))
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return providers.ErrOutcomeUnknown.WithMessage(w.cfg.Name + ": unreadable answer")
		}
	}
	return nil
}

// notConnected: the request certainly never reached the provider.
func notConnected(err error) bool {
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return true
	}
	return errors.Is(err, syscall.ECONNREFUSED)
}

func (w *Wallet) Initiate(ctx context.Context, req providers.InitiateCheckout, callbackURL string) (providers.CheckoutResult, error) {
	if req.CustomerPhone == "" {
		return providers.CheckoutResult{}, apperror.Invalid("phone_required", "the customer's phone number is needed for "+w.cfg.Method)
	}
	var res struct {
		ProviderRef string `json:"provider_ref"`
	}
	// PROVIDER-SPECIFIC: request body.
	err := w.call(ctx, http.MethodPost, "/v1/push", map[string]any{"reference": req.TxRef, "msisdn": req.CustomerPhone,
		"amount": money.FormatMajor(req.Amount), "currency": req.Currency, "description": "Payment " + req.TxRef,
		"callback_url": callbackURL}, &res)
	if err != nil {
		return providers.CheckoutResult{}, err
	}
	// The customer approves on the phone; our checkout page waits with them.
	return providers.CheckoutResult{CheckoutURL: w.cfg.PublicBaseURL + "/pay/" + req.TxRef, ProviderRef: res.ProviderRef}, nil
}

func (w *Wallet) Verify(ctx context.Context, txRef string) (providers.Transaction, error) {
	var res struct {
		Status   string `json:"status"`
		Amount   string `json:"amount"`
		Currency string `json:"currency"`
		Reason   string `json:"reason"`
		Ref      string `json:"provider_ref"`
	}
	if err := w.call(ctx, http.MethodGet, "/v1/transactions/"+url.PathEscape(txRef), nil, &res); err != nil {
		return providers.Transaction{}, err
	}
	amount, err := money.ParseMajor(res.Amount)
	if err != nil {
		return providers.Transaction{}, providers.ErrOutcomeUnknown.WithMessage("unreadable amount")
	}
	t := providers.Transaction{Provider: w.cfg.Name, TxRef: txRef, ProviderRef: res.Ref, Amount: amount, Currency: res.Currency,
		Status: providers.StatusPending}
	switch res.Status { // PROVIDER-SPECIFIC: status values
	case "succeeded":
		t.Status = providers.StatusSucceeded
	case "failed":
		t.Status, t.FailureReason = providers.StatusFailed, res.Reason
		if t.FailureReason == "" {
			t.FailureReason = "declined"
		}
	}
	return t, nil
}

func (w *Wallet) Refund(ctx context.Context, req providers.Refund) (providers.RefundResult, error) {
	var res struct {
		Status    string `json:"status"`
		RefundRef string `json:"refund_ref"`
		Reason    string `json:"reason"`
	}
	err := w.call(ctx, http.MethodPost, "/v1/refunds", map[string]any{"reference": req.TxRef, "refund_reference": req.RefundRef,
		"amount": money.FormatMajor(req.Amount), "currency": req.Currency}, &res)
	var ae *apperror.Error
	if errors.As(err, &ae) && ae.Code == "wallet_rejected" {
		return providers.RefundResult{Status: providers.StatusFailed, FailureReason: ae.Message}, nil
	}
	if err != nil {
		return providers.RefundResult{}, err
	}
	switch res.Status {
	case "succeeded":
		return providers.RefundResult{Status: providers.StatusSucceeded, ProviderRefundRef: res.RefundRef}, nil
	case "failed":
		return providers.RefundResult{Status: providers.StatusFailed, FailureReason: res.Reason}, nil
	}
	return providers.RefundResult{}, apperror.Unavailable("refund_pending", "the wallet has not confirmed the refund yet")
}

// ParseCallback authenticates the notification (PROVIDER-SPECIFIC: real
// wallets sign differently, e.g. RSA signatures or certificates).
func (w *Wallet) ParseCallback(r *http.Request, body []byte) (string, error) {
	got, err := hex.DecodeString(r.Header.Get("X-Signature"))
	m := hmac.New(sha256.New, []byte(w.cfg.WebhookSecret))
	m.Write(body)
	if err != nil || !hmac.Equal(got, m.Sum(nil)) {
		return "", apperror.Unauthorized("invalid_signature", "callback signature mismatch")
	}
	var n struct {
		Reference string `json:"reference"`
	}
	if err := json.Unmarshal(body, &n); err != nil || n.Reference == "" {
		return "", apperror.Invalid("invalid_callback", "callback without reference")
	}
	return n.Reference, nil
}
