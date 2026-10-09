-- +goose Up
CREATE TABLE accounts (
    id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    name       TEXT        NOT NULL,
    currency   CHAR(3)     NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Target for the composite foreign key on entries (Rule C).
    UNIQUE (id, currency)
);

-- +goose Down
DROP TABLE accounts;
