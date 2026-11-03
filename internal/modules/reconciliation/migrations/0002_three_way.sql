-- Three-way reconciliation: our payments vs the provider (API and settlement
-- files) vs our ledger. Differences are items that stay open until they are
-- repaired automatically or resolved by staff with a note.

ALTER TABLE reconciliation_runs DROP CONSTRAINT IF EXISTS reconciliation_runs_kind_check;
ALTER TABLE reconciliation_runs ADD CONSTRAINT reconciliation_runs_kind_check
    CHECK (kind IN ('manual', 'daily', 'settlement'));
ALTER TABLE reconciliation_runs ADD COLUMN IF NOT EXISTS provider TEXT NOT NULL DEFAULT '';
ALTER TABLE reconciliation_runs ADD COLUMN IF NOT EXISTS settlement_id UUID;
ALTER TABLE reconciliation_runs ADD COLUMN IF NOT EXISTS summary JSONB NOT NULL DEFAULT '{}';

ALTER TABLE reconciliation_items ALTER COLUMN payment_id DROP NOT NULL;
ALTER TABLE reconciliation_items ADD COLUMN IF NOT EXISTS reference   TEXT NOT NULL DEFAULT '';
ALTER TABLE reconciliation_items ADD COLUMN IF NOT EXISTS severity    TEXT NOT NULL DEFAULT 'high';
ALTER TABLE reconciliation_items ADD COLUMN IF NOT EXISTS status      TEXT NOT NULL DEFAULT 'open';
ALTER TABLE reconciliation_items ADD COLUMN IF NOT EXISTS resolution  TEXT NOT NULL DEFAULT '';
ALTER TABLE reconciliation_items ADD COLUMN IF NOT EXISTS resolved_by TEXT NOT NULL DEFAULT '';
ALTER TABLE reconciliation_items ADD COLUMN IF NOT EXISTS resolved_at TIMESTAMPTZ;
ALTER TABLE reconciliation_items DROP CONSTRAINT IF EXISTS reconciliation_items_status_check;
ALTER TABLE reconciliation_items ADD CONSTRAINT reconciliation_items_status_check
    CHECK (status IN ('open', 'auto_resolved', 'resolved'));
CREATE INDEX IF NOT EXISTS reconciliation_items_open_idx ON reconciliation_items (issue, created_at) WHERE status = 'open';

-- A provider's settlement report for one business day, as uploaded.
CREATE TABLE IF NOT EXISTS settlements (
    id              UUID PRIMARY KEY,
    provider        TEXT        NOT NULL,
    settlement_date DATE        NOT NULL,
    file_name       TEXT        NOT NULL DEFAULT '',
    sha256          TEXT        NOT NULL,
    line_count      INT         NOT NULL,
    deposit_amount  BIGINT,                  -- what the bank actually paid in (optional)
    deposit_currency CHAR(3),
    uploaded_by     TEXT        NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL,
    UNIQUE (provider, sha256)                -- the same file cannot be counted twice
);

CREATE TABLE IF NOT EXISTS settlement_lines (
    id            BIGSERIAL PRIMARY KEY,
    settlement_id UUID        NOT NULL REFERENCES settlements (id),
    line_no       INT         NOT NULL,
    reference     TEXT        NOT NULL,      -- our payment id (the order id we sent)
    kind          TEXT        NOT NULL CHECK (kind IN ('payment', 'refund')),
    amount        BIGINT      NOT NULL,      -- minor units
    fee           BIGINT      NOT NULL DEFAULT 0,
    currency      CHAR(3)     NOT NULL,
    settled_at    TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS settlement_lines_reference_idx ON settlement_lines (reference);
CREATE INDEX IF NOT EXISTS settlement_lines_settlement_idx ON settlement_lines (settlement_id);
