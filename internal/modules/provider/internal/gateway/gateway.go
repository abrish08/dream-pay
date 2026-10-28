// Package gateway contains one adapter per payment service provider. Adding a
// provider (e.g. telebirr) means adding one file that implements Gateway and
// registering it in the provider module; nothing else changes.
package gateway

import (
	"context"
	"net/http"

	"paygate/internal/contracts/providers"
)

type Gateway interface {
	Name() string
	Initiate(ctx context.Context, req providers.InitiateCheckout, callbackURL string) (providers.CheckoutResult, error)
	Verify(ctx context.Context, txRef string) (providers.Transaction, error)
	Refund(ctx context.Context, req providers.Refund) (providers.RefundResult, error)
	// ParseCallback authenticates an inbound provider notification and returns
	// the tx_ref it refers to. The caller then calls Verify for the real status.
	ParseCallback(r *http.Request, body []byte) (txRef string, err error)
}
