CREATE TABLE refunds (
    id                  UUID PRIMARY KEY,
    payment_id          UUID        NOT NULL,   -- payment module's id; no FK across modules
    merchant_id         UUID        NOT NULL,
    environment         TEXT        NOT NULL,
    provider            TEXT        NOT NULL,
    amount              BIGINT      NOT NULL CHECK (amount > 0),
    currency            CHAR(3)     NOT NULL,
    status              TEXT        NOT NULL CHECK (status IN ('pending', 'succeeded', 'failed')),
    reason              TEXT        NOT NULL DEFAULT '',
    provider_refund_ref TEXT        NOT NULL DEFAULT '',
    failure_reason      TEXT        NOT NULL DEFAULT '',
    idempotency_key     TEXT        NOT NULL,
    request_hash        TEXT        NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL,
    updated_at          TIMESTAMPTZ NOT NULL,
    UNIQUE (merchant_id, idempotency_key)
);
CREATE INDEX refunds_payment_idx ON refunds (payment_id);
CREATE INDEX refunds_pending_idx ON refunds (updated_at) WHERE status = 'pending';
