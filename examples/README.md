# Honeycomb TagDatabase - Examples

This directory contains a set of examples demonstrating the various features of the `honeycomb` TagDatabase library.

## One-Time Setup for Network Examples

The `network_server` and `network_client` examples require a TLS certificate and private key to enable secure HTTPS communication. Before running them, you must generate these files.

1.  Navigate to the `examples/shared` directory:
    ```bash 
    cd shared
    ``` 
2.  Run the following `openssl` command to generate `server.crt` and `server.key`:
    ```bash
    openssl req -x509 -newkey rsa:4096 -keyout server.key -out server.crt -sha256 -days 365 -nodes -subj "/CN=localhost"
    ```

These two files will be used by both network examples but should **not** be committed to version control.

## Available Examples

1.  **`simple/`**: The best starting point for new users. This example covers the core, in-memory features of the library.
2.  **`network_server/`**: Demonstrates how to expose a `TagDatabase` over a secure HTTPS network interface.
3.  **`network_client/`**: Shows how to use the powerful networked aliasing feature to connect a local tag to a tag on a remote server.

## Recommended Learning Path

1.  **Start with the `simple` example.**
    It provides a comprehensive, heavily commented walkthrough of fundamental concepts:
    *   Registering custom User-Defined Types (UDTs).
    *   Creating tags (simple types, arrays, UDTs).
    *   Reading and writing tag values, including nested fields.
    *   Persisting tag values to a file.
    *   Creating in-process aliases between two database instances.

2.  **Next, review the `network_server` example.**
    This shows how to build a standalone server application that exposes a `TagDatabase` to the network. It demonstrates:
    *   Setting up a secure HTTPS server.
    *   Implementing bearer token authentication.
    *   Handling `GET` (read) and `PUT` (write) requests for tags.

3.  **Finally, explore the `network_client` example.**
    This example showcases one of the most powerful features of `honeycomb`: transparently linking a local application's tag to a tag on a remote server. It demonstrates:
    *   Configuring a secure network client.
    *   Creating a "remote alias" tag.
    *   Reading and writing to the remote tag as if it were a local tag.

By following this path, you will gain a solid understanding of both the core capabilities and the advanced distributed features of the `honeycomb` library.

---

To run any example, navigate to its specific directory (`simple`, `network_server`, etc.) and follow the instructions in its local `README.md` file.
