package main

import (
	"context"
	"fmt"
	"log"
	"sync"

	tags "github.com/apiarytech/honeycomb"
	"github.com/apiarytech/honeycomb/shared"
)

func main() {
	// Use a WaitGroup to ensure we don't exit the main function before the
	// server has a chance to signal that it is ready.
	var serverReady sync.WaitGroup
	serverReady.Add(1)

	// Initialize a new TagDatabase for the server.
	db := tags.NewTagDatabase()

	// Populate the database with some sample tags.
	shared.PopulateDB(db)

	// Define server configuration.
	port := "8080"
	certFile := "../shared/server.crt"
	keyFile := "../shared/server.key"
	validTokens := []string{"super-secret-token-123"}

	tags.StartServer(db, validTokens, port, certFile, keyFile, &serverReady, context.Background())

	// Wait for the server to finish its initialization.
	serverReady.Wait()

	fmt.Println("\n--- Honeycomb Network Server ---")
	log.Println("[Server] Server is running. Press Ctrl+C to stop.")

	// Block forever to keep the server running.
	select {}
}
