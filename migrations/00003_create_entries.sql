-- +goose Up
CREATE TABLE entries (
    id             UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    transaction_id UUID        NOT NULL REFERENCES transactions (id),
    account_id     UUID        NOT NULL,
    currency       CHAR(3)     NOT NULL,
    -- Minor units (kobo, cents). Positive = debit, negative = credit.
    amount         BIGINT      NOT NULL CHECK (amount <> 0),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Rule C: an entry's currency must match its account's currency.
    FOREIGN KEY (account_id, currency) REFERENCES accounts (id, currency)
);

-- Balance is SUM(amount) per account; this index serves that query.
CREATE INDEX entries_account_id_idx ON entries (account_id);
CREATE INDEX entries_transaction_id_idx ON entries (transaction_id);

-- +goose Down
DROP TABLE entries;
