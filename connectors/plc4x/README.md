# honeycomb PLC4X connector

Exchanges field-device data with a honeycomb `TagDatabase` using
[Apache PLC4X](https://plc4x.apache.org) (Go). PLC4X supplies the protocol
drivers: S7, Modbus TCP/RTU/ASCII, EtherNet/IP, Logix, ADS, OPC UA, BACnet/IP,
KNXnet/IP, C-Bus, Firmata, IEC 60870-5-104, SLMP and UMAS.

```go
import "github.com/apiarytech/honeycomb/connectors/plc4x"

connector, err := plc4x.New(db, []plc4x.Connection{{
    Name:     "press1",
    URL:      "modbus-tcp://10.0.0.5:502",
    Interval: 100 * time.Millisecond,
    Bindings: []plc4x.Binding{
        {Tag: "Press1.Pressure", Address: "holding-register:1:REAL"},
        {Tag: "Press1.Cycles",   Address: "holding-register:3:DINT"},
        {Tag: "Press1.Valve",    Address: "holding-register:5:INT", Direction: plc4x.Output},
    },
}}, plc4x.WithDiagnostics("PLC4X."))
go connector.Run(ctx)            // runs until ctx is cancelled
status, _ := connector.Status("press1")
```

Each bound tag must already exist in the database with a value. The Go type
of that value (`plc.DINT`, `plc.REAL`, `[]plc.INT`, a UDT, ...) selects the
conversion to and from the PLC4X value. A failing device reconnects with
exponential backoff (0.5 s to 30 s) without affecting other connections.

## Directions

| Direction       | Data flow       | Behaviour |
|-----------------|-----------------|-----------|
| `Input` (default) | device → tag  | Read every `Interval`, or by subscription (see Modes). |
| `Output`        | tag → device    | Written when the tag's value changes, and once after every (re)connect so the device holds the commanded value. A forced tag writes its force value. |
| `InOut`         | both            | Device changes are read; tag changes are written. After a reconnect the device's value is read first. |

Outputs and InOut tags must be top-level tags: the connector subscribes to them
to see changes. Each output is written in its own request, because some drivers
(Modbus) accept only one tag per write.

**Echo suppression.** The connector remembers what each device holds. A value
read from the device is never written back, writing the same value twice is
skipped, and a quality-only change writes nothing. For `InOut`, a tag change
waiting to be written is not overwritten by a read that arrives first. A change
made in honeycomb and on the device in the same instant is resolved
last-writer-wins.

## Modes

| `Connection.Mode` | Inputs arrive by |
|-------------------|------------------|
| `Poll` (default)  | A read every `Interval`. |
| `ChangeOfState`   | A PLC4X subscription; the device reports each change. |
| `Cyclic`          | A PLC4X subscription; the device reports every `Interval`. |

Not every driver can subscribe (Modbus cannot). Such a connection polls instead,
reports `driver cannot subscribe; polling every …` once per connect, and shows
`Status.Subscribed == false`. Outputs are written in every mode.

## Quality

Input and InOut tags carry the quality of the last exchange:

| Event | Quality |
|-------|---------|
| Value read or reported with response code `OK` | Good |
| Response code `REMOTE_BUSY`, `RESPONSE_PENDING`, `REQUEST_TIMEOUT`, or a read the device did not answer | Uncertain (the last value is kept) |
| Any other response code, a value that cannot be converted, or a lost connection | Bad (the last value is kept) |

Output tags belong to the program, so the connector never changes their quality.

## UDTs

A PLC4X struct converts into a UDT (a struct or pointer to struct), including
nested UDTs and arrays of UDTs. Each exported field takes the struct member of
the same name, matched case-insensitively. Use a struct tag to choose another
member name or to leave a field out:

```go
type Motor struct {
    Speed   plc.REAL
    Running plc.BOOL `plc4x:"run"` // member "run"
    Note    plc.STRING `plc4x:"-"` // not exchanged with the device
}
```

Every remaining field must have a member, so a renamed member fails loudly
instead of leaving the field at zero. Writes send UDTs as PLC4X structs; whether
a device accepts them depends on its driver.

## Configuration file

`LoadConfig` reads connections from JSON, and `NewFromConfig` builds the
connector. Unknown keys are rejected, so a typo fails at startup.

```json
{
  "diagnostics": "PLC4X.",
  "connections": [{
    "name": "press1",
    "url": "modbus-tcp://10.0.0.5:502",
    "interval": "100ms",
    "mode": "poll",
    "bindings": [
      {"tag": "Press1.Pressure", "address": "holding-register:1:REAL"},
      {"tag": "Press1.Valve", "address": "holding-register:5:INT", "direction": "output"}
    ]
  }]
}
```

```go
cfg, err := plc4x.LoadConfig("plc4x.json")
connector, err := plc4x.NewFromConfig(db, cfg)
```

`mode` is `poll`, `change-of-state` or `cyclic`; `direction` is `input`,
`output` or `inout`; `interval` is a Go duration.

## Diagnostic tags

`WithDiagnostics(prefix)` (or `"diagnostics"` in the config file) publishes each
connection's `Status` as tags, created if missing:

| Tag                         | Type   | Meaning |
|-----------------------------|--------|---------|
| `<prefix><name>.Connected`  | BOOL   | The device is connected. |
| `<prefix><name>.Subscribed` | BOOL   | Inputs arrive by subscription. |
| `<prefix><name>.Reads`      | ULINT  | Answered polls and subscription events. |
| `<prefix><name>.Writes`     | ULINT  | Successful output writes. |
| `<prefix><name>.Errors`     | ULINT  | Errors of any kind. |
| `<prefix><name>.LastError`  | STRING | Most recent error; empty after a clean read. |
| `<prefix><name>.LastRead`   | DT     | Time of the last answered poll or event. |

Only fields that changed are written, so subscribers see real changes.

## Why a separate module

- Applications that do not talk to field devices do not download or compile PLC4X.
- PLC4X is Apache-2.0 licensed. Under honeycomb's GPLv3 option the combination is
  license-compatible; keeping it separate keeps the core's dependency list clean.
- PLC4X publishes no tagged Go module releases, so it is pinned to a `develop`
  commit (pseudo-version in `go.mod`). Upgrading it only touches this module.

Inside this repository `go.mod` replaces `github.com/apiarytech/honeycomb` with
`../..`. Release this module with tags of the form `connectors/plc4x/vX.Y.Z`.

## Building

PLC4X's `drivers` package compiles every protocol, including about 23 MB of
generated BACnet code. A first build can exhaust memory on machines with a small
page file (`VirtualAlloc ... errno=1455` on Windows). Build serially if that happens:

```bash
go build -p 1 -gcflags='all=-c=1' ./...
go test  -p 1 ./...
```

The first build takes several minutes; later builds use the Go build cache.

## Tests

`connector_test.go` runs a minimal in-process Modbus TCP server (reads and
writes), so the tests need no hardware. (PLC4X's own simulated driver is in an
`internal` package and cannot be imported from outside PLC4X.)

Modbus cannot subscribe, so subscription handling is tested with synthetic
PLC4X events, and the Modbus test covers the fallback to polling. Subscriptions
have not yet been run against a device that supports them (S7, ADS, OPC UA, KNX).

## Known limitations

- A whole UDT cannot be bound to an array element (e.g. `Motors[1]`); bind the
  array tag or individual fields instead. Honeycomb writes UDT values only to
  top-level tags.
- Output and InOut bindings must name top-level tags.
- Struct writes need a driver whose value handler supports structs; PLC4X's
  default handler rejects them.
