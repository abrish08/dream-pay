-- The books cannot be changed, only added to. Corrections are new postings
-- (approved manual adjustments). A database superuser can still disable
-- triggers: run the application with an ordinary database role.
CREATE OR REPLACE FUNCTION ledger_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'the ledger is append-only: % on % is not allowed (post a correction instead)', TG_OP, TG_TABLE_NAME;
END $$;

DROP TRIGGER IF EXISTS ledger_entries_no_change ON ledger_entries;
CREATE TRIGGER ledger_entries_no_change BEFORE UPDATE OR DELETE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_append_only();
DROP TRIGGER IF EXISTS ledger_entries_no_truncate ON ledger_entries;
CREATE TRIGGER ledger_entries_no_truncate BEFORE TRUNCATE ON ledger_entries
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_append_only();
DROP TRIGGER IF EXISTS ledger_transactions_no_change ON ledger_transactions;
CREATE TRIGGER ledger_transactions_no_change BEFORE UPDATE OR DELETE ON ledger_transactions
    FOR EACH ROW EXECUTE FUNCTION ledger_append_only();
DROP TRIGGER IF EXISTS ledger_transactions_no_truncate ON ledger_transactions;
CREATE TRIGGER ledger_transactions_no_truncate BEFORE TRUNCATE ON ledger_transactions
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_append_only();

-- Every posting balances, has at least two lines and one currency. Checked
-- when the database transaction commits, whoever wrote the rows.
-- SET search_path FROM CURRENT: the function finds the ledger tables whatever
-- the session's search_path is.
CREATE OR REPLACE FUNCTION ledger_check_posting() RETURNS trigger LANGUAGE plpgsql
    SET search_path FROM CURRENT AS $$
DECLARE
    tx_id UUID;
    d BIGINT; c BIGINT; n INT; bad_currency INT;
BEGIN
    IF TG_TABLE_NAME = 'ledger_transactions' THEN
        tx_id := NEW.id;
    ELSE
        tx_id := NEW.transaction_id;
    END IF;
    SELECT COALESCE(SUM(e.debit), 0), COALESCE(SUM(e.credit), 0), COUNT(*),
           COUNT(*) FILTER (WHERE e.currency <> t.currency)
      INTO d, c, n, bad_currency
      FROM ledger_transactions t LEFT JOIN ledger_entries e ON e.transaction_id = t.id
     WHERE t.id = tx_id;
    IF n < 2 OR d <> c OR bad_currency > 0 THEN
        RAISE EXCEPTION 'ledger posting % is invalid: % lines, debits %, credits %, % lines in another currency',
            tx_id, n, d, c, bad_currency;
    END IF;
    RETURN NULL;
END $$;

DROP TRIGGER IF EXISTS ledger_entries_balanced ON ledger_entries;
CREATE CONSTRAINT TRIGGER ledger_entries_balanced AFTER INSERT ON ledger_entries
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION ledger_check_posting();
DROP TRIGGER IF EXISTS ledger_transactions_balanced ON ledger_transactions;
CREATE CONSTRAINT TRIGGER ledger_transactions_balanced AFTER INSERT ON ledger_transactions
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION ledger_check_posting();
