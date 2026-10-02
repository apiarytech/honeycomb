package plc4x

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	apiModel "github.com/apache/plc4x/plc4go/pkg/api/model"
	apiValues "github.com/apache/plc4x/plc4go/pkg/api/values"
	spiValues "github.com/apache/plc4x/plc4go/spi/values"
	"github.com/apiarytech/honeycomb"
	plc "github.com/apiarytech/royaljelly/iec"
)

// run starts the connector and stops it when the test ends.
func run(t *testing.T, c *Connector) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
}

func tagValue(db *honeycomb.TagDatabase, name string) any {
	v, _ := db.GetTagValue(name)
	return v
}

func TestOutputsAreWrittenOnConnectAndOnChange(t *testing.T) {
	server := startModbusServer(t)
	db := honeycomb.NewTagDatabase()
	addTag(t, db, "Valve", honeycomb.TypeINT, plc.INT(5))

	c, err := New(db, []Connection{{
		Name: "sim", URL: "modbus-tcp://" + server.addr(), Interval: 50 * time.Millisecond,
		Bindings: []Binding{{Tag: "Valve", Address: "holding-register:7:INT", Direction: Output}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	run(t, c)

	// The commanded value reaches the device on connect...
	waitFor(t, func() bool { return server.get(6) == 5 })
	// ...and every change after that.
	if err := db.SetTagValue("Valve", plc.INT(11)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return server.get(6) == 11 })

	// Writing the same value again, or changing only the quality, is not a change.
	writes := server.writeCount()
	_ = db.SetTagValue("Valve", plc.INT(11))
	_ = db.SetTagQuality("Valve", honeycomb.QualityUncertain)
	time.Sleep(200 * time.Millisecond)
	if got := server.writeCount(); got != writes {
		t.Errorf("unchanged output was written %d more times", got-writes)
	}
	if s, _ := c.Status("sim"); s.Writes != 2 || s.LastWrite.IsZero() {
		t.Errorf("status %+v, want 2 writes", s)
	}
	// An output is owned by the program, so the connector leaves its quality alone.
	if q, _ := db.GetTagQuality("Valve"); q != honeycomb.QualityUncertain {
		t.Errorf("output quality = %v, want the Uncertain set above", q)
	}
}

func TestInOutSuppressesEchoes(t *testing.T) {
	server := startModbusServer(t)
	server.set(7, 7)
	db := honeycomb.NewTagDatabase()
	addTag(t, db, "Setpoint", honeycomb.TypeINT, plc.INT(0))

	c, err := New(db, []Connection{{
		Name: "sim", URL: "modbus-tcp://" + server.addr(), Interval: 50 * time.Millisecond,
		Bindings: []Binding{{Tag: "Setpoint", Address: "holding-register:8:INT", Direction: InOut}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	run(t, c)

	// The device's value is read first and never written back.
	waitFor(t, func() bool { return tagValue(db, "Setpoint") == plc.INT(7) })
	time.Sleep(200 * time.Millisecond)
	if n := server.writeCount(); n != 0 {
		t.Fatalf("value read from the device was written back %d times", n)
	}

	// A program change is written once; reading it back does not write it again.
	if err := db.SetTagValue("Setpoint", plc.INT(9)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return server.get(7) == 9 })
	time.Sleep(200 * time.Millisecond)
	if n := server.writeCount(); n != 1 {
		t.Errorf("program change written %d times, want 1", n)
	}

	// A change made on the device reaches the tag without a write.
	server.set(7, 3)
	waitFor(t, func() bool { return tagValue(db, "Setpoint") == plc.INT(3) })
	time.Sleep(200 * time.Millisecond)
	if n := server.writeCount(); n != 1 {
		t.Errorf("device change echoed back: %d writes, want 1", n)
	}
}

func TestPendingWriteWinsOverRead(t *testing.T) {
	l := &link{wake: make(chan struct{}, 1), pending: map[string]bool{}, device: map[string]string{}}
	if !l.accept("S", plc.INT(1)) {
		t.Fatal("read rejected with nothing pending")
	}
	if l.changed("S", snapshot(plc.INT(1))) {
		t.Error("value just read from the device counts as a change")
	}
	l.markPending("S")
	if l.accept("S", plc.INT(2)) {
		t.Error("read overwrote a change waiting to be written")
	}
}

func TestSubscriptionFallsBackToPolling(t *testing.T) {
	server := startModbusServer(t)
	server.set(4, 42)
	db := honeycomb.NewTagDatabase()
	addTag(t, db, "Level", honeycomb.TypeINT, plc.INT(0))

	var mu sync.Mutex
	var reported []string
	c, err := New(db, []Connection{{
		Name: "sim", URL: "modbus-tcp://" + server.addr(), Interval: 50 * time.Millisecond, Mode: ChangeOfState,
		Bindings: []Binding{{Tag: "Level", Address: "holding-register:5:INT"}},
	}}, WithErrorHandler(func(_ string, err error) {
		mu.Lock()
		reported = append(reported, err.Error())
		mu.Unlock()
	}))
	if err != nil {
		t.Fatal(err)
	}
	run(t, c)

	// Modbus cannot subscribe, so the connection polls and says so.
	waitFor(t, func() bool { return tagValue(db, "Level") == plc.INT(42) })
	if s, _ := c.Status("sim"); s.Subscribed {
		t.Error("Status reports a subscription Modbus cannot provide")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reported) == 0 || !strings.Contains(reported[0], "cannot subscribe") {
		t.Errorf("fallback not reported; errors: %q", reported)
	}
}

// fakeEvent is a subscription event with fixed codes and values.
type fakeEvent struct {
	codes  map[string]apiModel.PlcResponseCode
	values map[string]apiValues.PlcValue
}

func (e fakeEvent) String() string           { return "fakeEvent" }
func (e fakeEvent) IsAPlcMessage() bool      { return true }
func (e fakeEvent) GetAddress(string) string { return "" }
func (e fakeEvent) GetSource(string) string  { return "" }
func (e fakeEvent) GetTagNames() []string {
	var names []string
	for name := range e.codes {
		names = append(names, name)
	}
	return names
}
func (e fakeEvent) GetResponseCode(name string) apiModel.PlcResponseCode { return e.codes[name] }
func (e fakeEvent) GetValue(name string) apiValues.PlcValue              { return e.values[name] }

func TestSubscriptionEventsSetValuesAndQuality(t *testing.T) {
	db := honeycomb.NewTagDatabase()
	for _, name := range []string{"A", "B", "C", "Out"} {
		addTag(t, db, name, honeycomb.TypeINT, plc.INT(0))
	}
	c, err := New(db, []Connection{{
		Name: "sub", URL: "modbus-tcp://127.0.0.1:1", Mode: ChangeOfState, // never connected
		Bindings: []Binding{{Tag: "A", Address: "a"}, {Tag: "B", Address: "b"}, {Tag: "C", Address: "c"},
			{Tag: "Out", Address: "o", Direction: Output}},
	}}, WithDriverManager(nil))
	if err != nil {
		t.Fatal(err)
	}
	cs := c.connections[0]
	c.handleEvent(cs, fakeEvent{
		codes: map[string]apiModel.PlcResponseCode{
			"A": apiModel.PlcResponseCode_OK, "B": apiModel.PlcResponseCode_REQUEST_TIMEOUT,
			"C": apiModel.PlcResponseCode_NOT_FOUND, "Out": apiModel.PlcResponseCode_OK,
		},
		values: map[string]apiValues.PlcValue{"A": spiValues.NewPlcINT(12), "Out": spiValues.NewPlcINT(99)},
	})

	if v := tagValue(db, "A"); v != plc.INT(12) {
		t.Errorf("A = %v, want 12", v)
	}
	if v := tagValue(db, "Out"); v != plc.INT(0) {
		t.Errorf("an event changed output Out to %v", v)
	}
	for name, want := range map[string]honeycomb.Quality{
		"A": honeycomb.QualityGood, "B": honeycomb.QualityUncertain, "C": honeycomb.QualityBad,
	} {
		if q, _ := db.GetTagQuality(name); q != want {
			t.Errorf("%s quality = %v, want %v", name, q, want)
		}
	}
	if s, _ := c.Status("sub"); s.Reads != 1 || s.Errors != 1 || s.LastError == nil {
		t.Errorf("status %+v, want 1 read with 1 error", s)
	}
}

type testInner struct {
	Count plc.DINT
}

type testMotor struct {
	Speed   plc.REAL
	Running plc.BOOL `plc4x:"run"`
	Note    string   `plc4x:"-"`
	Inner   *testInner
	hidden  int
}

func TestConvertStructsToUDTs(t *testing.T) {
	device := spiValues.NewPlcStruct(map[string]apiValues.PlcValue{
		"SPEED": spiValues.NewPlcREAL(1500), // members match case-insensitively
		"run":   spiValues.NewPlcBOOL(true),
		"Inner": spiValues.NewPlcStruct(map[string]apiValues.PlcValue{"Count": spiValues.NewPlcDINT(3)}),
	})
	got, err := convert(reflect.TypeFor[*testMotor](), device)
	if err != nil {
		t.Fatal(err)
	}
	want := &testMotor{Speed: 1500, Running: true, Inner: &testInner{Count: 3}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}

	// Arrays of UDTs.
	list := spiValues.NewPlcList([]apiValues.PlcValue{device, device})
	if arr, err := convert(reflect.TypeFor[[]*testMotor](), list); err != nil || len(arr.([]*testMotor)) != 2 {
		t.Errorf("array of UDTs: %v (%v)", arr, err)
	}

	// The reverse direction produces a struct that converts back to the same UDT.
	back, err := toPlcValue(reflect.ValueOf(want))
	if err != nil {
		t.Fatal(err)
	}
	if !back.IsStruct() || !back.HasKey("run") || back.HasKey("Note") {
		t.Errorf("toPlcValue members = %v", back.GetKeys())
	}
	if again, err := convert(reflect.TypeFor[*testMotor](), back); err != nil || !reflect.DeepEqual(again, want) {
		t.Errorf("round trip = %+v (%v)", again, err)
	}

	missing := spiValues.NewPlcStruct(map[string]apiValues.PlcValue{"Speed": spiValues.NewPlcREAL(1)})
	if _, err := convert(reflect.TypeFor[*testMotor](), missing); err == nil || !strings.Contains(err.Error(), `"run"`) {
		t.Errorf("missing member: %v", err)
	}
	if _, err := convert(reflect.TypeFor[*testMotor](), spiValues.NewPlcINT(1)); err == nil {
		t.Error("a scalar converted into a UDT")
	}
}

func TestToDevice(t *testing.T) {
	cases := []struct {
		in   any
		want apiValues.PlcValueType
	}{
		{plc.DINT(-4), apiValues.DINT},
		{plc.WORD(4), apiValues.WORD},
		{plc.REAL(1.5), apiValues.REAL},
		{plc.TIME(time.Second), apiValues.TIME},
		{plc.DT(time.Now()), apiValues.DATE_AND_TIME},
		{plc.STRING("x"), apiValues.STRING},
		{plc.WSTRING("wide"), apiValues.WSTRING},
	}
	for _, tc := range cases {
		got, err := toDevice(tc.in)
		if err != nil {
			t.Errorf("toDevice(%T): %v", tc.in, err)
			continue
		}
		if pv := got.(apiValues.PlcValue); pv.GetPlcValueType() != tc.want {
			t.Errorf("toDevice(%T) = %s, want %s", tc.in, pv.GetPlcValueType(), tc.want)
		}
	}
	if got, _ := toDevice(plc.DINT(-4)); got.(apiValues.PlcValue).GetInt32() != -4 {
		t.Error("DINT value lost")
	}

	// PLC4X expects a Go slice for array addresses.
	arr, err := toDevice([]plc.INT{1, 2})
	if items, ok := arr.([]any); err != nil || !ok || len(items) != 2 {
		t.Errorf("toDevice(array) = %#v (%v)", arr, err)
	}
	if _, err := toDevice(complex(1, 2)); err == nil {
		t.Error("unsupported type accepted")
	}
}

const sampleConfig = `{
  "diagnostics": "PLC4X.",
  "connections": [{
    "name": "press1",
    "url": "modbus-tcp://10.0.0.5:502",
    "interval": "100ms",
    "mode": "cyclic",
    "bindings": [
      {"tag": "Pressure", "address": "holding-register:1:REAL"},
      {"tag": "Valve", "address": "holding-register:3:INT", "direction": "output"},
      {"tag": "Setpoint", "address": "holding-register:4:INT", "direction": "inout"}
    ]
  }]
}`

func TestParseConfig(t *testing.T) {
	cfg, err := ParseConfig(strings.NewReader(sampleConfig))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		Diagnostics: "PLC4X.",
		Connections: []Connection{{
			Name: "press1", URL: "modbus-tcp://10.0.0.5:502", Interval: 100 * time.Millisecond, Mode: Cyclic,
			Bindings: []Binding{
				{Tag: "Pressure", Address: "holding-register:1:REAL"},
				{Tag: "Valve", Address: "holding-register:3:INT", Direction: Output},
				{Tag: "Setpoint", Address: "holding-register:4:INT", Direction: InOut},
			},
		}},
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("got %+v\nwant %+v", cfg, want)
	}

	for name, bad := range map[string]string{
		"unknown key":   `{"connections": [{"name": "a", "url": "x", "intervall": "1s"}]}`,
		"bad direction": `{"connections": [{"name": "a", "url": "x", "bindings": [{"tag": "t", "address": "a", "direction": "sideways"}]}]}`,
		"bad mode":      `{"connections": [{"name": "a", "url": "x", "mode": "push"}]}`,
		"bad interval":  `{"connections": [{"name": "a", "url": "x", "interval": "soon"}]}`,
	} {
		if _, err := ParseConfig(strings.NewReader(bad)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	path := filepath.Join(t.TempDir(), "plc4x.json")
	if err := os.WriteFile(path, []byte(sampleConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	if loaded, err := LoadConfig(path); err != nil || !reflect.DeepEqual(loaded, want) {
		t.Errorf("LoadConfig = %+v (%v)", loaded, err)
	}
	if _, err := LoadConfig(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Error("missing file accepted")
	}
}

func TestDiagnosticTags(t *testing.T) {
	server := startModbusServer(t)
	server.set(0, 5)
	db := honeycomb.NewTagDatabase()
	addTag(t, db, "Level", honeycomb.TypeINT, plc.INT(0))
	cfg := Config{Diagnostics: "PLC4X.", Connections: []Connection{{
		Name: "sim", URL: "modbus-tcp://" + server.addr(), Interval: 50 * time.Millisecond,
		Bindings: []Binding{{Tag: "Level", Address: "holding-register:1:INT"}, {Tag: "Missing", Address: "holding-register:2:INT"}},
	}}}
	c, err := NewFromConfig(db, cfg)
	if err != nil {
		t.Fatal(err)
	}

	// The tags exist before Run, showing a disconnected connection.
	if v := tagValue(db, "PLC4X.sim.Connected"); v != plc.BOOL(false) {
		t.Fatalf("Connected before Run = %v", v)
	}
	run(t, c)
	waitFor(t, func() bool {
		reads, _ := tagValue(db, "PLC4X.sim.Reads").(plc.ULINT)
		return tagValue(db, "PLC4X.sim.Connected") == plc.BOOL(true) && reads > 0
	})
	// "Missing" is not a tag, so every poll reports an error.
	waitFor(t, func() bool {
		msg, _ := tagValue(db, "PLC4X.sim.LastError").(plc.STRING)
		return strings.Contains(string(msg), "Missing")
	})
	if lastRead, _ := tagValue(db, "PLC4X.sim.LastRead").(plc.DT); time.Time(lastRead).IsZero() {
		t.Error("LastRead not published")
	}
	if q, _ := db.GetTagQuality("PLC4X.sim.Connected"); q != honeycomb.QualityGood {
		t.Errorf("diagnostic tag quality = %v, want Good", q)
	}
}

func TestRunRejectsUnwatchableOutputs(t *testing.T) {
	db := honeycomb.NewTagDatabase()
	c, err := New(db, []Connection{{
		Name: "sim", URL: "modbus-tcp://127.0.0.1:1",
		Bindings: []Binding{{Tag: "NoSuchTag", Address: "holding-register:1:INT", Direction: Output}},
	}}, WithDriverManager(nil))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Run(ctx); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Run = %v, want an error naming the missing output tag", err)
	}
}

func TestNewValidatesBindings(t *testing.T) {
	db := honeycomb.NewTagDatabase()
	for name, conn := range map[string]Connection{
		"empty tag":     {Name: "a", URL: "x", Bindings: []Binding{{Address: "a"}}},
		"duplicate tag": {Name: "a", URL: "x", Bindings: []Binding{{Tag: "t", Address: "a"}, {Tag: "t", Address: "b"}}},
		"bad direction": {Name: "a", URL: "x", Bindings: []Binding{{Tag: "t", Address: "a", Direction: 9}}},
		"bad mode":      {Name: "a", URL: "x", Mode: 9},
	} {
		if _, err := New(db, []Connection{conn}, WithDriverManager(nil)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestConvertWSTRING(t *testing.T) {
	got, err := convert(reflect.TypeFor[plc.WSTRING](), spiValues.NewPlcWSTRING("äöü"))
	if err != nil || got != plc.WSTRING("äöü") {
		t.Errorf("convert WSTRING = %v (%v)", got, err)
	}
}
