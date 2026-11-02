<?php
// php test.php   (live part needs PAYGATE_BASE_URL and PAYGATE_API_KEY with a test key)
declare(strict_types=1);
require __DIR__ . '/Paygate.php';

use Paygate\Client;
use Paygate\PaygateError;
use Paygate\WebhookError;

function check(bool $ok, string $what): void
{
    echo ($ok ? 'ok   ' : 'FAIL ') . $what . PHP_EOL;
    if (!$ok) {
        exit(1);
    }
}

function rejects(callable $f): bool
{
    try {
        $f();
        return false;
    } catch (WebhookError $e) {
        return true;
    }
}

$v = json_decode(file_get_contents(__DIR__ . '/../../../docs/integration/testdata/webhook-vector.json'), true);
$t = $v['timestamp'];

$e = Client::verifyWebhook($v['body'], $v['header'], $v['secret'], 300, $t + 10);
check($e['type'] === 'payment.succeeded' && $e['data']['reference'] === 'ORDER-1001', 'published webhook vector');
check(is_array(Client::verifyWebhook($v['body'], $v['header'], ['whsec_old', $v['secret']], 300, $t)), 'rotation');
check(rejects(fn () => Client::verifyWebhook(str_replace('5000', '9000', $v['body']), $v['header'], $v['secret'], 300, $t)), 'tampered body');
check(rejects(fn () => Client::verifyWebhook($v['body'], $v['header'], 'whsec_wrong', 300, $t)), 'wrong secret');
check(rejects(fn () => Client::verifyWebhook($v['body'], $v['header'], $v['secret'], 300, $t + 301)), 'too old');
check(rejects(fn () => Client::verifyWebhook($v['body'], 'garbage', $v['secret'], 300, $t)), 'bad header');

if (getenv('PAYGATE_BASE_URL') && getenv('PAYGATE_API_KEY')) {
    $pg = new Client(getenv('PAYGATE_API_KEY'), getenv('PAYGATE_BASE_URL'));
    $ref = 'PHP-' . (int) (microtime(true) * 1000);
    $params = ['amount' => 5000, 'currency' => 'USD', 'payment_method' => 'card', 'reference' => $ref];
    $p = $pg->createPayment($params, $ref);
    check(strpos($p['checkout_url'], 'http') === 0, 'create payment');
    check($pg->createPayment($params, $ref)['id'] === $p['id'], 'same Idempotency-Key, same payment');
    check($pg->getPayment($p['id'])['reference'] === $ref, 'get payment');
    try {
        $pg->createPayment(['amount' => 1] + $params, $ref);
        check(false, 'reused key refused');
    } catch (PaygateError $e) {
        check($e->errorCode === 'idempotency_key_reused', 'reused key refused');
    }
}
