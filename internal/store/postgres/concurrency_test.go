package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/UncleKweks/ledger-service/internal/ledger"
)

// Many concurrent withdrawals race for the same balance. The row lock must
// serialise them so that exactly balance/amount succeed and the account never
// goes negative.
func TestConcurrentWithdrawalsNeverOverdraw(t *testing.T) {
	f := newFixture(t)
	alice := f.account(t, "alice", "NGN", false)
	bob := f.account(t, "bob", "NGN", false)
	f.post(t, transfer("fund", f.world.ID, alice.ID, 1_000))

	const workers, amount = 50, 100
	var (
		wg        sync.WaitGroup
		start     = make(chan struct{})
		mu        sync.Mutex
		succeeded int
		rejected  int
		other     []error
	)
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _, err := f.svc.PostTransaction(context.Background(),
				transfer(fmt.Sprintf("withdraw-%d", i), alice.ID, bob.ID, amount))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				succeeded++
			case errors.Is(err, ledger.ErrInsufficientFunds):
				rejected++
			default:
				other = append(other, err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(other) > 0 {
		t.Fatalf("unexpected errors: %v", other)
	}
	if succeeded != 10 || rejected != 40 {
		t.Errorf("succeeded=%d rejected=%d, want 10 and 40", succeeded, rejected)
	}
	if got := f.balance(t, alice.ID); got != 0 {
		t.Errorf("alice balance = %d, want 0", got)
	}
	if got := f.balance(t, bob.ID); got != 1_000 {
		t.Errorf("bob balance = %d, want 1000", got)
	}
}

// Transfers in both directions between the same two accounts run at once.
// With a consistent lock order and FOR NO KEY UPDATE there is no deadlock, so
// every transfer succeeds and money is conserved.
func TestConcurrentOpposingTransfersDoNotDeadlock(t *testing.T) {
	f := newFixture(t)
	alice := f.account(t, "alice", "NGN", false)
	bob := f.account(t, "bob", "NGN", false)
	f.post(t, transfer("fund-a", f.world.ID, alice.ID, 10_000))
	f.post(t, transfer("fund-b", f.world.ID, bob.ID, 10_000))

	const workers = 50
	var (
		wg    sync.WaitGroup
		start = make(chan struct{})
		errs  = make(chan error, workers)
	)
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			from, to := alice.ID, bob.ID
			if i%2 == 1 {
				from, to = to, from
			}
			if _, _, err := f.svc.PostTransaction(context.Background(),
				transfer(fmt.Sprintf("swap-%d", i), from, to, 10)); err != nil {
				errs <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("transfer failed: %v", err)
	}
	a, b := f.balance(t, alice.ID), f.balance(t, bob.ID)
	if a != 10_000 || b != 10_000 {
		t.Errorf("balances = %d, %d; want 10000 each (25 transfers each way)", a, b)
	}
}
