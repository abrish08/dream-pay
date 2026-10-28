-- Payment methods: the merchant (or the customer on the hosted page) picks a
-- method; the provider module routes it to a provider.
ALTER TABLE payments ADD COLUMN IF NOT EXISTS payment_method TEXT NOT NULL DEFAULT '';
