# Network API

A `TagDatabase` can be served over HTTPS, and read or written from another
process with `NetworkDatabaseClient`.

## Server

```go
srv, err := honeycomb.NewServer(db, honeycomb.ServerOptions{
    Addr:   "127.0.0.1:8443",
    Tokens: []string{os.Getenv("HONEYCOMB_TOKEN")},
})
honeycomb.Serve(ctx, srv, "server.crt", "server.key", nil) // until ctx is done
```

| `ServerOptions` | Meaning |
|---|---|
| `Addr` | Listen address; required |
| `Tokens` | Valid Bearer tokens, compared in constant time |
| `Authorize` | Decides every request instead of `Tokens`: `func(r, access) (subject, error)`; return `ErrForbidden` for 403, any other error for 401 |
| `ReadOnly` | Refuse every write with 403 |
| `OnWrite` | Called after each successful write with the caller's subject and the tag, e.g. to journal it |
| `MaxBodyBytes` | Bound on a write's body (default 1 MiB) |
| `TLSConfig` | TLS settings; nil uses Go's defaults |

The server always uses TLS and sets read, write and idle timeouts (the
write timeout leaves room for the longest change-feed wait). `StartServer`
is the older one-call form: every interface, Bearer tokens only.

Every request needs authentication. `PUT` is a write (`AccessWrite`);
`GET` and the `POST` reads below are reads (`AccessRead`).

## Endpoints

### GET /tags/{path}

Reads one tag, field, element or direct address.

```json
{"value": 42.5, "quality": 1, "timestamp": "2026-10-03T08:00:00Z"}
```

404 if it does not exist. `timestamp` is absent until the first write.

### PUT /tags/{path}

Writes a value, optionally with quality and timestamp, or a quality alone.

```json
{"value": 42.5}
{"value": 42.5, "quality": 2, "timestamp": "2026-10-03T08:00:00Z"}
{"quality": 3}
```

- `quality` defaults to Good; `timestamp` (RFC 3339) defaults to now.
- A whole-tag write decodes `value` into the tag's own type, so JSON
  numbers become the right IEC type; a UDT takes a JSON object.
- 400 for a body that does not fit the type, 403 on a read-only server,
  404 for an unknown tag, 413 for a body too large.

### GET /tags

Lists every tag (name, type, value, quality, ...). With names it is a batch
read instead: `GET /tags?names=Temp,Lid` or repeated `name=` parameters.

### POST /tags

A batch read for lists too long for a URL, at most `MaxReadTags` (10,000)
names:

```json
{"names": ["Temp", "Lid", "Motors[1]"]}
```

Reply, for `GET` with names too:

```json
{"tags": {
  "Temp": {"value": 42.5, "quality": 1, "timestamp": "…"},
  "Nope": {"quality": 3, "error": "…"}
}}
```

### POST /changes

Reads the change feed, long-polled (see [events](events.md#change-feed)):

```json
{"since": 41, "epoch": "…", "names": ["Pump1.Tripped"], "wait_ms": 30000, "max": 1000}
```

Reply:

```json
{"epoch": "…", "next": 42, "gap": false,
 "changes": [{"seq": 42, "name": "Pump1.Tripped", "value": true, "quality": 1, "timestamp": "…"}]}
```

The server answers at once when there are newer changes, or holds the
request until one arrives, at most `MaxChangesWait` (one minute).

## Client

```go
client := &honeycomb.NetworkDatabaseClient{
    RemoteAddress: "https://plc1:8443",
    Client:        &http.Client{Timeout: 10 * time.Second, Transport: tlsTransport},
    BearerToken:   token,
}
readings, err := client.ReadTags(ctx, []string{"Temp", "Lid"})
batch, err := client.Changes(ctx, honeycomb.ChangesRequest{Wait: 30 * time.Second})
```

- `ReadTags` and `Changes` return `ErrReadTagsUnsupported` and
  `ErrChangesUnsupported` for an older server, so a caller can fall back to
  single reads and polling.
- An unreachable server reads as `QualityBad`.
- Values from the network are JSON values (float64, bool, string, map):
  the client does not know the remote types.

## Remote aliases

A local database can expose a remote tag under a local name:

```go
local.RegisterDatabase("plc1", client) // or another *TagDatabase in process
local.AddTag(&honeycomb.Tag{
    Name:        "Line.Pressure",
    RemoteAlias: &honeycomb.RemoteAliasInfo{DBID: "plc1", TagName: "Press1.Pressure"},
})
v, _ := local.GetTagValue("Line.Pressure")            // reads plc1's tag
local.SetTagValue("Line.Pressure", plc.REAL(3.5))     // writes it
```

- Reads, writes, quality and `ReadTag` go to the remote database; an alias
  chain is followed up to ten hops.
- An alias cannot be subscribed to, and its timestamp lives remotely; use
  the remote database's change feed (`Changes` on the client) to follow it.

## Security notes

- Serve on a specific address (`Addr`), not every interface, unless the
  network is trusted.
- Prefer `Authorize` with per-caller identities over shared tokens, and
  `OnWrite` to journal writes.
- Use `ReadOnly` for servers that only publish values.
- The server logs every request with the standard `log` package.
