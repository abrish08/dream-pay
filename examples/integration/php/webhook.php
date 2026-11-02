<?php
// POST /webhook.php: paygate calls this. Verify the RAW body, then update the order once.
declare(strict_types=1);
require __DIR__ . '/Paygate.php';

use Paygate\Client;
use Paygate\WebhookError;

$raw = file_get_contents('php://input');
try {
    $event = Client::verifyWebhook($raw, $_SERVER['HTTP_PAYGATE_SIGNATURE'] ?? '', getenv('PAYGATE_WEBHOOK_SECRET'));
} catch (WebhookError $e) {
    http_response_code(400);
    exit('invalid signature');
}

// Events can arrive more than once: skip $event['id'] if you already processed it
// (e.g. a unique index on a processed_events table).
$orderId = $event['data']['reference'];
switch ($event['type']) {
    case 'payment.succeeded':
        // mark $orderId paid, ship the goods
        break;
    case 'payment.failed':
        // mark $orderId failed; $event['data']['reason'] says why (e.g. insufficient_funds)
        break;
    case 'payment.expired':
        // the customer did not pay in time
        break;
}
http_response_code(200); // answer 2xx quickly, or paygate retries
echo 'ok';
