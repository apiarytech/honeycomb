# Honeycomb - Simple Example

This example provides a comprehensive walkthrough of the core, in-memory features of the `honeycomb` package. It is the best starting point for new users.

The code in `main.go` is heavily commented to explain each step, including type registration, tag creation, value access, persistence, and in-process aliasing.

For a higher-level overview of the features demonstrated here, please see the Core Features (`examples/simple`) section in the main project `README.md`.

### How to Run

To run the example, navigate to this directory in your terminal and execute:

```bash
go run main.go
```

### Demonstrated Functionalities

This example showcases the following key features:

1.  **Custom Type Registration**:
    *   It defines a custom Go struct `MotorData` and implements the `tags.UDT` interface, turning it into a User-Defined Type.
    *   It registers the `MotorData` UDT and a new `MotorState` ENUM type with the `honeycomb` system, making them available for use in tags.

2.  **Tag Creation and Management**:
    *   It initializes a new `TagDatabase` instance.
    *   It adds a simple tag of type `DINT` (`MyDINT`).
    *   It demonstrates creating and adding a more complex tag: an `ARRAY` of the custom `MotorData` UDT (`MotorLine`).

3.  **Tag Value Access and Modification**:
    *   It shows how to get and set the value of a simple tag (`MyDINT`).
    *   It demonstrates nested access by reading a field from a UDT within an array (`MotorLine[0].Speed`).
    *   It shows how to write to a nested field within a different array element (`MotorLine[1].Running`).

4.  **Persistence**:
    *   It marks tags with the `Retain: true` flag, making them eligible for persistence.
    *   It writes all retain-qualified tags and their current values to a file (`tags.txt`) using `WriteTagsToFile`.
    *   To simulate an application restart, it creates a second `TagDatabase` instance, pre-populates it with the tag definitions, and then uses `ReadTagsFromFile` to load the persisted values.
    *   Finally, it verifies that the values were loaded correctly in the new database instance.

5.  **Cross-Database Aliasing**:
    *   It demonstrates a distributed system pattern by creating two separate database instances (`db1` and `db2`).
    *   It registers `db1` with `db2`, allowing `db2` to reference it.
    *   It creates a "remote alias" tag in `db2` that points to a source tag in `db1`.
    *   It shows that reading from and writing to the alias in `db2` transparently affects the source tag in `db1`, showcasing a powerful feature for building modular, in-process distributed systems.

This example serves as a great starting point for understanding how to integrate the `honeycomb` TagDatabase into your own Go applications.