/*
 * Copyright (C) 2026 Franklin D. Amador
 *
 * This software is dual-licensed under the terms of the GPL v3.0 and
 * a commercial license. You may choose to use this software under either
 * license.
 *
 * See the LICENSE files in the project root for full license text.
 */

// This file, batchread.go, reads many tags with one call: TagDatabase.ReadTags
// in process, and GET /tags?names=… or POST /tags over HTTP. A client that
// samples many tags then pays one round trip per poll instead of one per tag.
package honeycomb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// MaxReadTags caps the number of names in one batch read request.
const MaxReadTags = 10000

// ErrReadTagsUnsupported is returned by a NetworkDatabaseClient whose server
// has no batch read. Callers fall back to ReadTag per tag.
var ErrReadTagsUnsupported = errors.New("honeycomb: the server has no batch read")

// TagReader is implemented by TagDatabase and NetworkDatabaseClient, so a
// client reads tags the same way in process and over the network.
type TagReader interface {
	ReadTags(ctx context.Context, names []string) (map[string]Reading, error)
}

var (
	_ TagReader = (*TagDatabase)(nil)
	_ TagReader = (*NetworkDatabaseClient)(nil)
)

// ReadTags reads several tags at once; see ReadTag for the names it accepts.
// The result has an entry for every name. A name that cannot be read has
// QualityBad, and its error is joined into the returned error. If ctx ends
// first, it returns ctx's error; the readings are then incomplete.
func (db *TagDatabase) ReadTags(ctx context.Context, names []string) (map[string]Reading, error) {
	readings := make(map[string]Reading, len(names))
	var errs []error
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return readings, err
		}
		reading, err := db.ReadTag(name)
		readings[name] = reading
		if err != nil {
			errs = append(errs, err)
		}
	}
	return readings, errors.Join(errs...)
}

// readingWire is one tag in a batch read response.
type readingWire struct {
	Value     any       `json:"value"`
	Quality   Quality   `json:"quality"`
	Timestamp time.Time `json:"timestamp,omitzero"`
	Error     string    `json:"error,omitempty"`
}

// readTagsWire is the body of POST /tags and of a batch read response.
type readTagsWire struct {
	Names []string               `json:"names,omitempty"`
	Tags  map[string]readingWire `json:"tags,omitempty"`
}

// requestedNames returns the names of a GET /tags batch read: a comma-separated
// "names" parameter, repeated "name" parameters, or both.
func requestedNames(r *http.Request) []string {
	query := r.URL.Query()
	var names []string
	for _, list := range query["names"] {
		for _, name := range strings.Split(list, ",") {
			if name = strings.TrimSpace(name); name != "" {
				names = append(names, name)
			}
		}
	}
	for _, name := range query["name"] {
		if name != "" {
			names = append(names, name)
		}
	}
	return names
}

// handleReadTags serves a batch read. GET /tags?names=A,B carries the names in
// the URL; POST /tags with {"names": [...]} suits lists too long for a URL. The
// reply is {"tags": {"A": {"value": ..., "quality": 1, "timestamp": ...}, ...}};
// a tag that cannot be read has quality 3 (Bad) and an "error".
func (ts *tagServer) handleReadTags(w http.ResponseWriter, r *http.Request, names []string) {
	if len(names) > MaxReadTags {
		http.Error(w, fmt.Sprintf("too many names: %d (at most %d)", len(names), MaxReadTags), http.StatusBadRequest)
		return
	}
	response := readTagsWire{Tags: make(map[string]readingWire, len(names))}
	for _, name := range names {
		reading, err := ts.db.ReadTag(name)
		wire := readingWire{Value: reading.Value, Quality: reading.Quality, Timestamp: reading.Timestamp}
		if err != nil {
			wire.Error = err.Error()
		}
		response.Tags[name] = wire
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

// decodeReadTagsBody reads the names of a POST /tags batch read.
func decodeReadTagsBody(w http.ResponseWriter, r *http.Request) ([]string, error) {
	if r.Body == nil {
		return nil, errors.New("empty body")
	}
	var req readTagsWire
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		return nil, err
	}
	if len(req.Names) == 0 {
		return nil, errors.New(`"names" is empty`)
	}
	return req.Names, nil
}

// ReadTags reads several remote tags with one request (POST /tags). The result
// has an entry for every name; a name the server cannot read has QualityBad and
// its error is joined into the returned error. Values arrive as JSON values
// (float64, bool, string, map). It returns ErrReadTagsUnsupported if the server
// has no batch read.
func (ndc *NetworkDatabaseClient) ReadTags(ctx context.Context, names []string) (map[string]Reading, error) {
	if ndc.Client == nil {
		return nil, fmt.Errorf("NetworkDatabaseClient has a nil http.Client")
	}
	body, err := json.Marshal(readTagsWire{Names: names})
	if err != nil {
		return nil, err
	}
	url := strings.TrimSuffix(ndc.RemoteAddress, "/") + "/tags"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if ndc.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+ndc.BearerToken)
	}
	resp, err := ndc.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("network error reading tags: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		return nil, ErrReadTagsUnsupported
	default:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("remote server returned error for reading tags (%d): %s", resp.StatusCode, string(msg))
	}

	var reply readTagsWire
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
		return nil, fmt.Errorf("failed to decode tag readings: %w", err)
	}
	readings := make(map[string]Reading, len(names))
	var errs []error
	for _, name := range names {
		wire, ok := reply.Tags[name]
		if !ok {
			readings[name] = Reading{Quality: QualityBad}
			errs = append(errs, fmt.Errorf("tag '%s': missing from the server's reply", name))
			continue
		}
		readings[name] = Reading{Value: wire.Value, Quality: wire.Quality, Timestamp: wire.Timestamp}
		if wire.Error != "" {
			errs = append(errs, fmt.Errorf("tag '%s': %s", name, wire.Error))
		}
	}
	return readings, errors.Join(errs...)
}
