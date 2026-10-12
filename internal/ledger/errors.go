package ledger

import (
	"errors"
	"fmt"
)

// Errors returned by the ledger. Callers match them with errors.Is; transports
// map them to status codes.
var (
	// ErrInvalid is wrapped by every input validation error.
	ErrInvalid = errors.New("invalid input")
	// ErrUnbalanced means a transaction's entries do not sum to zero. It wraps ErrInvalid.
	ErrUnbalanced = fmt.Errorf("%w: entries do not balance", ErrInvalid)

	ErrAccountNotFound     = errors.New("account not found")
	ErrTransactionNotFound = errors.New("transaction not found")
	ErrCurrencyMismatch    = errors.New("entry currency does not match account currency")
	ErrInsufficientFunds   = errors.New("insufficient funds")
	// ErrIdempotencyConflict means the idempotency key was already used for a
	// different request.
	ErrIdempotencyConflict = errors.New("idempotency key already used with a different request")
)
