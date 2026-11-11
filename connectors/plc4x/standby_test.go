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
	"strings"
	"testing"
	"time"

	"github.com/apiarytech/honeycomb"
	plc "github.com/apiarytech/royaljelly/iec"
)

// A device on two paths (R-136): the connection takes the standby when the
// first stops answering, and goes back once the first answers again.
func TestStandbyPath(t *testing.T) {
	oldLost, oldReq := lostAfter, requestTimeout
	lostAfter, requestTimeout = 300*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { lostAfter, requestTimeout = oldLost, oldReq })
	first, standby := startModbusServer(t), startModbusServer(t)
	first.set(0, 1)
	standby.set(0, 2)
	db := honeycomb.NewTagDatabase()
	addTag(t, db, "Level", honeycomb.TypeINT, plc.INT(0))
	connector, err := New(db, []Connection{{
		Name: "rtu", URL: "modbus-tcp://" + first.addr(), Interval: 50 * time.Millisecond, ConnectTimeout: 300 * time.Millisecond,
		Standby:  Standby{URL: "modbus-tcp://" + standby.addr(), After: 2, ReturnEvery: 300 * time.Millisecond},
		Bindings: []Binding{{Tag: "Level", Address: "holding-register:1:INT"}},
	}}, WithErrorHandler(func(name string, err error) { t.Logf("%s: %v", name, err) }))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { connector.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	level := func(want int16) func() bool {
		return func() bool { v, _ := db.GetTagValue("Level"); return v == plc.INT(want) }
	}
	waitFor(t, level(1))

	// The first path stops answering: the standby's value arrives.
	first.setMute(true)
	waitFor(t, level(2))
	if s, _ := connector.Status("rtu"); !s.OnStandby || s.PathSwitches != 1 {
		t.Fatalf("on the standby: %+v", s)
	}

	// It answers again: back on it.
	first.setMute(false)
	waitFor(t, level(1))
	if s, _ := connector.Status("rtu"); s.OnStandby || s.PathSwitches != 2 {
		t.Fatalf("back on the first: %+v", s)
	}
}

func TestStandbyInConfigFile(t *testing.T) {
	cfg, err := ParseConfig(strings.NewReader(`{"connections":[{"name":"r","url":"modbus-tcp://10.0.0.1:502",
		"standby":{"url":"modbus-tcp://10.0.1.1:502","after":2,"returnEvery":"30s"},
		"bindings":[{"tag":"A","address":"holding-register:1:INT"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if s := cfg.Connections[0].Standby; s.URL != "modbus-tcp://10.0.1.1:502" || s.After != 2 || s.ReturnEvery != 30*time.Second {
		t.Fatalf("standby %+v", s)
	}
}

func TestStandbyValidated(t *testing.T) {
	db := honeycomb.NewTagDatabase()
	for _, c := range []Connection{
		{Name: "a", URL: "modbus-tcp://10.0.0.1:502", Standby: Standby{URL: "s7://10.0.0.2"}},
		{Name: "b", URL: "modbus-tcp://10.0.0.1:502", Standby: Standby{URL: "modbus-tcp://10.0.0.2:502", After: -1}},
		{Name: "c", URL: "modbus-tcp://0.0.0.0:502", Mode: Server, Standby: Standby{URL: "modbus-tcp://0.0.0.0:503"}},
	} {
		if _, err := New(db, []Connection{c}); err == nil || !strings.Contains(err.Error(), "standby") {
			t.Errorf("%s: %v", c.Name, err)
		}
	}
	if _, err := New(db, []Connection{{Name: "ok", URL: "modbus-tcp://10.0.0.1:502", Standby: Standby{URL: "modbus-tcp://10.0.0.2:502"}}}); err != nil {
		t.Error(err)
	}
}
