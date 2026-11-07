package plc4x

import (
	"context"
	"math"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apiarytech/honeycomb"
	plc "github.com/apiarytech/royaljelly/iec"
)

func runConnector(t *testing.T, c *Connector) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
}

func TestScalingBothWays(t *testing.T) {
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
	lin := Scaling{Kind: Linear, RawLow: 0, RawHigh: 4000, ScaledLow: 0, ScaledHigh: 10}
	for raw, want := range map[float64]float64{0: 0, 2000: 5, 4000: 10, 5000: 12.5} {
		if got := lin.Scale(raw); !near(got, want) {
			t.Errorf("linear Scale(%g) = %g, want %g", raw, got, want)
		}
		if back := lin.Unscale(want); !near(back, raw) {
			t.Errorf("linear Unscale(%g) = %g, want %g", want, back, raw)
		}
	}
	clamped := lin
	clamped.ClampLow, clamped.ClampHigh = true, true
	if got := clamped.Scale(5000); got != 10 {
		t.Errorf("clamped Scale(5000) = %g, want 10", got)
	}
	if got := clamped.Scale(-100); got != 0 {
		t.Errorf("clamped Scale(-100) = %g, want 0", got)
	}
	neg := lin
	neg.Negate = true
	if got := neg.Scale(2000); got != -5 {
		t.Errorf("negated Scale(2000) = %g, want -5", got)
	}
	if got := neg.Unscale(-5); !near(got, 2000) {
		t.Errorf("negated Unscale(-5) = %g, want 2000", got)
	}
	sq := Scaling{Kind: SquareRoot, RawLow: 0, RawHigh: 100, ScaledLow: 0, ScaledHigh: 50}
	if got := sq.Scale(25); !near(got, 25) { // sqrt(0.25) = 0.5
		t.Errorf("square root Scale(25) = %g, want 25", got)
	}
	if got := sq.Unscale(25); !near(got, 25) {
		t.Errorf("square root Unscale(25) = %g, want 25", got)
	}
	if got := sq.Scale(-10); got != 0 {
		t.Errorf("square root of a negative fraction = %g, want 0", got)
	}
	if (Scaling{}).Scale(7) != 7 || (Scaling{}).Unscale(7) != 7 {
		t.Error("no scaling changed the value")
	}
	if err := (Scaling{Kind: Linear, RawHigh: 1, RawLow: 1, ScaledHigh: 1}).check(); err == nil {
		t.Error("a raw range of zero was accepted")
	}
}

func TestScaledModbusReadAndWrite(t *testing.T) {
	server := startModbusServer(t)
	server.set(0, 2000)
	db := honeycomb.NewTagDatabase()
	addTag(t, db, "Pressure", honeycomb.TypeREAL, plc.REAL(0))
	addTag(t, db, "Setpoint", honeycomb.TypeREAL, plc.REAL(0))
	addTag(t, db, "Percent", honeycomb.TypeINT, plc.INT(0))
	sc := Scaling{Kind: Linear, RawLow: 0, RawHigh: 4000, ScaledLow: 0, ScaledHigh: 10, Units: "bar"}
	c, err := New(db, []Connection{{
		Name: "sim", URL: "modbus-tcp://" + server.addr(), Interval: 50 * time.Millisecond,
		Bindings: []Binding{
			{Tag: "Pressure", Address: "holding-register:1:INT", Scaling: sc},
			{Tag: "Percent", Address: "holding-register:1:INT", Scaling: Scaling{Kind: Linear, RawLow: 0, RawHigh: 4000, ScaledLow: 0, ScaledHigh: 100}},
			{Tag: "Setpoint", Address: "holding-register:2:INT", Direction: Output, Scaling: sc},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	runConnector(t, c)
	waitFor(t, func() bool { v, _ := db.GetTagValue("Pressure"); return v == plc.REAL(5) })
	waitFor(t, func() bool { v, _ := db.GetTagValue("Percent"); return v == plc.INT(50) })
	if err := db.SetTagValue("Setpoint", plc.REAL(2.5)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return server.get(1) == 1000 })
	// A whole register takes the raw value rounded half to even.
	if err := db.SetTagValue("Setpoint", plc.REAL(0.000625)); err != nil { // raw 0.25
		t.Fatal(err)
	}
	waitFor(t, func() bool { return server.get(1) == 0 })
}

func TestPollGroupsReadAtTheirOwnRate(t *testing.T) {
	server := startModbusServer(t)
	server.set(0, 1, 1)
	db := honeycomb.NewTagDatabase()
	addTag(t, db, "Fast", honeycomb.TypeINT, plc.INT(0))
	addTag(t, db, "Slow", honeycomb.TypeINT, plc.INT(0))
	c, err := New(db, []Connection{{
		Name: "sim", URL: "modbus-tcp://" + server.addr(), Interval: 50 * time.Millisecond,
		PollGroups: []PollGroup{{Name: "hourly", Interval: time.Hour}},
		Bindings: []Binding{
			{Tag: "Fast", Address: "holding-register:1:INT"},
			{Tag: "Slow", Address: "holding-register:2:INT", PollGroup: "hourly"},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	runConnector(t, c)
	// Both are read when the session starts.
	waitFor(t, func() bool {
		f, _ := db.GetTagValue("Fast")
		s, _ := db.GetTagValue("Slow")
		return f == plc.INT(1) && s == plc.INT(1)
	})
	server.set(0, 2, 2)
	waitFor(t, func() bool { v, _ := db.GetTagValue("Fast"); return v == plc.INT(2) })
	time.Sleep(300 * time.Millisecond)
	if v, _ := db.GetTagValue("Slow"); v != plc.INT(1) {
		t.Errorf("the hourly group was read again: %v", v)
	}
}

func TestDisabledConnectionStaysAway(t *testing.T) {
	server := startModbusServer(t)
	server.set(0, 7)
	db := honeycomb.NewTagDatabase()
	addTag(t, db, "Level", honeycomb.TypeINT, plc.INT(0))
	addTag(t, db, "Valve", honeycomb.TypeINT, plc.INT(0))
	c, err := New(db, []Connection{{
		Name: "off", URL: "modbus-tcp://" + server.addr(), Interval: 20 * time.Millisecond, Disabled: true,
		Bindings: []Binding{{Tag: "Level", Address: "holding-register:1:INT"}, {Tag: "Valve", Address: "holding-register:2:INT", Direction: Output}},
	}}, WithDiagnostics("D."))
	if err != nil {
		t.Fatal(err)
	}
	runConnector(t, c)
	waitFor(t, func() bool { s, _ := c.Status("off"); return s.Disabled })
	db.SetTagValue("Valve", plc.INT(3))
	time.Sleep(200 * time.Millisecond)
	if v, _ := db.GetTagValue("Level"); v != plc.INT(0) {
		t.Errorf("a disabled connection read %v", v)
	}
	if q, _ := db.GetTagQuality("Level"); q != honeycomb.QualityBad {
		t.Errorf("a disabled connection's input quality = %v, want Bad", q)
	}
	if server.writeCount() != 0 {
		t.Error("a disabled connection wrote")
	}
}

func TestDemotionAfterSuccessiveFailures(t *testing.T) {
	db := honeycomb.NewTagDatabase()
	addTag(t, db, "Level", honeycomb.TypeINT, plc.INT(0))
	c, err := New(db, []Connection{{
		Name: "gone", URL: "modbus-tcp://127.0.0.1:1", Interval: 20 * time.Millisecond,
		ConnectTimeout: 500 * time.Millisecond,
		Demotion:       Demotion{After: 2, Period: 400 * time.Millisecond},
		Bindings:       []Binding{{Tag: "Level", Address: "holding-register:1:INT"}},
	}}, WithDiagnostics("D."))
	if err != nil {
		t.Fatal(err)
	}
	runConnector(t, c)
	waitFor(t, func() bool { s, _ := c.Status("gone"); return s.Demoted && !s.DemotedUntil.IsZero() })
	if v, _ := db.GetTagValue("D.gone.Demoted"); v != plc.BOOL(true) {
		t.Errorf("D.gone.Demoted = %v, want TRUE", v)
	}
	if s, _ := c.Status("gone"); s.ErrorSince.IsZero() {
		t.Error("ErrorSince is not set while failing")
	} else if s.Failures != 2 {
		t.Errorf("Failures = %d when demoted, want the setpoint 2", s.Failures)
	}
	// It comes back after the period, tries again and is demoted again.
	waitFor(t, func() bool { s, _ := c.Status("gone"); return s.Demotions >= 2 })
}

func TestChannelTakesTurns(t *testing.T) {
	tn := newTurn(30 * time.Millisecond)
	a := &connState{Connection: Connection{Name: "a"}, turn: tn}
	b := &connState{Connection: Connection{Name: "b"}, turn: tn}
	var busy, overlap atomic.Int32
	var mu sync.Mutex
	var order []string
	var last string
	var lastEnd time.Time
	var gaps []time.Duration
	var wg sync.WaitGroup
	for _, cs := range []*connState{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 5 {
				release, err := cs.acquire(context.Background())
				if err != nil {
					t.Error(err)
					return
				}
				if busy.Add(1) > 1 {
					overlap.Add(1)
				}
				mu.Lock()
				if last != "" && last != cs.Name {
					gaps = append(gaps, time.Since(lastEnd))
				}
				order = append(order, cs.Name)
				mu.Unlock()
				time.Sleep(5 * time.Millisecond)
				busy.Add(-1)
				mu.Lock()
				last, lastEnd = cs.Name, time.Now()
				mu.Unlock()
				release()
			}
		}()
	}
	wg.Wait()
	if overlap.Load() > 0 {
		t.Errorf("%d requests overlapped on one channel", overlap.Load())
	}
	if len(order) != 10 {
		t.Fatalf("order %v", order)
	}
	for _, g := range gaps {
		if g < 25*time.Millisecond {
			t.Errorf("a request to another device followed after %s, want the 30ms delay", g)
		}
	}
}

func TestInterRequestDelay(t *testing.T) {
	cs := &connState{Connection: Connection{Name: "a", InterRequestDelay: 40 * time.Millisecond}}
	r, _ := cs.acquire(context.Background())
	r()
	start := time.Now()
	r, _ = cs.acquire(context.Background())
	r()
	if d := time.Since(start); d < 35*time.Millisecond {
		t.Errorf("second request after %s, want 40ms", d)
	}
}

func TestChannelsOverModbus(t *testing.T) {
	server := startModbusServer(t)
	server.set(0, 11, 22)
	db := honeycomb.NewTagDatabase()
	addTag(t, db, "A", honeycomb.TypeINT, plc.INT(0))
	addTag(t, db, "B", honeycomb.TypeINT, plc.INT(0))
	url := "modbus-tcp://" + server.addr()
	c, err := NewFromConfig(db, Config{
		Channels: []Channel{{Name: "line1", InterDeviceDelay: 5 * time.Millisecond}},
		Connections: []Connection{
			{Name: "u1", URL: url, Channel: "line1", Interval: 30 * time.Millisecond, Bindings: []Binding{{Tag: "A", Address: "holding-register:1:INT"}}},
			{Name: "u2", URL: url, Channel: "line1", Interval: 30 * time.Millisecond, Bindings: []Binding{{Tag: "B", Address: "holding-register:2:INT"}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	runConnector(t, c)
	waitFor(t, func() bool {
		a, _ := db.GetTagValue("A")
		b, _ := db.GetTagValue("B")
		return a == plc.INT(11) && b == plc.INT(22)
	})
}

func TestEventModeFallsBackToPolling(t *testing.T) {
	server := startModbusServer(t)
	server.set(0, 5)
	db := honeycomb.NewTagDatabase()
	addTag(t, db, "Level", honeycomb.TypeINT, plc.INT(0))
	c, err := New(db, []Connection{{Name: "sim", URL: "modbus-tcp://" + server.addr(), Mode: Event, Interval: 30 * time.Millisecond,
		Bindings: []Binding{{Tag: "Level", Address: "holding-register:1:INT"}}}})
	if err != nil {
		t.Fatal(err)
	}
	runConnector(t, c)
	waitFor(t, func() bool { v, _ := db.GetTagValue("Level"); return v == plc.INT(5) })
	if s, _ := c.Status("sim"); s.Subscribed {
		t.Error("Modbus reported a subscription")
	}
}

func TestNewChecksTheModel(t *testing.T) {
	db := honeycomb.NewTagDatabase()
	addTag(t, db, "X", honeycomb.TypeINT, plc.INT(0))
	cases := map[string]Connection{
		"no poll group":       {Name: "a", URL: "modbus-tcp://h", Bindings: []Binding{{Tag: "X", Address: "coil:1", PollGroup: "nope"}}},
		"group with no rate":  {Name: "a", URL: "modbus-tcp://h", PollGroups: []PollGroup{{Name: "g"}}},
		"bad scaling":         {Name: "a", URL: "modbus-tcp://h", Bindings: []Binding{{Tag: "X", Address: "coil:1", Scaling: Scaling{Kind: Linear}}}},
		"server on a channel": {Name: "a", URL: "modbus-tcp://0.0.0.0:1502?default-unit-identifier=1", Mode: Server, Channel: "c"},
		"negative timeout":    {Name: "a", URL: "modbus-tcp://h", RequestTimeout: -1},
	}
	for name, conn := range cases {
		if _, err := New(db, []Connection{conn}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestCapabilitiesCoverEveryProtocol(t *testing.T) {
	for _, code := range KnownProtocols() {
		if _, ok := capabilities[code]; !ok {
			t.Errorf("no capabilities for %s", code)
		}
	}
	if !CapabilitiesOf("modbus-tcp").Server || CapabilitiesOf("modbus-tcp").Browse {
		t.Errorf("modbus-tcp: %+v", CapabilitiesOf("modbus-tcp"))
	}
	if c := CapabilitiesOf("opcua"); !c.Subscribe || !c.Events {
		t.Errorf("opcua: %+v", c)
	}
	if _, err := Browse(context.Background(), "modbus-tcp://127.0.0.1:1", ""); err == nil || !strings.Contains(err.Error(), "cannot list") {
		t.Errorf("Browse on Modbus: %v", err)
	}
	if !slices.Contains(modeNames, "event") {
		t.Error("no name for Event")
	}
}
