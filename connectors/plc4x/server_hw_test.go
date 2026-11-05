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
	"os"
	"testing"
	"time"

	"github.com/apiarytech/honeycomb"
	plc "github.com/apiarytech/royaljelly/iec"
)

// TestServerOnRealPorts runs the server and PLC4X's client on two real
// serial ports wired together, such as two RS-485 channels of a unit with
// A and B jumpered across (drone D-66, bench). It runs only when asked:
//
//	PLC4X_HW_SERVER=/dev/ttyAMA2 PLC4X_HW_CLIENT=/dev/ttyAMA3 [PLC4X_HW_BAUD=19200] ./plc4x.test -test.run TestServerOnRealPorts -test.v
func TestServerOnRealPorts(t *testing.T) {
	serverPort, clientPort := os.Getenv("PLC4X_HW_SERVER"), os.Getenv("PLC4X_HW_CLIENT")
	if serverPort == "" || clientPort == "" {
		t.Skip("PLC4X_HW_SERVER and PLC4X_HW_CLIENT name two serial ports wired together")
	}
	baud := os.Getenv("PLC4X_HW_BAUD")
	if baud == "" {
		baud = "19200"
	}
	settings := "serial.baud-rate=" + baud + "&serial.data-bits=8&serial.parity=even&serial.stop-bits=1"

	db := honeycomb.NewTagDatabase()
	addTag(t, db, "Level", honeycomb.TypeREAL, plc.REAL(12.5))
	addTag(t, db, "Count", honeycomb.TypeDINT, plc.DINT(-70000))
	addTag(t, db, "Run", honeycomb.TypeBOOL, plc.BOOL(true))
	addTag(t, db, "Read.Level", honeycomb.TypeREAL, plc.REAL(0))
	addTag(t, db, "Read.Count", honeycomb.TypeDINT, plc.DINT(0))
	addTag(t, db, "Read.Run", honeycomb.TypeBOOL, plc.BOOL(false))
	addTag(t, db, "Read.Set", honeycomb.TypeINT, plc.INT(0))

	c, err := New(db, []Connection{
		{
			Name: "server", URL: fmt.Sprintf("modbus-rtu:serial://%s?%s&default-unit-identifier=7", serverPort, settings), Mode: Server,
			Bindings: []Binding{
				{Tag: "Level", Address: "holding-register:1:REAL", Direction: Output},
				{Tag: "Count", Address: "input-register:1:DINT", Direction: Output},
				{Tag: "Run", Address: "coil:1", Direction: Output},
				{Tag: "Count", Address: "holding-register:10:DINT", Direction: Output},
			},
		},
		{
			Name: "master", URL: fmt.Sprintf("modbus-rtu:serial://%s?%s&default-unit-identifier=7", clientPort, settings), Interval: 200 * time.Millisecond,
			Bindings: []Binding{
				{Tag: "Read.Level", Address: "holding-register:1:REAL"},
				{Tag: "Read.Count", Address: "input-register:1:DINT"},
				{Tag: "Read.Run", Address: "coil:1"},
				{Tag: "Read.Set", Address: "holding-register:10:INT", Direction: Output},
			},
		},
	}, WithErrorHandler(func(name string, err error) { t.Logf("%s: %v", name, err) }))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	start := time.Now()
	waitFor(t, func() bool {
		l, _ := db.GetTagValue("Read.Level")
		n, _ := db.GetTagValue("Read.Count")
		r, _ := db.GetTagValue("Read.Run")
		return l == plc.REAL(12.5) && n == plc.DINT(-70000) && r == plc.BOOL(true)
	})
	t.Logf("first read after %v", time.Since(start).Round(time.Millisecond))
	if err := db.SetTagValue("Level", plc.REAL(-3.25)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { v, _ := db.GetTagValue("Read.Level"); return v == plc.REAL(-3.25) })

	if err := db.SetTagValue("Read.Set", plc.INT(99)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	if v, _ := db.GetTagValue("Count"); v != plc.DINT(-70000) {
		t.Fatalf("a write changed the served tag: %v", v)
	}
	s, _ := c.Status("server")
	m, _ := c.Status("master")
	t.Logf("server %+v", s)
	t.Logf("master %+v", m)
	if !s.Connected || s.Reads == 0 {
		t.Errorf("server status %+v", s)
	}
	if m.Errors == 0 {
		t.Errorf("the master's write was not refused: %+v", m)
	}
}
