package service

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"paygate/internal/contracts/providers"
	"paygate/internal/modules/provider/internal/gateway"
	"paygate/internal/platform/apperror"
)

// fake is a provider adapter whose Initiate result the test controls.
type fake struct {
	name    string
	methods []string
	err     error
	calls   int
}

func (f *fake) Name() string         { return f.name }
func (f *fake) Methods() []string    { return f.methods }
func (f *fake) Currencies() []string { return []string{"ETB"} }
func (f *fake) Initiate(context.Context, providers.InitiateCheckout, string) (providers.CheckoutResult, error) {
	f.calls++
	return providers.CheckoutResult{CheckoutURL: "https://" + f.name}, f.err
}
func (f *fake) Verify(context.Context, string) (providers.Transaction, error) {
	return providers.Transaction{}, nil
}
func (f *fake) Refund(context.Context, providers.Refund) (providers.RefundResult, error) {
	return providers.RefundResult{}, nil
}
func (f *fake) ParseCallback(*http.Request, []byte) (string, error) { return "", nil }

func setup() (*Router, map[string]gateway.Gateway, *fake, *fake, *fake) {
	direct := &fake{name: "telebirr_direct", methods: []string{providers.MethodTelebirr}}
	backup := &fake{name: "aggregator", methods: []string{providers.MethodTelebirr, providers.MethodCard}}
	mock := &fake{name: "mock", methods: providers.Methods}
	gws := map[string]gateway.Gateway{direct.name: direct, backup.name: backup, mock.name: mock}
	r := NewRouter(map[string][]string{
		providers.MethodTelebirr: {"telebirr_direct", "aggregator"},
		providers.MethodCard:     {"aggregator"},
	}, false)
	return r, gws, direct, backup, mock
}

func names(gs []gateway.Gateway) []string {
	out := make([]string, len(gs))
	for i, g := range gs {
		out[i] = g.Name()
	}
	return out
}

func TestRouting(t *testing.T) {
	r, gws, _, _, mock := setup()
	live := providers.InitiateCheckout{Method: providers.MethodTelebirr, Environment: "live", Currency: "ETB"}
	got, err := r.Candidates(gws, mock, live)
	if err != nil || len(got) != 2 || got[0].Name() != "telebirr_direct" || got[1].Name() != "aggregator" {
		t.Fatalf("telebirr route: %v %v", names(got), err)
	}
	test := live
	test.Environment = "test"
	if got, _ := r.Candidates(gws, mock, test); len(got) != 1 || got[0].Name() != "mock" {
		t.Fatalf("test mode must use the sandbox only: %v", names(got))
	}
	none := live
	none.Method = providers.MethodMPesa
	if _, err := r.Candidates(gws, mock, none); !errors.Is(err, providers.ErrMethodUnavailable) {
		t.Fatalf("unrouted method: %v", err)
	}
	usd := live
	usd.Currency = "USD"
	if _, err := r.Candidates(gws, mock, usd); !errors.Is(err, providers.ErrMethodUnavailable) {
		t.Fatalf("unsupported currency: %v", err)
	}
	bad := live
	bad.Method = "bitcoin"
	if _, err := r.Candidates(gws, mock, bad); err == nil {
		t.Fatal("unknown method accepted")
	}
}

func TestBreakerMovesTrafficToBackup(t *testing.T) {
	r, gws, _, _, mock := setup()
	now := time.Now()
	r.now = func() time.Time { return now }
	req := providers.InitiateCheckout{Method: providers.MethodTelebirr, Environment: "live", Currency: "ETB"}
	for i := 0; i < breakerThreshold; i++ {
		r.Failure("telebirr_direct", errors.New("timeout"))
	}
	got, _ := r.Candidates(gws, mock, req)
	if got[0].Name() != "aggregator" {
		t.Fatalf("open breaker must put the backup first: %v", names(got))
	}
	if _, ok := r.Available(gws, mock, "live", providers.MethodTelebirr, "ETB"); !ok {
		t.Fatal("method still available through the backup")
	}
	now = now.Add(breakerCooldown + time.Second)
	if got, _ := r.Candidates(gws, mock, req); got[0].Name() != "telebirr_direct" {
		t.Fatalf("after cool-down the preferred provider is tried again: %v", names(got))
	}
	r.Success("telebirr_direct")
	for _, h := range r.Health(gws) {
		if !h.Healthy {
			t.Fatalf("unhealthy after success: %+v", h)
		}
	}
}

func TestRetryable(t *testing.T) {
	if !Retryable(errors.New("dial tcp: timeout")) || !Retryable(providers.ErrProviderUnavailable) {
		t.Fatal("outages must fail over")
	}
	if Retryable(apperror.Invalid("invalid_phone", "bad phone number")) {
		t.Fatal("a business error must not be retried at another provider")
	}
}
