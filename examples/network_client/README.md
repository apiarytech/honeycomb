# Honeycomb TagDatabase - Network Client Example

This example demonstrates a powerful feature of the `honeycomb` package: **networked cross-database aliasing**. It shows how a client application can create a local tag that acts as a transparent alias for a tag residing on a remote `honeycomb` server.

This program is self-contained. It starts a background HTTPS server (similar to the `network_server` example) and then runs a client that connects to it.

## Features Demonstrated

*   **`NetworkDatabaseClient`**: The core of the example. It shows how to instantiate and configure a `tags.NetworkDatabaseClient` to communicate with a remote `honeycomb` server.
*   **Secure Client Configuration**: The client is configured to use TLS by trusting the server's self-signed certificate, ensuring secure communication.
*   **Remote Alias Creation**: A local tag (`RemoteMotorSpeed`) is created with the `IsRemoteAlias` flag set. It's configured to point to a tag on the remote server (`MotorLine[0].Speed`) via the registered `NetworkDatabaseClient`.
*   **Transparent Read/Write**:
    *   Calling `GetTagValue("RemoteMotorSpeed")` on the client-side database transparently makes an authenticated `GET` request to the server to fetch the value.
    *   Calling `SetTagValue("RemoteMotorSpeed", ...)` on the client-side database transparently makes an authenticated `PUT` request to the server to update the value.
*   **In-Process Server**: The server is started in a background goroutine to make the example easy to run without external dependencies.

## Prerequisites

This example requires a private key and certificate file for its internal server. You can generate a self-signed pair for testing purposes using OpenSSL:

```bash
openssl req -x509 -newkey rsa:4096 -keyout server.key -out server.crt -sha256 -days 365 -nodes -subj "/CN=localhost"
```

This command will create `server.key` and `server.crt` in the current directory.

## How to Run

1.  Make sure you have generated the `server.key` and `server.crt` files as described above.
2.  Navigate to this directory (`examples/network_client`) in your terminal.
3.  Run the program:

    ```bash
    go run main.go
    ```

## Expected Output

The program will print a series of logs from both the server and the client as it executes the demonstration. You will see:

1.  The server starting up.
2.  The client creating its local database and registering the network client.
3.  The client creating the remote alias tag.
4.  The client reading the initial value, which triggers a `GET` request on the server.
5.  The client writing a new value, which triggers a `PUT` request on the server.
6.  The client reading the value again to confirm the write was successful.

```
[Server] Starting honeycomb network server on port 8080...

--- Network Client Example ---
[Client] Created local TagDatabase instance.
[Client] Registered the NetworkDatabaseClient as 'ServerDB'.
[Client] Created alias 'RemoteMotorSpeed' pointing to 'MotorLine.Speed' on the server.

[Client] Reading initial value through the alias...
[Server] GET /tags/MotorLine.Speed
[Client] Successfully read value: 1500 (Type: float64)

[Client] Writing new value '2150.75' through the alias...
[Server] PUT /tags/MotorLine.Speed
[Client] Write operation completed successfully.

[Client] Reading value again to confirm the write...
[Server] GET /tags/MotorLine.Speed
[Client] Successfully read updated value: 2150.75

--- Example Finished ---
```