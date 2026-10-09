// Package ledger holds the domain types and rules of the double-entry ledger.
// It has no knowledge of SQL or HTTP.
package ledger

import (
	"time"

	"github.com/google/uuid"
)

// Account is a named bucket of value in a single currency.
// Its balance is not stored; it is the sum of its entries.
type Account struct {
	ID        uuid.UUID
	Name      string
	Currency  string // ISO 4217 code, e.g. "NGN", "USD"
	CreatedAt time.Time
}

// Transaction groups the entries of one posting. Its entries must sum to zero.
type Transaction struct {
	ID             uuid.UUID
	IdempotencyKey string
	Description    string
	Entries        []Entry
	CreatedAt      time.Time
}

// Entry is one immutable line of a transaction against one account.
// Amount is in minor units (kobo, cents): positive is a debit, negative a credit.
type Entry struct {
	ID            uuid.UUID
	TransactionID uuid.UUID
	AccountID     uuid.UUID
	Currency      string
	Amount        int64
	CreatedAt     time.Time
}
