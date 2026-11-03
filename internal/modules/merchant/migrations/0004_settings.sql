-- Risk limits, reserve and payout schedule, set by platform staff.
ALTER TABLE merchants ADD COLUMN IF NOT EXISTS settings JSONB NOT NULL DEFAULT '{}';
