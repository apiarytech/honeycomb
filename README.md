# Honeycomb TagDatabase - IEC 61131-3 Compliant PLC Tag Management in Go

The `TagDatabase` project provides a robust, thread-safe, and feature-rich in-memory database for managing PLC-like tags, adhering closely to the specifications outlined in IEC 61131-3. Developed in Go, it offers a flexible and performant solution for applications requiring structured data management with industrial control system paradigms.

## Table of Contents

- [Features](#features)
- [Core Concepts](#core-concepts)
  - [DataType](#datatype)
  - [TypeInfo](#typeinfo)
  - [Tag](#tag)
  - [UDT Interface](#udt-interface)
- [Advanced Features](#advanced-features)
  - [Cross-Database Aliasing](#cross-database-aliasing)
  - [Tag Quality](#tag-quality)
  - [Timestamps](#timestamps)
  - [Subscriptions](#subscriptions)
  - [Change Feed](#change-feed)
  - [Batch Read](#batch-read)
- [Installation](#installation)
- [Usage](#usage)
  - [Initializing and Registering Types](#initializing-and-registering-types)
  - [Creating and Adding Tags](#creating-and-adding-tags)
  - [Accessing and Modifying Tag Values](#accessing-and-modifying-tag-values)
  - [Persistence](#persistence)
  - [Subscriptions (Event-Driven Updates)](#subscriptions-event-driven-updates)
  - [Networking Features](#networking-features)
- [Licensing](#licensing)
- [Contributing](#contributing)

## Features

This database is designed to encapsulate the complexities of PLC tag management, offering:

*   **Comprehensive IEC 61131-3 Data Type Support**: Built-in support for a wide range of standard PLC data types, including `BOOL`, `BYTE`, `WORD`, `DWORD`, `LWORD`, `SINT`, `INT`, `DINT`, `LINT`, `USINT`, `UINT`, `UDINT`, `ULINT`, `REAL`, `LREAL`, `STRING`, `WSTRING`, `TIME`, `DATE`, `TOD`, and `DT`.
*   **User-Defined Types (UDTs)**: Implements the concept of IEC 61131-3 `STRUCT`s through a flexible `UDT` interface, allowing users to define complex, nested data structures.
*   **Array Management**: Supports single and multi-dimensional arrays (`ARRAY` type) with dynamic element type checking and direct element access (e.g., `MyArray[index]`, `MyMultiDimArray[row,col]`).
*   **Enumerated Types (ENUMs)**: Provides mechanisms to define and validate enumerated data types, ensuring values are restricted to a predefined set of strings.
*   **Subrange Types**: Allows the definition of `Min` and `Max` values for numeric tags, enforcing value constraints similar to IEC 61131-3 `SUBRANGE` types.
*   **Direct Addressing**: Supports IEC 61131-3 direct addressing syntax (e.g., `%IX0.0`, `%QW10`, `%MD20`), enabling tags to be referenced by their memory addresses. `PopulateDatabaseFromImage` turns a [royaljelly](https://github.com/apiarytech/royaljelly) `vars.ProcessImage` into tags using royaljelly's address layout: each area is indexed by address (`%QW4` is `Q.W[4]`, `%MD2` is `M.D[2]`), and a bit can be written as `%IX10` or `%IX1.2`.
*   **Tag Qualifiers**: Incorporates `Constant` and `Retain` qualifiers, mirroring common PLC tag properties for immutability and persistence across restarts.
*   **Forcing Capabilities**: Tags can be "forced" with a `ForceValue`, overriding their actual `Value`, a critical feature for PLC diagnostics and commissioning.
*   **Thread-Safety**: All database operations are protected by mutexes and `sync.Map`, ensuring safe concurrent access.
*   **Subscription Mechanism**: Clients can subscribe to tag value changes, receiving notifications via Go channels, enabling reactive programming models.
*   **Persistence**: Tags and their values can be written to and read from a file, facilitating application restarts and configuration loading. This includes proper serialization/deserialization of UDTs and arrays.
*   **Flexible Tag Access**: Tags can be accessed by their symbolic name, alias, direct address, or even nested UDT field paths (e.g., `Motor.Config.MaxSpeed`).
*   **Cross-Database Aliasing**: A tag in one database instance can act as a transparent alias for a tag in a completely different database instance, enabling distributed and modular system architectures.

## Core Concepts

### `DataType`
An enumeration representing the fundamental type of a tag, aligning with IEC 61131-3 standard data types.

### `TypeInfo`
A struct that holds the defining characteristics of a tag's data type. This includes its `DataType`, `ElementType` (for arrays), `EnumValues`, `Min`/`Max` (for subranges), `MaxLength` (for strings), and `Dimensions` (for multi-dimensional arrays). This structure allows for rich type definition and validation.

### `Tag`
The central entity, representing a single variable or data point. It encapsulates:
-   `Name`: Unique symbolic identifier.
-   `Value`: Current data value.
-   `Quality`: How trustworthy `Value` is: `QualityUnknown` (0), `QualityGood` (1), `QualityUncertain` (2) or `QualityBad` (3). See [Tag Quality](#tag-quality).
-   `Alias`: Alternative name.
-   `DirectAddress`: IEC 61131-3 memory address.
-   `TypeInfo`: Pointer to the shared `TypeInfo` defining its characteristics.
-   `Description`: Human-readable explanation.
-   `Forced`: Boolean indicating if the tag's value is overridden.
-   `Constant`: Boolean indicating if the tag's value is immutable.
-   `Retain`: Boolean indicating if the tag's value should persist.
-   `ForceValue`: The value used when the tag is forced.

### `UDT` Interface
```go
type UDT interface {
	TypeName() DataType
}
```
Any Go struct intended to be used as a User-Defined Type (IEC 61131-3 STRUCT) must implement this interface, returning a unique `DataType` string for its type.

## Advanced Features

### Cross-Database Aliasing
A powerful feature of the `honeycomb` `TagDatabase` is the ability to create remote aliases. A tag in one database instance can be defined as an alias for a tag residing in a separate database instance. This allows for building complex, distributed systems where different modules or services can interact with each other's data seamlessly and transparently.

```go
// In Service A, which has a TagDatabase instance `db1`
db1.AddTag(&honeycomb.Tag{Name: "SourceTag", Value: plc.DINT(100), ...})

// In Service B, which has a TagDatabase instance `db2`
// First, register db1 with db2
db2.RegisterDatabase("DB1_ID", db1)
// Now, create an alias that points to the tag in db1
db2.AddTag(&honeycomb.Tag{
    Name: "AliasToDB1",
    RemoteAlias: &honeycomb.RemoteAliasInfo{DBID: "DB1_ID", TagName: "SourceTag"},
})

// Reading/writing "AliasToDB1" in db2 will now transparently access "SourceTag" in db1.
val, _ := db2.GetTagValue("AliasToDB1") // val will be plc.DINT(100)
```

### Tag Quality
Every tag carries a `Quality` (`uint8`) that says whether its value can be trusted:

| Quality            | Value | Set when                                                                    |
|--------------------|-------|-----------------------------------------------------------------------------|
| `QualityUnknown`   | 0     | The tag was created and nothing has written it yet.                         |
| `QualityGood`      | 1     | `SetTagValue` succeeds (a plain write asserts the value is good). Constants start Good. |
| `QualityUncertain` | 2     | A Good value is restored at power-up; it may be stale.                      |
| `QualityBad`       | 3     | A driver reports a failure.                                                 |

Quality is tracked per top-level tag: writing `Motor.Speed` or `Arr[1]` sets the quality of `Motor` or `Arr`.
Drivers and protocol bridges should write value and quality together, and downgrade the quality alone when a read fails:

```go
db.SetTagValueQuality("Level", plc.DINT(42), honeycomb.QualityGood)
db.SetTagQuality("Level", honeycomb.QualityBad) // keeps the last value; notifies only on change
q, err := db.GetTagQuality("Level")             // QualityBad on error
```

Quality converts to and from the industrial protocols without depending on their libraries:

| Protocol | To                     | From                                     | Notes |
|----------|------------------------|------------------------------------------|-------|
| OPC DA   | `q.OPCDA() uint16`     | `QualityFromOPCDA(uint16)`               | Good `0xC0`, Uncertain `0x40`, Bad `0x00`; Unknown is `0x20` (Bad, waiting for initial data). |
| OPC UA   | `q.OPCUA() uint32`     | `QualityFromOPCUA(uint32)`               | By severity bits; Unknown is `BadWaitingForInitialData` (`0x80320000`). |
| PLC4X    | —                      | `QualityFromPLC4X(code.GetName())`       | `OK` → Good; `REMOTE_BUSY`, `RESPONSE_PENDING`, `REQUEST_TIMEOUT` → Uncertain; everything else → Bad. |

Over the network API, `GET /tags/{name}` returns `{"value": ..., "quality": 1, "timestamp": "..."}`, and `PUT` accepts an optional `"quality"` (default Good) and `"timestamp"` (RFC 3339, default now), or `"quality"` alone to change only the quality.

### Timestamps
Every tag carries a `Timestamp`: when its value or quality last changed. A plain write stamps the current time. A driver that knows when the device saw the change passes that time through, so a sequence-of-events record shows the device's time rather than the time honeycomb received the value:

```go
db.SetTagValueQualityAt("Pump1.Tripped", plc.BOOL(true), honeycomb.QualityGood, deviceTime)
```

A quality change (`SetTagQuality`) is stamped too. The timestamp is zero until the tag's first write, and it survives restarts through both the TagStore and tag files.

### Subscriptions
`SubscribeToTag` returns a channel that always delivers the tag's **newest** state:

- The channel holds one update. If a newer update arrives before the subscriber has received the previous one, it replaces it. A spike that returns to normal while the subscriber is busy therefore leaves the subscriber with the normal value, never the stale one.
- Updates for a tag arrive in order, even with concurrent writers. Each carries a `Sequence` number that rises by one per update, so a gap tells the subscriber how many updates it missed.
- Each update is a complete copy of the tag, including `Quality`, `Timestamp` and `DirectAddress`.
- Writes never block on slow subscribers.
- A remote alias cannot be subscribed to, because its writes happen in the remote database; subscribe on the database that owns the tag.

```go
ch, id, _ := db.SubscribeToTag("Level")
defer db.UnsubscribeFromTag("Level", id)
var last uint64
for update := range ch {
    if update.Sequence > last+1 {
        // update.Sequence - last - 1 updates were replaced before we received them.
    }
    last = update.Sequence
}
```

Subscriptions keep only the latest state. To process **every** change, for example to record a sequence of events, use the change feed.

### Change Feed
Each database keeps an in-memory ring buffer of tag changes (`DefaultChangeFeedCapacity`, 10,000; see `SetChangeFeedCapacity`). A change is recorded when the write happens, before subscriptions replace older updates. Each one has a sequence number, the tag's value and quality, and the device timestamp. A reader asks for the changes after the last sequence it saw, and can wait (long-poll) until there are some:

```go
pos, _ := db.Changes(ctx, honeycomb.ChangesRequest{}) // a new reader starts from "now"
// ... read current values (ReadTags) to start from ...
for {
    batch, err := db.Changes(ctx, honeycomb.ChangesRequest{
        Since: pos.Next, Epoch: pos.Epoch, Wait: 30 * time.Second,
    })
    if err != nil {
        break
    }
    if batch.Gap {
        // Changes were lost (the reader fell behind the buffer, or the
        // database restarted): re-read current values before going on.
    }
    for _, c := range batch.Changes {
        // c.Seq, c.Name, c.Value, c.Quality, c.Timestamp: in order, none skipped.
    }
    pos = batch
}
```

- **Nothing is skipped while the buffer holds.** A 50 ms pulse between two polls still produces both its activation and its return to normal, each with its own device timestamp.
- **Gaps are reported.** If a reader falls further behind than the buffer holds, or the database restarted (a new `Epoch`), the batch has `Gap` set, and the reader re-reads current values.
- **Writes that change nothing are not recorded.** A driver that rewrites an unchanged value every poll does not fill the buffer; a change of quality alone is recorded.
- **Each change keeps its own copy** of array and UDT values, so later element writes do not alter the history.
- **Changes are per top-level tag.** A write to `Arr[1]` or `Motor.Speed` records the whole `Arr` or `Motor`.

Over HTTP, `POST /changes` takes `{"since": 41, "epoch": "…", "names": [...], "wait_ms": 30000, "max": 1000}` and returns `{"epoch": "…", "next": 42, "gap": false, "changes": [...]}`. The server answers at once if there are newer changes, or holds the request open (at most `MaxChangesWait`, one minute) until one arrives. `NetworkDatabaseClient.Changes` wraps this, and returns `ErrChangesUnsupported` for a server without a feed.

### Batch Read
`ReadTags` reads many tags (value, quality and timestamp) in one call, so a client that samples tags pays one round trip per poll instead of one per tag:

```go
readings, err := db.ReadTags(ctx, []string{"Temp", "Lid", "Motors[1]"}) // err joins any per-tag errors
```

Over HTTP, `GET /tags?names=Temp,Lid` (or repeated `name=` parameters) and `POST /tags` with `{"names": [...]}` (for lists too long for a URL; up to `MaxReadTags`) both return `{"tags": {"Temp": {"value": 42.5, "quality": 1, "timestamp": "…"}, ...}}`. A tag that cannot be read has quality 3 (Bad) and an `"error"`. `GET /tags` without names still lists all tags. `NetworkDatabaseClient.ReadTags` wraps the POST form, and returns `ErrReadTagsUnsupported` for an older server. Both implement `TagReader`, as both implement `ChangeSource`, so a client reads the same way in process and over the network.

## Installation

To use `TagDatabase`, you need to have Go installed. Then, you can fetch the library using `go get`:

```bash
go get github.com/apiarytech/honeycomb
```

### Initializing and Registering Types

Before using custom types (UDTs or ENUMs), they must be registered with the database:

```go
// Define a UDT
type MotorData struct {
	Speed   plc.REAL
	Current plc.REAL
	Running plc.BOOL
}

func (m *MotorData) TypeName() DataType {
	return "MotorData"
}

This library includes several examples in the `examples/` directory to demonstrate its capabilities.

### Core Features (`examples/simple`)

The `examples/simple/main.go` application is the best starting point. It provides a comprehensive walkthrough of the core, in-memory features of the `honeycomb` package.

It demonstrates:
-   **Type Registration**: Defining and registering custom `UDT` and `ENUM` types.
-   **Tag Management**: Creating and adding simple tags, arrays, and UDTs to the database.
-   **Value Access**: Reading from and writing to tags, including nested fields within arrays and UDTs (e.g., `MyArray[0].Field`).
-   **Persistence**: Marking tags with `Retain: true` and using `WriteTagsToFile` and `ReadTagsFromFile` to save and load tag values.
-   **In-Process Aliasing**: Using the cross-database aliasing feature between two database instances running in the same application.

To run this example, navigate to `examples/simple` and run:
```bash
go run main.go
```

## Licensing

This project is offered under a dual-license model. You have the choice of using it under either the GNU General Public License version 3 (GPLv3) or a commercial license.

*   **GPLv3:** If you are developing open-source software, you can use this library under the terms of the GPLv3. The full license text is available in the `gpl-3.0.md` file.
*   **Commercial License:** If you intend to use this library in a proprietary, closed-source application or product, a commercial license is required.

For more details on both licensing options, please see the `LICENSE.md` file.

## Contributing

Contributions to `honeycomb` are welcome! Please feel free to:
- Fork the repository.
- Submit issues for bugs or feature requests.
- Submit pull requests with improvements, bug fixes, or new IEC 61131-3 compliant implementations.

Please ensure that your contributions adhere to the existing code style and include appropriate tests.

Thank you for your interest in `honeycomb`!
