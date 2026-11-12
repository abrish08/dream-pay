-- Who did what, for the whole backend. Append-only and chained: each row
-- stores the hash of the previous row, so editing or deleting any row (even
-- by someone who disables the triggers) breaks the chain and is detected by
-- GET /admin/audit-logs/verify.
CREATE TABLE IF NOT EXISTS audit_logs (
    id             BIGSERIAL PRIMARY KEY,
    occurred_at    TIMESTAMPTZ NOT NULL,
    recorded_at    TIMESTAMPTZ NOT NULL,
    actor_kind     TEXT        NOT NULL,
    actor_id       TEXT        NOT NULL DEFAULT '',
    actor_label    TEXT        NOT NULL DEFAULT '',
    merchant_id    TEXT        NOT NULL DEFAULT '',
    environment    TEXT        NOT NULL DEFAULT '',
    action         TEXT        NOT NULL,
    method         TEXT        NOT NULL DEFAULT '',
    route          TEXT        NOT NULL DEFAULT '',
    resource_type  TEXT        NOT NULL DEFAULT '',
    resource_id    TEXT        NOT NULL DEFAULT '',
    outcome        TEXT        NOT NULL,
    status         INTEGER     NOT NULL DEFAULT 0,
    error_code     TEXT        NOT NULL DEFAULT '',
    ip             TEXT        NOT NULL DEFAULT '',
    user_agent     TEXT        NOT NULL DEFAULT '',
    correlation_id TEXT        NOT NULL DEFAULT '',
    duration_ms    BIGINT      NOT NULL DEFAULT 0,
    details        JSONB       NOT NULL DEFAULT '{}'::jsonb,
    source         TEXT        NOT NULL DEFAULT '',
    prev_hash      TEXT        NOT NULL,
    hash           TEXT        NOT NULL UNIQUE
);
CREATE INDEX IF NOT EXISTS audit_logs_merchant_idx ON audit_logs (merchant_id, id DESC) WHERE merchant_id <> '';
CREATE INDEX IF NOT EXISTS audit_logs_actor_idx ON audit_logs (actor_id, id DESC) WHERE actor_id <> '';
CREATE INDEX IF NOT EXISTS audit_logs_action_idx ON audit_logs (action, id DESC);
CREATE INDEX IF NOT EXISTS audit_logs_resource_idx ON audit_logs (resource_id) WHERE resource_id <> '';
CREATE INDEX IF NOT EXISTS audit_logs_time_idx ON audit_logs (occurred_at);

CREATE OR REPLACE FUNCTION audit_logs_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'audit_logs is append-only: % is not allowed', TG_OP;
END $$;

DROP TRIGGER IF EXISTS audit_logs_no_change ON audit_logs;
CREATE TRIGGER audit_logs_no_change BEFORE UPDATE OR DELETE ON audit_logs
    FOR EACH ROW EXECUTE FUNCTION audit_logs_append_only();
DROP TRIGGER IF EXISTS audit_logs_no_truncate ON audit_logs;
CREATE TRIGGER audit_logs_no_truncate BEFORE TRUNCATE ON audit_logs
    FOR EACH STATEMENT EXECUTE FUNCTION audit_logs_append_only();
