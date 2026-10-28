CREATE TABLE ledger_transactions (
    id          UUID PRIMARY KEY,
    reference   TEXT        NOT NULL UNIQUE,   -- idempotency: one posting per business event
    currency    CHAR(3)     NOT NULL,
    description TEXT        NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL
);

-- Append-only. Never UPDATE or DELETE; corrections are new postings.
CREATE TABLE ledger_entries (
    id             BIGSERIAL PRIMARY KEY,
    transaction_id UUID        NOT NULL REFERENCES ledger_transactions (id),
    account        TEXT        NOT NULL,
    debit          BIGINT      NOT NULL DEFAULT 0 CHECK (debit >= 0),
    credit         BIGINT      NOT NULL DEFAULT 0 CHECK (credit >= 0),
    currency       CHAR(3)     NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL,
    CHECK ((debit > 0) <> (credit > 0))
);
CREATE INDEX ledger_entries_account_idx ON ledger_entries (account, currency);
