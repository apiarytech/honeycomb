# honeycomb documentation

honeycomb is an in-memory tag database for Go, modelled on the tags of an
IEC 61131-3 PLC: typed values with quality and timestamps, structured types,
arrays, forcing, retained values, subscriptions, a change feed, persistence
to SQL databases, an HTTPS API, and a connector to field devices through
Apache PLC4X.

The [README](../README.md) is the quick tour. These pages are the reference.

| Page | What it covers |
|---|---|
| [Architecture](architecture.md) | Modules and packages, the data model, how values flow, the concurrency model |
| [Tags and types](tags.md) | Data types, `TypeInfo`, UDTs, enumerations, arrays, subranges, strings, names and access paths, direct addresses, aliases, constants, forcing |
| [Reading and writing values](values.md) | `GetTagValue`/`SetTagValue` and their variants, quality, timestamps, `ReadTag` and batch reads |
| [Subscriptions and the change feed](events.md) | Latest-value subscriptions, the ordered change feed, and which to use |
| [Persistence](persistence.md) | Retained values through a `TagStore`, the `sqlstore` package and its dialects, tag files |
| [Network API](network.md) | The HTTPS server, its endpoints and authentication, the network client, remote aliases |
| [PLC4X connector](plc4x.md) | Exchanging tags with S7, Modbus, EtherNet/IP, OPC UA and other devices |
| [Development](development.md) | Building, testing, CI, releases, conventions and known issues |

## Modules

| Module | Path | Depends on |
|---|---|---|
| honeycomb | `github.com/apiarytech/honeycomb` | royaljelly (IEC types), modernc.org/sqlite (tests and the SQLite example) |
| PLC4X connector | `github.com/apiarytech/honeycomb/connectors/plc4x` | honeycomb, Apache PLC4X (Go) |

The connector is a separate module so that an application which does not
talk to field devices does not download PLC4X and its drivers.

## License

Dual-licensed: GPL v3.0 or a commercial license. See [LICENSE.md](../LICENSE.md).
