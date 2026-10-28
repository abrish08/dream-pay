-- Self-signup: merchants start "pending" (test mode only) until staff approve them.
ALTER TABLE merchants DROP CONSTRAINT IF EXISTS merchants_status_check;
ALTER TABLE merchants ADD CONSTRAINT merchants_status_check CHECK (status IN ('pending', 'active', 'suspended'));
ALTER TABLE merchants ADD COLUMN IF NOT EXISTS phone       TEXT NOT NULL DEFAULT '';
ALTER TABLE merchants ADD COLUMN IF NOT EXISTS website     TEXT NOT NULL DEFAULT '';
ALTER TABLE merchants ADD COLUMN IF NOT EXISTS approved_at TIMESTAMPTZ;
UPDATE merchants SET approved_at = created_at WHERE status = 'active' AND approved_at IS NULL;
CREATE INDEX IF NOT EXISTS merchants_status_created_idx ON merchants (status, created_at DESC, id DESC);
