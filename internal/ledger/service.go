package ledger

import (
	"context"

	"github.com/google/uuid"
)

// Store persists the ledger. Implementations must post a transaction
// atomically, reject overdrafts on accounts that do not allow a negative
// balance, and treat a repeated idempotency key as a replay or a conflict.
type Store interface {
	CreateAccount(ctx context.Context, a NewAccount) (Account, error)
	GetAccount(ctx context.Context, id uuid.UUID) (Account, error)
	GetBalance(ctx context.Context, accountID uuid.UUID) (Balance, error)
	ListEntries(ctx context.Context, accountID uuid.UUID, limit int) ([]Entry, error)
	// PostTransaction reports created=false when the idempotency key matched
	// an earlier identical posting, which is returned unchanged.
	PostTransaction(ctx context.Context, t NewTransaction) (txn Transaction, created bool, err error)
	GetTransaction(ctx context.Context, id uuid.UUID) (Transaction, error)
}

// Entry listing limits.
const (
	DefaultListLimit = 50
	MaxListLimit     = 500
)

// Service is the ledger's core. Every transport (REST, gRPC) goes through it,
// so input is normalised and validated the same way whatever the entry point.
type Service struct {
	store Store
}

// NewService returns a Service backed by store.
func NewService(store Store) *Service {
	return &Service{store: store}
}

// CreateAccount validates and creates an account.
func (s *Service) CreateAccount(ctx context.Context, a NewAccount) (Account, error) {
	a = a.Normalize()
	if err := a.Validate(); err != nil {
		return Account{}, err
	}
	return s.store.CreateAccount(ctx, a)
}

// GetAccount returns an account by id.
func (s *Service) GetAccount(ctx context.Context, id uuid.UUID) (Account, error) {
	return s.store.GetAccount(ctx, id)
}

// GetBalance returns an account's current balance.
func (s *Service) GetBalance(ctx context.Context, accountID uuid.UUID) (Balance, error) {
	return s.store.GetBalance(ctx, accountID)
}

// ListEntries returns an account's most recent entries, newest first.
// A limit outside 1..MaxListLimit is replaced by DefaultListLimit or clamped.
func (s *Service) ListEntries(ctx context.Context, accountID uuid.UUID, limit int) ([]Entry, error) {
	switch {
	case limit <= 0:
		limit = DefaultListLimit
	case limit > MaxListLimit:
		limit = MaxListLimit
	}
	return s.store.ListEntries(ctx, accountID, limit)
}

// PostTransaction validates and posts a transaction. created is false when
// the request replayed an earlier posting with the same idempotency key.
func (s *Service) PostTransaction(ctx context.Context, t NewTransaction) (Transaction, bool, error) {
	t = t.Normalize()
	if err := t.Validate(); err != nil {
		return Transaction{}, false, err
	}
	return s.store.PostTransaction(ctx, t)
}

// GetTransaction returns a transaction and its entries.
func (s *Service) GetTransaction(ctx context.Context, id uuid.UUID) (Transaction, error) {
	return s.store.GetTransaction(ctx, id)
}
