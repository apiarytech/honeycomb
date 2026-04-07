// Package honeycomb provides a thread-safe, in-memory database for managing
// PLC-like tags, adhering to IEC 61131-3 concepts. This file, network_client.go,
// specifically defines the client-side logic for accessing a remote TagDatabase
// over a network.
package honeycomb

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// NetworkDatabaseClient is an implementation of DatabaseAccessor that communicates
// with a remote TagDatabase server over a network. It allows a local TagDatabase
// instance to treat a remote database as one of its own data sources.
//
// This is achieved by:
//  1. Registering an instance of NetworkDatabaseClient with a local TagDatabase using `RegisterDatabase`.
//  2. Creating a local "alias" tag with `IsRemoteAlias: true` that points to the registered
//     NetworkDatabaseClient and the name of the tag on the remote server.
type NetworkDatabaseClient struct {
	// RemoteAddress is the base URL of the remote honeycomb server (e.g., "https://localhost:8080").
	RemoteAddress string
	// Client is the HTTP client used to make requests. It should be configured for security (e.g., TLS).
	Client *http.Client
	// BearerToken is the secret token sent in the Authorization header for authentication.
	BearerToken string
}

// getTagValueRecursive implements the DatabaseAccessor interface. It is called by a
// local TagDatabase when it needs to resolve the value of a remote alias tag.
// This method makes an HTTP GET request to the remote server to fetch the tag's value.
func (ndc *NetworkDatabaseClient) getTagValueRecursive(name string, depth int) (interface{}, error) {
	if ndc.Client == nil {
		return nil, fmt.Errorf("NetworkDatabaseClient has a nil http.Client")
	}

	// 1. Construct the full URL for the GET request (e.g., "https://localhost:8080/tags/MyRemoteTag").
	url := fmt.Sprintf("%s/tags/%s", strings.TrimSuffix(ndc.RemoteAddress, "/"), name)

	// 2. Create a new HTTP GET request object.
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP request for tag '%s': %w", name, err)
	}

	// 3. Add the Authorization header for authentication if a token is configured.
	if ndc.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+ndc.BearerToken)
	}

	// 4. Execute the HTTP request.
	resp, err := ndc.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("network error getting tag '%s': %w", name, err)
	}
	defer resp.Body.Close()

	// 5. Check if the server responded with a success status code.
	// If not, read the error message from the response body for better diagnostics.
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("remote server returned error for tag '%s' (%d): %s", name, resp.StatusCode, string(body))
	}

	// 6. The server is expected to respond with a JSON object like `{"value": ...}`.
	// We decode this response into a temporary struct to extract the value.
	var payload struct {
		Value interface{} `json:"value"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("failed to decode JSON response from remote server for tag '%s': %w", name, err)
	}

	return payload.Value, nil
}

// setTagValueRecursive implements the DatabaseAccessor interface. It is called by a
// local TagDatabase when a value is set on a remote alias tag.
// This method makes an HTTP PUT request to the remote server to update the tag's value.
func (ndc *NetworkDatabaseClient) setTagValueRecursive(name string, value interface{}, depth int) error {
	if ndc.Client == nil {
		return fmt.Errorf("NetworkDatabaseClient has a nil http.Client")
	}

	// 1. Construct the full URL for the PUT request.
	url := fmt.Sprintf("%s/tags/%s", strings.TrimSuffix(ndc.RemoteAddress, "/"), name)

	// 2. The server expects a JSON payload in the format `{"value": ...}`.
	// We create a map and marshal it to JSON.
	payload := map[string]interface{}{
		"value": value,
	}

	// 3. Marshal the payload into a JSON byte slice.
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal value for tag '%s' to JSON: %w", name, err)
	}

	// 4. Create a new HTTP PUT request with the JSON body.
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create HTTP request for tag '%s': %w", name, err)
	}
	req.Header.Set("Content-Type", "application/json")

	// 5. Add the Authorization header for authentication.
	if ndc.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+ndc.BearerToken)
	}

	// 6. Execute the HTTP request.
	resp, err := ndc.Client.Do(req)
	if err != nil {
		return fmt.Errorf("network error setting tag '%s': %w", name, err)
	}
	defer resp.Body.Close()

	// 7. Check for a successful status code and return an error if the update failed.
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("remote server returned error for setting tag '%s' (%d): %s", name, resp.StatusCode, string(respBody))
	}

	return nil
}
