-- Payment links: a merchant shares a URL, each visit opens a payment.
CREATE TABLE IF NOT EXISTS payment_links (
    id           TEXT PRIMARY KEY,            -- pl_<128 random bits>: unguessable
    merchant_id  UUID        NOT NULL,
    environment  TEXT        NOT NULL CHECK (environment IN ('live', 'test')),
    amount       BIGINT      NOT NULL CHECK (amount > 0),
    currency     CHAR(3)     NOT NULL,
    description  TEXT        NOT NULL DEFAULT '',
    reference    TEXT        NOT NULL DEFAULT '',
    single_use   BOOLEAN     NOT NULL DEFAULT false,
    active       BOOLEAN     NOT NULL DEFAULT true,
    expires_at   TIMESTAMPTZ,
    paid_count   INT         NOT NULL DEFAULT 0,
    created_at   TIMESTAMPTZ NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS payment_links_merchant_idx ON payment_links (merchant_id, created_at DESC);

ALTER TABLE payments ADD COLUMN IF NOT EXISTS link_id TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS payments_link_idx ON payments (link_id) WHERE link_id <> '';
