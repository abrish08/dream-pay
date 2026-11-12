-- Manual corrections: requested by one person, approved by another, then
-- posted. The request itself is kept whatever the decision.
CREATE TABLE IF NOT EXISTS ledger_adjustments (
    id            UUID PRIMARY KEY,
    merchant_id   UUID        NOT NULL,
    environment   TEXT        NOT NULL CHECK (environment IN ('live', 'test')),
    currency      CHAR(3)     NOT NULL,
    direction     TEXT        NOT NULL CHECK (direction IN ('credit', 'debit')),
    amount        BIGINT      NOT NULL CHECK (amount > 0),
    reason        TEXT        NOT NULL,
    status        TEXT        NOT NULL CHECK (status IN ('pending_approval', 'approved', 'rejected')),
    requested_by  TEXT        NOT NULL,
    requested_at  TIMESTAMPTZ NOT NULL,
    decided_by    TEXT,
    decided_at    TIMESTAMPTZ,
    decision_note TEXT        NOT NULL DEFAULT '',
    CHECK (decided_by IS NULL OR decided_by <> requested_by)
);
CREATE INDEX IF NOT EXISTS ledger_adjustments_status_idx ON ledger_adjustments (status, requested_at DESC);
