# Honeycomb TagDatabase - IEC 61131-3 Compliant PLC Tag Management in Go

The `TagDatabase` project provides a robust, thread-safe, and feature-rich in-memory database for managing PLC-like tags, adhering closely to the specifications outlined in IEC 61131-3. Developed in Go, it offers a flexible and performant solution for applications requiring structured data management with industrial control system paradigms.

## Table of Contents

- [Features](#features)
- [Core Concepts](#core-concepts)
  - [DataType](#datatype)
  - [TypeInfo](#typeinfo)
  - [Tag](#tag)
  - [UDT Interface](#udt-interface)
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
*   **Direct Addressing**: Supports IEC 61131-3 direct addressing syntax (e.g., `%IX0.0`, `%QW10`, `%MD20`), enabling tags to be referenced by their memory addresses.
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
db2.AddTag(&honeycomb.Tag{Name: "AliasToDB1", IsRemoteAlias: true, RemoteDBID: "DB1_ID", RemoteTagName: "SourceTag"})

// Reading/writing "AliasToDB1" in db2 will now transparently access "SourceTag" in db1.
val, _ := db2.GetTagValue("AliasToDB1") // val will be plc.DINT(100)
```

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

This project is offered under a dual-license model. You have the choice of using it under either the GNU General Public License version 2 (GPLv2) or a commercial license.

*   **GPLv2:** If you are developing open-source software, you can use this library under the terms of the GPLv2. The full license text is available in the `gpl-2.0.md` file.
*   **Commercial License:** If you intend to use this library in a proprietary, closed-source application or product, a commercial license is required.

For more details on both licensing options, please see the `LICENSE.md` file.

## Contributing

Contributions to `honeycomb` are welcome! Please feel free to:
- Fork the repository.
- Submit issues for bugs or feature requests.
- Submit pull requests with improvements, bug fixes, or new IEC 61131-3 compliant implementations.

Please ensure that your contributions adhere to the existing code style and include appropriate tests.

Thank you for your interest in `honeycomb`!
