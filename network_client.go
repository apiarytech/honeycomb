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
	"time"
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

// tagResponse is the JSON body the server returns for GET /tags/{name}.
type tagResponse struct {
	Value interface{} `json:"value"`
	// Quality is nil when the server predates tag quality.
	Quality *Quality `json:"quality"`
}

// getTagValueRecursive implements the DatabaseAccessor interface. It is called by a
// local TagDatabase when it needs to resolve the value of a remote alias tag.
func (ndc *NetworkDatabaseClient) getTagValueRecursive(name string, depth int) (interface{}, error) {
	payload, err := ndc.getTag(name)
	if err != nil {
		return nil, err
	}
	return payload.Value, nil
}

// getTagQualityRecursive implements the DatabaseAccessor interface. It reads the
// remote tag's quality; a server that predates quality reports QualityUnknown.
// On error the quality is QualityBad, so an unreachable server reads as Bad.
func (ndc *NetworkDatabaseClient) getTagQualityRecursive(name string, depth int) (Quality, error) {
	payload, err := ndc.getTag(name)
	if err != nil {
		return QualityBad, err
	}
	if payload.Quality == nil {
		return QualityUnknown, nil
	}
	return *payload.Quality, nil
}

// setTagValueRecursive implements the DatabaseAccessor interface. It is called by a
// local TagDatabase when a value is set on a remote alias tag.
func (ndc *NetworkDatabaseClient) setTagValueRecursive(name string, value interface{}, quality Quality, timestamp time.Time, depth int) error {
	payload := map[string]interface{}{"value": value, "quality": quality}
	if !timestamp.IsZero() {
		payload["timestamp"] = timestamp // otherwise the server stamps the write itself
	}
	return ndc.putTag(name, payload)
}

// setTagQualityRecursive implements the DatabaseAccessor interface. It changes the
// remote tag's quality without touching its value.
func (ndc *NetworkDatabaseClient) setTagQualityRecursive(name string, quality Quality, depth int) error {
	return ndc.putTag(name, map[string]interface{}{"quality": quality})
}

// getTag makes an HTTP GET request to the remote server for a tag.
func (ndc *NetworkDatabaseClient) getTag(name string) (tagResponse, error) {
	if ndc.Client == nil {
		return tagResponse{}, fmt.Errorf("NetworkDatabaseClient has a nil http.Client")
	}

	// 1. Construct the full URL for the GET request (e.g., "https://localhost:8080/tags/MyRemoteTag").
	url := fmt.Sprintf("%s/tags/%s", strings.TrimSuffix(ndc.RemoteAddress, "/"), name)

	// 2. Create a new HTTP GET request object.
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return tagResponse{}, fmt.Errorf("failed to create HTTP request for tag '%s': %w", name, err)
	}

	// 3. Add the Authorization header for authentication if a token is configured.
	if ndc.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+ndc.BearerToken)
	}

	// 4. Execute the HTTP request.
	resp, err := ndc.Client.Do(req)
	if err != nil {
		return tagResponse{}, fmt.Errorf("network error getting tag '%s': %w", name, err)
	}
	defer resp.Body.Close()

	// 5. Check if the server responded with a success status code.
	// If not, read the error message from the response body for better diagnostics.
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return tagResponse{}, fmt.Errorf("remote server returned error for tag '%s' (%d): %s", name, resp.StatusCode, string(body))
	}

	// 6. The server is expected to respond with a JSON object like `{"value": ..., "quality": 1}`.
	var payload tagResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return tagResponse{}, fmt.Errorf("failed to decode JSON response from remote server for tag '%s': %w", name, err)
	}
	return payload, nil
}

// putTag makes an HTTP PUT request to the remote server to update a tag.
func (ndc *NetworkDatabaseClient) putTag(name string, payload map[string]interface{}) error {
	if ndc.Client == nil {
		return fmt.Errorf("NetworkDatabaseClient has a nil http.Client")
	}

	// 1. Construct the full URL for the PUT request.
	url := fmt.Sprintf("%s/tags/%s", strings.TrimSuffix(ndc.RemoteAddress, "/"), name)

	// 2. Marshal the payload, e.g. `{"value": ..., "quality": 1}`, into a JSON byte slice.
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal value for tag '%s' to JSON: %w", name, err)
	}

	// 3. Create a new HTTP PUT request with the JSON body.
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create HTTP request for tag '%s': %w", name, err)
	}
	req.Header.Set("Content-Type", "application/json")

	// 4. Add the Authorization header for authentication.
	if ndc.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+ndc.BearerToken)
	}

	// 5. Execute the HTTP request.
	resp, err := ndc.Client.Do(req)
	if err != nil {
		return fmt.Errorf("network error setting tag '%s': %w", name, err)
	}
	defer resp.Body.Close()

	// 6. Check for a successful status code and return an error if the update failed.
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("remote server returned error for setting tag '%s' (%d): %s", name, resp.StatusCode, string(respBody))
	}

	return nil
}
