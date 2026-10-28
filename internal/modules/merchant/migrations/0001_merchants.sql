CREATE TABLE merchants (
    id         UUID PRIMARY KEY,
    name       TEXT        NOT NULL,
    email      TEXT        NOT NULL UNIQUE,
    status     TEXT        NOT NULL CHECK (status IN ('active', 'suspended')),
    fee_bps    BIGINT      NOT NULL CHECK (fee_bps BETWEEN 0 AND 10000),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);
