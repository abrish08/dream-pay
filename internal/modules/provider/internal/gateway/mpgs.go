package gateway

// Mastercard Payment Gateway Services (MPGS, "Mastercard Gateway"): card
// payments (Visa, Mastercard, ...) through an acquiring bank. Ethiopian banks
// with card acquiring give their e-commerce merchants an MPGS merchant id,
// an API password and their gateway host.
//
// Flow (Hosted Checkout: card numbers are typed on Mastercard's page and never
// touch this server, which keeps PCI DSS scope to SAQ A):
//
//	Initiate  POST /api/rest/version/{v}/merchant/{id}/session   INITIATE_CHECKOUT (PURCHASE)
//	          → session id; checkout_url = our page /providers/mpgs/pay/{txRef}
//	            which loads checkout.min.js and opens Mastercard's payment page
//	customer  enters the card, 3-D Secure (OTP / bank app)
//	return    browser → /providers/{name}/return/{txRef} → Verify
//	notify    MPGS webhook → /providers/{name}/callback (X-Notification-Secret) → Verify
//	Verify    GET /order/{txRef}: the authoritative status and amount
//	Refund    PUT /order/{txRef}/transaction/{refundRef}  REFUND (idempotent per refundRef)
//
// The order id is the payment id and the refund transaction id is the refund
// id, so retries can never charge or refund twice.

import (
	"bytes"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"paygate/internal/contracts/providers"
	"paygate/internal/platform/apperror"
	"paygate/internal/platform/money"
	"paygate/internal/platform/netguard"
)

type MPGSConfig struct {
	Name               string   // adapter name in PROVIDER_ROUTES: "mpgs" (live) or "mpgs_test"
	BaseURL            string   // gateway host from the bank, e.g. https://ap-gateway.mastercard.com
	MerchantID         string   // e.g. DASHEN000123 (test merchants usually start with TEST)
	APIPassword        string   // Merchant Administration → Admin → Integration Settings
	APIVersion         string   // default 100
	MerchantName       string   // shown on the payment page
	NotificationSecret string   // webhook secret (Merchant Administration → Webhook Notifications)
	Currencies         []string // what the merchant account settles, default USD
	Sandbox            bool     // a TEST merchant: only test-mode payments may use it
	PublicBaseURL      string   // this gateway, for return URLs
	HTTP               *http.Client
}

type MPGS struct{ cfg MPGSConfig }

func NewMPGS(cfg MPGSConfig) (*MPGS, error) {
	if cfg.BaseURL == "" || cfg.MerchantID == "" || cfg.APIPassword == "" {
		return nil, fmt.Errorf("MPGS needs base URL, merchant id and API password")
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
		return nil, fmt.Errorf("MPGS base URL must be just the gateway address, e.g. https://ap-gateway.mastercard.com")
	}
	local := u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost" || u.Hostname() == "::1"
	switch {
	case u.Scheme == "https" && !local && netguard.CheckHost(u.Hostname()) != nil:
		return nil, fmt.Errorf("MPGS base URL %s is not a public internet host", u.Host)
	case u.Scheme == "https":
	case u.Scheme == "http" && local && cfg.Sandbox:
		// development only: the local simulator (go run ./cmd/mpgs-sim) as the TEST merchant
	default:
		return nil, fmt.Errorf("MPGS base URL must use https (plain http is allowed only for a local simulator as the TEST merchant)")
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.Name == "" {
		cfg.Name = "mpgs"
	}
	if cfg.APIVersion == "" {
		cfg.APIVersion = "100"
	}
	if len(cfg.Currencies) == 0 {
		cfg.Currencies = []string{"USD"}
	}
	if cfg.HTTP == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12} // Mastercard requires TLS 1.2+
		cfg.HTTP = &http.Client{Timeout: 20 * time.Second, Transport: tr,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	return &MPGS{cfg: cfg}, nil
}

// ErrMPGSCredentials: Mastercard refused the merchant id / API password.
var ErrMPGSCredentials = errors.New("Mastercard Gateway rejected the merchant id or API password")

// Check is the production health check, run at start-up:
//
//  1. GET /api/rest/version/{v}/information: the gateway is up ("OPERATING");
//  2. GET .../merchant/{id}/paymentOptionsInquiry: our credentials are
//     accepted and the merchant can take payments.
//
// It returns ErrMPGSCredentials (wrapped) for a configuration problem and an
// "unavailable" error when Mastercard cannot be reached.
func (m *MPGS) Check(ctx context.Context) (gatewayVersion string, err error) {
	var info struct {
		Status         string `json:"status"`
		GatewayVersion string `json:"gatewayVersion"`
	}
	if err := m.get(ctx, m.cfg.BaseURL+"/api/rest/version/"+m.cfg.APIVersion+"/information", false, &info); err != nil {
		return "", err
	}
	if info.Status != "OPERATING" {
		return info.GatewayVersion, apperror.Unavailable("provider_unavailable", "Mastercard Gateway status is "+info.Status)
	}
	if err := m.get(ctx, m.api("/paymentOptionsInquiry"), true, nil); err != nil {
		return info.GatewayVersion, err
	}
	return info.GatewayVersion, nil
}

func (m *MPGS) get(ctx context.Context, u string, auth bool, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if auth {
		req.SetBasicAuth("merchant."+m.cfg.MerchantID, m.cfg.APIPassword)
	}
	resp, err := m.cfg.HTTP.Do(req)
	if err != nil {
		return apperror.Unavailable("provider_unavailable", "Mastercard Gateway unreachable: "+err.Error())
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w (HTTP %d, merchant %s)", ErrMPGSCredentials, resp.StatusCode, m.cfg.MerchantID)
	case resp.StatusCode >= 500:
		return apperror.Unavailable("provider_unavailable", fmt.Sprintf("Mastercard Gateway answered %d", resp.StatusCode))
	case resp.StatusCode >= 400:
		var e mpgsError
		_ = json.Unmarshal(raw, &e)
		return fmt.Errorf("Mastercard Gateway %d %s: %s (check MPGS_API_VERSION and MPGS_MERCHANT_ID)",
			resp.StatusCode, e.Error.Cause, e.Error.Explanation)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return apperror.Unavailable("provider_unavailable", "unreadable Mastercard Gateway response")
		}
	}
	return nil
}

func (m *MPGS) Name() string         { return m.cfg.Name }
func (m *MPGS) Methods() []string    { return []string{providers.MethodCard} }
func (m *MPGS) Currencies() []string { return m.cfg.Currencies }
func (m *MPGS) IsSandbox() bool      { return m.cfg.Sandbox }

// ScriptURL is Mastercard's checkout library on the bank's gateway host.
func (m *MPGS) ScriptURL() string { return m.cfg.BaseURL + "/static/checkout/checkout.min.js" }
func (m *MPGS) Origin() string    { return m.cfg.BaseURL }

func (m *MPGS) api(path string) string {
	return m.cfg.BaseURL + "/api/rest/version/" + m.cfg.APIVersion + "/merchant/" + url.PathEscape(m.cfg.MerchantID) + path
}

type mpgsError struct {
	Result string `json:"result"`
	Error  struct {
		Cause       string `json:"cause"`
		Explanation string `json:"explanation"`
		Field       string `json:"field"`
	} `json:"error"`
}

// call sends one API request. 5xx, timeouts and authentication failures are
// "unavailable" (the router may fail over); other 4xx are business errors.
func (m *MPGS) call(ctx context.Context, method, path string, body any, out any) (status int, err error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, m.api(path), rd)
	if err != nil {
		return 0, err
	}
	req.SetBasicAuth("merchant."+m.cfg.MerchantID, m.cfg.APIPassword)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := m.cfg.HTTP.Do(req)
	if err != nil {
		return 0, apperror.Unavailable("provider_unavailable", "Mastercard Gateway unreachable: "+err.Error())
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return resp.StatusCode, apperror.Unavailable("provider_unavailable",
			fmt.Sprintf("Mastercard Gateway answered %d", resp.StatusCode))
	}
	if resp.StatusCode >= 400 {
		var e mpgsError
		_ = json.Unmarshal(raw, &e)
		return resp.StatusCode, &mpgsAPIError{status: resp.StatusCode, cause: e.Error.Cause, explanation: e.Error.Explanation}
	}
	if out != nil {
		d := json.NewDecoder(bytes.NewReader(raw))
		d.UseNumber()
		if err := d.Decode(out); err != nil {
			return resp.StatusCode, apperror.Unavailable("provider_unavailable", "unreadable Mastercard Gateway response")
		}
	}
	return resp.StatusCode, nil
}

type mpgsAPIError struct {
	status             int
	cause, explanation string
}

func (e *mpgsAPIError) Error() string {
	return fmt.Sprintf("Mastercard Gateway %d %s: %s", e.status, e.cause, e.explanation)
}

func (e *mpgsAPIError) notFound() bool {
	x := strings.ToLower(e.explanation)
	return e.status == http.StatusNotFound || strings.Contains(x, "unable to find") || strings.Contains(x, "not found")
}

func (m *MPGS) Initiate(ctx context.Context, req providers.InitiateCheckout, callbackURL string) (providers.CheckoutResult, error) {
	if !m.supports(req.Currency) {
		return providers.CheckoutResult{}, providers.ErrMethodUnavailable.WithMessage("card is not available for " + req.Currency)
	}
	order := map[string]any{"id": req.TxRef, "amount": money.FormatMajor(req.Amount), "currency": req.Currency,
		"description": "Payment " + req.TxRef}
	if strings.HasPrefix(callbackURL, "https://") && m.cfg.NotificationSecret != "" {
		order["notificationUrl"] = callbackURL // MPGS only posts webhooks to https
	}
	interaction := map[string]any{"operation": "PURCHASE",
		"returnUrl": m.cfg.PublicBaseURL + "/providers/" + m.cfg.Name + "/return/" + url.PathEscape(req.TxRef)}
	if m.cfg.MerchantName != "" {
		interaction["merchant"] = map[string]any{"name": m.cfg.MerchantName}
	}
	body := map[string]any{"apiOperation": "INITIATE_CHECKOUT", "order": order, "interaction": interaction}
	if req.CustomerEmail != "" {
		body["customer"] = map[string]any{"email": req.CustomerEmail}
	}
	var res struct {
		Result  string `json:"result"`
		Session struct {
			ID string `json:"id"`
		} `json:"session"`
	}
	if _, err := m.call(ctx, http.MethodPost, "/session", body, &res); err != nil {
		var ae *mpgsAPIError
		if asAPI(err, &ae) {
			return providers.CheckoutResult{}, apperror.Unprocessable("card_checkout_rejected", ae.Error())
		}
		return providers.CheckoutResult{}, err
	}
	if res.Session.ID == "" {
		return providers.CheckoutResult{}, apperror.Unavailable("provider_unavailable", "Mastercard Gateway returned no session")
	}
	return providers.CheckoutResult{
		CheckoutURL: m.cfg.PublicBaseURL + "/providers/" + m.cfg.Name + "/pay/" + url.PathEscape(req.TxRef) +
			"?session=" + url.QueryEscape(res.Session.ID),
		ProviderRef: req.TxRef, // MPGS order id
	}, nil
}

type mpgsOrder struct {
	ID                  string      `json:"id"`
	Status              string      `json:"status"`
	Amount              json.Number `json:"amount"`
	Currency            string      `json:"currency"`
	TotalCapturedAmount json.Number `json:"totalCapturedAmount"`
	// Every attempt on the order (a customer may try several cards).
	Transaction []mpgsTransaction `json:"transaction"`
}

// declineReason: the issuer's answer to the last failed attempt, e.g.
// INSUFFICIENT_FUNDS → insufficient_funds, EXPIRED_CARD → expired_card.
func (o mpgsOrder) declineReason() string {
	if o.Status == "AUTHENTICATION_UNSUCCESSFUL" {
		return "authentication_failed"
	}
	for i := len(o.Transaction) - 1; i >= 0; i-- {
		if c := o.Transaction[i].Response.GatewayCode; c != "" && c != "APPROVED" {
			return strings.ToLower(c)
		}
	}
	return "declined"
}

// Verify reads the order: the only source of truth for a card payment.
func (m *MPGS) Verify(ctx context.Context, txRef string) (providers.Transaction, error) {
	t := providers.Transaction{Provider: m.Name(), TxRef: txRef, ProviderRef: txRef, Status: providers.StatusPending}
	var o mpgsOrder
	if _, err := m.call(ctx, http.MethodGet, "/order/"+url.PathEscape(txRef), nil, &o); err != nil {
		var ae *mpgsAPIError
		if asAPI(err, &ae) && ae.notFound() {
			return t, nil // the customer has not paid (yet)
		}
		return t, err
	}
	amount, err := minor(o.Amount)
	if err != nil {
		return t, apperror.Unavailable("provider_unavailable", "unreadable order amount")
	}
	t.Amount, t.Currency = amount, strings.ToUpper(o.Currency)
	switch o.Status {
	case "CAPTURED", "PARTIALLY_REFUNDED", "REFUNDED", "REFUND_REQUESTED", "EXCESSIVELY_REFUNDED", "DISPUTED", "CHARGEBACK_PROCESSED":
		captured, _ := minor(o.TotalCapturedAmount)
		if captured >= amount {
			t.Status = providers.StatusSucceeded
		}
	case "FAILED", "CANCELLED", "AUTHENTICATION_UNSUCCESSFUL", "DECLINED":
		t.Status, t.FailureReason = providers.StatusFailed, o.declineReason()
	}
	return t, nil
}

type mpgsTransaction struct {
	Result   string `json:"result"`
	Response struct {
		GatewayCode string `json:"gatewayCode"`
	} `json:"response"`
	Transaction struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	} `json:"transaction"`
}

// Refund is idempotent per RefundRef: the refund's transaction id at MPGS is
// the refund id, and an existing one is read back instead of sent again.
func (m *MPGS) Refund(ctx context.Context, req providers.Refund) (providers.RefundResult, error) {
	path := "/order/" + url.PathEscape(req.TxRef) + "/transaction/" + url.PathEscape(req.RefundRef)
	var t mpgsTransaction
	_, err := m.call(ctx, http.MethodGet, path, nil, &t)
	var ae *mpgsAPIError
	switch {
	case err == nil && t.Transaction.ID != "":
		// already sent: report what happened
	case err == nil || (asAPI(err, &ae) && ae.notFound()):
		t = mpgsTransaction{}
		body := map[string]any{"apiOperation": "REFUND",
			"transaction": map[string]any{"amount": money.FormatMajor(req.Amount), "currency": req.Currency}}
		if _, err := m.call(ctx, http.MethodPut, path, body, &t); err != nil {
			if asAPI(err, &ae) {
				return providers.RefundResult{Status: providers.StatusFailed, FailureReason: ae.explanation}, nil
			}
			return providers.RefundResult{}, err
		}
	default:
		return providers.RefundResult{}, err
	}
	switch t.Result {
	case "SUCCESS":
		return providers.RefundResult{Status: providers.StatusSucceeded, ProviderRefundRef: req.RefundRef}, nil
	case "PENDING", "UNKNOWN":
		// Not final yet: "unavailable" keeps the refund pending; the retry
		// worker asks again and reads this same transaction back.
		return providers.RefundResult{}, apperror.Unavailable("refund_pending", "the bank has not confirmed the refund yet")
	default:
		reason := strings.ToLower(t.Response.GatewayCode)
		if reason == "" {
			reason = "declined_by_provider"
		}
		return providers.RefundResult{Status: providers.StatusFailed, FailureReason: reason}, nil
	}
}

// ParseCallback authenticates an MPGS webhook by its shared secret and
// returns the order id; the caller then reads the order (Verify).
func (m *MPGS) ParseCallback(r *http.Request, body []byte) (string, error) {
	if m.cfg.NotificationSecret == "" {
		return "", apperror.Unauthorized("callbacks_disabled", "MPGS notifications are not configured")
	}
	got := r.Header.Get("X-Notification-Secret")
	if subtle.ConstantTimeCompare([]byte(got), []byte(m.cfg.NotificationSecret)) != 1 {
		return "", apperror.Unauthorized("invalid_signature", "notification secret mismatch")
	}
	var n struct {
		Order struct {
			ID string `json:"id"`
		} `json:"order"`
	}
	if err := json.Unmarshal(body, &n); err != nil || n.Order.ID == "" {
		return "", apperror.Invalid("invalid_callback", "notification without order id")
	}
	return n.Order.ID, nil
}

func (m *MPGS) supports(currency string) bool {
	for _, c := range m.cfg.Currencies {
		if c == currency {
			return true
		}
	}
	return false
}

// minor converts an MPGS decimal amount ("50", "50.5", "50.00") to minor units.
func minor(n json.Number) (int64, error) {
	if n == "" {
		return 0, nil
	}
	return money.ParseMajor(n.String())
}

func asAPI(err error, target **mpgsAPIError) bool {
	e, ok := err.(*mpgsAPIError)
	if ok {
		*target = e
	}
	return ok
}
