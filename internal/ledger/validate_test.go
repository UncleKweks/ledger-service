package ledger

import (
	"errors"
	"math"
	"testing"

	"github.com/google/uuid"
)

func TestNewTransactionValidate(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	valid := func() NewTransaction {
		return NewTransaction{
			IdempotencyKey: "key-1",
			Entries: []NewEntry{
				{AccountID: a, Currency: "NGN", Amount: -500},
				{AccountID: b, Currency: "NGN", Amount: 500},
			},
		}
	}

	tests := []struct {
		name    string
		modify  func(*NewTransaction)
		wantErr error
	}{
		{"valid", func(*NewTransaction) {}, nil},
		{"missing idempotency key", func(t *NewTransaction) { t.IdempotencyKey = "" }, ErrInvalid},
		{"one entry", func(t *NewTransaction) { t.Entries = t.Entries[:1] }, ErrInvalid},
		{"zero amount", func(t *NewTransaction) { t.Entries[0].Amount = 0 }, ErrInvalid},
		{"nil account", func(t *NewTransaction) { t.Entries[0].AccountID = uuid.Nil }, ErrInvalid},
		{"bad currency", func(t *NewTransaction) { t.Entries[0].Currency = "naira" }, ErrInvalid},
		{"unbalanced", func(t *NewTransaction) { t.Entries[1].Amount = 400 }, ErrUnbalanced},
		{"balanced only across currencies", func(t *NewTransaction) { t.Entries[1].Currency = "USD" }, ErrUnbalanced},
		{"currency sum overflows", func(t *NewTransaction) {
			t.Entries = []NewEntry{
				{AccountID: a, Currency: "NGN", Amount: math.MaxInt64},
				{AccountID: b, Currency: "NGN", Amount: 1},
				{AccountID: b, Currency: "NGN", Amount: math.MinInt64},
			}
		}, ErrInvalid},
		{"account net overflows into a credit", func(t *NewTransaction) {
			t.Entries = []NewEntry{
				{AccountID: a, Currency: "NGN", Amount: -math.MaxInt64},
				{AccountID: b, Currency: "NGN", Amount: math.MaxInt64},
				{AccountID: a, Currency: "NGN", Amount: -2},
				{AccountID: b, Currency: "NGN", Amount: 2},
			}
		}, ErrInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			txn := valid()
			tt.modify(&txn)
			err := txn.Validate()
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestUnbalancedIsInvalid(t *testing.T) {
	if !errors.Is(ErrUnbalanced, ErrInvalid) {
		t.Fatal("ErrUnbalanced should wrap ErrInvalid")
	}
}

func TestNewTransactionNormalize(t *testing.T) {
	in := NewTransaction{
		IdempotencyKey: "  k  ",
		Description:    " rent ",
		Entries:        []NewEntry{{Currency: " ngn "}},
	}
	got := in.Normalize()
	if got.IdempotencyKey != "k" || got.Description != "rent" || got.Entries[0].Currency != "NGN" {
		t.Fatalf("Normalize() = %+v", got)
	}
	if in.Entries[0].Currency != " ngn " {
		t.Fatal("Normalize modified the caller's entries")
	}
}

func TestMatchesIgnoresEntryOrder(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	req := NewTransaction{
		Description: "rent",
		Entries: []NewEntry{
			{AccountID: a, Currency: "NGN", Amount: -500},
			{AccountID: b, Currency: "NGN", Amount: 500},
		},
	}
	stored := Transaction{
		Description: "rent",
		Entries: []Entry{
			{AccountID: b, Currency: "NGN", Amount: 500},
			{AccountID: a, Currency: "NGN", Amount: -500},
		},
	}
	if !req.Matches(stored) {
		t.Fatal("same posting in a different order should match")
	}
	stored.Entries[0].Amount, stored.Entries[1].Amount = 400, -400
	if req.Matches(stored) {
		t.Fatal("different amounts should not match")
	}
}

func TestNewAccountValidate(t *testing.T) {
	ok := NewAccount{Name: "Alice", Currency: "ngn"}.Normalize()
	if err := ok.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	for _, a := range []NewAccount{
		{Name: "", Currency: "NGN"},
		{Name: "Alice", Currency: "NG"},
		{Name: "Alice", Currency: "N1N"},
	} {
		if err := a.Normalize().Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("Validate(%+v) = %v, want ErrInvalid", a, err)
		}
	}
}
