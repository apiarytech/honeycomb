# Subscriptions and the change feed

honeycomb has two ways to follow changes. They answer different questions:

| | Subscriptions | Change feed |
|---|---|---|
| Question | "What is the tag now?" | "What happened, in order?" |
| Delivers | the newest state of one tag | every change of every (or the named) tags |
| A slow reader | gets the latest state; intermediate states are replaced | gets every change while the buffer holds them; a gap is reported if it falls behind |
| Typical user | HMI, display, a program reacting to the current value | sequence-of-events recorder, alarm engine, historian, replication |
| Over the network | no | `POST /changes` (long poll) |

## Subscriptions

```go
ch, id, err := db.SubscribeToTag("Level")
defer db.UnsubscribeFromTag("Level", id) // closes ch
var last uint64
for update := range ch {
    if update.Sequence > last+1 {
        // update.Sequence-last-1 updates were replaced before we took them
    }
    last = update.Sequence
}
```

- The channel holds **one** update. A newer update replaces one not yet
  received, so a reader that falls behind gets the current value, never a
  stale one.
- Updates of a tag arrive **in order**, even with concurrent writers;
  `Sequence` rises by one per update, so a gap counts the replaced updates.
- Each update is a lock-free copy of the tag, with value, quality,
  timestamp and direct address. Its `Value` may share memory with the
  database: treat it as read-only, and read the tag afresh if you need a
  consistent field of a UDT.
- Writes never block on subscribers.
- A field or element write notifies the subscribers of the whole tag.
- A remote alias cannot be subscribed to: subscribe on the database that
  owns the tag.

## Change feed

Each database keeps a ring buffer of changes, `DefaultChangeFeedCapacity`
(10,000) by default (`SetChangeFeedCapacity`). A change is recorded at the
write, before subscriptions replace anything, with a sequence number, the
tag's name, value, quality and timestamp.

```go
pos, _ := db.Changes(ctx, honeycomb.ChangesRequest{}) // a new reader: start from now
// read the current values (ReadTags) to start from, then:
for {
    batch, err := db.Changes(ctx, honeycomb.ChangesRequest{
        Since: pos.Next, Epoch: pos.Epoch, Wait: 30 * time.Second, Max: 1000,
        Names: []string{"Pump1.Tripped", "Tank1.Level"}, // optional filter
    })
    if err != nil {
        break
    }
    if batch.Gap {
        // changes were lost: the reader fell behind the buffer, or the
        // database restarted (a new Epoch). Read current values again.
    }
    for _, c := range batch.Changes {
        // c.Seq, c.Name, c.Value, c.Quality, c.Timestamp
    }
    pos = batch
}
```

- **Nothing is skipped while the buffer holds**: a pulse shorter than any
  poll interval shows as both edges, each with its own timestamp.
- **Gaps are reported**, never hidden: falling behind the buffer, or a new
  `Epoch` after the database restarted, sets `Gap`.
- **Writes that change nothing are not recorded**; a quality-only change is.
- **Each change keeps its own copy** of array and UDT values.
- **Changes are per top-level tag**: a write to `Arr[1]` records the whole
  `Arr`.
- `Wait` makes the call a long poll: it returns at once if there are newer
  changes, or when one arrives, or after `Wait`.

`TagDatabase` and `NetworkDatabaseClient` both implement `ChangeSource`.
The network form is described in [Network API](network.md#post-changes).
