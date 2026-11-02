<?php
// Paygate.php: PHP 7.4+ client for the paygate API (needs the curl and json extensions).
declare(strict_types=1);

namespace Paygate;

class PaygateError extends \RuntimeException
{
    /** @var int HTTP status, 0 = network error */
    public $status;
    /** @var string invalid | unauthorized | forbidden | not_found | conflict | unprocessable | ... */
    public $kind;
    /** @var string e.g. idempotency_key_reused, kyc_required, payment_method_unavailable */
    public $errorCode;

    public function __construct(int $status, array $body = [])
    {
        $e = $body['error'] ?? [];
        parent::__construct($e['message'] ?? "paygate request failed with HTTP $status");
        $this->status = $status;
        $this->kind = $e['kind'] ?? '';
        $this->errorCode = $e['code'] ?? '';
    }
}

class WebhookError extends \RuntimeException
{
}

class Client
{
    private $apiKey;
    private $baseUrl;
    private $timeout;
    private $maxRetries;

    /** Server side only: the API key must never reach a browser or a mobile app. */
    public function __construct(string $apiKey, string $baseUrl = 'https://api.paygate.example', int $timeout = 30, int $maxRetries = 2)
    {
        if ($apiKey === '') {
            throw new \InvalidArgumentException('paygate: API key is required');
        }
        $this->apiKey = $apiKey;
        $this->baseUrl = rtrim($baseUrl, '/');
        $this->timeout = $timeout;
        $this->maxRetries = $maxRetries;
    }

    /** amount in minor units (5000 = 50.00). $idempotencyKey: your order id. */
    public function createPayment(array $params, string $idempotencyKey): array
    {
        return $this->request('POST', '/v1/payments', $params, self::requireKey($idempotencyKey));
    }

    public function getPayment(string $id): array
    {
        return $this->request('GET', '/v1/payments/' . rawurlencode($id));
    }

    /** $query: status, reference, limit, starting_after, created_from, created_to */
    public function listPayments(array $query = []): array
    {
        return $this->request('GET', '/v1/payments' . ($query ? '?' . http_build_query($query) : ''));
    }

    /** $params: payment_id, amount (omit = the rest), reason */
    public function createRefund(array $params, string $idempotencyKey): array
    {
        return $this->request('POST', '/v1/refunds', $params, self::requireKey($idempotencyKey));
    }

    public function getRefund(string $id): array
    {
        return $this->request('GET', '/v1/refunds/' . rawurlencode($id));
    }

    public function getBalance(string $currency = 'ETB'): array
    {
        return $this->request('GET', '/v1/balance?currency=' . rawurlencode($currency));
    }

    private function request(string $method, string $path, ?array $body = null, ?string $idempotencyKey = null): array
    {
        $headers = ['Authorization: Bearer ' . $this->apiKey, 'Accept: application/json'];
        if ($body !== null) {
            $headers[] = 'Content-Type: application/json';
        }
        if ($idempotencyKey !== null) {
            $headers[] = 'Idempotency-Key: ' . $idempotencyKey;
        }
        // Safe to retry: GETs, and POSTs with an Idempotency-Key (never charges twice).
        for ($attempt = 0; ; $attempt++) {
            $ch = curl_init($this->baseUrl . $path);
            curl_setopt_array($ch, [
                CURLOPT_CUSTOMREQUEST => $method,
                CURLOPT_HTTPHEADER => $headers,
                CURLOPT_RETURNTRANSFER => true,
                CURLOPT_TIMEOUT => $this->timeout,
                CURLOPT_FOLLOWLOCATION => false,
            ]);
            if ($body !== null) {
                curl_setopt($ch, CURLOPT_POSTFIELDS, json_encode($body));
            }
            $raw = curl_exec($ch);
            $status = (int) curl_getinfo($ch, CURLINFO_RESPONSE_CODE);
            $netError = $raw === false ? curl_error($ch) : '';
            curl_close($ch);

            if ($raw === false) {
                if ($attempt < $this->maxRetries) {
                    self::backoff($attempt);
                    continue;
                }
                throw new PaygateError(0, ['error' => ['message' => "network error: $netError"]]);
            }
            $data = $raw === '' ? [] : (json_decode($raw, true) ?? []);
            if ($status >= 200 && $status < 300) {
                return $data;
            }
            if (($status >= 500 || $status === 429) && $attempt < $this->maxRetries) {
                self::backoff($attempt);
                continue;
            }
            throw new PaygateError($status, $data);
        }
    }

    /**
     * Verify a webhook and return the event. Throws WebhookError if it is not from paygate.
     *
     * @param string          $rawBody   file_get_contents('php://input'): the body exactly as received
     * @param string          $header    $_SERVER['HTTP_PAYGATE_SIGNATURE'], "t=<unix>,v1=<hex>"
     * @param string|string[] $secrets   the endpoint secret (several while rotating)
     */
    public static function verifyWebhook(string $rawBody, string $header, $secrets, int $tolerance = 300, ?int $now = null): array
    {
        $parts = [];
        foreach (explode(',', $header) as $p) {
            $kv = explode('=', trim($p), 2);
            if (count($kv) === 2) {
                $parts[$kv[0]] = $kv[1];
            }
        }
        $t = $parts['t'] ?? '';
        $v1 = $parts['v1'] ?? '';
        if (!ctype_digit($t) || !preg_match('/^[0-9a-f]{64}$/', $v1)) {
            throw new WebhookError('malformed signature header');
        }
        if (abs(($now ?? time()) - (int) $t) > $tolerance) {
            throw new WebhookError('signature timestamp outside tolerance');
        }
        foreach ((array) $secrets as $secret) {
            if (hash_equals(hash_hmac('sha256', $t . '.' . $rawBody, $secret), $v1)) {
                return json_decode($rawBody, true, 512, JSON_THROW_ON_ERROR);
            }
        }
        throw new WebhookError('signature mismatch');
    }

    private static function requireKey(string $key): string
    {
        if ($key === '') {
            throw new \InvalidArgumentException('paygate: an Idempotency-Key is required (use your order id)');
        }
        return $key;
    }

    private static function backoff(int $attempt): void
    {
        usleep((300 * (2 ** $attempt) + random_int(0, 100)) * 1000);
    }
}
