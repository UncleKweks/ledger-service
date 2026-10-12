package ledger

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"

	"github.com/google/uuid"
)

// Input limits.
const (
	MaxNameLength           = 200
	MaxDescriptionLength    = 1000
	MaxIdempotencyKeyLength = 255
	MaxEntries              = 100
)

var currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)

// Normalize trims whitespace and upper-cases the currency.
func (a NewAccount) Normalize() NewAccount {
	a.Name = strings.TrimSpace(a.Name)
	a.Currency = strings.ToUpper(strings.TrimSpace(a.Currency))
	return a
}

// Validate reports whether a is a valid account. Call Normalize first.
func (a NewAccount) Validate() error {
	if a.Name == "" || len(a.Name) > MaxNameLength {
		return fmt.Errorf("%w: name must be 1-%d characters", ErrInvalid, MaxNameLength)
	}
	if !currencyPattern.MatchString(a.Currency) {
		return fmt.Errorf("%w: currency must be a 3-letter ISO 4217 code", ErrInvalid)
	}
	return nil
}

// Normalize trims whitespace and upper-cases currencies.
func (t NewTransaction) Normalize() NewTransaction {
	t.IdempotencyKey = strings.TrimSpace(t.IdempotencyKey)
	t.Description = strings.TrimSpace(t.Description)
	t.Entries = slices.Clone(t.Entries)
	for i := range t.Entries {
		t.Entries[i].Currency = strings.ToUpper(strings.TrimSpace(t.Entries[i].Currency))
	}
	return t
}

// Validate checks the transaction's shape and Rule A: per currency, the
// entries sum to zero. Call Normalize first.
func (t NewTransaction) Validate() error {
	if t.IdempotencyKey == "" || len(t.IdempotencyKey) > MaxIdempotencyKeyLength {
		return fmt.Errorf("%w: idempotency key must be 1-%d characters", ErrInvalid, MaxIdempotencyKeyLength)
	}
	if len(t.Description) > MaxDescriptionLength {
		return fmt.Errorf("%w: description must be at most %d characters", ErrInvalid, MaxDescriptionLength)
	}
	if len(t.Entries) < 2 || len(t.Entries) > MaxEntries {
		return fmt.Errorf("%w: a transaction needs 2-%d entries", ErrInvalid, MaxEntries)
	}

	sums := make(map[string]int64)
	// Per-account net change. Checked here so the store can sum it without
	// overflow: a wrapped total could turn a large debit into a credit.
	net := make(map[uuid.UUID]int64)
	for i, e := range t.Entries {
		if e.AccountID == uuid.Nil {
			return fmt.Errorf("%w: entry %d: account id is required", ErrInvalid, i)
		}
		if !currencyPattern.MatchString(e.Currency) {
			return fmt.Errorf("%w: entry %d: currency must be a 3-letter ISO 4217 code", ErrInvalid, i)
		}
		if e.Amount == 0 {
			return fmt.Errorf("%w: entry %d: amount must not be zero", ErrInvalid, i)
		}
		sum, ok := addInt64(sums[e.Currency], e.Amount)
		if !ok {
			return fmt.Errorf("%w: entry %d: amounts overflow", ErrInvalid, i)
		}
		sums[e.Currency] = sum
		if net[e.AccountID], ok = addInt64(net[e.AccountID], e.Amount); !ok {
			return fmt.Errorf("%w: entry %d: amounts overflow", ErrInvalid, i)
		}
	}
	for currency, sum := range sums {
		if sum != 0 {
			return fmt.Errorf("%w: %s entries sum to %d", ErrUnbalanced, currency, sum)
		}
	}
	return nil
}

// Matches reports whether txn records the same posting as t, ignoring entry
// order. It decides whether a reused idempotency key is a retry or a conflict.
func (t NewTransaction) Matches(txn Transaction) bool {
	if t.Description != txn.Description || len(t.Entries) != len(txn.Entries) {
		return false
	}
	want := slices.Clone(t.Entries)
	got := make([]NewEntry, len(txn.Entries))
	for i, e := range txn.Entries {
		got[i] = NewEntry{AccountID: e.AccountID, Currency: e.Currency, Amount: e.Amount}
	}
	slices.SortFunc(want, compareEntries)
	slices.SortFunc(got, compareEntries)
	return slices.Equal(want, got)
}

func compareEntries(a, b NewEntry) int {
	if c := strings.Compare(a.AccountID.String(), b.AccountID.String()); c != 0 {
		return c
	}
	if c := strings.Compare(a.Currency, b.Currency); c != 0 {
		return c
	}
	switch {
	case a.Amount < b.Amount:
		return -1
	case a.Amount > b.Amount:
		return 1
	}
	return 0
}

// addInt64 returns a+b and false if the addition overflows.
func addInt64(a, b int64) (int64, bool) {
	if (b > 0 && a > math.MaxInt64-b) || (b < 0 && a < math.MinInt64-b) {
		return 0, false
	}
	return a + b, true
}
