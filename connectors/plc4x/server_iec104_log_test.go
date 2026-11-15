/*
 * Copyright (C) 2026 Franklin D. Amador
 *
 * This software is dual-licensed under the terms of the GPL v3.0 and
 * a commercial license. You may choose to use this software under either
 * license.
 *
 * See the LICENSE files in the project root for full license text.
 */

package plc4x

import (
	"context"
	"fmt"
	"testing"
	"time"

	iec "github.com/apache/plc4x/plc4go/protocols/iec608705104/readwrite/model"
	"github.com/apiarytech/honeycomb"
	plc "github.com/apiarytech/royaljelly/iec"
)

// timedBreaker reads a time-tagged single point: its value and time.
func timedBreaker(t *testing.T, a iec.ASDU) (bool, time.Time) {
	t.Helper()
	if a.GetTypeIdentification() != iec.TypeIdentification_SINGLE_POINT_INFORMATION_WITH_TIME_TAG_CP56TIME2A {
		t.Fatalf("type %s, want a time-tagged single point", a.GetTypeIdentification())
	}
	o := a.GetInformationObjects()[0].(iec.InformationObjectWithSevenByteTime_SINGLE_POINT_INFORMATION)
	return o.GetSiq().GetStausOn(), FromCP56(o.GetCp56Time2a())
}

// Store and forward (beehive R-142): changes made while no controlling
// station is there are sent to the next one in order with their own
// times, again to the one after if the first never acknowledged them,
// and never again once acknowledged.
func TestIECStoreAndForward(t *testing.T) {
	port := freePort(t)
	db := honeycomb.NewTagDatabase()
	addTag(t, db, "Breaker", honeycomb.TypeBOOL, plc.BOOL(false))
	addTag(t, db, "Level", honeycomb.TypeREAL, plc.REAL(1))
	c, err := New(db, []Connection{{Name: "outstation", Mode: Server, URL: fmt.Sprintf("iec-60870-5-104://127.0.0.1:%d?buffer=100", port),
		Bindings: []Binding{out("Breaker", "1/101:SP"), out("Level", "1/100:FLOAT")}}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	time.Sleep(400 * time.Millisecond) // the log notes the values

	t1 := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	t2 := t1.Add(1500 * time.Millisecond)
	db.SetTagValueQualityAt("Breaker", plc.BOOL(true), honeycomb.QualityGood, t1)
	time.Sleep(400 * time.Millisecond)
	db.SetTagValueQualityAt("Breaker", plc.BOOL(false), honeycomb.QualityGood, t2)
	time.Sleep(400 * time.Millisecond)

	// A station that receives the backlog and drops without acknowledging.
	b := dialIEC(t, port)
	b.handshake()
	for range 2 {
		for {
			ctrl, _ := b.readFrame()
			if ctrl[0]&0x01 == 0 {
				break // an I-frame, not acknowledged
			}
		}
	}
	b.close()

	// The next station gets them again, in order, with their times, then
	// the current values.
	cl := dialIEC(t, port)
	cl.handshake()
	on, at := timedBreaker(t, cl.expectASDU())
	if !on || !at.Equal(t1) {
		t.Errorf("first event %v at %s, want true at %s", on, at, t1)
	}
	on, at = timedBreaker(t, cl.expectASDU())
	if on || !at.Equal(t2) {
		t.Errorf("second event %v at %s, want false at %s", on, at, t2)
	}
	for range 2 {
		a := cl.expectASDU()
		if a.GetTypeIdentification() != iec.TypeIdentification_SINGLE_POINT_INFORMATION && a.GetTypeIdentification() != iec.TypeIdentification_MEASURED_VALUE_SHORT_FLOATING_POINT_NUMBER {
			t.Errorf("after the backlog %s, want the current values", a.GetTypeIdentification())
		}
	}
	// A change now goes out at once, time-tagged.
	t3 := t2.Add(time.Minute)
	db.SetTagValueQualityAt("Breaker", plc.BOOL(true), honeycomb.QualityGood, t3)
	if on, at := timedBreaker(t, cl.expectASDU()); !on || !at.Equal(t3) {
		t.Errorf("live event %v at %s", on, at)
	}
	cl.close()
	waitFor(t, func() bool { n, _ := c.connections[0].server.iec.log.Len(); return n == 0 })

	// Acknowledged: the next station gets only the current values.
	d := dialIEC(t, port)
	defer d.close()
	d.handshake()
	if a := d.expectASDU(); a.GetTypeIdentification() == iec.TypeIdentification_SINGLE_POINT_INFORMATION_WITH_TIME_TAG_CP56TIME2A {
		t.Error("an acknowledged event was sent again")
	}
}

func TestIECLogBounded(t *testing.T) {
	l := newIECLog(3)
	for i := range 5 {
		l.add(1, iec.TypeIdentification_SINGLE_POINT_INFORMATION_WITH_TIME_TAG_CP56TIME2A, nil)
		_ = i
	}
	if n, dropped := l.Len(); n != 3 || dropped != 2 || l.position() != 2 {
		t.Errorf("kept %d, dropped %d, position %d", n, dropped, l.position())
	}
	if ev := l.after(l.position(), 10); len(ev) != 3 || ev[0].seq != 3 {
		t.Errorf("after %+v", ev)
	}
	l.ack(4)
	if n, _ := l.Len(); n != 1 {
		t.Errorf("after an ack %d left", n)
	}
	for _, bad := range []string{"-1", "x", "100001", "1e3"} {
		if _, err := parseBuffer(bad); err == nil {
			t.Errorf("buffer %q accepted", bad)
		}
	}
}
