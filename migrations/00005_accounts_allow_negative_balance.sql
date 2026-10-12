-- Ordinary accounts may never be overdrawn. System accounts that represent
-- money outside the ledger (bank settlement, funding sources) may go negative.

-- +goose Up
ALTER TABLE accounts
    ADD COLUMN allow_negative_balance BOOLEAN NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE accounts
    DROP COLUMN allow_negative_balance;
