# Go integration

Go 1.22 or newer. No dependencies: standard library only. Read
[How a payment works](README.md#how-a-payment-works) first.

## 1. Add the client

Copy the [`paygate`](../../examples/integration/go/paygate/paygate.go) package
into your module and configure it once:

```go
pg := paygate.New(os.Getenv("PAYGATE_API_KEY"),
	paygate.WithBaseURL(os.Getenv("PAYGATE_BASE_URL"))) // https://api.paygate.example
```

```bash
PAYGATE_API_KEY=sk_test_...         # server only, never in a frontend or app
PAYGATE_WEBHOOK_SECRET=whsec_...
PAYGATE_BASE_URL=https://api.paygate.example
```

## 2. Create a payment and redirect the customer

```go
http.HandleFunc("POST /checkout", func(w http.ResponseWriter, r *http.Request) {
	order := createOrder(5000) // status "pending" in your database
	p, err := pg.CreatePayment(r.Context(), paygate.PaymentParams{
		Amount:        order.Total, // minor units: 5000 = 50.00
		Currency:      "ETB",
		PaymentMethod: "telebirr", // or "card", "cbe_birr", ...; empty = the customer chooses
		Reference:     order.ID,
		Customer:      &paygate.Customer{Email: order.Email},
		ReturnURL:     "https://shop.example/thanks?order=" + url.QueryEscape(order.ID),
	}, order.ID) // Idempotency-Key = your order id
	if err != nil {
		log.Print(err)
		http.Error(w, "Payment is unavailable, please try again.", http.StatusBadGateway)
		return
	}
	http.Redirect(w, r, p.CheckoutURL, http.StatusSeeOther)
})
```

## 3. The customer comes back

Show "payment is being confirmed" until the webhook has marked the order
paid. Do not mark it paid from the return URL.

## 4. Receive webhooks

```go
http.HandleFunc("POST /webhooks/paygate", func(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // the RAW body
	e, err := paygate.VerifyWebhook(raw, r.Header.Get("Paygate-Signature"),
		[]string{os.Getenv("PAYGATE_WEBHOOK_SECRET")}, 5*time.Minute, time.Now())
	if err != nil {
		http.Error(w, "invalid signature", http.StatusBadRequest)
		return
	}
	if alreadyProcessed(e.ID) { // events can arrive more than once
		return
	}
	var data struct {
		Reference string `json:"reference"`
		Reason    string `json:"reason"`
	}
	_ = json.Unmarshal(e.Data, &data)
	switch e.Type {
	case "payment.succeeded":
		markPaid(data.Reference)
	case "payment.failed":
		markFailed(data.Reference, data.Reason) // e.g. insufficient_funds
	case "payment.expired":
		markExpired(data.Reference)
	}
	w.WriteHeader(http.StatusOK) // answer quickly
})
```

While rotating the secret pass both: `[]string{oldSecret, newSecret}`.

## 5. Status, refunds

```go
p, err := pg.GetPayment(ctx, paymentID)                       // p.Status, p.FailureReason
list, err := pg.ListPayments(ctx, url.Values{"reference": {"ORDER-1001"}, "limit": {"20"}})
rf, err := pg.CreateRefund(ctx, paygate.RefundParams{PaymentID: paymentID, Amount: 2000, Reason: "one item returned"},
	"refund-"+orderID+"-1")                                   // Amount 0 = refund the rest
```

## Errors

```go
var pe *paygate.Error
if errors.As(err, &pe) {
	switch pe.Code {
	case "payment_method_unavailable": // offer another method
	case "kyc_required": // live mode: finish business verification
	default:
		log.Printf("paygate %d %s: %s", pe.Status, pe.Code, pe.Message)
	}
}
```

Network errors, `429` and `5xx` are retried automatically (2 retries) with the
same Idempotency-Key.

## Test it

```bash
cd examples/integration/go
go test ./...                                                                 # webhook verification
PAYGATE_BASE_URL=http://localhost:8080 PAYGATE_API_KEY=sk_test_... go test ./...   # also the live API
```

A complete runnable shop is in [`shop/main.go`](../../examples/integration/go/shop/main.go).
