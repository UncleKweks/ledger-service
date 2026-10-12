package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/UncleKweks/ledger-service/internal/ledger"
	"github.com/UncleKweks/ledger-service/internal/store/postgres"
	"github.com/UncleKweks/ledger-service/internal/testdb"
)

// fixture is a fresh database with a funding account that may go negative.
type fixture struct {
	pool  *pgxpool.Pool
	svc   *ledger.Service
	world ledger.Account
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := testdb.New(t)
	f := &fixture{pool: pool, svc: ledger.NewService(postgres.New(pool))}
	f.world = f.account(t, "world", "NGN", true)
	return f
}

func (f *fixture) account(t *testing.T, name, currency string, allowNegative bool) ledger.Account {
	t.Helper()
	acc, err := f.svc.CreateAccount(context.Background(), ledger.NewAccount{
		Name: name, Currency: currency, AllowNegativeBalance: allowNegative,
	})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	return acc
}

func transfer(key string, from, to uuid.UUID, amount int64) ledger.NewTransaction {
	return ledger.NewTransaction{
		IdempotencyKey: key,
		Description:    "transfer",
		Entries: []ledger.NewEntry{
			{AccountID: from, Currency: "NGN", Amount: -amount},
			{AccountID: to, Currency: "NGN", Amount: amount},
		},
	}
}

func (f *fixture) post(t *testing.T, nt ledger.NewTransaction) ledger.Transaction {
	t.Helper()
	txn, _, err := f.svc.PostTransaction(context.Background(), nt)
	if err != nil {
		t.Fatalf("PostTransaction: %v", err)
	}
	return txn
}

func (f *fixture) balance(t *testing.T, id uuid.UUID) int64 {
	t.Helper()
	b, err := f.svc.GetBalance(context.Background(), id)
	if err != nil {
		t.Fatalf("GetBalance: %v", err)
	}
	return b.Amount
}

func TestPostTransactionMovesMoney(t *testing.T) {
	f := newFixture(t)
	alice := f.account(t, "alice", "NGN", false)
	bob := f.account(t, "bob", "NGN", false)

	f.post(t, transfer("fund", f.world.ID, alice.ID, 10_000))
	txn := f.post(t, transfer("pay", alice.ID, bob.ID, 2_500))

	if got := f.balance(t, alice.ID); got != 7_500 {
		t.Errorf("alice balance = %d, want 7500", got)
	}
	if got := f.balance(t, bob.ID); got != 2_500 {
		t.Errorf("bob balance = %d, want 2500", got)
	}
	if got := f.balance(t, f.world.ID); got != -10_000 {
		t.Errorf("world balance = %d, want -10000", got)
	}

	got, err := f.svc.GetTransaction(context.Background(), txn.ID)
	if err != nil {
		t.Fatalf("GetTransaction: %v", err)
	}
	if len(got.Entries) != 2 || got.Entries[0].Amount != 2_500 || got.Entries[0].AccountID != bob.ID {
		t.Errorf("entries = %+v, want debit to bob first", got.Entries)
	}

	entries, err := f.svc.ListEntries(context.Background(), alice.ID, 10)
	if err != nil {
		t.Fatalf("ListEntries: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("alice has %d entries, want 2", len(entries))
	}
}

func TestPostTransactionRejectsOverdraft(t *testing.T) {
	f := newFixture(t)
	alice := f.account(t, "alice", "NGN", false)
	bob := f.account(t, "bob", "NGN", false)
	f.post(t, transfer("fund", f.world.ID, alice.ID, 1_000))

	_, _, err := f.svc.PostTransaction(context.Background(), transfer("too-much", alice.ID, bob.ID, 1_001))
	if !errors.Is(err, ledger.ErrInsufficientFunds) {
		t.Fatalf("err = %v, want ErrInsufficientFunds", err)
	}
	if got := f.balance(t, alice.ID); got != 1_000 {
		t.Errorf("alice balance = %d, want 1000 (nothing posted)", got)
	}

	// The failed attempt did not consume the idempotency key.
	f.post(t, transfer("too-much", alice.ID, bob.ID, 1_000))
	if got := f.balance(t, alice.ID); got != 0 {
		t.Errorf("alice balance = %d, want 0", got)
	}
}

func TestPostTransactionRejectsCurrencyMismatch(t *testing.T) {
	f := newFixture(t)
	usd := f.account(t, "dollars", "USD", false)

	nt := transfer("mismatch", f.world.ID, usd.ID, 100) // entries say NGN
	_, _, err := f.svc.PostTransaction(context.Background(), nt)
	if !errors.Is(err, ledger.ErrCurrencyMismatch) {
		t.Fatalf("err = %v, want ErrCurrencyMismatch", err)
	}
}

func TestPostTransactionRejectsUnknownAccount(t *testing.T) {
	f := newFixture(t)
	_, _, err := f.svc.PostTransaction(context.Background(), transfer("ghost", f.world.ID, uuid.New(), 100))
	if !errors.Is(err, ledger.ErrAccountNotFound) {
		t.Fatalf("err = %v, want ErrAccountNotFound", err)
	}
}

func TestNotFound(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.svc.GetAccount(ctx, uuid.New()); !errors.Is(err, ledger.ErrAccountNotFound) {
		t.Errorf("GetAccount err = %v", err)
	}
	if _, err := f.svc.GetBalance(ctx, uuid.New()); !errors.Is(err, ledger.ErrAccountNotFound) {
		t.Errorf("GetBalance err = %v", err)
	}
	if _, err := f.svc.ListEntries(ctx, uuid.New(), 10); !errors.Is(err, ledger.ErrAccountNotFound) {
		t.Errorf("ListEntries err = %v", err)
	}
	if _, err := f.svc.GetTransaction(ctx, uuid.New()); !errors.Is(err, ledger.ErrTransactionNotFound) {
		t.Errorf("GetTransaction err = %v", err)
	}
}

// The database enforces the invariants even for SQL that bypasses the service.
func TestDatabaseGuards(t *testing.T) {
	f := newFixture(t)
	alice := f.account(t, "alice", "NGN", false)
	txn := f.post(t, transfer("fund", f.world.ID, alice.ID, 500))
	ctx := context.Background()

	t.Run("entries cannot be updated", func(t *testing.T) {
		_, err := f.pool.Exec(ctx, `UPDATE entries SET amount = 1 WHERE transaction_id = $1`, txn.ID)
		if err == nil {
			t.Fatal("UPDATE succeeded")
		}
	})
	t.Run("entries cannot be deleted", func(t *testing.T) {
		_, err := f.pool.Exec(ctx, `DELETE FROM entries WHERE transaction_id = $1`, txn.ID)
		if err == nil {
			t.Fatal("DELETE succeeded")
		}
	})
	t.Run("unbalanced transaction cannot commit", func(t *testing.T) {
		err := pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error {
			var id uuid.UUID
			if err := tx.QueryRow(ctx,
				`INSERT INTO transactions (idempotency_key) VALUES ('raw') RETURNING id`).Scan(&id); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `
				INSERT INTO entries (transaction_id, account_id, currency, amount)
				VALUES ($1, $2, 'NGN', -100), ($1, $3, 'NGN', 99)`, id, f.world.ID, alice.ID)
			return err
		})
		if err == nil {
			t.Fatal("unbalanced transaction committed")
		}
	})
	t.Run("entry currency must match account", func(t *testing.T) {
		_, err := f.pool.Exec(ctx, `
			INSERT INTO entries (transaction_id, account_id, currency, amount)
			VALUES ($1, $2, 'USD', 1)`, txn.ID, alice.ID)
		if err == nil {
			t.Fatal("mismatched currency accepted")
		}
	})
}
