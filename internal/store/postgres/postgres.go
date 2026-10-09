// Package postgres implements ledger persistence on PostgreSQL using pgx.
package postgres

import "github.com/jackc/pgx/v5/pgxpool"

// Store wraps a pgx connection pool.
type Store struct {
	pool *pgxpool.Pool
}

// New returns a Store backed by pool.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}
