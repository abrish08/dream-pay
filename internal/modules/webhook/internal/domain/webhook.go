package domain

import (
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"time"

	"paygate/internal/contracts/payments"
	"paygate/internal/contracts/payouts"
	"paygate/internal/contracts/refunds"
	"paygate/internal/platform/apperror"
	"paygate/internal/platform/secrets"
)

// PublicEventTypes maps internal topics to the event names merchants see.
// Internal topics can change version without breaking merchants.
var PublicEventTypes = map[string]string{
	payments.TopicPaymentSucceeded: "payment.succeeded",
	payments.TopicPaymentFailed:    "payment.failed",
	payments.TopicPaymentExpired:   "payment.expired",
	refunds.TopicRefundSucceeded:   "refund.succeeded",
	refunds.TopicRefundFailed:      "refund.failed",
	payouts.TopicPayoutPaid:        "payout.paid",
	payouts.TopicPayoutFailed:      "payout.failed",
}

const MaxAttempts = 8

type Endpoint struct {
	ID              string
	MerchantID      string
	URL             string
	Events          []string // public event types, or "*"
	SecretEncrypted string
	Active          bool
	CreatedAt       time.Time
}

func (e Endpoint) Wants(eventType string) bool {
	return e.Active && (slices.Contains(e.Events, "*") || slices.Contains(e.Events, eventType))
}

func ValidateEvents(evts []string) error {
	if len(evts) == 0 {
		return apperror.Invalid("invalid_events", `events is required (use ["*"] for all)`)
	}
	for _, e := range evts {
		if e == "*" {
			continue
		}
		found := false
		for _, pub := range PublicEventTypes {
			if pub == e {
				found = true
			}
		}
		if !found {
			return apperror.Invalid("invalid_events", "unknown event type "+e)
		}
	}
	return nil
}

func ValidateURL(raw string, allowInsecure bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return apperror.Invalid("invalid_url", "url must be an absolute http(s) URL")
	}
	if u.Scheme == "http" && !allowInsecure {
		return apperror.Invalid("invalid_url", "webhook URLs must use https")
	}
	return nil
}

type Delivery struct {
	ID             string
	EndpointID     string
	MerchantID     string
	EventID        string
	EventType      string
	Payload        string
	Status         string // pending | delivered | failed
	Attempts       int
	NextAttemptAt  time.Time
	LockedUntil    *time.Time
	LastStatusCode int
	LastError      string
	CreatedAt      time.Time
	DeliveredAt    *time.Time
}

// Sign produces "t=<unix>,v1=<hex hmac>" over "<unix>.<body>". Including the
// timestamp lets receivers reject replays.
func Sign(secret string, ts int64, body []byte) string {
	mac := secrets.HMACHex([]byte(secret), append([]byte(strconv.FormatInt(ts, 10)+"."), body...))
	return fmt.Sprintf("t=%d,v1=%s", ts, mac)
}

// NextAttempt: 30s, 1m, 2m, 4m ... capped at 6h.
func NextAttempt(attempts int) time.Duration {
	d := 30 * time.Second << min(attempts-1, 10)
	return min(d, 6*time.Hour)
}
