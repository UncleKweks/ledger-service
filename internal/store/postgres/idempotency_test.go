package postgres_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/UncleKweks/ledger-service/internal/ledger"
)

func TestIdempotentRetryReturnsOriginal(t *testing.T) {
	f := newFixture(t)
	alice := f.account(t, "alice", "NGN", false)
	ctx := context.Background()

	req := transfer("retry-me", f.world.ID, alice.ID, 700)
	first, created, err := f.svc.PostTransaction(ctx, req)
	if err != nil || !created {
		t.Fatalf("first post: created=%v err=%v", created, err)
	}

	// Same request, entries in a different order: still the same posting.
	req.Entries[0], req.Entries[1] = req.Entries[1], req.Entries[0]
	second, created, err := f.svc.PostTransaction(ctx, req)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if created {
		t.Error("retry reported created=true")
	}
	if second.ID != first.ID {
		t.Errorf("retry returned transaction %s, want %s", second.ID, first.ID)
	}
	if got := f.balance(t, alice.ID); got != 700 {
		t.Errorf("alice balance = %d, want 700 (posted once)", got)
	}
}

func TestIdempotencyKeyReuseWithDifferentRequestConflicts(t *testing.T) {
	f := newFixture(t)
	alice := f.account(t, "alice", "NGN", false)
	ctx := context.Background()

	f.post(t, transfer("once", f.world.ID, alice.ID, 700))
	_, _, err := f.svc.PostTransaction(ctx, transfer("once", f.world.ID, alice.ID, 800))
	if !errors.Is(err, ledger.ErrIdempotencyConflict) {
		t.Fatalf("err = %v, want ErrIdempotencyConflict", err)
	}
	if got := f.balance(t, alice.ID); got != 700 {
		t.Errorf("alice balance = %d, want 700", got)
	}
}

// A client that times out and retries can land several identical requests on
// the server at once. Exactly one must post; the rest must return it.
func TestConcurrentRetriesPostOnce(t *testing.T) {
	f := newFixture(t)
	alice := f.account(t, "alice", "NGN", false)

	const workers = 20
	var (
		wg      sync.WaitGroup
		start   = make(chan struct{})
		mu      sync.Mutex
		created int
		ids     = make(map[uuid.UUID]int)
		errs    []error
	)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			txn, c, err := f.svc.PostTransaction(context.Background(),
				transfer("same-key", f.world.ID, alice.ID, 500))
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			if c {
				created++
			}
			ids[txn.ID]++
		}()
	}
	close(start)
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if created != 1 {
		t.Errorf("created = %d, want 1", created)
	}
	if len(ids) != 1 {
		t.Errorf("got %d distinct transaction ids, want 1: %v", len(ids), ids)
	}
	if got := f.balance(t, alice.ID); got != 500 {
		t.Errorf("alice balance = %d, want 500 (posted once)", got)
	}
}
