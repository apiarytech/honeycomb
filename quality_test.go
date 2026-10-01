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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	plc "github.com/apiarytech/royaljelly/iec"
)

var allQualities = []Quality{QualityUnknown, QualityGood, QualityUncertain, QualityBad}

func TestQualityBridgesRoundTrip(t *testing.T) {
	for _, q := range allQualities {
		if got := QualityFromOPCDA(q.OPCDA()); got != q {
			t.Errorf("OPC DA round trip of %v gave %v", q, got)
		}
		if got := QualityFromOPCUA(q.OPCUA()); got != q {
			t.Errorf("OPC UA round trip of %v gave %v", q, got)
		}
	}
}

func TestQualityFromOPCDA(t *testing.T) {
	cases := map[uint16]Quality{
		0xC0:   QualityGood,      // Good
		0xD8:   QualityGood,      // Good, local override
		0xC1:   QualityGood,      // Good, low limited
		0x40:   QualityUncertain, // Uncertain
		0x44:   QualityUncertain, // Uncertain, last usable value
		0x00:   QualityBad,       // Bad
		0x18:   QualityBad,       // Bad, comm failure
		0x80:   QualityBad,       // reserved quality bits
		0x20:   QualityUnknown,   // Bad, waiting for initial data
		0x1220: QualityUnknown,   // vendor bits in the high byte are ignored
	}
	for code, want := range cases {
		if got := QualityFromOPCDA(code); got != want {
			t.Errorf("QualityFromOPCDA(%#x) = %v, want %v", code, got, want)
		}
	}
}

func TestQualityFromOPCUA(t *testing.T) {
	cases := map[uint32]Quality{
		0x00000000: QualityGood,      // Good
		0x00A20000: QualityGood,      // GoodLocalOverride
		0x00000400: QualityGood,      // Good with info bits set
		0x40000000: QualityUncertain, // Uncertain
		0x408F0000: QualityUncertain, // UncertainLastUsableValue
		0x80000000: QualityBad,       // Bad
		0x808A0000: QualityBad,       // BadNotConnected
		0xC0000000: QualityBad,       // reserved severity
		0x80320000: QualityUnknown,   // BadWaitingForInitialData
	}
	for code, want := range cases {
		if got := QualityFromOPCUA(code); got != want {
			t.Errorf("QualityFromOPCUA(%#x) = %v, want %v", code, got, want)
		}
	}
}

func TestQualityFromPLC4X(t *testing.T) {
	cases := map[string]Quality{
		"OK":               QualityGood,
		"REMOTE_BUSY":      QualityUncertain,
		"RESPONSE_PENDING": QualityUncertain,
		"REQUEST_TIMEOUT":  QualityUncertain,
		"NOT_FOUND":        QualityBad,
		"ACCESS_DENIED":    QualityBad,
		"INVALID_ADDRESS":  QualityBad,
		"INVALID_DATATYPE": QualityBad,
		"INVALID_DATA":     QualityBad,
		"INTERNAL_ERROR":   QualityBad,
		"REMOTE_ERROR":     QualityBad,
		"UNSUPPORTED":      QualityBad,
		"SOMETHING_NEW":    QualityBad,
	}
	for code, want := range cases {
		if got := QualityFromPLC4X(code); got != want {
			t.Errorf("QualityFromPLC4X(%q) = %v, want %v", code, got, want)
		}
	}
}

func TestQualityString(t *testing.T) {
	want := []string{"Unknown", "Good", "Uncertain", "Bad"}
	for i, q := range allQualities {
		if q.String() != want[i] {
			t.Errorf("%d.String() = %q, want %q", uint8(q), q.String(), want[i])
		}
	}
	if Quality(9).IsValid() || Quality(9).String() != "Quality(9)" {
		t.Errorf("Quality(9) should be invalid and print as Quality(9), got %q", Quality(9).String())
	}
}

func newQualityTestDB(t *testing.T) *TagDatabase {
	t.Helper()
	RegisterUDT(&MotorData{})
	db := NewTagDatabase()
	for _, tag := range []*Tag{
		{Name: "Level", TypeInfo: &TypeInfo{DataType: TypeDINT}, Value: plc.DINT(0), DirectAddress: "%MD10"},
		{Name: "Arr", TypeInfo: &TypeInfo{DataType: TypeARRAY, ElementType: TypeDINT}, Value: []plc.DINT{1, 2, 3}},
		{Name: "Motor", TypeInfo: &TypeInfo{DataType: "MotorData"}, Value: &MotorData{}},
		{Name: "Pi", TypeInfo: &TypeInfo{DataType: TypeLREAL}, Value: plc.LREAL(3.14), Constant: true},
	} {
		if err := db.AddTag(tag); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func mustQuality(t *testing.T, db *TagDatabase, name string, want Quality) {
	t.Helper()
	got, err := db.GetTagQuality(name)
	if err != nil || got != want {
		t.Errorf("GetTagQuality(%q) = %v (%v), want %v", name, got, err, want)
	}
}

func TestTagQualityLifecycle(t *testing.T) {
	db := newQualityTestDB(t)

	// New tags have no value yet; constants are Good from the start.
	mustQuality(t, db, "Level", QualityUnknown)
	mustQuality(t, db, "Pi", QualityGood)

	// A plain write asserts the value is Good.
	if err := db.SetTagValue("Level", plc.DINT(5)); err != nil {
		t.Fatal(err)
	}
	mustQuality(t, db, "Level", QualityGood)
	mustQuality(t, db, "%MD10", QualityGood) // resolved through the direct address

	// A driver losing its connection keeps the last value but marks it Bad.
	if err := db.SetTagQuality("Level", QualityBad); err != nil {
		t.Fatal(err)
	}
	mustQuality(t, db, "Level", QualityBad)
	if v, _ := db.GetTagValue("Level"); v != plc.DINT(5) {
		t.Errorf("SetTagQuality changed the value to %v", v)
	}

	// Value and quality written together.
	if err := db.SetTagValueQuality("Level", plc.DINT(6), QualityUncertain); err != nil {
		t.Fatal(err)
	}
	mustQuality(t, db, "Level", QualityUncertain)

	// A rejected write leaves the quality alone.
	if err := db.SetTagValueQuality("Level", plc.REAL(1), QualityGood); err == nil {
		t.Fatal("type mismatch was accepted")
	}
	mustQuality(t, db, "Level", QualityUncertain)

	// Element and field writes set the quality of the whole tag, and reads of
	// elements and fields report it.
	if err := db.SetTagValueQuality("Arr[1]", plc.DINT(9), QualityBad); err != nil {
		t.Fatal(err)
	}
	mustQuality(t, db, "Arr", QualityBad)
	mustQuality(t, db, "Arr[0]", QualityBad)
	if tag, ok := db.GetTag("Arr[2]"); !ok || tag.Quality != QualityBad {
		t.Errorf("GetTag(Arr[2]).Quality = %v, want Bad", tag.Quality)
	}
	if err := db.SetTagValue("Motor.Speed", plc.REAL(1500)); err != nil {
		t.Fatal(err)
	}
	mustQuality(t, db, "Motor", QualityGood)
	if err := db.SetTagQuality("Motor.Speed", QualityUncertain); err != nil {
		t.Fatal(err)
	}
	if tag, ok := db.GetTag("Motor.Speed"); !ok || tag.Quality != QualityUncertain {
		t.Errorf("GetTag(Motor.Speed).Quality = %v, want Uncertain", tag.Quality)
	}

	// Invalid qualities and unknown tags are rejected; unknown tags read as Bad.
	if err := db.SetTagQuality("Level", Quality(7)); err == nil {
		t.Error("SetTagQuality accepted an invalid quality")
	}
	if err := db.SetTagValueQuality("Level", plc.DINT(1), Quality(7)); err == nil {
		t.Error("SetTagValueQuality accepted an invalid quality")
	}
	if q, err := db.GetTagQuality("Missing"); err == nil || q != QualityBad {
		t.Errorf("GetTagQuality(Missing) = %v (%v), want Bad and an error", q, err)
	}
}

func TestTagQualityNotifiesSubscribers(t *testing.T) {
	db := newQualityTestDB(t)
	ch, _, err := db.SubscribeToTag("Level")
	if err != nil {
		t.Fatal(err)
	}
	receive := func() (Tag, bool) {
		select {
		case tag := <-ch:
			return tag, true
		case <-time.After(200 * time.Millisecond):
			return Tag{}, false
		}
	}

	if err := db.SetTagQuality("Level", QualityBad); err != nil {
		t.Fatal(err)
	}
	if tag, ok := receive(); !ok || tag.Quality != QualityBad {
		t.Fatalf("subscriber got %v (received %v), want Bad", tag.Quality, ok)
	}

	// Setting the same quality again is not a change.
	if err := db.SetTagQuality("Level", QualityBad); err != nil {
		t.Fatal(err)
	}
	if tag, ok := receive(); ok {
		t.Errorf("unchanged quality notified subscribers with %v", tag.Quality)
	}
}

func TestTagQualityRemoteAlias(t *testing.T) {
	remote := newQualityTestDB(t)
	local := NewTagDatabase()
	if err := local.RegisterDatabase("remote", remote); err != nil {
		t.Fatal(err)
	}
	if err := local.AddTag(&Tag{Name: "RemoteLevel", RemoteAlias: &RemoteAliasInfo{DBID: "remote", TagName: "Level"}}); err != nil {
		t.Fatal(err)
	}

	if err := local.SetTagValueQuality("RemoteLevel", plc.DINT(3), QualityUncertain); err != nil {
		t.Fatal(err)
	}
	mustQuality(t, remote, "Level", QualityUncertain)
	mustQuality(t, local, "RemoteLevel", QualityUncertain)

	if err := local.SetTagQuality("RemoteLevel", QualityBad); err != nil {
		t.Fatal(err)
	}
	mustQuality(t, remote, "Level", QualityBad)
}

func TestTagQualityOverHTTP(t *testing.T) {
	ts := &tagServer{db: newQualityTestDB(t)}
	put := func(body string) int {
		rr := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPut, "/tags/Level", bytes.NewBufferString(body))
		ts.handleSetTagValue(rr, req, "Level")
		return rr.Code
	}

	if code := put(`{"value": 4, "quality": 2}`); code != http.StatusOK {
		t.Fatalf("PUT value+quality returned %d", code)
	}
	mustQuality(t, ts.db, "Level", QualityUncertain)
	if code := put(`{"quality": 3}`); code != http.StatusOK {
		t.Fatalf("PUT quality returned %d", code)
	}
	mustQuality(t, ts.db, "Level", QualityBad)
	if code := put(`{"value": 5}`); code != http.StatusOK {
		t.Fatalf("PUT value returned %d", code)
	}
	mustQuality(t, ts.db, "Level", QualityGood)
	if code := put(`{"quality": 9}`); code != http.StatusBadRequest {
		t.Errorf("PUT invalid quality returned %d, want 400", code)
	}

	rr := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/tags/Level", nil)
	ts.handleGetTagValue(rr, req, "Level")
	var resp tagResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil || resp.Quality == nil || *resp.Quality != QualityGood {
		t.Errorf("GET body %s: want quality 1", rr.Body.String())
	}
}

func TestTagQualityPersisterRestore(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	src := newPersistTestDB(t)
	if err := src.SetTagValue("A", plc.DINT(1)); err != nil {
		t.Fatal(err)
	}
	if err := src.SetTagValueQuality("B", plc.DINT(2), QualityBad); err != nil {
		t.Fatal(err)
	}
	p, _ := src.AttachStore(store, PersistOptions{})
	if err := p.Close(ctx); err != nil {
		t.Fatal(err)
	}

	// Good values come back Uncertain; Bad stays Bad. Both paths are checked:
	// restoring into configured tags and rebuilding definitions.
	for _, opts := range []PersistOptions{{}, {RestoreDefinitions: true}} {
		db := NewTagDatabase()
		if !opts.RestoreDefinitions {
			db = newPersistTestDB(t)
		}
		p, _ := db.AttachStore(store, opts)
		if err := p.Restore(ctx); err != nil {
			t.Fatal(err)
		}
		mustQuality(t, db, "A", QualityUncertain)
		mustQuality(t, db, "B", QualityBad)
		db.persister.Store(nil)
	}
}

func TestTagQualityFileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tags.json")
	src := newPersistTestDB(t)
	if err := src.SetTagValue("A", plc.DINT(1)); err != nil {
		t.Fatal(err)
	}
	if err := src.SetTagValueQuality("B", plc.DINT(2), QualityBad); err != nil {
		t.Fatal(err)
	}
	if err := src.WriteTagsToFile(path); err != nil {
		t.Fatal(err)
	}

	db := newPersistTestDB(t)
	if err := db.ReadTagsFromFile(path); err != nil {
		t.Fatal(err)
	}
	mustQuality(t, db, "A", QualityUncertain)
	mustQuality(t, db, "B", QualityBad)

	// Files written before quality existed restore their values as Uncertain.
	if err := os.WriteFile(path, []byte(`{"Name":"A","Value":7}`), 0o666); err != nil {
		t.Fatal(err)
	}
	db = newPersistTestDB(t)
	if err := db.ReadTagsFromFile(path); err != nil {
		t.Fatal(err)
	}
	mustQuality(t, db, "A", QualityUncertain)
}

func TestSetTagValueOnDottedTagName(t *testing.T) {
	db := NewTagDatabase()
	if err := db.AddTag(&Tag{Name: "Press1.Pressure", TypeInfo: &TypeInfo{DataType: TypeREAL}, Value: plc.REAL(0)}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetTagValue("Press1.Pressure", plc.REAL(4.5)); err != nil {
		t.Fatalf("a top-level tag whose name contains a dot could not be written: %v", err)
	}
	if v, _ := db.GetTagValue("Press1.Pressure"); v != plc.REAL(4.5) {
		t.Errorf("Press1.Pressure = %v, want 4.5", v)
	}
}
