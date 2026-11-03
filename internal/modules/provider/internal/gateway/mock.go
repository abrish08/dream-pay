package gateway

import (
	"context"
	"net/http"

	"paygate/internal/contracts/providers"
	"paygate/internal/platform/apperror"
)

// MockState persists the simulated provider's side of each transaction.
type MockState interface {
	SaveMock(ctx context.Context, txRef string, amount int64, currency, status string) error
	GetMock(ctx context.Context, txRef string) (amount int64, currency, status string, err error)
	SetMockStatus(ctx context.Context, txRef, status string) error
}

// Mock is a fully working fake provider for development and tests. Customers
// "pay" on a hosted page served by the provider module (/providers/mock/checkout/{txRef}).
type Mock struct {
	state   MockState
	baseURL string
}

func NewMock(state MockState, publicBaseURL string) *Mock {
	return &Mock{state: state, baseURL: publicBaseURL}
}

func (m *Mock) Name() string    { return "mock" }
func (m *Mock) IsSandbox() bool { return true }

// The sandbox simulates every method, so test mode works for all of them.
func (m *Mock) Methods() []string    { return providers.Methods }
func (m *Mock) Currencies() []string { return []string{"ETB", "USD"} }

// Sandbox amounts that simulate provider trouble (any currency):
//
//	...9.99 (amount % 1000 == 999): the provider receives the request but its
//	        answer never arrives (timeout). The customer can still pay on
//	        /providers/mock/checkout/{id}; the status checks find it.
//	...9.98 (amount % 1000 == 998): the provider refuses the connection
//	        (nothing sent): the payment fails at once.
const (
	MockOutcomeUnknown = 999
	MockUnreachable    = 998
)

func (m *Mock) Initiate(ctx context.Context, req providers.InitiateCheckout, _ string) (providers.CheckoutResult, error) {
	if req.Amount%1000 == MockUnreachable {
		return providers.CheckoutResult{}, providers.ErrProviderUnavailable.WithMessage("sandbox: connection refused")
	}
	if err := m.state.SaveMock(ctx, req.TxRef, req.Amount, req.Currency, providers.StatusPending); err != nil {
		return providers.CheckoutResult{}, err
	}
	if req.Amount%1000 == MockOutcomeUnknown {
		return providers.CheckoutResult{}, providers.ErrOutcomeUnknown.WithMessage("sandbox: no answer from the provider")
	}
	return providers.CheckoutResult{
		CheckoutURL: m.baseURL + "/providers/mock/checkout/" + req.TxRef,
		ProviderRef: "mock_" + req.TxRef,
	}, nil
}

func (m *Mock) Verify(ctx context.Context, txRef string) (providers.Transaction, error) {
	amount, currency, status, err := m.state.GetMock(ctx, txRef)
	if err != nil {
		return providers.Transaction{}, err
	}
	return providers.Transaction{Provider: m.Name(), TxRef: txRef, ProviderRef: "mock_" + txRef,
		Status: status, Amount: amount, Currency: currency}, nil
}

func (m *Mock) Refund(_ context.Context, req providers.Refund) (providers.RefundResult, error) {
	// Simulate a provider-side decline for amounts ending in 13 minor units.
	if req.Amount%100 == 13 {
		return providers.RefundResult{Status: providers.StatusFailed, FailureReason: "declined_by_provider"}, nil
	}
	return providers.RefundResult{Status: providers.StatusSucceeded, ProviderRefundRef: "mockrf_" + req.RefundRef}, nil
}

func (m *Mock) ParseCallback(*http.Request, []byte) (string, error) {
	return "", apperror.Invalid("callbacks_not_supported", "the mock provider completes payments via its checkout page")
}

// Complete simulates the customer finishing payment on the mock page.
func (m *Mock) Complete(ctx context.Context, txRef string, succeeded bool) error {
	status := providers.StatusFailed
	if succeeded {
		status = providers.StatusSucceeded
	}
	return m.state.SetMockStatus(ctx, txRef, status)
}
