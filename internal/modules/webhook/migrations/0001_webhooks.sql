CREATE TABLE webhook_endpoints (
    id               UUID PRIMARY KEY,
    merchant_id      UUID        NOT NULL,
    url              TEXT        NOT NULL,
    events           JSONB       NOT NULL,
    secret_encrypted TEXT        NOT NULL,   -- AES-256-GCM; the key lives in a secret manager
    active           BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at       TIMESTAMPTZ NOT NULL
);
CREATE INDEX webhook_endpoints_merchant_idx ON webhook_endpoints (merchant_id) WHERE active;

CREATE TABLE webhook_deliveries (
    id               UUID PRIMARY KEY,
    endpoint_id      UUID        NOT NULL REFERENCES webhook_endpoints (id),
    merchant_id      UUID        NOT NULL,
    event_id         UUID        NOT NULL,
    event_type       TEXT        NOT NULL,
    payload          TEXT        NOT NULL,
    status           TEXT        NOT NULL CHECK (status IN ('pending', 'delivered', 'failed')),
    attempts         INT         NOT NULL DEFAULT 0,
    next_attempt_at  TIMESTAMPTZ NOT NULL,
    locked_until     TIMESTAMPTZ,
    last_status_code INT         NOT NULL DEFAULT 0,
    last_error       TEXT        NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ NOT NULL,
    delivered_at     TIMESTAMPTZ,
    UNIQUE (endpoint_id, event_id)
);
CREATE INDEX webhook_deliveries_due_idx ON webhook_deliveries (next_attempt_at) WHERE status = 'pending';
CREATE INDEX webhook_deliveries_merchant_idx ON webhook_deliveries (merchant_id, created_at DESC);
