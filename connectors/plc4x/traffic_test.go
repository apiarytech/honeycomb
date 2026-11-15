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
	"errors"
	"testing"
	"time"

	"github.com/apiarytech/honeycomb"
	plc "github.com/apiarytech/royaljelly/iec"
)

// The traffic of a Modbus TCP device (R-137): requests sent and answers
// received, recorded only while switched on.
func TestTraffic(t *testing.T) {
	server := startModbusServer(t)
	server.set(0, 7)
	db := honeycomb.NewTagDatabase()
	addTag(t, db, "Level", honeycomb.TypeINT, plc.INT(0))
	connector, err := New(db, []Connection{{
		Name: "box", URL: "modbus-tcp://" + server.addr(), Interval: 50 * time.Millisecond,
		Bindings: []Binding{{Tag: "Level", Address: "holding-register:1:INT"}},
	}}, WithTraffic())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { connector.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	waitFor(t, func() bool { v, _ := db.GetTagValue("Level"); return v == plc.INT(7) })

	if f, _, on, err := connector.Traffic("box", 0); err != nil || on || len(f) != 0 {
		t.Fatalf("recorded before switched on: %v %v %v", f, on, err)
	}
	if err := connector.SetTraffic("box", true); err != nil {
		t.Fatal(err)
	}
	var frames []Frame
	next := 0
	waitFor(t, func() bool {
		f, n, _, _ := connector.Traffic("box", next)
		frames, next = append(frames, f...), n
		return len(frames) >= 4
	})
	var sent, received int
	for _, f := range frames {
		if f.Sent {
			sent++
			// MBAP header, unit, then "read holding registers".
			if len(f.Data) < 8 || f.Data[7] != 0x03 {
				t.Errorf("a request %x", f.Data)
			}
		} else {
			received++
		}
	}
	if sent == 0 || received == 0 {
		t.Fatalf("sent %d, received %d: %+v", sent, received, frames)
	}
	// Asking again from next gives only what came after.
	if f, n, on, _ := connector.Traffic("box", next); !on || n < next || len(f) > n-next {
		t.Fatalf("from %d: %d frames, next %d", next, len(f), n)
	}
	connector.SetTraffic("box", false)
	if _, _, on, _ := connector.Traffic("box", 0); on {
		t.Error("still recorded after switched off")
	}
	if err := connector.SetTraffic("nope", true); err == nil {
		t.Error("a connection that does not exist")
	}

	// Without WithTraffic: refused, and the connector works as before.
	plain, _ := New(honeycomb.NewTagDatabase(), []Connection{{Name: "box", URL: "modbus-tcp://" + server.addr()}})
	if err := plain.SetTraffic("box", true); !errors.Is(err, ErrNoTraffic) {
		t.Errorf("without WithTraffic: %v", err)
	}
}

func TestTrafficAddress(t *testing.T) {
	for in, want := range map[string]string{
		"modbus-tcp://10.0.0.5:502?unit-identifier=1":             "10.0.0.5:502",
		"modbus-rtu:serial:///dev/ttyAMA2?serial.baud-rate=19200": "/dev/ttyAMA2",
		"modbus-rtu:serial://COM3?serial.baud-rate=19200":         "COM3",
		"s7://192.168.0.10":             "192.168.0.10",
		"modbus-tcp:tcp://10.0.0.6:502": "10.0.0.6:502",
	} {
		if got := trafficAddress(in); got != want {
			t.Errorf("%s: %q, want %q", in, got, want)
		}
	}
}
