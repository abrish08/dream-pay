// Package gateway contains one adapter per payment provider. Adding a
// provider (telebirr, CBE Birr, M-Pesa, the card switch, a bank) means adding
// one file that implements Gateway, registering it in the provider module and
// listing it in PROVIDER_ROUTES; nothing else in the system changes.
//
// Contract every adapter must keep:
//   - Initiate is idempotent per TxRef (the payment id): a retry must not
//     charge twice. Return an apperror of kind "unavailable" for network
//     errors / 5xx / timeouts (the router then fails over to the next
//     provider) and any other kind for business errors (no failover).
//   - Verify asks the provider for the authoritative status. Callbacks are
//     never trusted on their own.
//   - Refund is idempotent per RefundRef.
//   - ParseCallback authenticates the provider (signature / certificate /
//     IP allow-list, as the provider specifies) before returning a TxRef.
//   - Amounts are minor units (ETB cents); convert at the edge if the
//     provider wants major units.
package gateway

import (
	"context"
	"net/http"

	"paygate/internal/contracts/providers"
)

type Gateway interface {
	Name() string
	// Methods this provider can process (providers.Method*) and currencies.
	Methods() []string
	Currencies() []string
	Initiate(ctx context.Context, req providers.InitiateCheckout, callbackURL string) (providers.CheckoutResult, error)
	Verify(ctx context.Context, txRef string) (providers.Transaction, error)
	Refund(ctx context.Context, req providers.Refund) (providers.RefundResult, error)
	// ParseCallback authenticates an inbound provider notification and returns
	// the tx_ref it refers to. The caller then calls Verify for the real status.
	ParseCallback(r *http.Request, body []byte) (txRef string, err error)
}
