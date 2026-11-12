-- Manual bank transfers: the customer transfers to our account with a unique
-- reference; finance confirms it from the bank statement.
CREATE TABLE IF NOT EXISTS bank_transfers (
    tx_ref         TEXT PRIMARY KEY,           -- our payment id
    provider       TEXT        NOT NULL,
    reference      TEXT        NOT NULL UNIQUE, -- written on the transfer by the customer
    amount         BIGINT      NOT NULL,
    currency       CHAR(3)     NOT NULL,
    status         TEXT        NOT NULL,        -- pending | succeeded
    bank_reference TEXT        NOT NULL DEFAULT '',
    confirmed_by   TEXT        NOT NULL DEFAULT '',
    confirmed_at   TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS bank_transfers_pending_idx ON bank_transfers (created_at) WHERE status = 'pending';
