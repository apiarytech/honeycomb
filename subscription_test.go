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
	"path/filepath"
	"sync"
	"testing"
	"time"

	plc "github.com/apiarytech/royaljelly/iec"
)

func newSubscriptionTestDB(t *testing.T) *TagDatabase {
	t.Helper()
	db := NewTagDatabase()
	for _, tag := range []*Tag{
		{Name: "Level", TypeInfo: &TypeInfo{DataType: TypeDINT}, Value: plc.DINT(0), DirectAddress: "%MD4"},
		{Name: "Arr", TypeInfo: &TypeInfo{DataType: TypeARRAY, ElementType: TypeDINT}, Value: []plc.DINT{0, 0}},
	} {
		if err := db.AddTag(tag); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func receive(t *testing.T, ch <-chan Tag) Tag {
	t.Helper()
	select {
	case tag := <-ch:
		return tag
	case <-time.After(time.Second):
		t.Fatal("no update received")
		return Tag{}
	}
}

func expectNothing(t *testing.T, ch <-chan Tag) {
	t.Helper()
	select {
	case tag := <-ch:
		t.Fatalf("unexpected update %v (sequence %d)", tag.Value, tag.Sequence)
	case <-time.After(50 * time.Millisecond):
	}
}

// Gap 1: an alarm spike that returns to normal while the evaluator is busy must
// leave the evaluator with the normal value, not the stale spike.
func TestSubscriptionNewestValueWins(t *testing.T) {
	db := newSubscriptionTestDB(t)
	ch, _, err := db.SubscribeToTag("Level")
	if err != nil {
		t.Fatal(err)
	}

	// The subscriber is busy and receives nothing during these writes.
	for _, v := range []plc.DINT{100, 250, 0} {
		if err := db.SetTagValue("Level", v); err != nil {
			t.Fatal(err)
		}
	}

	update := receive(t, ch)
	if update.Value != plc.DINT(0) {
		t.Errorf("subscriber got %v, want the newest value 0", update.Value)
	}
	// Sequence 3 after nothing received tells the subscriber it missed two updates.
	if update.Sequence != 3 {
		t.Errorf("Sequence = %d, want 3", update.Sequence)
	}
	expectNothing(t, ch)

	// The next update continues the sequence.
	_ = db.SetTagValue("Level", plc.DINT(5))
	if next := receive(t, ch); next.Value != plc.DINT(5) || next.Sequence != 4 {
		t.Errorf("next update = %v (sequence %d), want 5 (sequence 4)", next.Value, next.Sequence)
	}
}

// Gap 2: concurrent writers must never make a subscriber see an older update
// after a newer one, and the last update it sees is the tag's final state.
func TestSubscriptionUpdatesArriveInOrder(t *testing.T) {
	db := newSubscriptionTestDB(t)
	ch, _, err := db.SubscribeToTag("Level")
	if err != nil {
		t.Fatal(err)
	}

	const writers, writes = 8, 500
	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range writes {
				_ = db.SetTagValue("Level", plc.DINT(w*writes+i))
			}
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	var last Tag
	for finished := false; !finished; {
		select {
		case update := <-ch:
			if update.Sequence <= last.Sequence {
				t.Fatalf("sequence %d arrived after %d", update.Sequence, last.Sequence)
			}
			last = update
		case <-done:
			finished = true
		}
	}
	// Collect the final update if it is still waiting.
	select {
	case update := <-ch:
		if update.Sequence <= last.Sequence {
			t.Fatalf("sequence %d arrived after %d", update.Sequence, last.Sequence)
		}
		last = update
	default:
	}

	final, _ := db.GetTagValue("Level")
	if last.Value != final {
		t.Errorf("last update %v, but the tag holds %v", last.Value, final)
	}
	if last.Sequence != writers*writes {
		t.Errorf("last Sequence = %d, want %d (one per write)", last.Sequence, writers*writes)
	}
}

// Unsubscribing, or removing the tag, while writes are being delivered must not
// panic with a send on a closed channel.
func TestSubscriptionCloseDuringWrites(t *testing.T) {
	db := newSubscriptionTestDB(t)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
				_ = db.SetTagValue("Level", plc.DINT(i))
			}
		}
	}()
	for range 200 {
		_, id, err := db.SubscribeToTag("Level")
		if err != nil {
			t.Fatal(err)
		}
		if err := db.UnsubscribeFromTag("Level", id); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
}

// Gap 6: updates carry the whole tag state, including DirectAddress.
func TestSubscriptionUpdateIsComplete(t *testing.T) {
	db := newSubscriptionTestDB(t)
	ch, _, _ := db.SubscribeToTag("Level")
	before := time.Now()
	_ = db.SetTagValueQuality("Level", plc.DINT(7), QualityUncertain)
	update := receive(t, ch)
	if update.DirectAddress != "%MD4" || update.Quality != QualityUncertain || update.Timestamp.Before(before) {
		t.Errorf("update = %+v, want DirectAddress %%MD4, Uncertain and a timestamp", update)
	}
}

// Gap 4: a subscription to a remote alias would never fire, so it is refused.
func TestSubscriptionToRemoteAliasIsRefused(t *testing.T) {
	remote := newSubscriptionTestDB(t)
	local := NewTagDatabase()
	_ = local.RegisterDatabase("remote", remote)
	if err := local.AddTag(&Tag{Name: "RemoteLevel", RemoteAlias: &RemoteAliasInfo{DBID: "remote", TagName: "Level"}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := local.SubscribeToTag("RemoteLevel"); err == nil {
		t.Error("subscribing to a remote alias succeeded, but it would never be notified")
	}
	// Subscribing on the owning database works, and sees writes made through the alias.
	ch, _, err := remote.SubscribeToTag("Level")
	if err != nil {
		t.Fatal(err)
	}
	_ = local.SetTagValue("RemoteLevel", plc.DINT(9))
	if update := receive(t, ch); update.Value != plc.DINT(9) {
		t.Errorf("owner subscription got %v, want 9", update.Value)
	}
}

// Gap 3: every change is timestamped, and drivers can pass the device's time.
func TestTagTimestamps(t *testing.T) {
	db := newSubscriptionTestDB(t)
	if ts, _ := db.GetTag("Level"); !ts.Timestamp.IsZero() {
		t.Errorf("a never-written tag has timestamp %v, want zero", ts.Timestamp)
	}

	before := time.Now()
	_ = db.SetTagValue("Level", plc.DINT(1))
	written, _ := db.GetTag("Level")
	if written.Timestamp.Before(before) || written.Timestamp.After(time.Now()) {
		t.Errorf("SetTagValue timestamp %v not between %v and now", written.Timestamp, before)
	}

	// A driver passes the time the device reported the value.
	device := time.Date(2026, 9, 30, 3, 12, 45, 250_000_000, time.UTC)
	if err := db.SetTagValueQualityAt("Level", plc.DINT(2), QualityGood, device); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.GetTag("Level"); !got.Timestamp.Equal(device) {
		t.Errorf("device timestamp = %v, want %v", got.Timestamp, device)
	}

	// A quality change is an event; setting the same quality again is not.
	_ = db.SetTagQuality("Level", QualityBad)
	bad, _ := db.GetTag("Level")
	if !bad.Timestamp.After(device) {
		t.Errorf("quality change kept timestamp %v", bad.Timestamp)
	}
	_ = db.SetTagQuality("Level", QualityBad)
	if again, _ := db.GetTag("Level"); !again.Timestamp.Equal(bad.Timestamp) {
		t.Errorf("unchanged quality moved the timestamp to %v", again.Timestamp)
	}

	// Element writes timestamp the whole array, and element views report it.
	_ = db.SetTagValueQualityAt("Arr[1]", plc.DINT(4), QualityGood, device)
	if elem, _ := db.GetTag("Arr[0]"); !elem.Timestamp.Equal(device) {
		t.Errorf("element view timestamp = %v, want %v", elem.Timestamp, device)
	}
}

func TestTimestampSurvivesRestart(t *testing.T) {
	device := time.Date(2026, 9, 30, 3, 12, 45, 0, time.UTC)

	// Through a TagStore.
	ctx := context.Background()
	store := newFakeStore()
	src := newPersistTestDB(t)
	_ = src.SetTagValueQualityAt("A", plc.DINT(1), QualityGood, device)
	p, _ := src.AttachStore(store, PersistOptions{})
	if err := p.Close(ctx); err != nil {
		t.Fatal(err)
	}
	db := newPersistTestDB(t)
	p, _ = db.AttachStore(store, PersistOptions{})
	if err := p.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.GetTag("A"); !got.Timestamp.Equal(device) {
		t.Errorf("restored timestamp = %v, want %v", got.Timestamp, device)
	}

	// Through a tags file.
	path := filepath.Join(t.TempDir(), "tags.json")
	if err := src.WriteTagsToFile(path); err != nil {
		t.Fatal(err)
	}
	db = newPersistTestDB(t)
	if err := db.ReadTagsFromFile(path); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.GetTag("A"); !got.Timestamp.Equal(device) {
		t.Errorf("file timestamp = %v, want %v", got.Timestamp, device)
	}
}

// SetTagForced and SetTagForceValue used to return a copy of the tag taken
// while its mutex was held, so the copy's mutex stayed locked forever.
func TestForceReturnsUsableCopy(t *testing.T) {
	db := newSubscriptionTestDB(t)
	forced, err := db.SetTagForced("Level", true)
	if err != nil {
		t.Fatal(err)
	}
	withValue, err := db.SetTagForceValue("Level", plc.DINT(42))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_ = forced.SetValue(plc.DINT(1)) // takes the copy's write lock
		_ = withValue.GetValue()         // takes the copy's read lock
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("using the returned Tag deadlocked: its mutex was copied while locked")
	}
}

// GetTagsByType used to dereference the nil TypeInfo of a remote alias.
func TestGetTagsByTypeSkipsRemoteAliases(t *testing.T) {
	db := newSubscriptionTestDB(t)
	_ = db.AddTag(&Tag{Name: "Alias", RemoteAlias: &RemoteAliasInfo{DBID: "x", TagName: "y"}})
	if tags := db.GetTagsByType(TypeDINT); len(tags) != 1 || tags[0].Name != "Level" {
		t.Errorf("GetTagsByType(DINT) = %v, want only Level", tags)
	}
}
