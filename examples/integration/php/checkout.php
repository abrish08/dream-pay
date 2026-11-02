<?php
// POST /checkout.php: create the payment on the server, send the customer to checkout_url.
declare(strict_types=1);
require __DIR__ . '/Paygate.php';

use Paygate\Client;
use Paygate\PaygateError;

$paygate = new Client(getenv('PAYGATE_API_KEY'), getenv('PAYGATE_BASE_URL') ?: 'https://api.paygate.example');
$shop = getenv('SHOP_URL') ?: 'http://localhost:3003';

$orderId = 'ORDER-' . (int) (microtime(true) * 1000);
// save the order as "pending" in your database here

try {
    $payment = $paygate->createPayment([
        'amount' => 5000,               // 50.00
        'currency' => 'USD',
        'payment_method' => 'card',     // or leave out: the customer chooses
        'reference' => $orderId,
        'description' => '2 bags of coffee',
        'customer' => ['email' => 'buyer@example.com'],
        'return_url' => "$shop/thanks.php?order=$orderId",
    ], $orderId);                       // idempotency key = order id
} catch (PaygateError $e) {
    error_log("paygate {$e->status} {$e->errorCode}: {$e->getMessage()}");
    http_response_code(502);
    exit('payment service error');
}

header('Location: ' . $payment['checkout_url'], true, 303);
