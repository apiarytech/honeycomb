/*
 * Copyright (C) 2026 Franklin D. Amador
 *
 * This software is dual-licensed under the terms of the GPL v3.0 and
 * a commercial license. You may choose to use this software under either
 * license.
 *
 * See the LICENSE files in the project root for full license text.
 */

// This file, changefeed_network.go, serves the change feed over HTTP
// (POST /changes) and reads it from a remote server.
package honeycomb

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// changesWire is the JSON body of POST /changes; the wait is in milliseconds.
type changesWire struct {
	Since  uint64   `json:"since"`
	Epoch  string   `json:"epoch,omitempty"`
	Names  []string `json:"names,omitempty"`
	WaitMS int64    `json:"wait_ms,omitempty"`
	Max    int      `json:"max,omitempty"`
}

// handleChanges serves POST /changes: the body is a changesWire and the reply
// a ChangeBatch. It waits up to wait_ms (at most MaxChangesWait) for changes.
func (ts *tagServer) handleChanges(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}
	var req changesWire
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	batch, err := ts.db.Changes(r.Context(), ChangesRequest{
		Since: req.Since, Epoch: req.Epoch, Names: req.Names,
		Wait: time.Duration(req.WaitMS) * time.Millisecond, Max: req.Max,
	})
	if err != nil { // the client went away
		return
	}
	if batch.Changes == nil {
		batch.Changes = []Change{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(batch)
}

// Changes reads the remote server's change feed with one request, waiting up
// to req.Wait. It returns ErrChangesUnsupported if the server has no feed.
// Values arrive as JSON values (float64, bool, string, map).
func (ndc *NetworkDatabaseClient) Changes(ctx context.Context, req ChangesRequest) (ChangeBatch, error) {
	if ndc.Client == nil {
		return ChangeBatch{}, fmt.Errorf("NetworkDatabaseClient has a nil http.Client")
	}
	body, err := json.Marshal(changesWire{
		Since: req.Since, Epoch: req.Epoch, Names: req.Names,
		WaitMS: min(req.Wait, MaxChangesWait).Milliseconds(), Max: req.Max,
	})
	if err != nil {
		return ChangeBatch{}, err
	}
	url := strings.TrimSuffix(ndc.RemoteAddress, "/") + "/changes"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return ChangeBatch{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if ndc.BearerToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+ndc.BearerToken)
	}
	// The request may wait for changes longer than the client's own timeout.
	client := *ndc.Client
	if client.Timeout > 0 {
		client.Timeout += req.Wait
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return ChangeBatch{}, fmt.Errorf("network error reading changes: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		return ChangeBatch{}, ErrChangesUnsupported
	default:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return ChangeBatch{}, fmt.Errorf("remote server returned error for changes (%d): %s", resp.StatusCode, string(msg))
	}
	var batch ChangeBatch
	if err := json.NewDecoder(resp.Body).Decode(&batch); err != nil {
		return ChangeBatch{}, fmt.Errorf("failed to decode changes: %w", err)
	}
	return batch, nil
}
