CREATE TABLE transactions (
    tx_ref       TEXT PRIMARY KEY,           -- our payment id
    provider     TEXT        NOT NULL,
    provider_ref TEXT        NOT NULL DEFAULT '',
    amount       BIGINT      NOT NULL,
    currency     CHAR(3)     NOT NULL,
    status       TEXT        NOT NULL,
    checkout_url TEXT        NOT NULL DEFAULT '',
    return_url   TEXT        NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL
);

CREATE TABLE refunds (
    refund_ref          TEXT PRIMARY KEY,    -- our refund id (idempotency key)
    provider            TEXT        NOT NULL,
    tx_ref              TEXT        NOT NULL,
    amount              BIGINT      NOT NULL,
    currency            CHAR(3)     NOT NULL,
    status              TEXT        NOT NULL,
    provider_refund_ref TEXT        NOT NULL DEFAULT '',
    failure_reason      TEXT        NOT NULL DEFAULT '',
    created_at          TIMESTAMPTZ NOT NULL
);

-- State of the simulated provider (development / tests only).
CREATE TABLE mock_transactions (
    tx_ref     TEXT PRIMARY KEY,
    amount     BIGINT      NOT NULL,
    currency   CHAR(3)     NOT NULL,
    status     TEXT        NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);
