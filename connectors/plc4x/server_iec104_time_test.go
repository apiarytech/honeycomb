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

// With time-tags=true a change goes out with its source time (CP56Time2a)
// and its quality; a clock synchronisation is confirmed with the
// outstation's own time (beehive R-141).
func TestIECTimeTags(t *testing.T) {
	port := freePort(t)
	db := honeycomb.NewTagDatabase()
	addTag(t, db, "Level", honeycomb.TypeREAL, plc.REAL(1))
	addTag(t, db, "Breaker", honeycomb.TypeBOOL, plc.BOOL(false))
	c, err := New(db, []Connection{{Name: "outstation", Mode: Server, URL: fmt.Sprintf("iec-60870-5-104://127.0.0.1:%d?time-tags=true", port),
		Bindings: []Binding{out("Level", "1/100:FLOAT"), out("Breaker", "1/101:SP")}}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	cl := dialIEC(t, port)
	defer cl.close()
	cl.handshake()
	cl.expectASDU()
	cl.expectASDU() // the current values after STARTDT

	at := time.Date(2026, 11, 10, 8, 30, 15, 250*int(time.Millisecond), time.UTC)
	db.SetTagValueQualityAt("Breaker", plc.BOOL(true), honeycomb.QualityGood, at)
	a := cl.expectASDU()
	if a.GetTypeIdentification() != iec.TypeIdentification_SINGLE_POINT_INFORMATION_WITH_TIME_TAG_CP56TIME2A {
		t.Fatalf("type %s, want M_SP_TB_1", a.GetTypeIdentification())
	}
	o := a.GetInformationObjects()[0].(iec.InformationObjectWithSevenByteTime_SINGLE_POINT_INFORMATION)
	if got := FromCP56(o.GetCp56Time2a()); !got.Equal(at) || !o.GetSiq().GetStausOn() {
		t.Errorf("breaker at %s, value %v; want %s", got, o.GetSiq().GetStausOn(), at)
	}

	db.SetTagValueQualityAt("Level", plc.REAL(9.5), honeycomb.QualityBad, at.Add(time.Second))
	a = cl.expectASDU()
	f := a.GetInformationObjects()[0].(iec.InformationObjectWithSevenByteTime_MEASURED_VALUE_SHORT_FLOATING_POINT_NUMBER)
	if !f.GetQds().GetInvalid() || f.GetValue() != 9.5 || !FromCP56(f.GetCp56Time2a()).Equal(at.Add(time.Second)) {
		t.Errorf("level %v invalid=%v at %s", f.GetValue(), f.GetQds().GetInvalid(), FromCP56(f.GetCp56Time2a()))
	}

	cs := iec.NewInformationObjectWithoutTime_CLOCK_SYNCHRONISATION_COMMAND(0, cp56(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), false))
	cl.writeI(iec.NewASDU(iec.TypeIdentification_CLOCK_SYNCHRONISATION_COMMAND, false, false, false, cotActivation, 0, 1, []iec.InformationObject{cs}))
	a = cl.expectASDU()
	if a.GetTypeIdentification() != iec.TypeIdentification_CLOCK_SYNCHRONISATION_COMMAND || a.GetCauseOfTransmission() != cotActivationCon {
		t.Fatalf("clock sync: %s %s", a.GetTypeIdentification(), a.GetCauseOfTransmission())
	}
	own := FromCP56(a.GetInformationObjects()[0].(iec.InformationObjectWithoutTime_CLOCK_SYNCHRONISATION_COMMAND).GetCp56Time2a())
	if d := time.Since(own); d < -time.Second || d > 5*time.Second {
		t.Errorf("the outstation's time %s, %s from now", own, d)
	}
}

func TestCP56RoundTrip(t *testing.T) {
	for _, at := range []time.Time{
		time.Date(2026, 11, 15, 23, 59, 59, 999*int(time.Millisecond), time.UTC), // a Sunday
		time.Date(2000, 1, 3, 0, 0, 0, 0, time.UTC),
	} {
		c := cp56(at, false)
		if got := FromCP56(c); !got.Equal(at) || c.GetInvalid() {
			t.Errorf("%s: %s invalid=%v", at, got, c.GetInvalid())
		}
	}
	if c := cp56(time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC), false); c.GetDayOfWeek() != 7 {
		t.Errorf("Sunday is %d", c.GetDayOfWeek())
	}
	if !cp56(time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC), false).GetInvalid() {
		t.Error("a year before 2000 is not marked invalid")
	}
}

// The connector as a controlling station, subscribed to an outstation that
// sends time tags: the client's tag takes the outstation's source time and
// quality, not the time it arrived (beehive R-141, end to end).
func TestIECClientKeepsSourceTime(t *testing.T) {
	port := freePort(t)
	srvDB := honeycomb.NewTagDatabase()
	addTag(t, srvDB, "Level", honeycomb.TypeREAL, plc.REAL(1))
	srv, err := New(srvDB, []Connection{{Name: "outstation", Mode: Server, URL: fmt.Sprintf("iec-60870-5-104://127.0.0.1:%d?time-tags=true", port),
		Bindings: []Binding{out("Level", "1/100:FLOAT")}}})
	if err != nil {
		t.Fatal(err)
	}
	cliDB := honeycomb.NewTagDatabase()
	addTag(t, cliDB, "RTU.Level", honeycomb.TypeREAL, plc.REAL(0))
	cli, err := New(cliDB, []Connection{{Name: "rtu", Mode: ChangeOfState, URL: fmt.Sprintf("iec-60870-5-104://127.0.0.1:%d", port),
		Bindings: []Binding{{Tag: "RTU.Level", Address: "1/100", Direction: Input}}}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var done [2]chan struct{}
	for i, c := range []*Connector{srv, cli} {
		done[i] = make(chan struct{})
		go func() { c.Run(ctx); close(done[i]) }()
	}
	defer func() { cancel(); <-done[0]; <-done[1] }()
	waitFor(t, func() bool { v, _ := cliDB.GetTagValue("RTU.Level"); return v == plc.REAL(1) })

	at := time.Date(2026, 11, 10, 9, 0, 0, 125*int(time.Millisecond), time.UTC)
	srvDB.SetTagValueQualityAt("Level", plc.REAL(4.75), honeycomb.QualityUncertain, at)
	waitFor(t, func() bool { v, _ := cliDB.GetTagValue("RTU.Level"); return v == plc.REAL(4.75) })
	tag, _ := cliDB.GetTag("RTU.Level")
	if !tag.Timestamp.Equal(at) || tag.Quality != honeycomb.QualityUncertain {
		t.Errorf("client tag at %s quality %s; want %s uncertain", tag.Timestamp, tag.Quality, at)
	}
}
