CREATE TABLE reconciliation_runs (
    id           UUID PRIMARY KEY,
    kind         TEXT        NOT NULL CHECK (kind IN ('manual', 'daily')),
    period_start TIMESTAMPTZ NOT NULL,
    period_end   TIMESTAMPTZ NOT NULL,
    status       TEXT        NOT NULL CHECK (status IN ('queued', 'running', 'completed', 'failed')),
    checked      INT         NOT NULL DEFAULT 0,
    mismatched   INT         NOT NULL DEFAULT 0,
    error        TEXT        NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL,
    started_at   TIMESTAMPTZ,
    completed_at TIMESTAMPTZ
);
-- At most one scheduled daily run per period, even with several replicas.
CREATE UNIQUE INDEX reconciliation_daily_uniq ON reconciliation_runs (period_start) WHERE kind = 'daily';
CREATE INDEX reconciliation_queued_idx ON reconciliation_runs (created_at) WHERE status = 'queued';

CREATE TABLE reconciliation_items (
    id         BIGSERIAL PRIMARY KEY,
    run_id     UUID        NOT NULL REFERENCES reconciliation_runs (id),
    payment_id UUID        NOT NULL,
    provider   TEXT        NOT NULL,
    issue      TEXT        NOT NULL,
    expected   TEXT        NOT NULL,
    actual     TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX reconciliation_items_run_idx ON reconciliation_items (run_id);
