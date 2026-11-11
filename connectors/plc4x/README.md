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

## Server mode

`Connection.Mode = Server` turns a connection around: instead of talking to a
field device, the connector answers one, serving its bound tags. Every comm
connection can be a client or a server (beehive R-133). PLC4X ships only
clients, so each server is built from PLC4X's public parts: its serial package,
and its protocol models to parse requests and serialize responses, so a value
is encoded exactly as PLC4X's own client decodes it. A server owns its transport
and is on no channel.

An `Output` binding is read only (the client reads the tag); an `InOut` binding
the client may also write (or command), through to the tag. A server's bindings
have no scaling and no poll group. `WithServerWrites` reports each write.

| Protocol | URL | Binding address | Serves |
|----------|-----|-----------------|--------|
| `modbus-rtu` | `modbus-rtu:serial:///dev/ttyAMA2?serial.baud-rate=19200&default-unit-identifier=1` | `holding-register:1:REAL`, `coil:5`, `40010:DINT` | Modbus RTU slave on a serial port |
| `modbus-ascii` | `modbus-ascii:serial:///dev/ttyAMA2?serial.data-bits=7&default-unit-identifier=1` | the Modbus forms above | Modbus ASCII slave on a serial port (`:` + hex + LRC + CR/LF) |
| `modbus-tcp` | `modbus-tcp://0.0.0.0:502?default-unit-identifier=1&read-only-interfaces=eth1` | the Modbus forms above | Modbus TCP server |
| `iec-60870-5-104` | `iec-60870-5-104://0.0.0.0:2404?read-only-interfaces=eth1&k=12&w=8` | `<ca>/<ioa>:<type>`, e.g. `1/100:FLOAT` (types `SP`, `DP`, `FLOAT`, `SCALED`, `NORM`); the client's tag is the same without the type | IEC 60870-5-104 outstation: connect handshake, general interrogation, spontaneous transmission of changed points, and single/double/set-point commands for `InOut` points |
| `s7` | `s7://0.0.0.0:102?read-only-interfaces=eth1` | the PLC4X S7 form, e.g. `%DB1.DBW0:INT`, `%DB1.DBX0.0:BOOL`, `%M4.0:BOOL`, `%Q0.0:BOOL` | S7 server over ISO-on-TCP: COTP connect, setup communication, read-var and write-var on DB, M, I and Q areas |

For the Modbus servers, `holding-register`/`coil` write requests (function
codes 5, 6, 15, 16) are answered only for `InOut` points; everything else is
read only and other functions get a Modbus exception. For a TCP server,
`read-only-interfaces` names the interfaces whose clients may read (or receive)
but not write (or command): a client reaching the server there is refused any
write, as on IEC 60870-5-104 LAN 2 (drone D-66). A server records
`Status.Connected`, `Reads` (requests answered or frames sent) and `Writes`
(tags written), the same diagnostic tags as a client.

### DNP3 outstation (the connector's own code)

PLC4X has no DNP3 driver, so the connector serves DNP3 (IEEE 1815) itself,
package `dnp3`, as a server connection only:

```
dnp3://0.0.0.0:20000?address=10&master=1&events=1000
```

- `address` is the outstation's link address (required); `master` the only
  master it answers (0 or absent: any); `events` the events kept for the
  masters (1000).
- Bindings are points, `<kind>:<index>[:<class>]`: `BI:0` binary input
  (g1v2; events g2v2 with time), `AI:3` single-precision analog input
  (g30v5; events g32v7), `AI32:4` 32-bit analog input (g30v1; events
  g32v3), `BO:0` binary output (status g10v2; CROB g12v1), `AO:1` analog
  output (status g40v3; g41v1 and g41v3). An input's event class is 1 by
  default, 0 to 3; inputs are `Output` bindings. Outputs bound `InOut` may
  be commanded (latch on, close and pulse on set the tag true; latch off,
  trip and pulse off false); select-before-operate is honoured.
- Class 0 reads give every point; class 1 to 3 reads give the events with
  the tag's own time and quality (a bad tag is reported offline). Events
  stay until a master confirms them, so a master back after an outage gets
  them in order. The restart indication is set until a master clears it;
  a written time is noted but never sets the machine's clock.
- The TLS options of IEC 60870-5-104 apply (IEC 62351-3), with the roles of
  IEC 62351-8: only an OPERATOR commands. A read-only interface reads
  only.
- Not yet: a DNP3 client (master) connection, unsolicited responses,
  multi-fragment responses (one fragment of 2048 octets; more events wait
  for the next read), counters, Secure Authentication (IEC 62351-5).

### IEC 60870-5-104 time and quality

- **Time tags (outstation):** `time-tags=true` sends each spontaneous change
  with its tag's own time as a CP56Time2a, in UTC (`M_SP_TB_1`,
  `M_DP_TB_1`, `M_ME_TF_1`, `M_ME_TE_1`, `M_ME_TD_1`). General
  interrogation answers carry no time, as the standard has it.
- **Quality:** a point carries its tag's quality: invalid when the tag is
  bad, not topical when it is uncertain.
- **Clock synchronisation:** a `C_CS_NA_1` is confirmed with the
  outstation's own time; the machine's clock is never set from a station
  (it is kept by the machine's own time service).
- **Client:** a point received with a time tag is stored with that time
  and with its quality (invalid: bad, not topical: uncertain), so the
  source time is kept end to end.

### IEC 60870-5-104 store and forward

`buffer=N` (up to 100000) keeps every change of the outstation's points,
time-tagged, in a log, whether a controlling station is connected or not.
A station that starts data transfer receives the events no station has
acknowledged, in order and with their original times, then the current
values, then each new change as it comes. An event leaves the log only
when a station acknowledges the frame that carried it, so a link that
drops in the middle loses nothing. Past N events the oldest are dropped
(the station should interrogate after a long outage). Without `buffer`
the outstation sends only current values and changes while a station is
connected, as before. Acknowledgements only move forward: one behind what
was already acknowledged is ignored.

### IEC 60870-5-104 over TLS (IEC 62351-3 and -8)

Both the outstation and the client take TLS with options on the URL:

```
iec-60870-5-104://0.0.0.0:19998?tls=true&tls-cert=rtu.pem&tls-key=rtu.key&tls-ca=ca.pem
iec-60870-5-104://10.0.0.5:19998?tls=true&tls-cert=scada.pem&tls-key=scada.key&tls-ca=ca.pem
```

- TLS is mutual: each end presents `tls-cert` and `tls-key` and checks the
  other's against `tls-ca`. TLS 1.2 (ECDHE with AES-GCM or
  ChaCha20-Poly1305 only) or 1.3. A client checks the outstation's name
  against the URL's host, or `tls-server-name`. The usual port is 19998.
- **Roles (outstation):** with `tls-roles=on` (the default), a
  controlling station commands only when its certificate carries the
  IEC 62351-8 role OPERATOR in the role extension (`RolesOf`, OID
  1.2.840.10070.8.1, each role's validity checked); a certificate without
  roles, or with others only, receives data and general interrogation but
  every command is refused. `tls-roles=off` lets any authenticated station
  command. A read-only interface stays read only either way. The record
  of a write names the certificate's common name and roles.
- **Client:** PLC4X's IEC 60870-5-104 client speaks plain TCP, so the
  connector runs a tunnel for it: a listener on loopback whose
  connections it carries over TLS to the outstation. PLC4X dials the
  tunnel; nothing leaves the machine in clear.

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

## Modbus unit scan

`ScanUnits(ctx, url, ScanOptions{From, To, Timeout})` (R-138) lists the unit
IDs that answer on a Modbus RTU serial line
(`modbus-rtu:serial:///dev/ttyAMA2?serial.baud-rate=19200&...`) or behind a
Modbus TCP gateway (`modbus-tcp://10.0.0.9:502`): each unit is asked for one
holding register with a short timeout (200 ms); a normal answer or an
exception both mean a device is there, a gateway's exception 0x0B (no
response from the target) does not. The scan opens the port itself, so no
connection may use it meanwhile.

## Traffic view

`New(..., WithTraffic())` (R-137) lets a connection's bytes be recorded
for commissioning: `SetTraffic(name, true)` starts a ring of its own (2000
frames), `Traffic(name, since)` reads the frames sent to the device and
received from it with their times, `SetTraffic(name, false)` stops and
drops it. It wraps PLC4X's TCP and serial transports, registered before
the drivers' own; without `WithTraffic` they are PLC4X's, untouched.
Nothing recorded goes to a log. A driver on another transport (UDP, a
driver's own) records nothing.

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
| `<prefix><name>.Demoted`    | BOOL   | The connection is demoted (`Demotion`). |
| `<prefix><name>.Failures`   | UDINT  | Failed exchanges in a row, counted toward `Demotion.After`; 0 after a success. |

Only fields that changed are written, so subscribers see real changes.

## Channels, timing and scaling

All optional; the zero value is the behaviour above.

| Field | Effect |
|-------|--------|
| `Connection.Channel` + `WithChannels` / `Config.Channels` | Connections naming one channel share a transport (an RS-485 port with `serial.reuse-port=true`) and take turns: one request at a time, `InterDeviceDelay` before a request to another device than the last. A server is on no channel. |
| `Connection.Mode = Event` | Inputs are PLC4X event subscriptions: the device reports when it has something to say. A driver that cannot subscribe is polled. |
| `Connection.PollGroups`, `Binding.PollGroup` | Named rates: each group is one read request on its own ticker; other inputs are read every `Interval`. |
| `ConnectTimeout`, `RequestTimeout`, `Attempts`, `InterRequestDelay` | Bounds and pauses per connection; `RequestTimeout` defaults to 10 s. |
| `Demotion{After, Period, DiscardWrites}` | Every failed exchange adds one to `Status.Failures` (diagnostic tag `Failures`), a success sets it to 0; when it reaches `After` the connection pauses for `Period` (10 s): inputs Bad, `Status.Demoted`, diagnostic tag `Demoted`. `DiscardWrites` drops in-out changes made meanwhile. |
| `Standby{URL, After, ReturnEvery}` | A second path to the same device (R-136): another address, a redundant network, a gateway; same protocol. After `After` (3) attempts in a row fail on one path, the connection takes the other; on the standby it tries the first every `ReturnEvery` (a minute) and goes back once it answers. A switch never writes safe values. `Status.OnStandby`, `Status.PathSwitches`. Not for servers. JSON: `"standby": {"url": ..., "after": 3, "returnEvery": "1m"}`. |
| `Disabled` | No connection; inputs Bad; `Status.Disabled`. |
| `Binding.Scaling` | Linear or square-root scaling with clamps and negate: reads scaled into the tag (the tag's numeric type), writes unscaled (a whole-number address gets the raw value rounded half to even). |

`CapabilitiesOf(protocol)` says whether a driver subscribes, sends events,
browses and has a server here; `Browse(ctx, url, query)` lists a device's
addresses for the drivers that browse.

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
