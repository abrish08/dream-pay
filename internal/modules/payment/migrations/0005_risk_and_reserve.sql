-- Customer phone and IP (risk checks), and the reserve held from each
-- payment: a snapshot of the merchant's reserve at creation, like the fee.
ALTER TABLE payments ADD COLUMN IF NOT EXISTS customer_phone TEXT NOT NULL DEFAULT '';
ALTER TABLE payments ADD COLUMN IF NOT EXISTS customer_ip    TEXT NOT NULL DEFAULT '';
ALTER TABLE payments ADD COLUMN IF NOT EXISTS reserve        BIGINT NOT NULL DEFAULT 0 CHECK (reserve >= 0);
ALTER TABLE payments ADD COLUMN IF NOT EXISTS reserve_days   INT NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS payments_limits_idx ON payments (merchant_id, environment, currency, created_at);
