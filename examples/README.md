# Honeycomb TagDatabase Usage Example

This directory contains example programs demonstrating the features of the `honeycomb` TagDatabase package.

## `main.go` - Core Features Demonstration

The `main.go` program is a comprehensive example that walks through several of the most common and powerful features of the `honeycomb` package. It is structured to be read from top to bottom, with comments explaining each step.

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
    *   It demonstrates the powerful nested access feature by reading a field from a UDT within an array (`MotorLine[0].Speed`).
    *   It also shows how to write to a nested field within an array element (`MotorLine[1].Running`).

4.  **Persistence**:
    *   It marks tags with the `Retain: true` flag to make them eligible for persistence.
    *   It writes all retain-qualified tags and their current values to a file (`tags.txt`) using `WriteTagsToFile`.
    *   To simulate an application restart, it creates a second `TagDatabase` instance, pre-populates it with the tag definitions, and then uses `ReadTagsFromFile` to load the persisted values.
    *   Finally, it verifies that the values were loaded correctly in the new database instance.

5.  **Cross-Database Aliasing**:
    *   It demonstrates a distributed system pattern by creating two separate database instances (`db1` and `db2`).
    *   It registers `db1` with `db2`, allowing `db2` to reference it.
    *   It creates a "remote alias" tag in `db2` that points to a source tag in `db1`.
    *   It shows that reading from and writing to the alias in `db2` transparently affects the source tag in `db1`, showcasing a powerful feature for building modular applications.

This example serves as a great starting point for understanding how to integrate the `honeycomb` TagDatabase into your own Go applications.