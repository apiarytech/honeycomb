# honeycomb PLC4X connector

Reads field-device data into a honeycomb `TagDatabase` using
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
    },
}})
go connector.Run(ctx)            // polls until ctx is cancelled
status, _ := connector.Status("press1")
```

Each bound tag must already exist in the database with a value. The Go type
of that value (`plc.DINT`, `plc.REAL`, `[]plc.INT`, ...) selects the conversion
from the PLC4X value. A failing device reconnects with exponential backoff
(0.5 s to 30 s) without affecting other connections.

## Why a separate module

- Applications that do not talk to field devices do not download or compile PLC4X.
- PLC4X requires Go 1.27; the honeycomb core does not.
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

`connector_test.go` runs a minimal in-process Modbus TCP server, so the tests
need no hardware. (PLC4X's own simulated driver is in an `internal` package and
cannot be imported from outside PLC4X.)

## Not yet implemented

- Writing tags to devices (outputs) and echo suppression.
- PLC4X subscriptions (change-of-state / cyclic) instead of polling.
- Conversion of PLC4X structs into honeycomb UDTs.
- Loading connections from a configuration file.
- Diagnostic tags per connection (use `Status` for now).
