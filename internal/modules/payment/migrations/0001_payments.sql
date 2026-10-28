CREATE TABLE payments (
    id              UUID PRIMARY KEY,
    merchant_id     UUID        NOT NULL,
    environment     TEXT        NOT NULL CHECK (environment IN ('live', 'test')),
    amount          BIGINT      NOT NULL CHECK (amount > 0),
    fee             BIGINT      NOT NULL CHECK (fee >= 0),
    refunded_amount BIGINT      NOT NULL DEFAULT 0 CHECK (refunded_amount >= 0 AND refunded_amount <= amount),
    currency        CHAR(3)     NOT NULL,
    status          TEXT        NOT NULL,
    provider        TEXT        NOT NULL,
    provider_ref    TEXT        NOT NULL DEFAULT '',
    reference       TEXT        NOT NULL,
    customer_email  TEXT        NOT NULL DEFAULT '',
    customer_name   TEXT        NOT NULL DEFAULT '',
    return_url      TEXT        NOT NULL DEFAULT '',
    checkout_url    TEXT        NOT NULL DEFAULT '',
    failure_reason  TEXT        NOT NULL DEFAULT '',
    idempotency_key TEXT        NOT NULL,
    request_hash    TEXT        NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL,
    updated_at      TIMESTAMPTZ NOT NULL,
    UNIQUE (merchant_id, idempotency_key)
);
CREATE INDEX payments_merchant_created_idx ON payments (merchant_id, created_at DESC);
CREATE INDEX payments_status_created_idx ON payments (status, created_at);
