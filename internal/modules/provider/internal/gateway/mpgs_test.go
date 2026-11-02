package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"paygate/internal/contracts/providers"
	"paygate/internal/modules/provider/mpgstest"
	"paygate/internal/platform/apperror"
)

const orderID = "6f1c2a9e-8d41-4b7e-a0c5-91e2d7f4b8a1"

func newMPGS(t *testing.T, bank *mpgstest.Server, password string) *MPGS {
	t.Helper()
	m, err := NewMPGS(MPGSConfig{BaseURL: bank.URL, MerchantID: bank.MerchantID, APIPassword: password,
		NotificationSecret: "hook-secret", PublicBaseURL: "https://pay.example.et", Currencies: []string{"USD"},
		HTTP: bank.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestMPGSPaymentAndRefund(t *testing.T) {
	bank := mpgstest.New("TESTDASHEN01", "api-pass")
	defer bank.Close()
	m := newMPGS(t, bank, "api-pass")
	ctx := context.Background()

	res, err := m.Initiate(ctx, providers.InitiateCheckout{TxRef: orderID, Amount: 5000, Currency: "USD", Method: "card"},
		"https://pay.example.et/providers/mpgs/callback")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(res.CheckoutURL)
	session := u.Query().Get("session")
	if u.Host != "pay.example.et" || u.Path != "/providers/mpgs/pay/"+orderID || session == "" {
		t.Fatalf("checkout url: %s", res.CheckoutURL)
	}
	if _, err := m.Initiate(ctx, providers.InitiateCheckout{TxRef: "x", Amount: 100, Currency: "ETB"}, ""); err == nil {
		t.Fatal("currency the merchant account does not settle was accepted")
	}

	// Not paid yet: pending, never "failed".
	if tx, err := m.Verify(ctx, orderID); err != nil || tx.Status != providers.StatusPending {
		t.Fatalf("unpaid order: %+v %v", tx, err)
	}
	back := bank.Pay(session, true)
	if !strings.HasPrefix(back, "https://pay.example.et/providers/mpgs/return/"+orderID) {
		t.Fatalf("return url: %s", back)
	}
	tx, err := m.Verify(ctx, orderID)
	if err != nil || tx.Status != providers.StatusSucceeded || tx.Amount != 5000 || tx.Currency != "USD" {
		t.Fatalf("paid order: %+v %v", tx, err)
	}

	// Refund twice with the same id: sent once.
	r1, err := m.Refund(ctx, providers.Refund{TxRef: orderID, RefundRef: "rf-1", Amount: 1500, Currency: "USD"})
	r2, err2 := m.Refund(ctx, providers.Refund{TxRef: orderID, RefundRef: "rf-1", Amount: 1500, Currency: "USD"})
	if err != nil || err2 != nil || r1.Status != providers.StatusSucceeded || r2.Status != providers.StatusSucceeded || len(bank.Refunds) != 1 {
		t.Fatalf("idempotent refund: %+v %+v %v %v %d", r1, r2, err, err2, len(bank.Refunds))
	}
	bank.Declines["10.00"] = true
	if r, err := m.Refund(ctx, providers.Refund{TxRef: orderID, RefundRef: "rf-2", Amount: 1000, Currency: "USD"}); err != nil ||
		r.Status != providers.StatusFailed || r.FailureReason != "declined" {
		t.Fatalf("declined refund: %+v %v", r, err)
	}
	// Still paid after partial refunds.
	if tx, _ := m.Verify(ctx, orderID); tx.Status != providers.StatusSucceeded {
		t.Fatalf("partially refunded order: %+v", tx)
	}
}

func TestMPGSDeclinedCard(t *testing.T) {
	bank := mpgstest.New("TESTDASHEN01", "api-pass")
	defer bank.Close()
	m := newMPGS(t, bank, "api-pass")
	res, _ := m.Initiate(context.Background(), providers.InitiateCheckout{TxRef: orderID, Amount: 5000, Currency: "USD"}, "")
	u, _ := url.Parse(res.CheckoutURL)
	bank.Pay(u.Query().Get("session"), false)
	if tx, _ := m.Verify(context.Background(), orderID); tx.Status != providers.StatusFailed {
		t.Fatalf("declined: %+v", tx)
	}
}

func TestMPGSWrongCredentialsFailOver(t *testing.T) {
	bank := mpgstest.New("TESTDASHEN01", "api-pass")
	defer bank.Close()
	m := newMPGS(t, bank, "wrong")
	_, err := m.Initiate(context.Background(), providers.InitiateCheckout{TxRef: orderID, Amount: 5000, Currency: "USD"}, "")
	var ae *apperror.Error
	if !errors.As(err, &ae) || ae.Kind != apperror.KindUnavailable {
		t.Fatalf("auth failure must be 'unavailable' (router fails over): %v", err)
	}
	bank.Close()
	if _, err := m.Verify(context.Background(), orderID); !errors.As(err, &ae) || ae.Kind != apperror.KindUnavailable {
		t.Fatalf("network error: %v", err)
	}
}

func TestMPGSCallbackSecret(t *testing.T) {
	bank := mpgstest.New("TESTDASHEN01", "api-pass")
	defer bank.Close()
	m := newMPGS(t, bank, "api-pass")
	body := []byte(`{"order":{"id":"` + orderID + `"}}`)
	req := httptest.NewRequest(http.MethodPost, "/providers/mpgs/callback", nil)
	if _, err := m.ParseCallback(req, body); err == nil {
		t.Fatal("unsigned notification accepted")
	}
	req.Header.Set("X-Notification-Secret", "hook-secret")
	if id, err := m.ParseCallback(req, body); err != nil || id != orderID {
		t.Fatalf("notification: %s %v", id, err)
	}
	for _, base := range []string{"http://gateway.example", "http://127.0.0.1:9100", "https://10.0.0.5",
		"https://ap-gateway.mastercard.com/api", "https://user:pw@ap-gateway.mastercard.com"} {
		if _, err := NewMPGS(MPGSConfig{BaseURL: base, MerchantID: "x", APIPassword: "y"}); err == nil {
			t.Errorf("live merchant accepted base URL %s", base)
		}
	}
	if _, err := NewMPGS(MPGSConfig{BaseURL: "http://127.0.0.1:9100", MerchantID: "x", APIPassword: "y", Sandbox: true}); err != nil {
		t.Fatal("local simulator as TEST merchant refused")
	}
	if _, err := NewMPGS(MPGSConfig{BaseURL: "https://ap-gateway.mastercard.com", MerchantID: "x", APIPassword: "y"}); err != nil {
		t.Fatal(err)
	}
}

func TestMPGSHealthCheck(t *testing.T) {
	bank := mpgstest.New("TESTDASHEN01", "api-pass")
	defer bank.Close()
	if v, err := newMPGS(t, bank, "api-pass").Check(context.Background()); err != nil || v == "" {
		t.Fatalf("check: %q %v", v, err)
	}
	if _, err := newMPGS(t, bank, "wrong").Check(context.Background()); !errors.Is(err, ErrMPGSCredentials) {
		t.Fatalf("wrong password must be a credentials error: %v", err)
	}
	m := newMPGS(t, bank, "api-pass")
	bank.Close()
	var ae *apperror.Error
	if _, err := m.Check(context.Background()); !errors.As(err, &ae) || ae.Kind != apperror.KindUnavailable {
		t.Fatalf("down: %v", err)
	}
}
