# Durable tag storage

`TagDatabase` stays an in-memory database: the PLC scan cycle reads and writes
memory only. A `TagStore` makes tags survive power cycles, and a `Persister`
keeps the two in sync.

```
               PLC program / HTTP server / subscribers
                              │  Get/SetTagValue (memory speed)
                              ▼
┌───────────────────────── TagDatabase ─────────────────────────┐
│ every write path calls markChanged / markRemoved ──► dirty set │
└────────────────────────────────────────────────────────────────┘
                              │ Restore ▲        │ Flush every FlushInterval,
                              │ (power-up)       │ SaveAll on Close (shutdown)
                              ▼                  ▼
                 Persister ── honeycomb.TagStore interface ──
                              │
            ┌─────────────────┼──────────────────────┐
            ▼                 ▼                      ▼
     store/sqlstore     (future) store/redis   (future) store/file
   database/sql + Dialect
   ├─ sqlite     (modernc.org/sqlite, mattn/go-sqlite3)
   ├─ postgres   (pgx, lib/pq)
   ├─ cockroachdb (pgx, lib/pq; set Options.Dialect)
   ├─ mysql      (go-sql-driver/mysql, parseTime=true)
   └─ sqlserver  (microsoft/go-mssqldb)
```

## Lifecycle

| Phase     | Call                          | What happens                                                        |
|-----------|-------------------------------|---------------------------------------------------------------------|
| Power-up  | `sqlstore.Open(..., AutoSetup)` | Connects and applies pending schema migrations.                   |
|           | `db.AttachStore(store, opts)` | Hooks the database's write paths.                                   |
|           | `persister.Restore(ctx)`      | Loads saved values into configured tags (and optionally definitions/forces). |
|           | `persister.Start()`           | Starts the write-behind loop.                                       |
| Runtime   | `db.SetTagValue(...)`         | Marks the tag dirty; never touches the database.                   |
|           | every `FlushInterval`         | Writes each dirty tag once, in batches, in one transaction.         |
| Shutdown  | `persister.Close(ctx)`        | Stops the loop, saves every in-scope tag, closes the store.         |

A failed flush keeps its tags queued and retries on the next interval, so a
database outage never stalls the scan cycle.

## Folder layout

```
persistence.go                 TagStore interface, StoredTag, Persister (root package)
store/
  README.md                    this file
  sqlstore/
    sqlstore.go                Store: Open/New, Load/Save/DeleteTags, Purge, Close
    dialect.go                 Dialect interface, registry, the five built-in dialects
    migrate.go                 Setup (migrate up) / Teardown (migrate down) runner
    migrations/
      sqlite/      0001_create_tags.*.sql  0002_add_quality.*.sql
      postgres/    0001_create_tags.*.sql  0002_add_quality.*.sql
      cockroachdb/ 0001_create_tags.*.sql  0002_add_quality.*.sql
      mysql/       0001_create_tags.*.sql  0002_add_quality.*.sql
      sqlserver/   0001_create_tags.*.sql  0002_add_quality.*.sql
```

The scripts are embedded in the binary with `go:embed`, so a deployed PLC
runtime needs no SQL files on disk.

## Setup and clean-up

| Operation                 | Effect                                                     |
|---------------------------|------------------------------------------------------------|
| `store.Setup(ctx)`        | Applies pending `*.up.sql` in version order; idempotent.   |
| `store.Purge(ctx)`        | Deletes this instance's tags; keeps the schema.            |
| `store.Teardown(ctx)`     | Runs `*.down.sql` in reverse, drops all honeycomb tables.  |

Applied versions are recorded in `honeycomb_schema_migrations`. To change the
schema, add `0002_<name>.up.sql` and `0002_<name>.down.sql` to **every** dialect
folder; never edit a released migration. Scripts are split on lines ending in
`;`, so keep one statement per `;`-terminated block.

## Adding a database

1. Implement `sqlstore.Dialect` (placeholder style, upsert syntax, migrations-table DDL).
2. Provide its migration scripts as an `fs.FS` from `Migrations()`.
3. Call `sqlstore.Register(myDialect{}, "driver-name")`.

A non-SQL backend implements `honeycomb.TagStore` directly in its own package.

## Schema

One table, `honeycomb_tags`, keyed by `(instance_id, tag_name)` so several PLCs
can share one server database. Values are stored as JSON (`JSONB` on
PostgreSQL and CockroachDB, `JSON` on MySQL, `NVARCHAR(MAX)` on SQL Server, `TEXT` on SQLite),
which handles primitives, arrays and UDTs uniformly. The stored value is always
the tag's actual value; force values are stored separately and only restored
when `PersistOptions.RestoreForces` is set.

The `quality` column (0 Unknown, 1 Good, 2 Uncertain, 3 Bad) holds the value's
quality when it was saved. `Restore` brings a Good value back as Uncertain,
because the process may have changed while the runtime was down; Uncertain, Bad
and Unknown are restored unchanged. Rows saved before migration `0002` default
to Uncertain. The SQLite rollback uses `DROP COLUMN`, which needs SQLite 3.35+.

## CockroachDB

CockroachDB speaks the PostgreSQL wire protocol, so it uses the `pgx` or
`lib/pq` driver. Those driver names resolve to the `postgres` dialect, so select
the CockroachDB dialect explicitly:

```go
import _ "github.com/jackc/pgx/v5/stdlib"

d, _ := sqlstore.Lookup("cockroachdb") // also "cockroach", "crdb"
store, err := sqlstore.Open(ctx, "pgx",
	"postgresql://root@localhost:26257/plc?sslmode=disable",
	sqlstore.Options{Dialect: d, AutoSetup: true})
```

Saves use CockroachDB's native `UPSERT INTO`, which writes a row without
reading it first. CockroachDB runs transactions at `SERIALIZABLE` and can abort
one with a retryable error (SQLSTATE `40001`) under contention. A failed flush
keeps its tags queued, so the persister retries on the next interval.
