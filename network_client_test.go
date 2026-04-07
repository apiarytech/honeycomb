/*
 * Copyright (C) 2026 Franklin D. Amador
 *
 * This software is dual-licensed under:
 * - GPL v2.0
 * - Commercial
 *
 * You may choose to use this software under the terms of either license.
 * See the LICENSE files in the project root for full license text.
 */

package honeycomb

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	plc "github.com/apiarytech/royaljelly"
)

// TestNetworkClient_GetTagValue tests the getTagValueRecursive method of the NetworkDatabaseClient.
func TestNetworkClient_GetTagValue(t *testing.T) {
	// Test case 1: Successful retrieval
	t.Run("Success", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Verify the request is correct
			if r.Method != http.MethodGet {
				t.Errorf("Expected GET request, got %s", r.Method)
			}
			if r.URL.Path != "/tags/MyTag" {
				t.Errorf("Expected path /tags/MyTag, got %s", r.URL.Path)
			}
			// Send a successful response
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{"value": 123.45})
		}))
		defer server.Close()

		client := &NetworkDatabaseClient{
			RemoteAddress: server.URL,
			Client:        server.Client(),
		}

		value, err := client.getTagValueRecursive("MyTag", 0)
		if err != nil {
			t.Fatalf("Expected no error, got %v", err)
		}
		if val, ok := value.(float64); !ok || val != 123.45 {
			t.Errorf("Expected value 123.45, got %v", value)
		}
	})

	// Test case 2: Server returns an error
	t.Run("ServerError", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "Tag not found", http.StatusNotFound)
		}))
		defer server.Close()

		client := &NetworkDatabaseClient{
			RemoteAddress: server.URL,
			Client:        server.Client(),
		}

		_, err := client.getTagValueRecursive("MyTag", 0)
		if err == nil {
			t.Fatal("Expected an error, but got nil")
		}
		if !strings.Contains(err.Error(), "404") {
			t.Errorf("Expected error to contain '404', got: %v", err)
		}
	})
}

// TestNetworkClient_SetTagValue tests the setTagValueRecursive method of the NetworkDatabaseClient.
func TestNetworkClient_SetTagValue(t *testing.T) {
	// Test case 1: Successful update
	t.Run("Success", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Verify the request is correct
			if r.Method != http.MethodPut {
				t.Errorf("Expected PUT request, got %s", r.Method)
			}
			if r.Header.Get("Content-Type") != "application/json" {
				t.Errorf("Expected Content-Type application/json, got %s", r.Header.Get("Content-Type"))
			}

			// Decode the body to verify the payload
			var payload map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("Failed to decode request body: %v", err)
			}
			if val, ok := payload["value"].(float64); !ok || val != 543.21 {
				t.Errorf("Expected payload value 543.21, got %v", payload["value"])
			}

			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		client := &NetworkDatabaseClient{
			RemoteAddress: server.URL,
			Client:        server.Client(),
		}

		err := client.setTagValueRecursive("MyTag", plc.REAL(543.21), 0)
		if err != nil {
			t.Fatalf("Expected no error, got %v", err)
		}
	})

	// Test case 2: Server returns an error
	t.Run("ServerError", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "Invalid value", http.StatusBadRequest)
		}))
		defer server.Close()

		client := &NetworkDatabaseClient{
			RemoteAddress: server.URL,
			Client:        server.Client(),
		}

		err := client.setTagValueRecursive("MyTag", plc.REAL(1.0), 0)
		if err == nil {
			t.Fatal("Expected an error, but got nil")
		}
		if !strings.Contains(err.Error(), "400") {
			t.Errorf("Expected error to contain '400', got: %v", err)
		}
	})
}

// TestNetworkClient_Authentication tests that the Bearer token is correctly added to requests.
func TestNetworkClient_Authentication(t *testing.T) {
	const expectedToken = "my-secret-test-token"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		expectedHeader := "Bearer " + expectedToken
		if authHeader != expectedHeader {
			t.Errorf("Incorrect Authorization header. Got '%s', want '%s'", authHeader, expectedHeader)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := &NetworkDatabaseClient{
		RemoteAddress: server.URL,
		Client:        server.Client(),
		BearerToken:   expectedToken,
	}

	// Test GET
	t.Run("GET with Auth", func(t *testing.T) {
		// We don't care about the response body, just that the request succeeds (implying auth was correct)
		_, err := client.getTagValueRecursive("any-tag", 0)
		if err != nil {
			// The error message will contain "Unauthorized" if the header was wrong.
			if strings.Contains(err.Error(), "Unauthorized") {
				t.Fatalf("Authentication failed unexpectedly: %v", err)
			}
			// Ignore other errors for this test, as we are only focused on the auth header.
		}
	})

	// Test PUT
	t.Run("PUT with Auth", func(t *testing.T) {
		err := client.setTagValueRecursive("any-tag", plc.INT(1), 0)
		if err != nil {
			if strings.Contains(err.Error(), "Unauthorized") {
				t.Fatalf("Authentication failed unexpectedly: %v", err)
			}
		}
	})
}
