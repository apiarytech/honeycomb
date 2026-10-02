/*
 * Copyright (C) 2026 Franklin D. Amador
 *
 * This software is dual-licensed under:
 * - GPL v3.0
 * - Commercial
 *
 * You may choose to use this software under the terms of either license.
 * See the LICENSE files in the project root for full license text.
 */

package honeycomb

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	plc "github.com/apiarytech/royaljelly/iec"
)

func newReadTagDB(t *testing.T, stamp time.Time) *TagDatabase {
	t.Helper()
	db := NewTagDatabase()
	if err := db.AddTag(&Tag{Name: "Temp", TypeInfo: &TypeInfo{DataType: TypeREAL}, Value: plc.REAL(0)}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetTagValueQualityAt("Temp", plc.REAL(42.5), QualityGood, stamp); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestReadTagLocal(t *testing.T) {
	stamp := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	db := newReadTagDB(t, stamp)
	r, err := db.ReadTag("Temp")
	if err != nil {
		t.Fatal(err)
	}
	if r.Value != plc.REAL(42.5) || r.Quality != QualityGood || !r.Timestamp.Equal(stamp) {
		t.Fatalf("reading = %+v", r)
	}
	if r, err := db.ReadTag("Missing"); err == nil || r.Quality != QualityBad {
		t.Fatalf("missing tag: %+v, %v", r, err)
	}
}

func TestReadTagInProcessAlias(t *testing.T) {
	stamp := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	remote := newReadTagDB(t, stamp)
	local := NewTagDatabase()
	if err := local.RegisterDatabase("field", remote); err != nil {
		t.Fatal(err)
	}
	if err := local.AddTag(&Tag{Name: "FieldTemp", RemoteAlias: &RemoteAliasInfo{DBID: "field", TagName: "Temp"}}); err != nil {
		t.Fatal(err)
	}
	r, err := local.ReadTag("FieldTemp")
	if err != nil || r.Value != plc.REAL(42.5) || !r.Timestamp.Equal(stamp) {
		t.Fatalf("reading = %+v, %v", r, err)
	}
}

func TestReadTagOverNetwork(t *testing.T) {
	stamp := time.Date(2026, 10, 1, 8, 0, 0, 123000000, time.UTC)
	ts := &tagServer{db: newReadTagDB(t, stamp), validTokens: []string{"secret"}}
	mux := http.NewServeMux()
	mux.Handle("/tags/", ts.authMiddleware(http.HandlerFunc(ts.tagHandler)))
	server := httptest.NewServer(mux)
	defer server.Close()

	local := NewTagDatabase()
	client := &NetworkDatabaseClient{RemoteAddress: server.URL, Client: server.Client(), BearerToken: "secret"}
	if err := local.RegisterDatabase("field", client); err != nil {
		t.Fatal(err)
	}
	if err := local.AddTag(&Tag{Name: "FieldTemp", RemoteAlias: &RemoteAliasInfo{DBID: "field", TagName: "Temp"}}); err != nil {
		t.Fatal(err)
	}
	r, err := local.ReadTag("FieldTemp")
	if err != nil {
		t.Fatal(err)
	}
	// JSON carries numbers as float64.
	if r.Value != 42.5 || r.Quality != QualityGood || !r.Timestamp.Equal(stamp) {
		t.Fatalf("reading = %+v", r)
	}

	server.Close()
	if r, err := local.ReadTag("FieldTemp"); err == nil || r.Quality != QualityBad {
		t.Fatalf("unreachable server: %+v, %v", r, err)
	}
}
