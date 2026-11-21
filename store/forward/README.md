# Store-and-forward (`store/forward`)

Durable delivery of a `TagDatabase`'s changes to databases that may be
unreachable for a while: a plant historian over a WAN, a cloud database over
LTE, a server being patched.

```
TagDatabase ──Changes──► Collector ──► Spool (local SQLite) ──► Forwarder ──► Sink
 (or a NetworkDatabaseClient)          rows + position in        one per sink:   SQLSink (5 dialects)
                                        one transaction          retry, backoff  or your own
```

- **The collector only writes to local disk.** It follows the change feed and
  appends each change to the spool in the same transaction as its position,
  so it keeps up with the feed however long the network is down, and after a
  crash or power cut it carries on exactly where the spool ends.
- **Every change, or a deadband.** With no `Deadbands`, every change is kept.
  A deadband keeps a numeric change only when it moves more than `Absolute`
  or `Percent` from the last value kept; a change of quality, and any change
  of a non-numeric value, is always kept. The first rule whose pattern
  matches a tag applies; a rule with no band exempts the tags it matches.
- **Gaps are recorded, never hidden.** If the source restarted (a new epoch)
  or the collector fell more than the feed's buffer behind, it records a
  `gap` and then each tag's current value as a `snapshot`.
- **Forwarding is in order and at least once.** Each forwarder sends batches
  in spool order and confirms a batch only after its sink has stored it.
  While the sink fails, it retries the same batch, backing off from 1 s to
  1 min with jitter; once the sink is back, it sends the backlog at full
  speed. Each send is bounded (`SendTimeout`, 30 s), so a link that drops
  without a reset (a pulled cable) is noticed instead of holding a
  connection. While changes trickle in, the forwarder waits `Linger`
  (50 ms) between sends so they go in groups. Sinks must store a repeated
  batch once: `SQLSink` upserts on (source, epoch, seq, kind, tag).
- **A full spool drops the oldest records** (`MaxBytes`, default 1 GiB;
  `MaxRows`; `MaxAge`) down to 90 % of the limit. Every sink that had not
  received them gets a `lost` row with their count and time range, the
  `Dropped` status tag counts them, and `OnError` is told.
- **Redundant pairs:** each node runs its own service with its own spool and
  forwards under its own `SourceID`, so the history keeps both nodes' views
  and neither depends on the other.

## Use

```go
import (
    _ "modernc.org/sqlite"           // the spool's driver
    _ "github.com/jackc/pgx/v5/stdlib" // the plant database's driver

    "github.com/apiarytech/honeycomb/store/forward"
    "github.com/apiarytech/honeycomb/store/sqlstore"
)

plant, _ := sql.Open("pgx", dsn)          // not contacted until the first send
pg, _ := sqlstore.Lookup("postgres")
sink, _ := forward.NewSQLSink(plant, forward.SQLSinkOptions{
    Name: "plant", SourceID: "unit-CM5004000", Dialect: pg,
})
svc, err := forward.Open(ctx, forward.Options{
    Source:    db, Reader: db,            // a *TagDatabase, or a NetworkDatabaseClient
    SpoolPath: "/var/lib/honeycomb/spool.db",
    Sinks:     []forward.Sink{sink},
    Deadbands: []forward.Deadband{{Tags: "Tank*.Level", Absolute: 0.5}},
    Status:    db,                        // SF_Backlog, SF_OldestAgeS, SF_Online, SF_Dropped, SF_LastError
    OnError:   func(err error) { log.Print(err) },
})
defer svc.Close(ctx) // stops collecting; sends what it can until ctx ends; the rest waits in the spool
```

`Spool`, `Collector` and `Forwarder` can also be used on their own: a spool
holds any records (`Kind`, `Time`, `Payload`), and any `Sink` can receive
them.

## Status tags

With `Options.Status`, the service keeps these tags in that database
(prefix `SF_` by default), so a PLC program, an HMI or an alarm can watch
them like any other tag. The collector never collects them.

| Tag             | Type   | Meaning                                                     |
|-----------------|--------|-------------------------------------------------------------|
| `SF_Backlog`    | LINT   | records the slowest sink has not stored yet                 |
| `SF_OldestAgeS` | LREAL  | age, in seconds, of the oldest of them                      |
| `SF_Online`     | BOOL   | every sink's last send succeeded                            |
| `SF_Dropped`    | LINT   | records dropped from a full spool since it was made         |
| `SF_LastError`  | STRING | the last error                                              |

## History table

`SQLSink` creates `honeycomb_history` on its first send (its versions are kept
in `honeycomb_history_migrations`, apart from `honeycomb_tags`'s), for SQLite,
PostgreSQL, CockroachDB, MySQL/MariaDB and SQL Server:

| Column        | Meaning                                                            |
|---------------|--------------------------------------------------------------------|
| `source_id`   | the forwarding database and node                                   |
| `epoch`, `seq`| the change's place in its source's feed                            |
| `kind`        | `change`, `snapshot`, `gap` or `lost`                              |
| `tag_name`    | empty for `gap` and `lost`                                         |
| `tag_value`   | JSON; NULL without a value                                         |
| `quality`     | honeycomb's quality                                                |
| `value_time`  | the device's time of the change                                    |
| `reason`      | why a gap or a loss happened                                       |
| `recorded_at` | when the row was written                                           |

`SQLSink.History` reads one tag's rows for a period, with the gaps and losses
of that period.

## Wear and power loss

The spool uses WAL with `synchronous=FULL`: a committed append survives a power
cut. The collector commits in groups (`CommitEvery`, 250 ms by default, after a
poll that did not fill a batch), and saves a position that has no records to
keep (every change inside a deadband) at most every 10 s.
