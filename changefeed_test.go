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
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	plc "github.com/apiarytech/royaljelly/iec"
)

func newFeedDB(t *testing.T) *TagDatabase {
	t.Helper()
	db := NewTagDatabase()
	for _, tag := range []*Tag{
		{Name: "Lid", TypeInfo: &TypeInfo{DataType: TypeBOOL}, Value: plc.BOOL(false)},
		{Name: "Temp", TypeInfo: &TypeInfo{DataType: TypeREAL}, Value: plc.REAL(0)},
	} {
		if err := db.AddTag(tag); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func start(t *testing.T, src ChangeSource) ChangeBatch {
	t.Helper()
	b, err := src.Changes(context.Background(), ChangesRequest{})
	if err != nil || len(b.Changes) != 0 || b.Gap || b.Epoch == "" {
		t.Fatalf("start = %+v, %v", b, err)
	}
	return b
}

// A pulse shorter than any poll is still seen: every change is kept in order.
func TestChangesKeepEveryChange(t *testing.T) {
	db := newFeedDB(t)
	pos := start(t, db)
	stamp := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	_ = db.SetTagValueQualityAt("Lid", plc.BOOL(true), QualityGood, stamp)
	_ = db.SetTagValueQualityAt("Lid", plc.BOOL(false), QualityGood, stamp.Add(50*time.Millisecond))
	_ = db.SetTagValue("Temp", plc.REAL(39))

	b, err := db.Changes(context.Background(), ChangesRequest{Since: pos.Next, Epoch: pos.Epoch, Names: []string{"Lid"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Changes) != 2 || b.Changes[0].Value != plc.BOOL(true) || b.Changes[1].Value != plc.BOOL(false) ||
		!b.Changes[1].Timestamp.Equal(stamp.Add(50*time.Millisecond)) || b.Gap {
		t.Fatalf("batch = %+v", b)
	}
	if b.Next != pos.Next+3 {
		t.Fatalf("Next = %d, want %d (filtered changes still advance it)", b.Next, pos.Next+3)
	}
	if b, _ := db.Changes(context.Background(), ChangesRequest{Since: b.Next, Epoch: b.Epoch}); len(b.Changes) != 0 {
		t.Fatalf("nothing new expected: %+v", b)
	}
}

func TestChangesWaitForAChange(t *testing.T) {
	db := newFeedDB(t)
	pos := start(t, db)
	go func() {
		time.Sleep(30 * time.Millisecond)
		_ = db.SetTagValue("Temp", plc.REAL(1))
	}()
	began := time.Now()
	b, err := db.Changes(context.Background(), ChangesRequest{Since: pos.Next, Epoch: pos.Epoch, Wait: 5 * time.Second})
	if err != nil || len(b.Changes) != 1 || time.Since(began) > 2*time.Second {
		t.Fatalf("batch = %+v, %v after %v", b, err, time.Since(began))
	}
	// With nothing new the call returns when the wait is over.
	b, err = db.Changes(context.Background(), ChangesRequest{Since: b.Next, Epoch: b.Epoch, Wait: 20 * time.Millisecond})
	if err != nil || len(b.Changes) != 0 {
		t.Fatalf("idle batch = %+v, %v", b, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := db.Changes(ctx, ChangesRequest{Since: b.Next, Epoch: b.Epoch, Wait: time.Second}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait: %v", err)
	}
}

func TestChangesGap(t *testing.T) {
	db := newFeedDB(t)
	db.SetChangeFeedCapacity(3)
	pos := start(t, db)
	for i := range 5 {
		_ = db.SetTagValue("Temp", plc.REAL(float32(i)))
	}
	b, _ := db.Changes(context.Background(), ChangesRequest{Since: pos.Next, Epoch: pos.Epoch})
	if !b.Gap || len(b.Changes) != 3 || b.Changes[0].Value != plc.REAL(2) {
		t.Fatalf("overrun: %+v", b)
	}
	// Another epoch means the database restarted.
	b, _ = db.Changes(context.Background(), ChangesRequest{Since: b.Next, Epoch: "old"})
	if !b.Gap || len(b.Changes) != 0 {
		t.Fatalf("restart: %+v", b)
	}
}

func TestChangesMax(t *testing.T) {
	db := newFeedDB(t)
	pos := start(t, db)
	for i := range 5 {
		_ = db.SetTagValue("Temp", plc.REAL(float32(i)))
	}
	b, _ := db.Changes(context.Background(), ChangesRequest{Since: pos.Next, Epoch: pos.Epoch, Max: 2})
	if len(b.Changes) != 2 || b.Next != pos.Next+2 {
		t.Fatalf("first page: %+v", b)
	}
	b, _ = db.Changes(context.Background(), ChangesRequest{Since: b.Next, Epoch: b.Epoch, Max: 10})
	if len(b.Changes) != 3 {
		t.Fatalf("second page: %+v", b)
	}
}

func TestChangesOverNetwork(t *testing.T) {
	db := newFeedDB(t)
	ts := &tagServer{db: db, validTokens: []string{"secret"}}
	mux := http.NewServeMux()
	mux.Handle("/changes", ts.authMiddleware(http.HandlerFunc(ts.handleChanges)))
	server := httptest.NewServer(mux)
	defer server.Close()
	client := &NetworkDatabaseClient{RemoteAddress: server.URL, Client: &http.Client{Timeout: time.Second}, BearerToken: "secret"}

	pos := start(t, client)
	go func() {
		time.Sleep(30 * time.Millisecond)
		_ = db.SetTagValue("Lid", plc.BOOL(true))
	}()
	// The wait is longer than the client's own timeout.
	b, err := client.Changes(context.Background(), ChangesRequest{Since: pos.Next, Epoch: pos.Epoch, Wait: 3 * time.Second})
	if err != nil || len(b.Changes) != 1 || b.Changes[0].Value != true || b.Changes[0].Quality != QualityGood {
		t.Fatalf("batch = %+v, %v", b, err)
	}

	// A server without the feed.
	old := httptest.NewServer(http.NotFoundHandler())
	defer old.Close()
	oldClient := &NetworkDatabaseClient{RemoteAddress: old.URL, Client: old.Client()}
	if _, err := oldClient.Changes(context.Background(), ChangesRequest{}); !errors.Is(err, ErrChangesUnsupported) {
		t.Fatalf("old server: %v", err)
	}
}

func TestRegisteredDatabaseIsChangeSource(t *testing.T) {
	local, remote := NewTagDatabase(), newFeedDB(t)
	_ = local.RegisterDatabase("field", remote)
	acc, ok := local.RegisteredDatabase("field")
	if _, isSource := acc.(ChangeSource); !ok || !isSource {
		t.Fatal("a registered TagDatabase must be a ChangeSource")
	}
}

// A recorded change keeps the value it had: array element writes modify the
// stored slice in place, which must not rewrite the history.
func TestChangesKeepTheirOwnValues(t *testing.T) {
	db := NewTagDatabase()
	if err := db.AddTag(&Tag{Name: "Arr", TypeInfo: &TypeInfo{DataType: TypeARRAY, ElementType: TypeDINT}, Value: []plc.DINT{0, 0}}); err != nil {
		t.Fatal(err)
	}
	pos := start(t, db)
	_ = db.SetTagValue("Arr[0]", plc.DINT(1))
	_ = db.SetTagValue("Arr[0]", plc.DINT(2))

	b, err := db.Changes(context.Background(), ChangesRequest{Since: pos.Next, Epoch: pos.Epoch})
	if err != nil || len(b.Changes) != 2 {
		t.Fatalf("changes = %+v, %v", b, err)
	}
	first, second := b.Changes[0].Value.([]plc.DINT), b.Changes[1].Value.([]plc.DINT)
	if first[0] != 1 || second[0] != 2 {
		t.Errorf("history = %v then %v, want [1 0] then [2 0]", first, second)
	}
	// Editing a returned value does not reach the tag either.
	second[1] = 99
	if v, _ := db.GetTagValue("Arr[1]"); v != plc.DINT(0) {
		t.Errorf("Arr[1] = %v after editing a recorded change, want 0", v)
	}
}

// A poller that rewrites unchanged values must not push real changes out of the buffer.
func TestChangesSkipUnchangedRewrites(t *testing.T) {
	db := newFeedDB(t)
	db.SetChangeFeedCapacity(3)
	pos := start(t, db)
	_ = db.SetTagValue("Lid", plc.BOOL(true)) // a real change
	for range 100 {
		_ = db.SetTagValue("Temp", plc.REAL(20)) // the first is a change, the rest rewrites
	}
	_ = db.SetTagQuality("Temp", QualityBad)                    // a quality change is a change
	_ = db.SetTagValueQuality("Temp", plc.REAL(20), QualityBad) // same value and quality: not a change

	b, err := db.Changes(context.Background(), ChangesRequest{Since: pos.Next, Epoch: pos.Epoch})
	if err != nil || b.Gap {
		t.Fatalf("changes = %+v, %v: rewrites overran the buffer", b, err)
	}
	if len(b.Changes) != 3 || b.Changes[0].Name != "Lid" || b.Changes[1].Value != plc.REAL(20) || b.Changes[2].Quality != QualityBad {
		t.Errorf("changes = %+v, want Lid, Temp=20, Temp Bad", b.Changes)
	}
}

// A tag removed and added again records its first write, even if the value matches.
func TestChangesAfterTagIsReadded(t *testing.T) {
	db := newFeedDB(t)
	_ = db.SetTagValue("Temp", plc.REAL(5))
	pos := start(t, db)
	_ = db.RemoveTag("Temp")
	_ = db.AddTag(&Tag{Name: "Temp", TypeInfo: &TypeInfo{DataType: TypeREAL}, Value: plc.REAL(0)})
	_ = db.SetTagValue("Temp", plc.REAL(5))
	b, _ := db.Changes(context.Background(), ChangesRequest{Since: pos.Next, Epoch: pos.Epoch})
	if len(b.Changes) != 1 || b.Changes[0].Value != plc.REAL(5) {
		t.Errorf("changes = %+v, want the re-added tag's first write", b.Changes)
	}
}

func TestCloneValue(t *testing.T) {
	type inner struct{ N plc.DINT }
	type udt struct {
		Speed plc.REAL
		In    *inner
		List  []plc.INT
		When  plc.DT
	}
	when := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	orig := &udt{Speed: 1, In: &inner{N: 2}, List: []plc.INT{3}, When: plc.DT(when)}
	clone := cloneValue(orig).(*udt)
	clone.In.N, clone.List[0], clone.Speed = 20, 30, 10
	if orig.In.N != 2 || orig.List[0] != 3 || orig.Speed != 1 {
		t.Errorf("clone shares memory with the original: %+v %+v", orig, *orig.In)
	}
	if !time.Time(clone.When).Equal(when) {
		t.Error("clone lost the DT value")
	}
	if cloneValue(nil) != nil || cloneValue(plc.DINT(4)) != plc.DINT(4) {
		t.Error("scalars and nil must clone to themselves")
	}
}
