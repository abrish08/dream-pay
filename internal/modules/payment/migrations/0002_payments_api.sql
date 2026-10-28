-- Integration API: description, metadata, expiry. Additive only.
ALTER TABLE payments ADD COLUMN IF NOT EXISTS description TEXT        NOT NULL DEFAULT '';
ALTER TABLE payments ADD COLUMN IF NOT EXISTS metadata    JSONB       NOT NULL DEFAULT '{}';
ALTER TABLE payments ADD COLUMN IF NOT EXISTS expires_at  TIMESTAMPTZ;
UPDATE payments SET expires_at = created_at + interval '30 minutes' WHERE expires_at IS NULL;
ALTER TABLE payments ALTER COLUMN expires_at SET NOT NULL;

-- The expiry worker scans only open payments.
CREATE INDEX IF NOT EXISTS payments_open_expiry_idx ON payments (expires_at) WHERE status IN ('pending', 'processing');
-- Merchants look payments up by their own order reference.
CREATE INDEX IF NOT EXISTS payments_merchant_reference_idx ON payments (merchant_id, reference);
-- Cursor pagination: newest first, ties broken by id.
CREATE INDEX IF NOT EXISTS payments_merchant_env_created_idx ON payments (merchant_id, environment, created_at DESC, id DESC);
