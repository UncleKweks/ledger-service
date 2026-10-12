# ledger-service

[![CI](https://github.com/UncleKweks/ledger-service/actions/workflows/ci.yml/badge.svg)](https://github.com/UncleKweks/ledger-service/actions/workflows/ci.yml)
![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)
![PostgreSQL](https://img.shields.io/badge/PostgreSQL-16-4169E1?logo=postgresql&logoColor=white)
![gRPC](https://img.shields.io/badge/gRPC-ledger.v1-244c5a)

A double-entry ledger service in Go and PostgreSQL, served over REST and gRPC.

Every movement of money is recorded as a balanced set of immutable entries.
Correctness does not depend on the application alone: PostgreSQL enforces the
ledger's invariants itself, and concurrent postings are serialised per account,
so balances cannot be overdrawn, history cannot be rewritten, and a retried
request never posts twice.

## Highlights

- **Double-entry by construction.** Every transaction's entries sum to zero,
  checked in Go and again by a deferred constraint trigger at `COMMIT`.
- **Append-only history.** Entries can never be updated, deleted or truncated.
  Corrections are new, reversing transactions.
- **Safe under concurrency.** Per-account row locks prevent overdrafts without
  deadlocks or a global bottleneck. This is proven by tests that fail when the
  locking is weakened.
- **Idempotent writes.** Client retries return the original result, including
  when duplicates arrive at the same moment.
- **One core, two transports.** REST and gRPC call the same service layer, so
  their rules cannot drift apart.
- **Tested against real PostgreSQL**: no mocks, a fresh migrated database per
  test, and race-detector runs in CI.

## Contents

- [Quick start](#quick-start)
- [Concepts](#concepts)
- [Architecture](#architecture)
- [Data model](#data-model)
- [Invariants](#invariants)
- [How a posting works](#how-a-posting-works)
- [Design decisions](#design-decisions)
- [REST API](#rest-api)
- [gRPC API](#grpc-api)
- [Configuration](#configuration)
- [Testing](#testing)
- [Continuous integration](#continuous-integration)
- [Development](#development)
- [Troubleshooting](#troubleshooting)
- [Project layout](#project-layout)
- [Dependencies](#dependencies)
- [Limitations and roadmap](#limitations-and-roadmap)

## Quick start

Requirements: [Go 1.26+](https://go.dev/dl/) and
[Docker](https://docs.docker.com/get-docker/).

```bash
git clone https://github.com/UncleKweks/ledger-service.git
cd ledger-service

docker compose up -d --wait   # PostgreSQL 16 on 127.0.0.1:55432
go run ./cmd/migrate up       # create the schema
go run ./cmd/ledger           # REST on :8080, gRPC on :9090
```

In a second terminal, open two accounts and move money between them:

```bash
# A settlement account that represents money outside the ledger.
curl -s -X POST localhost:8080/accounts \
  -d '{"name":"Bank settlement","currency":"NGN","allow_negative_balance":true}'

# A customer account. Ordinary accounts can never go below zero.
curl -s -X POST localhost:8080/accounts -d '{"name":"Alice","currency":"NGN"}'

# Credit Alice with ₦50,000.00 (5,000,000 kobo) from settlement.
curl -s -X POST localhost:8080/transactions \
  -H 'Idempotency-Key: deposit-0001' \
  -d '{
        "description": "Card top-up",
        "entries": [
          {"account_id": "<settlement-id>", "currency": "NGN", "amount": -5000000},
          {"account_id": "<alice-id>",      "currency": "NGN", "amount":  5000000}
        ]
      }'

curl -s localhost:8080/accounts/<alice-id>/balance
# {"account_id":"<alice-id>","currency":"NGN","balance":5000000}
```

Run the same `POST /transactions` again and you get `200 OK` with
`Idempotent-Replayed: true` and the original transaction. Alice's balance does
not change.

To stop and remove the database, including its data:

```bash
docker compose down -v
```

## Concepts

### Double-entry in one paragraph

Money is never created or destroyed inside the ledger, only moved. Each
**transaction** is a set of **entries**, and each entry changes one
**account**. The entries of a transaction must sum to zero, so every amount
leaving one account arrives in another. Because every transaction sums to
zero, the balances of all accounts together also always sum to zero. This
"trial balance" property makes errors detectable.

### Amounts and signs

Amounts are signed 64-bit integers in the currency's **minor unit**: kobo for
NGN, cents for USD. `5000000` NGN is ₦50,000.00.

| Sign     | Meaning | Effect on the account's balance |
|----------|---------|---------------------------------|
| positive | debit   | increases it                    |
| negative | credit  | decreases it                    |

An account's balance is the sum of all its entries.

### Account kinds

| Kind              | `allow_negative_balance` | Typical use                                         |
|-------------------|--------------------------|-----------------------------------------------------|
| Ordinary account  | `false` (default)        | Customer wallets, fee income, anything that holds money |
| System account    | `true`                   | Settlement or funding accounts that represent money outside the ledger |

A deposit is a transfer from a system account. The system account goes
negative by the amount that has entered the ledger, and that is how the
ledger keeps track of it.

### Worked example

This sequence was run against the service. The balances afterwards are shown
below.

| # | Transaction             | Entries (kobo)                                                 |
|---|-------------------------|----------------------------------------------------------------|
| 1 | Card top-up             | Settlement `−5,000,000`, Alice `+5,000,000`                    |
| 2 | Rent share              | Alice `−1,250,000`, Bob `+1,250,000`                           |
| 3 | Marketplace order 7781  | Alice `−1,000,000`, Bob `+950,000`, Fees `+50,000` (three legs) |
| 4 | Reversal of #2          | Bob `−1,250,000`, Alice `+1,250,000`                           |

| Account         | Balance (kobo) | Balance       |
|-----------------|---------------:|---------------|
| Alice           |      4,000,000 | ₦40,000.00    |
| Bob             |        950,000 | ₦9,500.00     |
| Fees            |         50,000 | ₦500.00       |
| Bank settlement |     −5,000,000 | −₦50,000.00   |
| **Total**       |          **0** |               |

Transaction 3 shows that a transaction may have more than two legs. Transaction
4 shows how mistakes are corrected: #2 is never edited or deleted, it is
offset by a new transaction, so the audit trail keeps both.

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

| Layer                     | Responsibility                                                                  |
|---------------------------|---------------------------------------------------------------------------------|
| `internal/httpapi`        | JSON encoding, `Idempotency-Key` header, error → HTTP status mapping            |
| `internal/grpcapi`        | Protobuf mapping, error → gRPC code mapping                                     |
| `internal/ledger`         | Domain types, validation (including Rule A), the `Service` both transports call |
| `internal/store/postgres` | Atomic posting, row locking, overdraft checks, idempotent replay                |
| PostgreSQL                | Last line of defence: foreign keys, checks, append-only and balance triggers    |

Dependencies point inwards. `internal/ledger` imports neither SQL nor HTTP
packages. It defines a `Store` interface that `internal/store/postgres`
implements, and a `Service` that both transports call. Adding a transport,
such as a message-queue consumer, means writing one more thin adapter over
`Service`.

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

| Table          | Column                   | Type          | Notes                                                   |
|----------------|--------------------------|---------------|---------------------------------------------------------|
| `accounts`     | `id`                     | `UUID`        | Primary key, `gen_random_uuid()`                        |
|                | `name`                   | `TEXT`        | 1–200 characters                                        |
|                | `currency`               | `CHAR(3)`     | ISO 4217, `CHECK (currency ~ '^[A-Z]{3}$')`             |
|                | `allow_negative_balance` | `BOOLEAN`     | Default `false`                                         |
|                | `created_at`             | `TIMESTAMPTZ` | Default `now()`                                         |
| `transactions` | `id`                     | `UUID`        | Primary key                                             |
|                | `idempotency_key`        | `TEXT`        | `UNIQUE`, 1–255 characters                              |
|                | `description`            | `TEXT`        | Up to 1,000 characters (validated in Go)                |
|                | `created_at`             | `TIMESTAMPTZ` | Default `now()`                                         |
| `entries`      | `id`                     | `UUID`        | Primary key                                             |
|                | `transaction_id`         | `UUID`        | FK → `transactions(id)`, indexed                        |
|                | `account_id`, `currency` | `UUID`, `CHAR(3)` | Composite FK → `accounts(id, currency)`; `account_id` indexed |
|                | `amount`                 | `BIGINT`      | Minor units, `CHECK (amount <> 0)`                      |
|                | `created_at`             | `TIMESTAMPTZ` | Default `now()`; identical for all entries of a posting |

The schema is built by six numbered migrations in [`migrations/`](migrations/).

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

Rules A, B and C hold even for SQL run directly against the database, not just
for requests through the service. The test suite attempts each violation with
raw SQL and expects PostgreSQL to refuse it:

```text
ERROR:  entries are append-only: UPDATE is not allowed
ERROR:  transaction 0000…00a2 does not balance: NGN entries sum to -100
ERROR:  insert or update on table "entries" violates foreign key constraint "entries_account_id_currency_fkey"
```

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

Every step runs inside one database transaction. Any error rolls the whole
thing back, so a posting is never partially applied, and a rejected posting
leaves no trace, not even its idempotency key.

## Design decisions

### Balances are computed, not stored

An account's balance is `SUM(entries.amount)`; there is no `balance` column.

- **Single source of truth.** A stored balance is a second copy of the same
  fact. Any bug, partial write or manual fix that updates one without the
  other makes the books silently disagree with themselves. A computed balance
  cannot drift.
- **Simpler writes.** Posting is pure `INSERT`s, with no read-modify-write of a
  balance row.
- **Auditable by construction.** The balance at any past moment is the same
  query with `WHERE created_at <= t`.
- **Indexed.** `entries (account_id)` keeps the sum to an index-backed scan of
  one account's entries.

The trade-off is that read cost grows with an account's entry count. If that
ever matters, the next step is a derived cache, such as periodic balance
snapshots plus the sum of later entries. A cache can always be rebuilt from
`entries`, so it never becomes a second source of truth.

### Concurrency: per-account row locks

With computed balances, two concurrent withdrawals could both read a balance of
₦1,000 and both spend it. Each posting therefore locks the accounts it debits
before reading their balances:

```sql
SELECT id FROM accounts
WHERE id = ANY($1) AND NOT allow_negative_balance
ORDER BY id
FOR NO KEY UPDATE
```

- **Serialised per account, not globally.** Postings that touch different
  accounts run fully in parallel.
- **Deadlock-free ordering.** Locks are always taken in `id` order, so two
  postings never wait on each other in a cycle.
- **`FOR NO KEY UPDATE`, not `FOR UPDATE`.** Inserting an entry makes
  PostgreSQL take a `FOR KEY SHARE` lock on the referenced account to check
  the foreign key. `FOR UPDATE` conflicts with that lock, so a concurrent A→B
  and B→A deadlock on each other's foreign-key checks. `FOR NO KEY UPDATE`
  still serialises postings that debit the same account, but does not block
  foreign-key checks. The test suite reproduces the deadlock when the code is
  changed to `FOR UPDATE`.
- **No lock on accounts allowed to go negative.** Settlement and funding
  accounts take part in a large share of postings. They are never
  balance-checked, so they are never locked and never become a hot row.
- **`READ COMMITTED` is sufficient.** Each statement takes a fresh snapshot, so
  the balance `SUM` that runs after the lock is acquired sees every entry
  committed by earlier lock holders. `SERIALIZABLE` would also be correct, but
  it would push retry loops onto every caller.

### Idempotency

Every posting carries a client-chosen idempotency key, stored under a unique
constraint:

- **Retry**: the same key with the same request returns the original
  transaction unchanged (`200` with `Idempotent-Replayed: true` over REST,
  `replayed: true` over gRPC). Entry order does not matter.
- **Conflict**: the same key with a different request is rejected (`409` /
  `ALREADY_EXISTS`). A key identifies one intended posting, not a slot to
  reuse.
- **Concurrent duplicates**: a second in-flight request with the same key
  blocks on the unique index until the first commits, then replays it. Exactly
  one posting happens.
- **Failures are not recorded**: a rejected posting, such as one with
  insufficient funds, rolls back with its key, so the client may fix the
  situation and retry with the same key.

Clients should generate a fresh key, for example a UUID, per intended posting
and reuse it only when retrying that posting.

### Money is an integer in minor units

Floating-point types cannot represent most decimal fractions exactly and
accumulate rounding errors (`0.1 + 0.2 != 0.3`). Amounts are `int64` minor units
end to end. Sums are overflow-checked during validation, both per currency and
per account, so a crafted request cannot wrap a large debit around into a
credit.

### Entries are append-only

History is never edited. A mistaken posting is corrected by a reversing
transaction, so the full sequence of events stays visible. The triggers
guarantee this even for ad-hoc SQL.

### Currency is denormalised onto entries

`entries.currency` repeats the account's currency on purpose, so that a plain
composite foreign key enforces Rule C declaratively, without a trigger.
`accounts` carries `UNIQUE (id, currency)` as the key's target.

### Validation happens twice

The Go domain layer validates every request first, so clients get precise,
typed errors such as `unbalanced` or `currency_mismatch`. The database
re-checks the same invariants so they hold even if the application has a bug
or someone runs SQL by hand. Each layer has its own job: precise errors from
Go, guarantees from the database.

### Technology choices

| Choice                      | Reason                                                                                         |
|-----------------------------|------------------------------------------------------------------------------------------------|
| PostgreSQL                  | Transactions, row-level locks, deferred constraint triggers and composite foreign keys        |
| `pgx` with hand-written SQL | Locking clauses and `ON CONFLICT` are explicit and reviewable; no ORM hiding the queries      |
| Standard-library router     | Go 1.22+ `ServeMux` supports method and path patterns, so no web framework is needed          |
| goose, embedded migrations  | Plain SQL files, compiled into the binary; tests and CI apply exactly the same files         |
| UUID identifiers            | Can be generated anywhere, and do not leak record counts                                      |
| buf for protobuf            | Linting against the standard API style rules, and reproducible code generation                |

## REST API

Base URL `http://localhost:8080`. Request and response bodies are JSON. Amounts
are integers in minor units. Timestamps are RFC 3339.

| Method | Path                     | Description                                               |
|--------|--------------------------|-----------------------------------------------------------|
| `POST` | `/accounts`              | Create an account                                         |
| `GET`  | `/accounts/{id}`         | Get an account                                            |
| `GET`  | `/accounts/{id}/balance` | Current balance                                           |
| `GET`  | `/accounts/{id}/entries` | Entries, newest first                                     |
| `POST` | `/transactions`          | Post a transaction (requires `Idempotency-Key`)           |
| `GET`  | `/transactions/{id}`     | Get a transaction and its entries                         |
| `GET`  | `/health`                | Liveness                                                  |

### `POST /accounts`

| Field                    | Type    | Required | Notes                                          |
|--------------------------|---------|----------|------------------------------------------------|
| `name`                   | string  | yes      | 1–200 characters, trimmed                      |
| `currency`               | string  | yes      | ISO 4217 code; normalised to upper case        |
| `allow_negative_balance` | boolean | no       | Default `false`                                |

```http
POST /accounts
{"name": "Alice", "currency": "ngn"}
```

```http
HTTP/1.1 201 Created
Location: /accounts/3d896348-12a7-43d3-a5de-e25dc16feac6

{
  "id": "3d896348-12a7-43d3-a5de-e25dc16feac6",
  "name": "Alice",
  "currency": "NGN",
  "allow_negative_balance": false,
  "created_at": "2026-10-12T12:03:12.345367+01:00"
}
```

### `GET /accounts/{id}`

Returns the account object above, or `404 account_not_found`.

### `GET /accounts/{id}/balance`

```json
{"account_id": "3d896348-12a7-43d3-a5de-e25dc16feac6", "currency": "NGN", "balance": 4000000}
```

### `GET /accounts/{id}/entries?limit=N`

Most recent entries first. `limit` defaults to 50; values above 500 are capped
at 500.

```json
{
  "entries": [
    {
      "id": "0661f6f2-9fc7-43e8-bf1e-8016c908b02a",
      "transaction_id": "87b822b5-2b6d-4b35-bdd2-8da3bb1b01c3",
      "account_id": "3d896348-12a7-43d3-a5de-e25dc16feac6",
      "currency": "NGN",
      "amount": 1250000,
      "created_at": "2026-10-12T12:03:31.141422+01:00"
    }
  ]
}
```

### `POST /transactions`

| Header            | Required | Notes                                             |
|-------------------|----------|---------------------------------------------------|
| `Idempotency-Key` | yes      | 1–255 characters; one key per intended posting    |

| Field                  | Type    | Required | Notes                                   |
|------------------------|---------|----------|-----------------------------------------|
| `description`          | string  | no       | Up to 1,000 characters                  |
| `entries`              | array   | yes      | 2–100 entries                           |
| `entries[].account_id` | UUID    | yes      |                                         |
| `entries[].currency`   | string  | yes      | Must match the account's currency       |
| `entries[].amount`     | integer | yes      | Non-zero; per currency, entries sum to 0 |

```http
POST /transactions
Idempotency-Key: rent-2026-10

{
  "description": "Rent share",
  "entries": [
    {"account_id": "3d896348-12a7-43d3-a5de-e25dc16feac6", "currency": "NGN", "amount": -1250000},
    {"account_id": "aec6fabb-12e5-4c4b-9aee-17f737ca21bd", "currency": "NGN", "amount": 1250000}
  ]
}
```

```http
HTTP/1.1 201 Created
Location: /transactions/a227b4a7-a6ea-4cc9-abf1-a7232b7d129a

{
  "id": "a227b4a7-a6ea-4cc9-abf1-a7232b7d129a",
  "idempotency_key": "rent-2026-10",
  "description": "Rent share",
  "entries": [
    {
      "id": "c9e5fbd9-3d8f-4517-85c6-210b373785c6",
      "transaction_id": "a227b4a7-a6ea-4cc9-abf1-a7232b7d129a",
      "account_id": "aec6fabb-12e5-4c4b-9aee-17f737ca21bd",
      "currency": "NGN",
      "amount": 1250000,
      "created_at": "2026-10-12T12:03:23.600969+01:00"
    },
    {
      "id": "6300a13f-347b-4ea0-9e2d-49b1d2ef4003",
      "transaction_id": "a227b4a7-a6ea-4cc9-abf1-a7232b7d129a",
      "account_id": "3d896348-12a7-43d3-a5de-e25dc16feac6",
      "currency": "NGN",
      "amount": -1250000,
      "created_at": "2026-10-12T12:03:23.600969+01:00"
    }
  ],
  "created_at": "2026-10-12T12:03:23.600969+01:00"
}
```

Entries are returned debits first, then by account.

| Outcome                                   | Status            | Headers                                |
|-------------------------------------------|-------------------|----------------------------------------|
| New posting                               | `201 Created`     | `Location`                             |
| Retry of an earlier posting (same body)   | `200 OK`          | `Location`, `Idempotent-Replayed: true` |
| Key already used for a different body     | `409 Conflict`    |                                        |

### `GET /transactions/{id}`

Returns the transaction object above, or `404 transaction_not_found`.

### Errors

All errors share one shape:

```json
{"error": {"code": "insufficient_funds", "message": "insufficient funds: account aec6fabb-… has 950000, needs 99000000"}}
```

| Status | Code                      | When                                                      |
|--------|---------------------------|-----------------------------------------------------------|
| 400    | `invalid_json`            | Malformed JSON, unknown field, or body over 1 MiB         |
| 400    | `invalid_request`         | Path id is not a UUID, or `limit` is not a positive integer |
| 400    | `missing_idempotency_key` | `POST /transactions` without `Idempotency-Key`            |
| 404    | `account_not_found`       | Unknown account id, including inside a posting            |
| 404    | `transaction_not_found`   | Unknown transaction id                                    |
| 409    | `idempotency_conflict`    | Key reused with a different request                       |
| 422    | `invalid_request`         | Field validation failed (name, currency, entry count, …)  |
| 422    | `unbalanced`              | Entries do not sum to zero in some currency               |
| 422    | `insufficient_funds`      | Posting would overdraw an ordinary account                |
| 422    | `currency_mismatch`       | Entry currency differs from its account's currency        |
| 500    | `internal`                | Unexpected failure; details are logged, never returned    |

Unknown JSON fields are rejected rather than ignored, so a typo such as
`"ammount"` fails loudly instead of posting the wrong thing.

## gRPC API

Service `ledger.v1.LedgerService` on port `9090`, defined in
[`proto/ledger/v1/ledger.proto`](proto/ledger/v1/ledger.proto). It offers the
same operations as the REST API:

```text
rpc CreateAccount   (CreateAccountRequest)   returns (CreateAccountResponse);
rpc GetAccount      (GetAccountRequest)      returns (GetAccountResponse);
rpc GetBalance      (GetBalanceRequest)      returns (GetBalanceResponse);
rpc ListEntries     (ListEntriesRequest)     returns (ListEntriesResponse);
rpc PostTransaction (PostTransactionRequest) returns (PostTransactionResponse);
rpc GetTransaction  (GetTransactionRequest)  returns (GetTransactionResponse);
```

The idempotency key is the `idempotency_key` request field, and
`PostTransactionResponse.replayed` is `true` for a retry. The server also
exposes the standard **health** service (`grpc.health.v1.Health`) and **server
reflection**, so tools such as [grpcurl](https://github.com/fullstorydev/grpcurl)
work without the `.proto` file:

```console
$ grpcurl -plaintext localhost:9090 list
grpc.health.v1.Health
grpc.reflection.v1.ServerReflection
grpc.reflection.v1alpha.ServerReflection
ledger.v1.LedgerService

$ grpcurl -plaintext -d '{"account_id":"aec6fabb-12e5-4c4b-9aee-17f737ca21bd"}' \
    localhost:9090 ledger.v1.LedgerService/GetBalance
{
  "accountId": "aec6fabb-12e5-4c4b-9aee-17f737ca21bd",
  "currency": "NGN",
  "balance": "950000"
}

$ grpcurl -plaintext -d '{
    "idempotency_key": "grpc-big",
    "entries": [
      {"account_id": "aec6fabb-12e5-4c4b-9aee-17f737ca21bd", "currency": "NGN", "amount": -99000000},
      {"account_id": "3d896348-12a7-43d3-a5de-e25dc16feac6", "currency": "NGN", "amount": 99000000}
    ]}' localhost:9090 ledger.v1.LedgerService/PostTransaction
ERROR:
  Code: FailedPrecondition
  Message: insufficient funds: account aec6fabb-12e5-4c4b-9aee-17f737ca21bd has 650000, needs 99000000

$ grpcurl -plaintext localhost:9090 grpc.health.v1.Health/Check
{
  "status": "SERVING"
}
```

In protobuf's JSON form, as printed by grpcurl, `int64` values appear as
strings (`"950000"`). On the wire they are ordinary 64-bit integers.

| Condition                             | gRPC code             |
|---------------------------------------|-----------------------|
| Invalid input, unbalanced entries     | `INVALID_ARGUMENT`    |
| Unknown account or transaction        | `NOT_FOUND`           |
| Insufficient funds, currency mismatch | `FAILED_PRECONDITION` |
| Idempotency key reused                | `ALREADY_EXISTS`      |
| Unexpected failure                    | `INTERNAL`            |

## Configuration

All configuration is through environment variables:

| Variable                   | Default                                                               | Used by                 |
|----------------------------|-----------------------------------------------------------------------|-------------------------|
| `DATABASE_URL`             | `postgres://ledger:ledger_dev@127.0.0.1:55432/ledger?sslmode=disable` | `cmd/ledger`, `cmd/migrate` |
| `HTTP_ADDR`                | `:8080`                                                               | `cmd/ledger`            |
| `GRPC_ADDR`                | `:9090`                                                               | `cmd/ledger`            |
| `LEDGER_TEST_DATABASE_URL` | unset                                                                 | tests (see [Testing](#testing)) |

The service logs structured JSON to stdout: lifecycle events, one line per
HTTP request, and the details of any internal error:

```json
{"time":"2026-10-12T12:03:08.5590144+01:00","level":"INFO","msg":"grpc listening","addr":":9090"}
```

It applies HTTP read, write and idle timeouts, and on `SIGINT` or `SIGTERM` it
stops accepting work and gives in-flight requests up to 10 seconds to finish on
both servers.

### Migrations

```bash
go run ./cmd/migrate up       # apply all pending migrations
go run ./cmd/migrate status   # list applied migrations
go run ./cmd/migrate down     # roll back the latest migration
go run ./cmd/migrate reset    # roll back everything
```

## Testing

```bash
docker compose up -d --wait
go test ./...
```

Each test that needs a database gets its own freshly created and migrated
PostgreSQL database, which is dropped when the test ends. Tests run against the
real schema, constraints and triggers, with no mocks, and they can run in
parallel without interfering.

| Suite                       | Covers                                                                                                    |
|-----------------------------|-----------------------------------------------------------------------------------------------------------|
| `internal/ledger`           | Validation table tests: balancing, currencies, overflow, idempotent request matching                      |
| `internal/store/postgres`   | Posting, balances, overdrafts, not-found cases, and database guards exercised with raw SQL                |
| ↳ `concurrency_test.go`     | 50 concurrent ₦100 withdrawals from ₦1,000 (exactly 10 succeed); 50 opposing A↔B transfers (no deadlock)   |
| ↳ `idempotency_test.go`     | Sequential retry, reordered retry, key-reuse conflict, 20 concurrent identical requests post exactly once |
| `internal/httpapi`          | End-to-end REST flow, replay headers, every error status                                                  |
| `internal/grpcapi`          | End-to-end gRPC flow over an in-memory connection (`bufconn`), status codes                               |

The concurrency tests were checked against deliberately broken builds:

| Sabotage                                     | Result                                                       |
|----------------------------------------------|--------------------------------------------------------------|
| Row lock removed                             | 19 of 50 withdrawals succeed; the account ends at −₦900       |
| `FOR UPDATE` instead of `FOR NO KEY UPDATE`  | `ERROR: deadlock detected (SQLSTATE 40P01)`                  |

**Choosing the database.** Tests use `LEDGER_TEST_DATABASE_URL` when it is set,
and an unreachable database then **fails** the run, as in CI. When it is unset,
tests fall back to the docker-compose database and **skip** if it is not
running.

**Race detector.** `go test -race` needs a C toolchain. Without one (for
example on Windows), run the tests in the official Go image on the compose
network:

```bash
docker run --rm --network ledger-service_default -v "$PWD:/src" -w /src \
  -e LEDGER_TEST_DATABASE_URL="postgres://ledger:ledger_dev@db:5432/ledger?sslmode=disable" \
  golang:1.26 go test -race ./...
```

## Continuous integration

[GitHub Actions](.github/workflows/ci.yml) runs three jobs on every push to
`main` and every pull request:

| Job     | Steps                                                                                                  |
|---------|--------------------------------------------------------------------------------------------------------|
| `lint`  | `gofmt` check, `go vet`, `staticcheck`                                                                 |
| `test`  | PostgreSQL 16 service container; migrations applied, fully rolled back and re-applied; `go test -race` |
| `proto` | `buf lint`; regenerate code and fail if it differs from what is committed                              |

Tool versions are pinned, so a new release of a linter or code generator
cannot change the result of an old commit.

## Development

### Changing the gRPC contract

Generated code is committed under `gen/`, so building the project needs no
protobuf tools. After editing the `.proto` file, regenerate it with the same
pinned versions CI uses:

```bash
go install github.com/bufbuild/buf/cmd/buf@v1.73.0
go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.2
buf lint && buf generate
```

### Adding a migration

Create the next numbered file in `migrations/` with `-- +goose Up` and
`-- +goose Down` sections. Wrap function bodies that contain semicolons in
`-- +goose StatementBegin` / `-- +goose StatementEnd`. Never edit a migration
that has already been applied anywhere; add a new one instead. CI checks that
every migration rolls back cleanly.

### Checks to run before pushing

```bash
gofmt -l .                                              # should print nothing
go vet ./...
go run honnef.co/go/tools/cmd/staticcheck@2026.2.1 ./...
go test ./...
```

## Troubleshooting

**`password authentication failed for user "ledger"`.** Another PostgreSQL is
answering instead of the container. This is common with PostgreSQL installed
natively or in WSL, which listen on `5432` and `5433`. The container is
published on `127.0.0.1:55432` for this reason. Check `DATABASE_URL` points
there, and use `127.0.0.1` rather than `localhost`, which can resolve to a
different server over IPv6.

**Changed `POSTGRES_PASSWORD` but nothing happened.** The official image only
reads it when the data volume is first created. Run `docker compose down -v`
to remove the volume, then start again.

**Can't delete test data from `entries`.** That is the append-only trigger
working. To start over in development, run `go run ./cmd/migrate reset`
followed by `go run ./cmd/migrate up`.

**Tests are skipped.** The database is not running and
`LEDGER_TEST_DATABASE_URL` is unset. Run `docker compose up -d --wait`.

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
.github/workflows/        CI
```

## Dependencies

| Module                         | Purpose                                          |
|--------------------------------|--------------------------------------------------|
| `github.com/jackc/pgx/v5`      | PostgreSQL driver and connection pool            |
| `github.com/pressly/goose/v3`  | Versioned SQL migrations from embedded files     |
| `github.com/google/uuid`       | UUID identifiers                                 |
| `google.golang.org/grpc`       | gRPC server, health and reflection services      |
| `google.golang.org/protobuf`   | Protobuf runtime                                 |
| `golang.org/x/sync`            | `errgroup` for running and stopping both servers |

## Limitations and roadmap

- **Balance reads scale with entry count.** Snapshots plus a delta sum would
  bound read cost for very active accounts.
- **Contention on hot debited accounts.** Postings that debit the same
  ordinary account are serialised by design. Very high-volume debit accounts
  would need sharded sub-accounts or batched posting.
- **Entry listing is not paginated.** `ListEntries` returns the latest N
  entries; a keyset cursor on `(created_at, id)` would allow paging through
  full history.
- **No authentication or authorisation.** Callers are trusted. Production use
  needs authentication, per-account access control, and TLS on both ports.
- **JSON amounts are numbers.** Values above 2⁵³ lose precision in JavaScript
  clients, which should parse amounts as `BigInt` or strings.
- **Triggers can be bypassed by a superuser.** In production the service
  should connect as a role granted only `SELECT` and `INSERT` on `entries`.
- **Single currency per account, no FX.** Cross-currency movements would be
  modelled as two balanced legs through FX accounts, which the
  per-currency balancing rule already supports.
