# PHP integration

PHP 7.4 or newer with the `curl` and `json` extensions (standard on most
hosts). Works with plain PHP, Laravel, Symfony or WordPress. Read
[How a payment works](README.md#how-a-payment-works) first.

## 1. Add the client

Copy [`Paygate.php`](../../examples/integration/php/Paygate.php) into your
project and configure it once:

```php
require __DIR__ . '/Paygate.php';

use Paygate\Client;
use Paygate\PaygateError;
use Paygate\WebhookError;

$paygate = new Client(getenv('PAYGATE_API_KEY'), getenv('PAYGATE_BASE_URL')); // https://api.paygate.example
```

```bash
# server environment / .env (never commit it, never print it in a page)
PAYGATE_API_KEY=sk_test_...
PAYGATE_WEBHOOK_SECRET=whsec_...
PAYGATE_BASE_URL=https://api.paygate.example
```

## 2. Create a payment and redirect the customer

Plain PHP ([`checkout.php`](../../examples/integration/php/checkout.php)):

```php
$orderId = createOrder(5000); // status "pending" in your database
try {
    $payment = $paygate->createPayment([
        'amount' => 5000,                 // minor units: 5000 = 50.00
        'currency' => 'ETB',
        'payment_method' => 'telebirr',   // or 'card', 'cbe_birr', ...; leave out to let the customer choose
        'reference' => $orderId,
        'customer' => ['email' => $customerEmail],
        'return_url' => "https://shop.example/thanks.php?order=$orderId",
    ], $orderId);                         // Idempotency-Key = your order id
} catch (PaygateError $e) {
    error_log("paygate {$e->status} {$e->errorCode}: {$e->getMessage()}");
    http_response_code(502);
    exit('Payment is unavailable, please try again.');
}
header('Location: ' . $payment['checkout_url'], true, 303);
```

Laravel:

```php
public function checkout(Request $request, Client $paygate)   // bind Client in a service provider
{
    $order = Order::create(['total' => 5000, 'status' => 'pending']);
    $payment = $paygate->createPayment([
        'amount' => $order->total, 'currency' => 'ETB', 'reference' => (string) $order->id,
        'return_url' => route('thanks', ['order' => $order->id]),
    ], (string) $order->id);
    $order->update(['payment_id' => $payment['id']]);
    return redirect()->away($payment['checkout_url'], 303);
}
```

## 3. The customer comes back

Show "payment is being confirmed" until the webhook has marked the order
paid. Do not mark it paid from the return URL.

## 4. Receive webhooks

Plain PHP ([`webhook.php`](../../examples/integration/php/webhook.php)):

```php
$raw = file_get_contents('php://input');      // the RAW body, not $_POST
try {
    $event = Client::verifyWebhook($raw, $_SERVER['HTTP_PAYGATE_SIGNATURE'] ?? '', getenv('PAYGATE_WEBHOOK_SECRET'));
} catch (WebhookError $e) {
    http_response_code(400);
    exit('invalid signature');
}
if (alreadyProcessed($event['id'])) { exit('ok'); }   // events can arrive more than once

$orderId = $event['data']['reference'];
switch ($event['type']) {
    case 'payment.succeeded': markPaid($orderId); break;
    case 'payment.failed':    markFailed($orderId, $event['data']['reason'] ?? ''); break;
    case 'payment.expired':   markExpired($orderId); break;
}
echo 'ok';                                    // answer quickly
```

Laravel: use `$request->getContent()` for the raw body and
`$request->header('Paygate-Signature')`, and exclude the route from CSRF
protection (`VerifyCsrfToken::$except = ['webhooks/paygate']`): the signature
protects it instead.

While rotating the secret pass both: `Client::verifyWebhook($raw, $header, [$oldSecret, $newSecret])`.

## 5. Status, refunds, balance

```php
$p = $paygate->getPayment($paymentId);                  // $p['status'], $p['failure_reason'] ?? null
$page = $paygate->listPayments(['reference' => 'ORDER-1001', 'limit' => 20]);
$refund = $paygate->createRefund(
    ['payment_id' => $paymentId, 'amount' => 2000, 'reason' => 'one item returned'], // omit amount = the rest
    "refund-$orderId-1");
$balance = $paygate->getBalance('ETB');                 // $balance['available']
```

## Errors

```php
try {
    $paygate->createPayment($params, $orderId);
} catch (PaygateError $e) {
    switch ($e->errorCode) {
        case 'payment_method_unavailable': /* offer another method */ break;
        case 'kyc_required':               /* live mode: finish business verification */ break;
        default: error_log("paygate {$e->status} {$e->errorCode}: {$e->getMessage()}");
    }
}
```

Network errors, `429` and `5xx` are retried automatically (2 retries) with the
same Idempotency-Key.

## Test it

```bash
cd examples/integration/php
php test.php                                                               # webhook verification
PAYGATE_BASE_URL=http://localhost:8080 PAYGATE_API_KEY=sk_test_... php test.php   # also the live API
```
