-- Payment history: one row per status change (and notable notes), written in
-- the same transaction as the change. Append-only: the trigger below refuses
-- UPDATE and DELETE, so the history cannot be rewritten, only added to.
CREATE TABLE IF NOT EXISTS payment_events (
    id          BIGSERIAL PRIMARY KEY,
    payment_id  UUID        NOT NULL REFERENCES payments (id),
    from_status TEXT        NOT NULL DEFAULT '',
    to_status   TEXT        NOT NULL,
    source      TEXT        NOT NULL,              -- merchant_api, customer, provider, status_check, expiry, refund, reconciliation, ...
    reason      TEXT        NOT NULL DEFAULT '',
    provider    TEXT        NOT NULL DEFAULT '',
    details     JSONB       NOT NULL DEFAULT '{}', -- e.g. what the provider answered
    created_at  TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS payment_events_payment_idx ON payment_events (payment_id, id);

CREATE OR REPLACE FUNCTION payment_events_append_only() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'payment_events is append-only';
END
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS payment_events_no_rewrite ON payment_events;
CREATE TRIGGER payment_events_no_rewrite BEFORE UPDATE OR DELETE ON payment_events
    FOR EACH ROW EXECUTE FUNCTION payment_events_append_only();

-- Existing payments start their history with their current status.
INSERT INTO payment_events (payment_id, from_status, to_status, source, reason, provider, created_at)
SELECT id, '', status, 'migration', 'history started', provider, updated_at FROM payments;

-- Status checks: open payments are verified with the provider on a schedule
-- (1, 2, 5, 10... minutes), so a lost callback is found in minutes.
ALTER TABLE payments ADD COLUMN IF NOT EXISTS next_check_at TIMESTAMPTZ;
ALTER TABLE payments ADD COLUMN IF NOT EXISTS check_count INT NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS payments_next_check_idx ON payments (next_check_at) WHERE next_check_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS payments_provider_created_idx ON payments (provider, created_at);
