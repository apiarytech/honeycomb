// Copyright (C) 2026 Franklin D. Amador
//
// This software is dual-licensed under the terms of the GPL v3.0 and
// a commercial license. You may choose to use this software under either
// license.
//
// See the LICENSE files in the project root for full license text.

package plc4x

import (
	"testing"
	"time"

	"github.com/apache/plc4x/plc4go/spi/values"
	"github.com/apiarytech/honeycomb"
	plc "github.com/apiarytech/royaljelly/iec"
)

// A value read again and again unchanged turns Uncertain once StaleAfter
// has passed, and Good again when it changes (beehive R-4). A binding
// without StaleAfter stays Good.
func TestStaleAfter(t *testing.T) {
	db := honeycomb.NewTagDatabase()
	addTag(t, db, "Level", honeycomb.TypeINT, plc.INT(0))
	addTag(t, db, "Flow", honeycomb.TypeINT, plc.INT(0))
	c, err := New(db, []Connection{{Name: "dev", URL: "modbus-tcp://127.0.0.1:1",
		Bindings: []Binding{{Tag: "Level", Address: "a", StaleAfter: 10 * time.Second}, {Tag: "Flow", Address: "b"}}}},
		WithDriverManager(nil))
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	defer func(old func() time.Time) { now = old }(now)
	now = func() time.Time { return clock }
	cs := c.connections[0]
	read := func(tag string, v int16, at time.Duration) honeycomb.Quality {
		clock = time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC).Add(at)
		if err := c.store(cs, tag, values.NewPlcINT(v)); err != nil {
			t.Fatal(err)
		}
		q, _ := db.GetTagQuality(tag)
		return q
	}
	steps := []struct {
		v    int16
		at   time.Duration
		want honeycomb.Quality
	}{
		{5, 0, honeycomb.QualityGood},                     // first read
		{5, 9 * time.Second, honeycomb.QualityGood},       // unchanged, not long enough
		{5, 10 * time.Second, honeycomb.QualityUncertain}, // frozen
		{5, 30 * time.Second, honeycomb.QualityUncertain},
		{6, 31 * time.Second, honeycomb.QualityGood}, // moving again
		{6, 40 * time.Second, honeycomb.QualityGood},
		{6, 41 * time.Second, honeycomb.QualityUncertain},
	}
	for _, s := range steps {
		if q := read("Level", s.v, s.at); q != s.want {
			t.Errorf("Level %d at %s: %v, want %v", s.v, s.at, q, s.want)
		}
	}
	if q := read("Flow", 1, 0); q != honeycomb.QualityGood {
		t.Errorf("Flow: %v", q)
	}
	if q := read("Flow", 1, time.Hour); q != honeycomb.QualityGood {
		t.Errorf("Flow unchanged an hour without StaleAfter: %v", q)
	}
}
