package gateway

import (
	"context"
	"net/http"
	"strings"

	"paygate/internal/contracts/providers"
	"paygate/internal/platform/apperror"
)

// BankTransfer is the manual bank transfer method, usable before any bank
// API exists: the customer transfers the exact amount to one of our accounts
// with a unique reference; finance matches it on the bank statement and
// confirms it (POST /admin/bank-transfers/{payment id}/confirm). Until then
// the payment is processing and expires like any other.
//
// A bank API adapter (pay-by-bank, statement API) can later replace the
// confirmation step without changing merchants or the checkout.
type BankTransfer struct {
	name       string
	sandbox    bool
	accounts   []providers.BankAccount
	currencies []string
	publicBase string
	state      TransferState
}

// TransferState persists the transfers (provider store).
type TransferState interface {
	SaveTransfer(ctx context.Context, provider, txRef, reference string, amount int64, currency string) error
	GetTransfer(ctx context.Context, txRef string) (status string, amount int64, currency string, err error)
}

func NewBankTransfer(name string, sandbox bool, accounts []providers.BankAccount, currencies []string, publicBase string,
	state TransferState) *BankTransfer {
	if len(currencies) == 0 {
		currencies = []string{"ETB"}
	}
	return &BankTransfer{name: name, sandbox: sandbox, accounts: accounts, currencies: currencies,
		publicBase: strings.TrimRight(publicBase, "/"), state: state}
}

func (b *BankTransfer) Name() string         { return b.name }
func (b *BankTransfer) IsSandbox() bool      { return b.sandbox }
func (b *BankTransfer) Methods() []string    { return []string{providers.MethodBankTransfer} }
func (b *BankTransfer) Currencies() []string { return b.currencies }

// TransferReference: short, unique, easy to type on a bank transfer form.
func TransferReference(txRef string) string {
	return "PG" + strings.ToUpper(strings.ReplaceAll(txRef, "-", "")[:10])
}

func (b *BankTransfer) Initiate(ctx context.Context, req providers.InitiateCheckout, _ string) (providers.CheckoutResult, error) {
	ref := TransferReference(req.TxRef)
	if err := b.state.SaveTransfer(ctx, b.name, req.TxRef, ref, req.Amount, req.Currency); err != nil {
		return providers.CheckoutResult{}, err
	}
	// Our own checkout page shows the accounts and the reference.
	return providers.CheckoutResult{CheckoutURL: b.publicBase + "/pay/" + req.TxRef, ProviderRef: ref}, nil
}

func (b *BankTransfer) Verify(ctx context.Context, txRef string) (providers.Transaction, error) {
	status, amount, currency, err := b.state.GetTransfer(ctx, txRef)
	if err != nil {
		return providers.Transaction{}, err
	}
	return providers.Transaction{Provider: b.name, TxRef: txRef, ProviderRef: TransferReference(txRef), Status: status,
		Amount: amount, Currency: currency}, nil
}

// Refund: money received by transfer goes back by transfer, done by finance.
func (b *BankTransfer) Refund(context.Context, providers.Refund) (providers.RefundResult, error) {
	return providers.RefundResult{Status: providers.StatusFailed,
		FailureReason: "manual_refund_required: finance must send the money back by bank transfer"}, nil
}

func (b *BankTransfer) ParseCallback(*http.Request, []byte) (string, error) {
	return "", apperror.Invalid("callbacks_not_supported", "bank transfers are confirmed by finance")
}

func (b *BankTransfer) Instructions(txRef string) providers.Instructions {
	return providers.Instructions{Kind: "bank_transfer", Reference: TransferReference(txRef), Accounts: b.accounts}
}
