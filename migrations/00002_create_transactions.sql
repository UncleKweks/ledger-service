-- +goose Up
CREATE TABLE transactions (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    idempotency_key TEXT        NOT NULL UNIQUE
                                CHECK (length(idempotency_key) BETWEEN 1 AND 255),
    description     TEXT        NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE transactions;
