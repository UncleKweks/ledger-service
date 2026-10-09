# ledger-service

A double-entry ledger service in Go and PostgreSQL. Every movement of money is
recorded as a balanced set of immutable entries, and the database itself
enforces the invariants that keep the books correct.

## Core model

| Table          | Purpose                                                                 |
|----------------|-------------------------------------------------------------------------|
| `accounts`     | A named bucket of value in exactly one currency (ISO 4217, e.g. `NGN`). |
| `transactions` | One posting. Carries a unique `idempotency_key` and a description.      |
| `entries`      | One line of a transaction against one account. Append-only.             |

`entries.amount` is a signed `BIGINT` in **minor units** (kobo, cents).
Positive is a debit, negative is a credit. The entries of a transaction always
sum to zero.

```
accounts 1 ──< entries >── 1 transactions
   (id, currency) ◄── FK ── (account_id, currency)
```

## Invariants and where they are enforced

| Rule | Guarantee                                         | Enforcement                                                                                         |
|------|---------------------------------------------------|-----------------------------------------------------------------------------------------------------|
| A    | A transaction's entries sum to zero               | Posting code, inside the same database transaction that writes the entries (DB-level check planned) |
| B    | Entries are never updated or deleted              | `BEFORE UPDATE OR DELETE` row trigger and `BEFORE TRUNCATE` statement trigger raise an error         |
| C    | An entry's currency matches its account's currency | Composite foreign key `entries (account_id, currency) → accounts (id, currency)`                    |
| —    | A client retry never posts twice                  | `UNIQUE (idempotency_key)` on `transactions`                                                        |
| —    | No zero-value or malformed rows                   | `CHECK (amount <> 0)`, `CHECK (currency ~ '^[A-Z]{3}$')`                                             |

## Design decisions

### Balances are computed, not stored

An account's balance is `SUM(entries.amount)` for that account. There is no
`balance` column on `accounts`.

- **Single source of truth.** The entries table is the ledger. A stored
  balance would be a second copy of the same fact, and any bug, partial
  write or manual fix that updates one without the other makes the books
  silently disagree with themselves. A computed balance cannot drift.
- **Simpler writes.** Posting a transaction is pure `INSERT`s. There is no
  read-modify-write on a hot balance row, so no lost-update risk on that row.
- **Auditable by construction.** The balance at any moment in time is
  `SUM(amount) WHERE created_at <= t`, using the same data and the same query.
- **Cost is acceptable and indexed.** `entries (account_id)` is indexed, so
  the sum is an index-backed scan of one account's entries.

The trade-off is read cost growing with an account's entry count. If that
becomes a bottleneck, the next step is a derived cache, such as periodic
balance snapshots plus the sum of entries after the snapshot, that can always
be rebuilt from the entries table rather than replacing it.

### Money is an integer in minor units

Floating-point types cannot represent most decimal fractions exactly
(`0.1 + 0.2 != 0.3`), and rounding errors accumulate. All amounts are `BIGINT`
minor units. `BIGINT` holds about ±9.2 × 10¹⁸, so overflow is not a practical
concern.

### Entries are append-only

History is never edited. A mistaken posting is corrected by posting a new,
reversing transaction, so the full sequence of events stays visible. The
trigger guarantees this even for ad-hoc SQL run against the database, not
only for code paths in this service.

Note: a superuser or the table owner can still disable triggers. In
production the service should connect as a role that owns nothing and is
granted only `SELECT` and `INSERT` on `entries`.

### Currency is denormalised onto entries

`entries.currency` duplicates the account's currency on purpose. It lets a
plain foreign key on `(account_id, currency)` enforce Rule C declaratively,
with no trigger and no application check. `accounts` carries
`UNIQUE (id, currency)` to serve as the target of that key.

### Migrations are embedded

SQL migrations live in `migrations/` as numbered goose files and are compiled
into the binary with `go:embed`. The migrate command and the test suite apply
exactly the same files.

## Project layout

```
cmd/ledger/               HTTP service entry point (wiring only)
cmd/migrate/              applies the embedded migrations (goose)
internal/ledger/          domain types and rules; no SQL, no HTTP
internal/store/postgres/  PostgreSQL persistence (pgx)
migrations/               numbered SQL migrations, embedded via go:embed
docker-compose.yml        PostgreSQL 16 for local development
```

## Running locally

Requirements: Go 1.26+, Docker.

```bash
docker compose up -d --wait        # Postgres 16 on 127.0.0.1:55432
go run ./cmd/migrate up            # apply migrations
go run ./cmd/ledger                # serve on :8080
curl localhost:8080/health
```

Postgres is published on host port `55432` rather than `5432` so it never
collides with a locally installed PostgreSQL.

Configuration:

| Variable       | Default                                                               |
|----------------|-----------------------------------------------------------------------|
| `DATABASE_URL` | `postgres://ledger:ledger_dev@127.0.0.1:55432/ledger?sslmode=disable` |

Migration commands: `go run ./cmd/migrate up | down | status | reset`.

## Dependencies

| Module                       | Why                                                        |
|------------------------------|------------------------------------------------------------|
| `github.com/jackc/pgx/v5`    | PostgreSQL driver and connection pool                      |
| `github.com/google/uuid`     | UUID type for identifiers                                  |
| `github.com/pressly/goose/v3` | Versioned SQL migrations, run from embedded files         |

## Roadmap

- [x] Schema, migrations, database-level invariants
- [ ] Posting: one database transaction per posting, row locks, balance reads
- [ ] REST API with idempotency keys
- [ ] Tests against real PostgreSQL, including concurrency and idempotent retries
- [ ] gRPC API over the same core
- [ ] CI and architecture diagram
