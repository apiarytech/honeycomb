# PLC4X connector

`github.com/apiarytech/honeycomb/connectors/plc4x` exchanges tags with
field devices through [Apache PLC4X](https://plc4x.apache.org) (Go): S7,
Modbus TCP/RTU/ASCII, EtherNet/IP, Logix, ADS, OPC UA, BACnet/IP,
KNXnet/IP, C-Bus, Firmata, IEC 60870-5-104, SLMP and UMAS.

It is a module of its own: only applications that talk to devices
download PLC4X. Its [README](../connectors/plc4x/README.md) has the full
reference; this page is the overview.

## Configuration

In code:

```go
connector, err := plc4x.New(db, []plc4x.Connection{{
    Name:     "press1",
    URL:      "modbus-tcp://10.0.0.5:502",
    Interval: 100 * time.Millisecond,
    Bindings: []plc4x.Binding{
        {Tag: "Press1.Pressure", Address: "holding-register:1:REAL"},
        {Tag: "Press1.Valve", Address: "coil:1:BOOL", Direction: plc4x.Output},
    },
}}, plc4x.WithDiagnostics("PLC4X."))
go connector.Run(ctx)
```

or from a JSON file (`LoadConfig`, `NewFromConfig`); unknown keys are
refused:

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
      {"tag": "Press1.Valve", "address": "coil:1:BOOL", "direction": "output"}
    ]
  }]
}
```

Each bound tag must exist with a value: the Go type of the value selects
the conversion to and from the PLC4X value, including arrays and UDTs.

## Behaviour

| Topic | Behaviour |
|---|---|
| Directions | `Input` (device to tag, default), `Output` (tag to device on change, and once after each connect), `InOut` (both, with echo suppression) |
| Modes | `Poll` every `Interval` (default), `ChangeOfState` or `Cyclic` by subscription; a driver that cannot subscribe falls back to polling and says so once |
| Quality | `OK` Good; busy, pending, timeout or no answer Uncertain (last value kept); anything else or a lost connection Bad (last value kept). Output tags' quality is never changed |
| Timestamps | Inputs are stamped with the time the connector writes them; device timestamps are not used yet |
| Reconnects | Exponential backoff from 0.5 s to 30 s, per connection |
| Forcing | A forced output writes its force value |
| Diagnostics | With `WithDiagnostics(prefix)`, tags `prefix+name.Connected`, `.Subscribed`, `.Reads`, `.Writes`, `.Errors`, `.LastError`, `.LastRead`; also `Status(name)` |
| Errors | `WithErrorHandler(fn)`; otherwise kept in `Status` |

Output and InOut bindings must name top-level tags: the connector
subscribes to them to see changes. Each output is written in its own
request, since some drivers (Modbus) accept one tag per write.

## Platforms

PLC4X does not compile for 32-bit targets yet (it passes `math.MaxInt64` as
an `int`), so the connector builds for 64-bit platforms only, including
64-bit Raspberry Pi.
