package paygate

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

type vector struct {
	Secret, Body, Header string
	Timestamp            int64
}

func load(t *testing.T) vector {
	raw, err := os.ReadFile("../../../../docs/integration/testdata/webhook-vector.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vector
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestWebhookVector(t *testing.T) {
	v := load(t)
	at := time.Unix(v.Timestamp+10, 0)
	e, err := VerifyWebhook([]byte(v.Body), v.Header, []string{v.Secret}, 5*time.Minute, at)
	if err != nil || e.Type != "payment.succeeded" || !strings.Contains(string(e.Data), "ORDER-1001") {
		t.Fatalf("vector: %+v %v", e, err)
	}
	if _, err := VerifyWebhook([]byte(v.Body), v.Header, []string{"whsec_old", v.Secret}, 5*time.Minute, at); err != nil {
		t.Fatal("rotation:", err)
	}
	for name, c := range map[string]struct {
		body, header, secret string
		at                   time.Time
	}{
		"tampered":   {strings.Replace(v.Body, "5000", "9000", 1), v.Header, v.Secret, at},
		"wrong key":  {v.Body, v.Header, "whsec_wrong", at},
		"too old":    {v.Body, v.Header, v.Secret, time.Unix(v.Timestamp+301, 0)},
		"bad header": {v.Body, "garbage", v.Secret, at},
	} {
		if _, err := VerifyWebhook([]byte(c.body), c.header, []string{c.secret}, 5*time.Minute, c.at); !errors.Is(err, ErrInvalidSignature) {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
}

func TestLive(t *testing.T) {
	base, key := os.Getenv("PAYGATE_BASE_URL"), os.Getenv("PAYGATE_API_KEY")
	if base == "" || key == "" {
		t.Skip("set PAYGATE_BASE_URL and PAYGATE_API_KEY (test key)")
	}
	ctx := context.Background()
	pg := New(key, WithBaseURL(base))
	ref := "GO-" + strconv.FormatInt(time.Now().UnixMilli(), 10)
	params := PaymentParams{Amount: 5000, Currency: "USD", PaymentMethod: "card", Reference: ref}
	p, err := pg.CreatePayment(ctx, params, ref)
	if err != nil || !strings.HasPrefix(p.CheckoutURL, "http") {
		t.Fatalf("create: %+v %v", p, err)
	}
	if again, _ := pg.CreatePayment(ctx, params, ref); again.ID != p.ID {
		t.Fatal("same Idempotency-Key created a second payment")
	}
	if got, err := pg.GetPayment(ctx, p.ID); err != nil || got.Reference != ref {
		t.Fatalf("get: %v", err)
	}
	if l, err := pg.ListPayments(ctx, url.Values{"reference": {ref}}); err != nil || len(l.Data) != 1 {
		t.Fatalf("list: %v", err)
	}
	params.Amount = 1
	var e *Error
	if _, err := pg.CreatePayment(ctx, params, ref); !errors.As(err, &e) || e.Code != "idempotency_key_reused" {
		t.Fatalf("reused key: %v", err)
	}
	if _, err := pg.GetPayment(ctx, "00000000-0000-0000-0000-000000000000"); !errors.As(err, &e) || e.Status != 404 {
		t.Fatalf("not found: %v", err)
	}
}
