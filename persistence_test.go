package honeycomb

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"

	plc "github.com/apiarytech/royaljelly"
)

// fakeStore is an in-memory TagStore that counts writes and can be made to fail.
type fakeStore struct {
	mu     sync.Mutex
	tags   map[string]StoredTag
	saves  int
	fail   error
	closed bool
}

func newFakeStore() *fakeStore { return &fakeStore{tags: make(map[string]StoredTag)} }

func (s *fakeStore) LoadTags(context.Context) ([]StoredTag, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]StoredTag, 0, len(s.tags))
	for _, t := range s.tags {
		out = append(out, t)
	}
	return out, nil
}

func (s *fakeStore) SaveTags(_ context.Context, tags []StoredTag) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return s.fail
	}
	for _, t := range tags {
		s.tags[t.Name] = t
		s.saves++
	}
	return nil
}

func (s *fakeStore) DeleteTags(_ context.Context, names []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return s.fail
	}
	for _, n := range names {
		delete(s.tags, n)
	}
	return nil
}

func (s *fakeStore) Close() error { s.closed = true; return nil }

func (s *fakeStore) names() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var names []string
	for n := range s.tags {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func newPersistTestDB(t *testing.T) *TagDatabase {
	t.Helper()
	db := NewTagDatabase()
	for _, tag := range []*Tag{
		{Name: "A", TypeInfo: &TypeInfo{DataType: TypeDINT}, Value: plc.DINT(0), Retain: true},
		{Name: "B", TypeInfo: &TypeInfo{DataType: TypeDINT, Min: plc.DINT(0), Max: plc.DINT(100)}, Value: plc.DINT(0), Retain: true},
		{Name: "Volatile", TypeInfo: &TypeInfo{DataType: TypeDINT}, Value: plc.DINT(0)},
	} {
		if err := db.AddTag(tag); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func TestPersisterCoalescesWritesAndRespectsScope(t *testing.T) {
	ctx := context.Background()
	db := newPersistTestDB(t)
	store := newFakeStore()
	p, err := db.AttachStore(store, PersistOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AttachStore(newFakeStore(), PersistOptions{}); err == nil {
		t.Fatal("second AttachStore should fail")
	}

	for i := 0; i < 100; i++ {
		_ = db.SetTagValue("A", plc.DINT(i))
		_ = db.SetTagValue("Volatile", plc.DINT(i))
	}
	if err := p.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if store.saves != 1 || string(store.tags["A"].Value) != "99" {
		t.Fatalf("expected one coalesced save of A=99, got %d saves, tags %v", store.saves, store.tags)
	}
	if _, ok := store.tags["Volatile"]; ok {
		t.Fatal("non-retain tag persisted with PersistRetainOnly")
	}
}

func TestPersisterTracksRenameAndRemove(t *testing.T) {
	ctx := context.Background()
	db := newPersistTestDB(t)
	store := newFakeStore()
	p, _ := db.AttachStore(store, PersistOptions{})
	if err := p.SaveAll(ctx); err != nil {
		t.Fatal(err)
	}

	if _, err := db.RenameTag("A", "A2"); err != nil {
		t.Fatal(err)
	}
	if err := db.RemoveTag("B"); err != nil {
		t.Fatal(err)
	}
	if err := p.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if got := store.names(); len(got) != 1 || got[0] != "A2" {
		t.Fatalf("store holds %v, want [A2]", got)
	}
}

func TestPersisterRetriesFailedFlush(t *testing.T) {
	ctx := context.Background()
	db := newPersistTestDB(t)
	store := newFakeStore()
	p, _ := db.AttachStore(store, PersistOptions{})

	store.fail = errors.New("disk unavailable")
	_ = db.SetTagValue("A", plc.DINT(5))
	if err := p.Flush(ctx); err == nil {
		t.Fatal("expected flush error")
	}
	store.fail = nil
	if err := p.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if string(store.tags["A"].Value) != "5" {
		t.Fatalf("A not written on retry: %v", store.tags)
	}
}

func TestPersisterCloseSavesSnapshotAndDetaches(t *testing.T) {
	ctx := context.Background()
	db := newPersistTestDB(t)
	store := newFakeStore()
	p, _ := db.AttachStore(store, PersistOptions{Scope: PersistAll})
	p.Start()

	if err := p.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if got := store.names(); len(got) != 3 || !store.closed {
		t.Fatalf("after Close: store holds %v, closed=%v", got, store.closed)
	}
	if _, err := db.AttachStore(newFakeStore(), PersistOptions{}); err != nil {
		t.Fatalf("store not detached by Close: %v", err)
	}
}

func TestPersisterRestoreDefinitions(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	src := newPersistTestDB(t)
	_ = src.SetTagValue("B", plc.DINT(50))
	p, _ := src.AttachStore(store, PersistOptions{})
	if err := p.Close(ctx); err != nil {
		t.Fatal(err)
	}

	// An empty database rebuilds its tags, including subrange limits, from the store.
	db := NewTagDatabase()
	p, _ = db.AttachStore(store, PersistOptions{RestoreDefinitions: true})
	if err := p.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	if v, err := db.GetTagValue("B"); err != nil || v != plc.DINT(50) {
		t.Fatalf("B = %v (%v), want 50", v, err)
	}
	if err := db.SetTagValue("B", plc.DINT(101)); err == nil {
		t.Fatal("restored subrange not enforced")
	}
}
