// Package paygate is a Go client for the paygate API (standard library only).
//
//	pg := paygate.New(os.Getenv("PAYGATE_API_KEY"), paygate.WithBaseURL("https://api.paygate.example"))
//	p, err := pg.CreatePayment(ctx, paygate.PaymentParams{Amount: 5000, Currency: "USD", Reference: "ORDER-1001"}, "ORDER-1001")
//	http.Redirect(w, r, p.CheckoutURL, http.StatusSeeOther)
package paygate

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
	"math"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client talks to the paygate API. Use it on your server only: the API key
// is a secret and must never reach a browser or a mobile app.
type Client struct {
	apiKey     string
	baseURL    string
	http       *http.Client
	maxRetries int
}

type Option func(*Client)

func WithBaseURL(u string) Option          { return func(c *Client) { c.baseURL = strings.TrimRight(u, "/") } }
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }
func WithMaxRetries(n int) Option          { return func(c *Client) { c.maxRetries = n } }

func New(apiKey string, opts ...Option) *Client {
	c := &Client{apiKey: apiKey, baseURL: "https://api.paygate.example", http: &http.Client{Timeout: 30 * time.Second}, maxRetries: 2}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Error is an API error. Status 0 means a network error.
type Error struct {
	Status  int
	Kind    string // invalid, unauthorized, forbidden, not_found, conflict, unprocessable, ...
	Code    string // e.g. idempotency_key_reused, kyc_required, payment_method_unavailable
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("paygate: %d %s: %s", e.Status, e.Code, e.Message) }

// ---- Payments ----

type Customer struct {
	Email string `json:"email,omitempty"`
	Name  string `json:"name,omitempty"`
}

type PaymentParams struct {
	Amount           int64             `json:"amount"` // minor units: 5000 = 50.00
	Currency         string            `json:"currency"`
	PaymentMethod    string            `json:"payment_method,omitempty"` // telebirr, cbe_birr, mpesa, card, bank_transfer; empty = customer chooses
	Reference        string            `json:"reference"`                // your order id
	Description      string            `json:"description,omitempty"`
	Customer         *Customer         `json:"customer,omitempty"`
	ReturnURL        string            `json:"return_url,omitempty"`
	ExpiresInMinutes int               `json:"expires_in_minutes,omitempty"`
	Metadata         map[string]string `json:"metadata,omitempty"`
}

type Payment struct {
	ID             string            `json:"id"`
	Environment    string            `json:"environment"`
	Amount         int64             `json:"amount"`
	Fee            int64             `json:"fee"`
	RefundedAmount int64             `json:"refunded_amount"`
	Currency       string            `json:"currency"`
	Status         string            `json:"status"` // processing, succeeded, failed, expired, partially_refunded, refunded
	PaymentMethod  string            `json:"payment_method"`
	Reference      string            `json:"reference"`
	Description    string            `json:"description"`
	Metadata       map[string]string `json:"metadata"`
	CheckoutURL    string            `json:"checkout_url"`
	FailureReason  string            `json:"failure_reason"`
	ExpiresAt      time.Time         `json:"expires_at"`
	CreatedAt      time.Time         `json:"created_at"`
}

type PaymentList struct {
	Data    []Payment `json:"data"`
	HasMore bool      `json:"has_more"`
}

// CreatePayment: idempotencyKey is your order id; a retry with the same key
// returns the same payment instead of creating a second one.
func (c *Client) CreatePayment(ctx context.Context, p PaymentParams, idempotencyKey string) (*Payment, error) {
	var out Payment
	return &out, c.do(ctx, http.MethodPost, "/v1/payments", p, idempotencyKey, &out)
}

func (c *Client) GetPayment(ctx context.Context, id string) (*Payment, error) {
	var out Payment
	return &out, c.do(ctx, http.MethodGet, "/v1/payments/"+url.PathEscape(id), nil, "", &out)
}

// ListPayments: query keys status, reference, limit, starting_after, created_from, created_to.
func (c *Client) ListPayments(ctx context.Context, query url.Values) (*PaymentList, error) {
	var out PaymentList
	path := "/v1/payments"
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	return &out, c.do(ctx, http.MethodGet, path, nil, "", &out)
}

// ---- Refunds ----

type RefundParams struct {
	PaymentID string `json:"payment_id"`
	Amount    int64  `json:"amount,omitempty"` // 0 = refund the rest
	Reason    string `json:"reason,omitempty"`
}

type Refund struct {
	ID            string `json:"id"`
	PaymentID     string `json:"payment_id"`
	Amount        int64  `json:"amount"`
	Currency      string `json:"currency"`
	Status        string `json:"status"` // pending, succeeded, failed
	FailureReason string `json:"failure_reason"`
}

func (c *Client) CreateRefund(ctx context.Context, p RefundParams, idempotencyKey string) (*Refund, error) {
	var out Refund
	return &out, c.do(ctx, http.MethodPost, "/v1/refunds", p, idempotencyKey, &out)
}

func (c *Client) do(ctx context.Context, method, path string, body any, idemKey string, out any) error {
	if method == http.MethodPost && idemKey == "" {
		return errors.New("paygate: an Idempotency-Key is required (use your order id)")
	}
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}
	// Safe to retry: GETs, and POSTs with an Idempotency-Key (never charges twice).
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if idemKey != "" {
			req.Header.Set("Idempotency-Key", idemKey)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			if attempt < c.maxRetries && ctx.Err() == nil {
				sleep(ctx, attempt)
				continue
			}
			return &Error{Status: 0, Code: "network_error", Message: err.Error()}
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode < 300 {
			return json.Unmarshal(raw, out)
		}
		if (resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests) && attempt < c.maxRetries {
			sleep(ctx, attempt)
			continue
		}
		var e struct {
			Error struct{ Kind, Code, Message string } `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		return &Error{Status: resp.StatusCode, Kind: e.Error.Kind, Code: e.Error.Code, Message: e.Error.Message}
	}
}

func sleep(ctx context.Context, attempt int) {
	d := time.Duration(300*math.Pow(2, float64(attempt)))*time.Millisecond + time.Duration(rand.IntN(100))*time.Millisecond
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// ---- Webhooks ----

// Event is a webhook. Data holds the event's fields, e.g. payment_id,
// reference, amount, currency (and reason for payment.failed).
type Event struct {
	ID      string          `json:"id"`
	Type    string          `json:"type"` // payment.succeeded, payment.failed, payment.expired, refund.succeeded, ...
	Created int64           `json:"created"`
	Data    json.RawMessage `json:"data"`
}

var ErrInvalidSignature = errors.New("paygate: invalid webhook signature")

// VerifyWebhook checks the Paygate-Signature header ("t=<unix>,v1=<hex>")
// against the raw body and returns the event. Pass several secrets while
// rotating. tolerance limits replay (use 5 minutes).
func VerifyWebhook(rawBody []byte, header string, secrets []string, tolerance time.Duration, now time.Time) (*Event, error) {
	var ts int64
	var given []byte
	for _, part := range strings.Split(header, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch k {
		case "t":
			ts, _ = strconv.ParseInt(v, 10, 64)
		case "v1":
			given, _ = hex.DecodeString(v)
		}
	}
	if ts == 0 || len(given) != sha256.Size {
		return nil, fmt.Errorf("%w: malformed header", ErrInvalidSignature)
	}
	if d := now.Sub(time.Unix(ts, 0)); d > tolerance || d < -tolerance {
		return nil, fmt.Errorf("%w: timestamp outside tolerance", ErrInvalidSignature)
	}
	for _, s := range secrets {
		m := hmac.New(sha256.New, []byte(s))
		m.Write([]byte(strconv.FormatInt(ts, 10) + "."))
		m.Write(rawBody)
		if hmac.Equal(m.Sum(nil), given) {
			var e Event
			if err := json.Unmarshal(rawBody, &e); err != nil {
				return nil, err
			}
			return &e, nil
		}
	}
	return nil, fmt.Errorf("%w: mismatch", ErrInvalidSignature)
}
