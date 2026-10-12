// Package postgres implements ledger.Store on PostgreSQL using pgx.
package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/UncleKweks/ledger-service/internal/ledger"
)

// Store implements ledger.Store.
type Store struct {
	pool *pgxpool.Pool
}

var _ ledger.Store = (*Store)(nil)

// New returns a Store backed by pool.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// querier is satisfied by both *pgxpool.Pool and pgx.Tx.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

const accountColumns = `id, name, currency, allow_negative_balance, created_at`

func scanAccount(row pgx.Row) (ledger.Account, error) {
	var a ledger.Account
	err := row.Scan(&a.ID, &a.Name, &a.Currency, &a.AllowNegativeBalance, &a.CreatedAt)
	return a, err
}

// CreateAccount inserts a new account.
func (s *Store) CreateAccount(ctx context.Context, a ledger.NewAccount) (ledger.Account, error) {
	row := s.pool.QueryRow(ctx, `
		INSERT INTO accounts (name, currency, allow_negative_balance)
		VALUES ($1, $2, $3)
		RETURNING `+accountColumns,
		a.Name, a.Currency, a.AllowNegativeBalance)
	acc, err := scanAccount(row)
	if err != nil {
		return ledger.Account{}, fmt.Errorf("create account: %w", err)
	}
	return acc, nil
}

// GetAccount returns the account with the given id.
func (s *Store) GetAccount(ctx context.Context, id uuid.UUID) (ledger.Account, error) {
	acc, err := scanAccount(s.pool.QueryRow(ctx,
		`SELECT `+accountColumns+` FROM accounts WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return ledger.Account{}, ledger.ErrAccountNotFound
	}
	if err != nil {
		return ledger.Account{}, fmt.Errorf("get account: %w", err)
	}
	return acc, nil
}

// GetBalance returns SUM(entries.amount) for the account.
func (s *Store) GetBalance(ctx context.Context, accountID uuid.UUID) (ledger.Balance, error) {
	b := ledger.Balance{AccountID: accountID}
	err := s.pool.QueryRow(ctx, `
		SELECT a.currency,
		       (SELECT COALESCE(SUM(e.amount), 0)::BIGINT FROM entries e WHERE e.account_id = a.id)
		FROM accounts a
		WHERE a.id = $1`, accountID).Scan(&b.Currency, &b.Amount)
	if errors.Is(err, pgx.ErrNoRows) {
		return ledger.Balance{}, ledger.ErrAccountNotFound
	}
	if err != nil {
		return ledger.Balance{}, fmt.Errorf("get balance: %w", err)
	}
	return b, nil
}

const entryColumns = `id, transaction_id, account_id, currency, amount, created_at`

func scanEntries(rows pgx.Rows) ([]ledger.Entry, error) {
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (ledger.Entry, error) {
		var e ledger.Entry
		err := row.Scan(&e.ID, &e.TransactionID, &e.AccountID, &e.Currency, &e.Amount, &e.CreatedAt)
		return e, err
	})
}

// ListEntries returns the account's most recent entries, newest first.
func (s *Store) ListEntries(ctx context.Context, accountID uuid.UUID, limit int) ([]ledger.Entry, error) {
	if _, err := s.GetAccount(ctx, accountID); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+entryColumns+`
		FROM entries
		WHERE account_id = $1
		ORDER BY created_at DESC, id DESC
		LIMIT $2`, accountID, limit)
	if err != nil {
		return nil, fmt.Errorf("list entries: %w", err)
	}
	entries, err := scanEntries(rows)
	if err != nil {
		return nil, fmt.Errorf("list entries: %w", err)
	}
	return entries, nil
}

// GetTransaction returns a transaction and its entries.
func (s *Store) GetTransaction(ctx context.Context, id uuid.UUID) (ledger.Transaction, error) {
	return getTransaction(ctx, s.pool, "id", id)
}

// getTransaction loads a transaction by a unique column ("id" or
// "idempotency_key"). Entries are ordered debits first, then by account.
func getTransaction(ctx context.Context, q querier, column string, value any) (ledger.Transaction, error) {
	var t ledger.Transaction
	err := q.QueryRow(ctx, `
		SELECT id, idempotency_key, description, created_at
		FROM transactions
		WHERE `+column+` = $1`, value).
		Scan(&t.ID, &t.IdempotencyKey, &t.Description, &t.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ledger.Transaction{}, ledger.ErrTransactionNotFound
	}
	if err != nil {
		return ledger.Transaction{}, fmt.Errorf("get transaction: %w", err)
	}

	rows, err := q.Query(ctx, `
		SELECT `+entryColumns+`
		FROM entries
		WHERE transaction_id = $1
		ORDER BY amount DESC, account_id`, t.ID)
	if err != nil {
		return ledger.Transaction{}, fmt.Errorf("get transaction entries: %w", err)
	}
	if t.Entries, err = scanEntries(rows); err != nil {
		return ledger.Transaction{}, fmt.Errorf("get transaction entries: %w", err)
	}
	return t, nil
}

// PostTransaction writes a transaction and its entries in one database
// transaction. The steps, in order:
//
//  1. Claim the idempotency key. If it already exists, return the earlier
//     posting when it matches the request, or ErrIdempotencyConflict.
//  2. Check every account exists and has the entry's currency.
//  3. Lock, in id order, each debited account that may not go negative, then
//     check the debit does not overdraw it.
//  4. Insert the entries. The deferred balance trigger re-checks Rule A at COMMIT.
func (s *Store) PostTransaction(ctx context.Context, nt ledger.NewTransaction) (ledger.Transaction, bool, error) {
	var (
		txn     ledger.Transaction
		created bool
	)
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// 1. A concurrent request with the same key blocks here on the unique
		// index until the first one commits or rolls back.
		var txnID uuid.UUID
		err := tx.QueryRow(ctx, `
			INSERT INTO transactions (idempotency_key, description)
			VALUES ($1, $2)
			ON CONFLICT (idempotency_key) DO NOTHING
			RETURNING id`, nt.IdempotencyKey, nt.Description).Scan(&txnID)
		if errors.Is(err, pgx.ErrNoRows) {
			existing, err := getTransaction(ctx, tx, "idempotency_key", nt.IdempotencyKey)
			if err != nil {
				return err
			}
			if !nt.Matches(existing) {
				return ledger.ErrIdempotencyConflict
			}
			txn = existing
			return nil
		}
		if err != nil {
			return fmt.Errorf("insert transaction: %w", err)
		}

		if err := checkAccountsAndFunds(ctx, tx, nt.Entries); err != nil {
			return err
		}

		// 4. One round trip for all entries.
		accountIDs := make([]uuid.UUID, len(nt.Entries))
		currencies := make([]string, len(nt.Entries))
		amounts := make([]int64, len(nt.Entries))
		for i, e := range nt.Entries {
			accountIDs[i], currencies[i], amounts[i] = e.AccountID, e.Currency, e.Amount
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO entries (transaction_id, account_id, currency, amount)
			SELECT $1, account_id, currency, amount
			FROM unnest($2::uuid[], $3::char(3)[], $4::bigint[]) AS e(account_id, currency, amount)`,
			txnID, accountIDs, currencies, amounts); err != nil {
			return fmt.Errorf("insert entries: %w", err)
		}

		txn, err = getTransaction(ctx, tx, "id", txnID)
		created = true
		return err
	})
	if err != nil {
		return ledger.Transaction{}, false, err
	}
	return txn, created, nil
}

// checkAccountsAndFunds runs steps 2 and 3 of PostTransaction inside tx.
func checkAccountsAndFunds(ctx context.Context, tx pgx.Tx, entries []ledger.NewEntry) error {
	// Net change per account (overflow already ruled out by Validate), and
	// the currency each entry claims.
	delta := make(map[uuid.UUID]int64)
	currency := make(map[uuid.UUID]string)
	for _, e := range entries {
		delta[e.AccountID] += e.Amount
		if c, ok := currency[e.AccountID]; ok && c != e.Currency {
			return fmt.Errorf("%w: account %s", ledger.ErrCurrencyMismatch, e.AccountID)
		}
		currency[e.AccountID] = e.Currency
	}

	var all, debited []uuid.UUID
	for id, d := range delta {
		all = append(all, id)
		if d < 0 {
			debited = append(debited, id)
		}
	}

	// 2. Existence and currency. The composite foreign key would also catch a
	// mismatch, but checking first gives the caller a precise error.
	rows, err := tx.Query(ctx, `SELECT id, currency FROM accounts WHERE id = ANY($1)`, all)
	if err != nil {
		return fmt.Errorf("load accounts: %w", err)
	}
	found := make(map[uuid.UUID]string, len(all))
	var (
		id  uuid.UUID
		cur string
	)
	if _, err := pgx.ForEachRow(rows, []any{&id, &cur}, func() error {
		found[id] = cur
		return nil
	}); err != nil {
		return fmt.Errorf("load accounts: %w", err)
	}
	for _, id := range all {
		c, ok := found[id]
		if !ok {
			return fmt.Errorf("%w: %s", ledger.ErrAccountNotFound, id)
		}
		if c != currency[id] {
			return fmt.Errorf("%w: account %s is %s, entry is %s", ledger.ErrCurrencyMismatch, id, c, currency[id])
		}
	}

	if len(debited) == 0 {
		return nil
	}

	// 3. Lock in a fixed (id) order so two postings never wait on each other in
	// a cycle. FOR NO KEY UPDATE, unlike FOR UPDATE, does not block the
	// FOR KEY SHARE locks that foreign-key checks on entries take, so a
	// concurrent A->B and B->A cannot deadlock on each other's FK checks.
	// Accounts allowed to go negative are not locked, so a funding account
	// credited by every deposit never becomes a point of contention.
	rows, err = tx.Query(ctx, `
		SELECT id
		FROM accounts
		WHERE id = ANY($1) AND NOT allow_negative_balance
		ORDER BY id
		FOR NO KEY UPDATE`, debited)
	if err != nil {
		return fmt.Errorf("lock accounts: %w", err)
	}
	locked, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return fmt.Errorf("lock accounts: %w", err)
	}

	// Under READ COMMITTED each statement takes a fresh snapshot, so this SUM,
	// run after the lock is held, sees every entry committed by earlier holders.
	for _, id := range locked {
		var balance int64
		if err := tx.QueryRow(ctx,
			`SELECT COALESCE(SUM(amount), 0)::BIGINT FROM entries WHERE account_id = $1`, id).
			Scan(&balance); err != nil {
			return fmt.Errorf("read balance: %w", err)
		}
		if balance+delta[id] < 0 {
			return fmt.Errorf("%w: account %s has %d, needs %d", ledger.ErrInsufficientFunds, id, balance, -delta[id])
		}
	}
	return nil
}
