package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	tags "github.com/apiarytech/honeycomb"
	"github.com/apiarytech/honeycomb/shared"
	plc "github.com/apiarytech/royaljelly"
)

// --- Client Implementation ---
// The main function acts as the client application.
func main() {
	// --- 1. Start the server in the background ---
	// Use a WaitGroup to ensure the client doesn't start making requests
	// before the server is ready to listen.
	var serverReady sync.WaitGroup
	serverReady.Add(1)
	go shared.StartServer(&serverReady)
	// Wait for the server to signal that it's ready.
	serverReady.Wait()

	fmt.Println("\n--- Network Client Example ---")

	// --- 2. Create the client-side database instance ---
	clientDB := tags.NewTagDatabase()
	fmt.Println("[Client] Created local TagDatabase instance.")

	// --- 3. Create a secure HTTP client for TLS ---
	// Load the server's certificate file
	caCert, err := os.ReadFile("../shared/server.crt")
	if err != nil {
		log.Fatalf("[Client] Failed to read server certificate: %v", err)
	}
	// Create a certificate pool and add the server's cert to it
	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM(caCert)

	// Create a custom TLS configuration that trusts our certificate pool
	tlsConfig := &tls.Config{
		RootCAs: caCertPool,
	}

	// Create a custom HTTP transport with our TLS config
	transport := &http.Transport{
		TLSClientConfig: tlsConfig,
	}

	// Create the final HTTP client
	secureClient := &http.Client{
		Transport: transport,
		Timeout:   5 * time.Second,
	}

	// --- 4. Create and register the Network Client ---
	// This client knows how to talk to our server.
	networkClient := &tags.NetworkDatabaseClient{
		RemoteAddress: "https://localhost:8080", // Use https://
		Client:        secureClient,             // Use our new secure client
		BearerToken:   "super-secret-token-123", // Set the authentication token
	}

	// Register the network client, giving it a local ID.
	clientDB.RegisterDatabase("ServerDB", networkClient)
	fmt.Println("[Client] Registered the NetworkDatabaseClient as 'ServerDB'.")

	// --- 5. Create a remote alias tag ---
	// This tag lives in our clientDB but points to a tag on the server.
	aliasTag := &tags.Tag{
		Name:          "RemoteMotorSpeed",
		IsRemoteAlias: true,
		RemoteDBID:    "ServerDB",           // The ID we just registered.
		RemoteTagName: "MotorLine[0].Speed", // The full name of the tag on the server.
	}
	if err := clientDB.AddTag(aliasTag); err != nil {
		log.Fatalf("[Client] Failed to add alias tag: %v", err)
	}
	fmt.Println("[Client] Created alias 'RemoteMotorSpeed' pointing to 'MotorLine[0].Speed' on the server.")

	// --- 6. Read the value through the alias ---
	fmt.Println("\n[Client] Reading initial value through the alias...")
	val, err := clientDB.GetTagValue("RemoteMotorSpeed")
	if err != nil {
		log.Fatalf("[Client] GetTagValue on alias failed: %v", err)
	}
	fmt.Printf("[Client] Successfully read value: %v (Type: %T)\n", val, val)

	// --- 7. Write a new value through the alias ---
	fmt.Println("\n[Client] Writing new value '2150.75' through the alias...")
	newValue := plc.REAL(2150.75)
	if err := clientDB.SetTagValue("RemoteMotorSpeed", newValue); err != nil {
		log.Fatalf("[Client] SetTagValue on alias failed: %v", err)
	}
	fmt.Println("[Client] Write operation completed successfully.")

	// --- 8. Read the value again to verify the change ---
	fmt.Println("\n[Client] Reading value again to confirm the write...")
	val, err = clientDB.GetTagValue("RemoteMotorSpeed")
	if err != nil {
		log.Fatalf("[Client] GetTagValue after write failed: %v", err)
	}
	fmt.Printf("[Client] Successfully read updated value: %v\n", val)

	fmt.Println("\n--- Example Finished ---")
}
