# Honeycomb TagDatabase - Network Server Example

This example demonstrates how to create a standalone, secure HTTPS server to expose a `honeycomb` TagDatabase over the network. It showcases handling authenticated read and write requests for PLC-like tags.

## Features Demonstrated

*   **HTTPS Server**: Serves tag data using `http.ListenAndServeTLS` for secure communication.
*   **Bearer Token Authentication**: Implements an `authMiddleware` to protect the `/tags/` endpoint, requiring a valid `Authorization: Bearer <token>` header for all requests.
*   **GET /tags/{tagName}**: Handles `GET` requests to read the value of a specific tag. The response is a JSON object (e.g., `{"value": 100}`).
*   **PUT /tags/{tagName}**: Handles `PUT` requests to update the value of a specific tag from a JSON payload (e.g., `{"value": 200}`).
*   **UDT and Array Support**: The server is initialized with a sample `DINT` tag and an `ARRAY` of a custom `MotorData` User-Defined Type (UDT).

## Prerequisites

This server uses TLS and requires a private key and certificate file. You can generate a self-signed pair for testing purposes using OpenSSL:

```bash
openssl req -x509 -newkey rsa:4096 -keyout server.key -out server.crt -sha256 -days 365 -nodes -subj "/CN=localhost"
```

This command will create `server.key` and `server.crt` in the current directory.

## How to Run

1.  Make sure you have generated the `server.key` and `server.crt` files as described above.
2.  Navigate to this directory (`examples/network_server`) in your terminal.
3.  Run the program:

    ```bash
    go run main.go
    ```

The server will start and listen on `https://localhost:8080`.

## How to Interact

You can use a tool like `curl` to interact with the server. The hardcoded authentication token in this example is `super-secret-token-123`.

Since the server uses a self-signed certificate, you'll need to use the `-k` or `--insecure` flag with `curl`.

### Read a Tag Value (GET)

```bash
# Read the value of the 'MyDINT' tag
curl -k -H "Authorization: Bearer super-secret-token-123" https://localhost:8080/tags/MyDINT

# Read a nested field from a UDT in an array
curl -k -H "Authorization: Bearer super-secret-token-123" https://localhost:8080/tags/MotorLine.Speed
```

### Update a Tag Value (PUT)

```bash
# Update the value of the 'MyDINT' tag to 250
curl -k -X PUT -H "Authorization: Bearer super-secret-token-123" -H "Content-Type: application/json" -d '{"value": 250}' https://localhost:8080/tags/MyDINT
```

