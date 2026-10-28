-- Payment methods: which method a transaction was routed for (telebirr, card, ...).
ALTER TABLE transactions ADD COLUMN IF NOT EXISTS method TEXT NOT NULL DEFAULT '';
