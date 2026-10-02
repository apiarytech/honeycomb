/*
 * Copyright (C) 2026 Franklin D. Amador
 *
 * This software is dual-licensed under the terms of the GPL v3.0 and
 * a commercial license. You may choose to use this software under either
 * license.
 *
 * See the LICENSE files in the project root for full license text.
 */

package honeycomb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	plc "github.com/apiarytech/royaljelly/iec"
)

func newBatchDB(t *testing.T) (*TagDatabase, time.Time) {
	t.Helper()
	stamp := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	db := NewTagDatabase()
	for _, tag := range []*Tag{
		{Name: "Temp", TypeInfo: &TypeInfo{DataType: TypeREAL}, Value: plc.REAL(0)},
		{Name: "Lid", TypeInfo: &TypeInfo{DataType: TypeBOOL}, Value: plc.BOOL(false)},
		{Name: "Arr", TypeInfo: &TypeInfo{DataType: TypeARRAY, ElementType: TypeDINT}, Value: []plc.DINT{7, 8}},
	} {
		if err := db.AddTag(tag); err != nil {
			t.Fatal(err)
		}
	}
	_ = db.SetTagValueQualityAt("Temp", plc.REAL(42.5), QualityGood, stamp)
	_ = db.SetTagValueQuality("Lid", plc.BOOL(true), QualityUncertain)
	return db, stamp
}

func TestReadTagsLocal(t *testing.T) {
	db, stamp := newBatchDB(t)
	readings, err := db.ReadTags(context.Background(), []string{"Temp", "Lid", "Arr[1]", "Missing"})
	if err == nil || !strings.Contains(err.Error(), "Missing") {
		t.Errorf("error = %v, want one naming Missing", err)
	}
	if r := readings["Temp"]; r.Value != plc.REAL(42.5) || r.Quality != QualityGood || !r.Timestamp.Equal(stamp) {
		t.Errorf("Temp = %+v", r)
	}
	if r := readings["Lid"]; r.Value != plc.BOOL(true) || r.Quality != QualityUncertain {
		t.Errorf("Lid = %+v", r)
	}
	if r := readings["Arr[1]"]; r.Value != plc.DINT(8) {
		t.Errorf("Arr[1] = %+v", r)
	}
	if r, ok := readings["Missing"]; !ok || r.Quality != QualityBad {
		t.Errorf("Missing = %+v (present %v), want QualityBad", r, ok)
	}

	// A cancelled context stops the batch.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := db.ReadTags(ctx, []string{"Temp"}); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled ReadTags: %v, want context.Canceled", err)
	}
}

func startBatchServer(t *testing.T, db *TagDatabase) *httptest.Server {
	t.Helper()
	ts := &tagServer{db: db, validTokens: []string{"secret"}}
	mux := http.NewServeMux()
	mux.Handle("/tags", ts.authMiddleware(http.HandlerFunc(ts.handleGetAllTags)))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func authorized(t *testing.T, method, url string, body []byte) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	return req
}

func TestReadTagsOverHTTP(t *testing.T) {
	db, stamp := newBatchDB(t)
	server := startBatchServer(t, db)

	// GET with a comma-separated list plus a repeated parameter.
	resp, err := server.Client().Do(authorized(t, http.MethodGet, server.URL+"/tags?names=Temp,Lid&name=Missing", nil))
	if err != nil {
		t.Fatal(err)
	}
	var reply readTagsWire
	_ = json.NewDecoder(resp.Body).Decode(&reply)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(reply.Tags) != 3 {
		t.Fatalf("GET = %d %+v", resp.StatusCode, reply)
	}
	if temp := reply.Tags["Temp"]; temp.Value != 42.5 || temp.Quality != QualityGood || !temp.Timestamp.Equal(stamp) {
		t.Errorf("Temp = %+v", temp)
	}
	if missing := reply.Tags["Missing"]; missing.Quality != QualityBad || missing.Error == "" {
		t.Errorf("Missing = %+v, want Bad with an error", missing)
	}

	// Without names, GET /tags still lists every tag.
	resp, err = server.Client().Do(authorized(t, http.MethodGet, server.URL+"/tags", nil))
	if err != nil {
		t.Fatal(err)
	}
	var all []json.RawMessage
	_ = json.NewDecoder(resp.Body).Decode(&all)
	resp.Body.Close()
	if len(all) != 3 {
		t.Errorf("GET /tags listed %d tags, want 3", len(all))
	}

	// Too many names, and no names, are rejected.
	names := make([]string, MaxReadTags+1)
	for i := range names {
		names[i] = fmt.Sprint("T", i)
	}
	tooMany, _ := json.Marshal(readTagsWire{Names: names})
	for label, body := range map[string][]byte{"too many": tooMany, "empty": []byte(`{"names": []}`)} {
		resp, err := server.Client().Do(authorized(t, http.MethodPost, server.URL+"/tags", body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", label, resp.StatusCode)
		}
	}
}

func TestReadTagsClient(t *testing.T) {
	db, stamp := newBatchDB(t)
	server := startBatchServer(t, db)
	client := &NetworkDatabaseClient{RemoteAddress: server.URL, Client: server.Client(), BearerToken: "secret"}

	readings, err := client.ReadTags(context.Background(), []string{"Temp", "Lid", "Missing"})
	if err == nil || !strings.Contains(err.Error(), "Missing") {
		t.Errorf("error = %v, want one naming Missing", err)
	}
	// JSON carries numbers as float64 and BOOL as bool.
	if r := readings["Temp"]; r.Value != 42.5 || r.Quality != QualityGood || !r.Timestamp.Equal(stamp) {
		t.Errorf("Temp = %+v", r)
	}
	if r := readings["Lid"]; r.Value != true || r.Quality != QualityUncertain {
		t.Errorf("Lid = %+v", r)
	}
	if r := readings["Missing"]; r.Quality != QualityBad {
		t.Errorf("Missing = %+v", r)
	}

	// A server without batch read.
	old := httptest.NewServer(http.NotFoundHandler())
	defer old.Close()
	oldClient := &NetworkDatabaseClient{RemoteAddress: old.URL, Client: old.Client()}
	if _, err := oldClient.ReadTags(context.Background(), []string{"Temp"}); !errors.Is(err, ErrReadTagsUnsupported) {
		t.Errorf("old server: %v, want ErrReadTagsUnsupported", err)
	}
}
