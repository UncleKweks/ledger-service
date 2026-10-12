// Package testdb gives each test its own freshly migrated PostgreSQL database.
//
// It connects to LEDGER_TEST_DATABASE_URL, or to the local docker-compose
// database when that is unset. With the variable unset and no database
// reachable, tests are skipped; with it set, an unreachable database fails them.
package testdb

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/UncleKweks/ledger-service/migrations"
)

const (
	envVar     = "LEDGER_TEST_DATABASE_URL"
	defaultURL = "postgres://ledger:ledger_dev@127.0.0.1:55432/ledger?sslmode=disable"
)

// New creates an empty database, applies all migrations, and returns a pool
// connected to it. The database is dropped when the test ends.
func New(t testing.TB) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	baseURL, required := os.LookupEnv(envVar)
	if !required {
		baseURL = defaultURL
	}

	admin, err := pgx.Connect(ctx, baseURL)
	if err != nil {
		if required {
			t.Fatalf("testdb: cannot connect to %s: %v", envVar, err)
		}
		t.Skipf("testdb: no database at %s (start it with `docker compose up -d` or set %s): %v", defaultURL, envVar, err)
	}
	defer admin.Close(context.Background())

	name := "ledger_test_" + randomHex(8)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("testdb: create database: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		conn, err := pgx.Connect(ctx, baseURL)
		if err != nil {
			t.Errorf("testdb: drop database: %v", err)
			return
		}
		defer conn.Close(ctx)
		if _, err := conn.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
			t.Errorf("testdb: drop database: %v", err)
		}
	})

	u, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("testdb: parse url: %v", err)
	}
	u.Path = "/" + name
	dbURL := u.String()

	migrate(ctx, t, dbURL)

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("testdb: connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func migrate(ctx context.Context, t testing.TB, dbURL string) {
	t.Helper()
	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		t.Fatalf("testdb: open: %v", err)
	}
	defer db.Close()

	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS)
	if err != nil {
		t.Fatalf("testdb: goose provider: %v", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("testdb: migrate: %v", err)
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
