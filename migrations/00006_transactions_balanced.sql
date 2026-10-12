-- Rule A at the database level: when a database transaction commits, every
-- ledger transaction it touched has at least two entries, and its entries sum
-- to zero in each currency. The triggers are DEFERRED so the check runs at
-- COMMIT, after all entries of the posting have been inserted.

-- +goose Up
-- +goose StatementBegin
CREATE FUNCTION check_transaction_balanced() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    txn_id    UUID;
    n_entries INT;
    bad       RECORD;
BEGIN
    IF TG_TABLE_NAME = 'entries' THEN
        txn_id := NEW.transaction_id;
    ELSE
        txn_id := NEW.id;
    END IF;

    SELECT count(*) INTO n_entries FROM entries WHERE transaction_id = txn_id;
    IF n_entries < 2 THEN
        RAISE EXCEPTION 'transaction % has % entries; at least 2 are required', txn_id, n_entries
            USING ERRCODE = 'check_violation';
    END IF;

    SELECT currency, sum(amount) AS total INTO bad
    FROM entries
    WHERE transaction_id = txn_id
    GROUP BY currency
    HAVING sum(amount) <> 0
    LIMIT 1;
    IF FOUND THEN
        RAISE EXCEPTION 'transaction % does not balance: % entries sum to %', txn_id, bad.currency, bad.total
            USING ERRCODE = 'check_violation';
    END IF;

    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER transactions_balanced
    AFTER INSERT ON transactions
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION check_transaction_balanced();

CREATE CONSTRAINT TRIGGER entries_balanced
    AFTER INSERT ON entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION check_transaction_balanced();

-- +goose Down
DROP TRIGGER entries_balanced ON entries;
DROP TRIGGER transactions_balanced ON transactions;
DROP FUNCTION check_transaction_balanced();
