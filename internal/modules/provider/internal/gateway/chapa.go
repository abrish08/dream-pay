package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"paygate/internal/contracts/providers"
	"paygate/internal/platform/apperror"
	"paygate/internal/platform/money"
	"paygate/internal/platform/secrets"
)

// Chapa adapter. Endpoints and field names follow Chapa's public API
// (transaction/initialize, transaction/verify, refund). Confirm them, and the
// webhook signature header, against Chapa's current documentation before
// going live.
type Chapa struct {
	baseURL       string
	secretKey     string
	webhookSecret string
	hc            *http.Client
}

func NewChapa(baseURL, secretKey, webhookSecret string) *Chapa {
	return &Chapa{baseURL: strings.TrimRight(baseURL, "/"), secretKey: secretKey,
		webhookSecret: webhookSecret, hc: &http.Client{Timeout: 20 * time.Second}}
}

func (c *Chapa) Name() string { return "chapa" }

type chapaResponse struct {
	Status  string          `json:"status"`
	Message json.RawMessage `json:"message"`
	Data    json.RawMessage `json:"data"`
}

func (c *Chapa) do(ctx context.Context, method, path string, body any) (*chapaResponse, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.secretKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, providers.ErrProviderUnavailable.WithMessage("chapa: " + err.Error())
	}
	defer resp.Body.Close()
	var out chapaResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, providers.ErrProviderUnavailable.WithMessage(fmt.Sprintf("chapa: HTTP %d, unreadable body", resp.StatusCode))
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, providers.ErrTransactionNotFound
	}
	if resp.StatusCode >= 500 {
		return nil, providers.ErrProviderUnavailable.WithMessage(fmt.Sprintf("chapa: HTTP %d", resp.StatusCode))
	}
	if resp.StatusCode >= 400 || out.Status != "success" {
		return nil, apperror.Unprocessable("provider_rejected", "chapa: "+strings.Trim(string(out.Message), `"`))
	}
	return &out, nil
}

func (c *Chapa) Initiate(ctx context.Context, req providers.InitiateCheckout, callbackURL string) (providers.CheckoutResult, error) {
	first, last, _ := strings.Cut(req.CustomerName, " ")
	out, err := c.do(ctx, http.MethodPost, "/v1/transaction/initialize", map[string]any{
		"amount":       money.FormatMajor(req.Amount),
		"currency":     req.Currency,
		"email":        req.CustomerEmail,
		"first_name":   first,
		"last_name":    last,
		"tx_ref":       req.TxRef,
		"callback_url": callbackURL,
		"return_url":   req.ReturnURL,
	})
	if err != nil {
		return providers.CheckoutResult{}, err
	}
	var data struct {
		CheckoutURL string `json:"checkout_url"`
	}
	if err := json.Unmarshal(out.Data, &data); err != nil || data.CheckoutURL == "" {
		return providers.CheckoutResult{}, providers.ErrProviderUnavailable.WithMessage("chapa: missing checkout_url")
	}
	return providers.CheckoutResult{CheckoutURL: data.CheckoutURL, ProviderRef: req.TxRef}, nil
}

func (c *Chapa) Verify(ctx context.Context, txRef string) (providers.Transaction, error) {
	out, err := c.do(ctx, http.MethodGet, "/v1/transaction/verify/"+txRef, nil)
	if err != nil {
		return providers.Transaction{}, err
	}
	var data struct {
		Status    string          `json:"status"`
		Amount    json.RawMessage `json:"amount"` // number or string
		Currency  string          `json:"currency"`
		Reference string          `json:"reference"`
	}
	if err := json.Unmarshal(out.Data, &data); err != nil {
		return providers.Transaction{}, providers.ErrProviderUnavailable.WithMessage("chapa: bad verify payload")
	}
	amount, err := money.ParseMajor(strings.Trim(string(data.Amount), `"`))
	if err != nil {
		return providers.Transaction{}, providers.ErrProviderUnavailable.WithMessage("chapa: bad amount")
	}
	status := providers.StatusPending
	switch strings.ToLower(data.Status) {
	case "success":
		status = providers.StatusSucceeded
	case "failed", "cancelled":
		status = providers.StatusFailed
	}
	return providers.Transaction{Provider: c.Name(), TxRef: txRef, ProviderRef: data.Reference,
		Status: status, Amount: amount, Currency: data.Currency}, nil
}

func (c *Chapa) Refund(ctx context.Context, req providers.Refund) (providers.RefundResult, error) {
	out, err := c.do(ctx, http.MethodPost, "/v1/refund/"+req.TxRef, map[string]any{
		"amount":    money.FormatMajor(req.Amount),
		"reason":    req.Reason,
		"reference": req.RefundRef,
	})
	if err != nil {
		var ae *apperror.Error
		if errors.As(err, &ae) && ae.Kind == apperror.KindUnprocessable {
			return providers.RefundResult{Status: providers.StatusFailed, FailureReason: ae.Message}, nil
		}
		return providers.RefundResult{}, err
	}
	var data struct {
		RefID string `json:"ref_id"`
	}
	_ = json.Unmarshal(out.Data, &data)
	return providers.RefundResult{Status: providers.StatusSucceeded, ProviderRefundRef: data.RefID}, nil
}

// ParseCallback accepts:
//   - POST webhooks signed with the webhook secret (x-chapa-signature = HMAC-SHA256(secret, body))
//   - GET callback_url redirects (?trx_ref=...), which are unsigned; that is
//     safe because the caller always re-verifies with the API.
func (c *Chapa) ParseCallback(r *http.Request, body []byte) (string, error) {
	if r.Method == http.MethodGet {
		ref := r.URL.Query().Get("trx_ref")
		if ref == "" {
			ref = r.URL.Query().Get("tx_ref")
		}
		if ref == "" {
			return "", apperror.Invalid("missing_tx_ref", "callback has no trx_ref")
		}
		return ref, nil
	}
	sig := r.Header.Get("x-chapa-signature")
	if sig == "" {
		sig = r.Header.Get("Chapa-Signature")
	}
	if c.webhookSecret == "" || !secrets.EqualHex(sig, secrets.HMACHex([]byte(c.webhookSecret), body)) {
		return "", apperror.Unauthorized("invalid_signature", "webhook signature verification failed")
	}
	var payload struct {
		TxRef string `json:"tx_ref"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || payload.TxRef == "" {
		return "", apperror.Invalid("missing_tx_ref", "webhook has no tx_ref")
	}
	return payload.TxRef, nil
}
