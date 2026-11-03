-- Rolling reserve: part of each captured payment held for N days.
CREATE TABLE IF NOT EXISTS reserve_holds (
    payment_id  UUID PRIMARY KEY,
    merchant_id UUID        NOT NULL,
    environment TEXT        NOT NULL,
    currency    CHAR(3)     NOT NULL,
    amount      BIGINT      NOT NULL CHECK (amount > 0),
    release_at  TIMESTAMPTZ NOT NULL,
    released_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS reserve_holds_due_idx ON reserve_holds (release_at) WHERE released_at IS NULL;
