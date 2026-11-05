//go:build linux

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
	"io"
	"testing"
	"time"

	"github.com/apiarytech/honeycomb"
	plc "github.com/apiarytech/royaljelly/iec"
)

// TestServerReadByPLC4XClient: PLC4X's own Modbus RTU client reads the
// server over a serial line (drone D-66). Two pseudo-terminals joined back
// to back stand for the RS-485 line: the client polls one, the server
// answers on the other. A write from the client is refused and changes
// nothing.
func TestServerReadByPLC4XClient(t *testing.T) {
	m1, s1 := openPTY(t)
	m2, s2 := openPTY(t)
	go io.Copy(m2, m1) // the master's requests to the server
	go io.Copy(m1, m2) // the server's replies to the master

	db := honeycomb.NewTagDatabase()
	addTag(t, db, "Level", honeycomb.TypeREAL, plc.REAL(12.5))
	addTag(t, db, "Count", honeycomb.TypeDINT, plc.DINT(-70000))
	addTag(t, db, "Run", honeycomb.TypeBOOL, plc.BOOL(true))
	addTag(t, db, "Read.Level", honeycomb.TypeREAL, plc.REAL(0))
	addTag(t, db, "Read.Count", honeycomb.TypeDINT, plc.DINT(0))
	addTag(t, db, "Read.Run", honeycomb.TypeBOOL, plc.BOOL(false))
	addTag(t, db, "Read.Set", honeycomb.TypeINT, plc.INT(0))

	settings := "serial.baud-rate=19200&serial.parity=even&serial.stop-bits=1"
	errs := make(chan string, 64)
	c, err := New(db, []Connection{
		{
			Name: "server", URL: fmt.Sprintf("modbus-rtu:serial://%s?%s&unit-identifier=7", s2, settings), Mode: Server,
			Bindings: []Binding{
				{Tag: "Level", Address: "holding-register:1:REAL", Direction: Output},
				{Tag: "Count", Address: "input-register:1:DINT", Direction: Output},
				{Tag: "Run", Address: "coil:1", Direction: Output},
				{Tag: "Count", Address: "holding-register:10:DINT", Direction: Output},
			},
		},
		{
			Name: "master", URL: fmt.Sprintf("modbus-rtu:serial://%s?%s&default-unit-identifier=7", s1, settings), Interval: 100 * time.Millisecond,
			Bindings: []Binding{
				{Tag: "Read.Level", Address: "holding-register:1:REAL"},
				{Tag: "Read.Count", Address: "input-register:1:DINT"},
				{Tag: "Read.Run", Address: "coil:1"},
				{Tag: "Read.Set", Address: "holding-register:10:INT", Direction: Output},
			},
		},
	}, WithErrorHandler(func(name string, err error) {
		select {
		case errs <- name + ": " + err.Error():
		default:
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	waitFor(t, func() bool {
		l, _ := db.GetTagValue("Read.Level")
		n, _ := db.GetTagValue("Read.Count")
		r, _ := db.GetTagValue("Read.Run")
		return l == plc.REAL(12.5) && n == plc.DINT(-70000) && r == plc.BOOL(true)
	})
	// A change on the server side reaches the master at its next poll.
	if err := db.SetTagValue("Level", plc.REAL(-3.25)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { v, _ := db.GetTagValue("Read.Level"); return v == plc.REAL(-3.25) })

	// The master writes: refused (read only), the served tag unchanged.
	if err := db.SetTagValue("Read.Set", plc.INT(99)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if v, _ := db.GetTagValue("Count"); v != plc.DINT(-70000) {
		t.Fatalf("a write changed the served tag: %v", v)
	}
	if s, _ := c.Status("server"); !s.Connected || s.Reads == 0 {
		t.Errorf("server status %+v", s)
	}
	if s, _ := c.Status("master"); s.Errors == 0 {
		t.Errorf("the master's write was not refused: %+v", s)
	}
}
