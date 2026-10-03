# Persistence

While the program runs, memory is the system of record. Persistence keeps
retained values across restarts, the way a PLC's retain memory does:
values are restored at power-up, written behind the scan at runtime, and
saved in a final snapshot at shutdown.

## The lifecycle

```go
store, err := sqlstore.Open(ctx, "sqlite", "file:plc.db", sqlstore.Options{AutoSetup: true})
// configure the tags first (AddTag), then:
p, err := db.AttachStore(store, honeycomb.PersistOptions{})
err = p.Restore(ctx) // power-up: load persisted values
p.Start()            // runtime: write changed tags every FlushInterval
// ...
err = p.Close(ctx)   // shutdown: final snapshot, then close the store
```

| Option (`PersistOptions`) | Default | Meaning |
|---|---|---|
| `Scope` | `PersistRetainOnly` | Only `Retain` tags, or `PersistAll` |
| `FlushInterval` | 1 s | How often changed tags are written; a tag written every scan is stored at most once per interval |
| `BatchSize` | 500 | Tags per `SaveTags` call |
| `RestoreDefinitions` | false | Create tags found in the store but not configured; otherwise only values of configured tags are restored |
| `RestoreForces` | false | Re-apply the force state saved at shutdown |
| `OnError` | discard | Receives errors from background flushes |

## What is restored

- A value saved as **Good is restored as Uncertain**: the process may have
  moved while the program was stopped. A program or driver that writes it
  makes it Good again.
- **Constant** tags and **remote aliases** keep their configured values.
- The timestamp, description, alias, direct address and force state (with
  `RestoreForces`) come back with the value.
- A tag whose write fails stays queued and is retried on the next flush.
- `Flush(ctx)` writes now; `SaveAll(ctx)` writes every in-scope tag
  whether it changed or not.

## TagStore

Any durable backend implements:

```go
type TagStore interface {
    LoadTags(ctx context.Context) ([]StoredTag, error)
    SaveTags(ctx context.Context, tags []StoredTag) error // atomically
    DeleteTags(ctx context.Context, names []string) error
    Close() error
}
```

`StoredTag` is storage-neutral: values, `TypeInfo` and force values are
JSON, so primitives, arrays and UDTs fit one column.

## sqlstore

`store/sqlstore` implements `TagStore` on `database/sql`. It imports no
driver: the application imports the one it needs and sqlstore picks the
dialect from the driver name.

| Database | Driver name | Notes |
|---|---|---|
| SQLite | `sqlite` | e.g. `modernc.org/sqlite` (pure Go) |
| PostgreSQL | `postgres` | |
| CockroachDB | `cockroachdb` | native `UPSERT` |
| MySQL / MariaDB | `mysql` | the DSN must include `parseTime=true` |
| SQL Server | `sqlserver` | |

```go
import _ "modernc.org/sqlite"

store, err := sqlstore.Open(ctx, "sqlite", "file:plc.db", sqlstore.Options{
    InstanceID: "press-line-1", // several databases can share one SQL database
    AutoSetup:  true,           // create or migrate the schema
})
// or wrap a pool you own: sqlstore.New(ctx, db, opts)
```

- **One table**, `honeycomb_tags`, keyed by `(instance_id, tag_name)`.
- **Versioned migrations** per dialect (`NNNN_name.up.sql` /
  `.down.sql`), applied by `Setup` and rolled back by `Teardown`, recorded
  in a migrations table. Other modules reuse the runner and the dialects
  for their own tables (`LoadMigrations`, `Setup` with a dialect whose
  `Migrations()` returns their scripts).
- `Purge` deletes one instance's tags and keeps the schema.
- `Register(dialect, aliases...)` adds a database.

## Tag files

`WriteTagsToFile(path)` writes every `Retain` tag as one JSON line (name,
`TypeInfo`, value, quality, timestamp), sorted by line so the file diffs
well. `ReadTagsFromFile(path)` reads such a file back into tags that already
exist. They suit snapshots and tests; for retained values at runtime use a
`TagStore`, which writes only what changed. The file is written with mode
0666 (before the umask): keep it in a directory only the service can read.
