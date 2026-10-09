-- Rule B: entries are an append-only audit trail. Corrections are made by
-- posting a new, reversing transaction, never by editing history.

-- +goose Up
-- +goose StatementBegin
CREATE FUNCTION entries_reject_modification() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'entries are append-only: % is not allowed', TG_OP
        USING ERRCODE = 'restrict_violation';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER entries_no_update_delete
    BEFORE UPDATE OR DELETE ON entries
    FOR EACH ROW EXECUTE FUNCTION entries_reject_modification();

-- TRUNCATE bypasses row-level triggers, so block it separately.
CREATE TRIGGER entries_no_truncate
    BEFORE TRUNCATE ON entries
    FOR EACH STATEMENT EXECUTE FUNCTION entries_reject_modification();

-- +goose Down
DROP TRIGGER entries_no_truncate ON entries;
DROP TRIGGER entries_no_update_delete ON entries;
DROP FUNCTION entries_reject_modification();
