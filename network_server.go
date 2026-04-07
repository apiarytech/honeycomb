// Package honeycomb provides a thread-safe, in-memory database for managing
// PLC-like tags. This file, network_server.go, provides the public-facing
// function to start a network server that exposes a TagDatabase instance
// over HTTP/S.
package honeycomb

import (
	"log"
	"net/http"
	"sync"
)

// StartServer initializes and runs the HTTP/S server in a background goroutine.
// It configures the server with the necessary handlers and authentication middleware
// to serve tag data from the provided TagDatabase.
//
// Parameters:
//   - db: The TagDatabase instance that the server will expose.
//   - validTokens: A slice of strings representing the valid Bearer tokens for authentication.
//   - port: The network port on which the server will listen (e.g., "8080").
//   - certFile: The path to the TLS certificate file for enabling HTTPS.
//   - keyFile: The path to the TLS private key file for enabling HTTPS.
//   - serverReady: A *sync.WaitGroup used to signal when the server has completed its
//     initial setup and is about to start listening for connections. The caller can
//     use this to wait until the server is ready before proceeding. It can be nil.
func StartServer(db *TagDatabase, validTokens []string, port, certFile, keyFile string, serverReady *sync.WaitGroup) {
	// 1. Create an instance of the internal `tagServer` struct, which holds the
	//    database and authentication tokens.
	server := &tagServer{
		db:          db,
		validTokens: validTokens,
	}
	// 2. Create a new HTTP request multiplexer (router).
	mux := http.NewServeMux()
	// 3. Register the handler for the `/tags/` endpoint. All requests to this path
	//    will first pass through the authentication middleware and then be handled
	//    by the `tagHandler` method.
	mux.Handle("/tags/", server.authMiddleware(http.HandlerFunc(server.tagHandler)))

	log.Printf("[Server] Starting honeycomb network server on port %s...", port)

	// 4. Signal that the server setup is complete and it's about to start listening.
	if serverReady != nil {
		serverReady.Done()
	}

	// 5. Start the HTTPS server in a new goroutine so it doesn't block the main thread.
	go func() {
		if err := http.ListenAndServeTLS(":"+port, certFile, keyFile, mux); err != nil {
			log.Fatalf("[Server] Failed to start: %v", err)
		}
	}()
}
