# Honeycomb Examples - Shared Code

This directory contains Go code that is shared across multiple examples in the `examples/` directory.

## Purpose

The primary purpose of this directory is to avoid code duplication. By centralizing common code, such as the `MotorData` User-Defined Type (UDT) and the `MotorState` ENUM, we can ensure that all examples are working with the same data structures.

This approach makes the individual examples (`simple`, `network_server`, `network_client`) easier to read and maintain, as they can focus on demonstrating specific features of the `honeycomb` library without being cluttered by redundant type definitions.

## Contents

Typically, you will find files here that define:
*   Custom Go structs that implement the `tags.UDT` interface.
*   Constants and variables for shared ENUM types.
*   Other helper functions or types used by more than one example.

**Note:** This directory does not contain a runnable `main.go` file. It is a library of shared components, not a standalone example.