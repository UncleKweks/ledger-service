# ledger-service

[![CI](https://github.com/UncleKweks/ledger-service/actions/workflows/ci.yml/badge.svg)](https://github.com/UncleKweks/ledger-service/actions/workflows/ci.yml)

A double-entry ledger service in Go and PostgreSQL, served over REST and gRPC.
Every movement of money is recorded as a balanced set of immutable entries.
Correctness does not rest on the application alone: the database enforces
the ledger's invariants itself, and concurrent postings are serialised per
account so balances can never be overdrawn.

- **Double-entry**: every transaction's entries sum to zero, checked in Go
  and again by a deferred constraint trigger at `COMMIT`.
- **Append-only history**: entries can never be updated, deleted or truncated.
- **Safe under concurrency**: per-account row locks prevent overdrafts without
  deadlocks or a global bottleneck.
- **Idempotent**: retried requests return the original result and never post twice.
- **One core, two transports**: REST and gRPC share the same service layer.
- **Tested against real PostgreSQL**, including race-detector runs of the
  concurrency and idempotency tests.

## Architecture

```mermaid
flowchart LR
    RC[REST client] -->|"JSON / HTTP :8080"| H[internal/httpapi]
    GC[gRPC client] -->|"protobuf / HTTP/2 :9090"| G[internal/grpcapi]
    H --> S["internal/ledger<br/>Service: normalise + validate"]
    G --> S
    S -->|Store interface| P["internal/store/postgres<br/>locking, idempotency, SQL"]
    P --> DB[("PostgreSQL 16<br/>constraints + triggers")]
```

| Layer                     | Responsibility                                                                 |
|---------------------------|--------------------------------------------------------------------------------|
| `internal/httpapi`        | JSON encoding, `Idempotency-Key` header, error → HTTP status mapping           |
| `internal/grpcapi`        | Protobuf mapping, error → gRPC code mapping                                    |
| `internal/ledger`         | Domain types, validation (including Rule A), the `Service` both transports use |
| `internal/store/postgres` | Atomic posting, row locking, overdraft checks, idempotent replay               |
| PostgreSQL                | Last line of defence: foreign keys, checks, append-only and balance triggers   |

The domain package has no SQL or HTTP dependencies. The store implements a
`ledger.Store` interface, and both transports call the same `ledger.Service`,
so validation and business rules cannot diverge between REST and gRPC.

## Data model

```mermaid
erDiagram
    accounts ||--o{ entries : "account_id, currency"
    transactions ||--|{ entries : transaction_id
    accounts {
        uuid id PK
        text name
        char3 currency
        bool allow_negative_balance
        timestamptz created_at
    }
    transactions {
        uuid id PK
        text idempotency_key UK
        text description
        timestamptz created_at
    }
    entries {
        uuid id PK
        uuid transaction_id FK
        uuid account_id FK
        char3 currency FK
        bigint amount
        timestamptz created_at
    }
```

`entries.amount` is a signed `BIGINT` in **minor units** (kobo, cents).
Positive is a debit, negative is a credit.

## Invariants

| Rule | Guarantee                                           | Enforced by                                                                                              |
|------|-----------------------------------------------------|----------------------------------------------------------------------------------------------------------|
| A    | Each transaction's entries sum to zero per currency | `NewTransaction.Validate` in Go, and deferred constraint triggers checked at `COMMIT`                    |
| A    | Each transaction has at least two entries           | Same as above                                                                                            |
| B    | Entries are never updated, deleted or truncated     | `BEFORE UPDATE OR DELETE` row trigger and `BEFORE TRUNCATE` statement trigger                            |
| C    | An entry's currency matches its account's currency  | Composite foreign key `entries (account_id, currency) → accounts (id, currency)`, plus a pre-check in Go |
| —    | Ordinary accounts are never overdrawn               | Row lock and balance check inside the posting transaction                                                |
| —    | A retried request never posts twice                 | `UNIQUE (idempotency_key)` and `INSERT … ON CONFLICT DO NOTHING`                                         |
| —    | No zero amounts or malformed currencies             | `CHECK (amount <> 0)`, `CHECK (currency ~ '^[A-Z]{3}$')`                                                 |

Rules A, B and C hold even for SQL run directly against the database, not
only for requests through this service. The test suite verifies this by
attempting each violation with raw SQL.

## How a posting works

```mermaid
sequenceDiagram
    participant C as Client
    participant S as ledger.Service
    participant P as postgres.Store
    participant DB as PostgreSQL

    C->>S: PostTransaction(key, entries)
    S->>S: normalise, validate (Rule A, overflow)
    S->>P: PostTransaction
    P->>DB: BEGIN
    P->>DB: INSERT transactions ... ON CONFLICT (idempotency_key) DO NOTHING
    alt key already used
        P->>DB: load existing transaction
        P-->>C: same request → original (replayed) / different → conflict
    else new key
        P->>DB: check accounts exist and currencies match
        P->>DB: SELECT ... ORDER BY id FOR NO KEY UPDATE (debited, non-negative accounts)
        P->>DB: SUM(entries.amount) per locked account → reject overdraft
        P->>DB: INSERT entries (one statement, unnest)
        P->>DB: COMMIT (deferred balance triggers run)
        P-->>C: created transaction
    end
```

Any error rolls the whole database transaction back, so a posting is never
partially applied.

## Design decisions

### Balances are computed, not stored

An account's balance is `SUM(entries.amount)`; there is no `balance` column.

- **Single source of truth.** A stored balance is a second copy of the same
  fact. Any bug, partial write or manual fix that updates one without the
  other makes the books silently disagree with themselves. A computed
  balance cannot drift.
- **Simpler writes.** Posting is pure `INSERT`s, with no read-modify-write
  of a balance row.
- **Auditable by construction.** The balance at any past moment is the same
  query with `WHERE created_at <= t`.
- **Indexed.** `entries (account_id)` keeps the sum to an index-backed scan of
  one account's entries.

The trade-off is read cost growing with an account's entry count. The next
step, if needed, is a derived cache such as periodic balance snapshots plus
the sum of later entries. A cache can always be rebuilt from `entries`, so it
never becomes a second source of truth.

### Concurrency: per-account row locks

With computed balances, two concurrent withdrawals could both read a balance
of 100 and both spend it. Each posting therefore locks the rows of the
accounts it debits before reading their balances:

```sql
SELECT id FROM accounts
WHERE id = ANY($1) AND NOT allow_negative_balance
ORDER BY id
FOR NO KEY UPDATE
```

- **Serialised per account, not globally.** Postings that touch different
  accounts run fully in parallel.
- **Deadlock-free ordering.** Locks are always taken in `id` order, so two
  postings can never wait on each other in a cycle.
- **`FOR NO KEY UPDATE`, not `FOR UPDATE`.** Inserting an entry makes
  PostgreSQL take a `FOR KEY SHARE` lock on the referenced account to check
  the foreign key. `FOR UPDATE` conflicts with that lock, so a concurrent
  A→B and B→A deadlock on each other's foreign-key checks. `FOR NO KEY UPDATE`
  still serialises postings that debit the same account, but does not block
  foreign-key checks. The test suite reproduces the deadlock when this is
  changed to `FOR UPDATE`.
- **No lock on accounts allowed to go negative.** Funding and settlement
  accounts take part in a large share of postings. Since they are never
  balance-checked, they are never locked and never become a hot row.
- **`READ COMMITTED` is sufficient.** Each statement takes a fresh snapshot,
  so the balance `SUM` that runs after the lock is acquired sees every entry
  committed by earlier lock holders. `SERIALIZABLE` would also be correct but
  would push retry loops onto every caller.

### Idempotency

Every posting carries a client-chosen idempotency key, stored under a unique
constraint:

- **Retry**: the same key with the same request returns the original
  transaction unchanged (`200` with `Idempotent-Replayed: true` over REST,
  `replayed = true` over gRPC).
- **Conflict**: the same key with a different request is rejected (`409` /
  `ALREADY_EXISTS`). Comparison ignores entry order.
- **Concurrent duplicates**: a second in-flight request with the same key
  blocks on the unique index until the first commits, then replays it.
  Exactly one posting happens.
- **Failures are not recorded**: a rejected posting (for example insufficient
  funds) rolls back with its key, so the client may retry the same key.

### Money is an integer in minor units

Floating-point types cannot represent most decimal fractions exactly and
accumulate rounding errors. Amounts are `int64` minor units end to end. Sums
are overflow-checked during validation, both per currency and per account,
so a crafted request cannot wrap a large debit into a credit.

### Entries are append-only

History is never edited. A mistaken posting is corrected by posting a
reversing transaction, so the full sequence of events stays visible. The
triggers guarantee this even for ad-hoc SQL.

### Currency is denormalised onto entries

`entries.currency` repeats the account's currency on purpose, so a plain
composite foreign key enforces Rule C declaratively. `accounts` carries
`UNIQUE (id, currency)` as the key's target.

### Migrations are embedded

Numbered goose migrations in `migrations/` are compiled into the binaries
with `go:embed`. The migrate command, the test suite and CI apply exactly the
same files, and CI verifies every migration can be rolled back.

## API

### REST (`:8080`)

| Method | Path                       | Description                                                 |
|--------|----------------------------|-------------------------------------------------------------|
| `POST` | `/accounts`                | Create an account                                           |
| `GET`  | `/accounts/{id}`           | Get an account                                              |
| `GET`  | `/accounts/{id}/balance`   | Current balance                                             |
| `GET`  | `/accounts/{id}/entries`   | Entries, newest first (`?limit=`, default 50, max 500)      |
| `POST` | `/transactions`            | Post a transaction (requires an `Idempotency-Key` header)   |
| `GET`  | `/transactions/{id}`       | Get a transaction and its entries                           |
| `GET`  | `/health`                  | Liveness                                                    |

```bash
# A funding account that may go negative, and a customer account.
curl -s -X POST localhost:8080/accounts \
  -d '{"name":"funding","currency":"NGN","allow_negative_balance":true}'
curl -s -X POST localhost:8080/accounts -d '{"name":"alice","currency":"NGN"}'

# Move ₦100.00 (10000 kobo) from funding to alice.
curl -s -X POST localhost:8080/transactions \
  -H 'Idempotency-Key: 6f1c2a0e-top-up-1' \
  -d '{
        "description": "top-up",
        "entries": [
          {"account_id": "<funding-id>", "currency": "NGN", "amount": -10000},
          {"account_id": "<alice-id>",   "currency": "NGN", "amount":  10000}
        ]
      }'

curl -s localhost:8080/accounts/<alice-id>/balance
# {"account_id":"<alice-id>","currency":"NGN","balance":10000}
```

Errors share one shape: `{"error": {"code": "insufficient_funds", "message": "..."}}`.

| Status | Codes                                                                    |
|--------|--------------------------------------------------------------------------|
| 400    | `invalid_json`, `invalid_request`, `missing_idempotency_key`             |
| 404    | `account_not_found`, `transaction_not_found`                             |
| 409    | `idempotency_conflict`                                                   |
| 422    | `invalid_request`, `unbalanced`, `insufficient_funds`, `currency_mismatch` |
| 500    | `internal` (details are logged, never returned)                          |

Request bodies are limited to 1 MiB, and unknown JSON fields are rejected.

### gRPC (`:9090`)

The contract is [`proto/ledger/v1/ledger.proto`](proto/ledger/v1/ledger.proto)
(`ledger.v1.LedgerService`), with the same operations as the REST API. The
server also exposes the standard health service and server reflection:

```bash
grpcurl -plaintext localhost:9090 list
grpcurl -plaintext -d '{"account_id":"<alice-id>"}' \
  localhost:9090 ledger.v1.LedgerService/GetBalance
```

| Condition                              | gRPC code             |
|----------------------------------------|-----------------------|
| Invalid input, unbalanced entries      | `INVALID_ARGUMENT`    |
| Unknown account or transaction         | `NOT_FOUND`           |
| Insufficient funds, currency mismatch  | `FAILED_PRECONDITION` |
| Idempotency key reused                 | `ALREADY_EXISTS`      |

## Running locally

Requirements: Go 1.26+, Docker.

```bash
docker compose up -d --wait   # PostgreSQL 16 on 127.0.0.1:55432
go run ./cmd/migrate up       # apply migrations
go run ./cmd/ledger           # REST on :8080, gRPC on :9090
```

PostgreSQL is published on host port `55432`, bound to `127.0.0.1`, so it never
collides with a locally installed PostgreSQL and is not exposed to the network.

| Variable       | Default                                                               |
|----------------|-----------------------------------------------------------------------|
| `DATABASE_URL` | `postgres://ledger:ledger_dev@127.0.0.1:55432/ledger?sslmode=disable` |
| `HTTP_ADDR`    | `:8080`                                                               |
| `GRPC_ADDR`    | `:9090`                                                               |

Migration commands: `go run ./cmd/migrate up | down | status | reset`.

The service logs structured JSON, applies HTTP server timeouts, and shuts both
servers down gracefully on `SIGINT` or `SIGTERM`.

## Testing

```bash
docker compose up -d --wait
go test ./...
```

Every test that needs a database gets its own freshly created and migrated
PostgreSQL database, dropped when the test ends. Tests run against the real
schema, constraints and triggers, with no mocks.

| Suite                     | Covers                                                                                                 |
|---------------------------|--------------------------------------------------------------------------------------------------------|
| `internal/ledger`         | Validation table tests: balancing, currencies, overflow, idempotent request matching                   |
| `internal/store/postgres` | Posting, balances, overdrafts, not-found cases, and database guards exercised with raw SQL             |
| ↳ concurrency             | 50 concurrent withdrawals against one balance (exactly 10 of 10 affordable succeed); 50 opposing A↔B transfers (no deadlock) |
| ↳ idempotency             | Sequential retry, retry with reordered entries, key reuse conflict, 20 concurrent identical requests post exactly once |
| `internal/httpapi`        | End-to-end REST flow, replay headers, every error status                                               |
| `internal/grpcapi`        | End-to-end gRPC flow over an in-memory connection, status codes                                        |

The concurrency tests were checked against deliberately broken builds: with
the row lock removed, the withdrawal test overdraws the account, and with
`FOR UPDATE` in place of `FOR NO KEY UPDATE`, the opposing-transfer test
deadlocks.

Database selection: tests use `LEDGER_TEST_DATABASE_URL`. When it is unset,
they fall back to the docker-compose database and skip if it is unreachable.
When it is set, as in CI, an unreachable database fails the run.

To run with the race detector without a local C toolchain:

```bash
docker run --rm --network ledger-service_default -v "$PWD:/src" -w /src \
  -e LEDGER_TEST_DATABASE_URL="postgres://ledger:ledger_dev@db:5432/ledger?sslmode=disable" \
  golang:1.26 go test -race ./...
```

### Continuous integration

GitHub Actions runs three jobs on every push and pull request:

- **lint**: `gofmt`, `go vet`, `staticcheck`.
- **test**: PostgreSQL 16 service container; migrations applied, fully
  rolled back and re-applied; `go test -race`.
- **proto**: `buf lint`, and a check that the committed generated code matches
  a fresh `buf generate`.

## Development

Generated gRPC code is committed under `gen/`. After editing the `.proto` file,
regenerate it:

```bash
go install github.com/bufbuild/buf/cmd/buf@v1.73.0
go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.2
buf lint && buf generate
```

## Project layout

```text
cmd/ledger/               service entry point: wiring only
cmd/migrate/              applies the embedded migrations
internal/ledger/          domain types, validation, Service (no SQL, no HTTP)
internal/store/postgres/  PostgreSQL implementation of ledger.Store
internal/httpapi/         REST transport
internal/grpcapi/         gRPC transport
internal/testdb/          per-test migrated PostgreSQL databases
migrations/               numbered SQL migrations, embedded via go:embed
proto/ledger/v1/          gRPC contract
gen/ledger/v1/            generated protobuf and gRPC code
```

## Dependencies

| Module                         | Purpose                                         |
|--------------------------------|-------------------------------------------------|
| `github.com/jackc/pgx/v5`      | PostgreSQL driver and connection pool           |
| `github.com/pressly/goose/v3`  | Versioned SQL migrations from embedded files    |
| `github.com/google/uuid`       | UUID identifiers                                |
| `google.golang.org/grpc`       | gRPC server, health and reflection services     |
| `google.golang.org/protobuf`   | Protobuf runtime                                |
| `golang.org/x/sync`            | `errgroup` for running and stopping both servers |

## Limitations and next steps

- **Balance reads scale with entry count.** Snapshots plus a delta sum would
  bound read cost for very active accounts.
- **Contention on hot debited accounts.** Postings that debit the same
  non-negative account are serialised by design. Very high-volume debit
  accounts would need sharded sub-accounts or batched posting.
- **Entry listing is not paginated.** `ListEntries` returns the latest N
  entries; a keyset cursor on `(created_at, id)` would allow paging through
  full history.
- **No authentication or authorisation.** Callers are trusted; production use
  needs authentication and per-account access control.
- **JSON amounts are numbers.** Values above 2⁵³ lose precision in JavaScript
  clients; such clients should treat amounts as strings or `BigInt`.
- **Triggers can be bypassed by a superuser.** In production the service
  should connect as a role granted only `SELECT` and `INSERT` on `entries`.
