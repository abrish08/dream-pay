-- Applied into every module's schema: each module owns its outbox and its
-- record of processed events.
CREATE TABLE IF NOT EXISTS outbox (
    id              BIGSERIAL PRIMARY KEY,
    event_id        UUID        NOT NULL UNIQUE,
    topic           TEXT        NOT NULL,
    payload         JSONB       NOT NULL,
    headers         JSONB       NOT NULL DEFAULT '{}',
    attempts        INT         NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error      TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at    TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS outbox_pending_idx ON outbox (next_attempt_at, id) WHERE published_at IS NULL;

CREATE TABLE IF NOT EXISTS processed_events (
    event_id     UUID        NOT NULL,
    consumer     TEXT        NOT NULL,
    processed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (event_id, consumer)
);
