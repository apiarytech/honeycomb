/*
 * Copyright (C) 2026 Franklin D. Amador
 *
 * This software is dual-licensed under the terms of the GPL v3.0 and
 * a commercial license. You may choose to use this software under either
 * license.
 *
 * See the LICENSE files in the project root for full license text.
 */

package dnp3

import (
	"bytes"
	"context"
	"net"
	"sync"
	"testing"
	"time"
)

// The reset link states frame of the standard's examples: master 1024 to
// outstation 1, with its header CRC.
func TestCRC(t *testing.T) {
	b, err := Frame{Dir: true, Prm: true, Func: LinkResetLinkStates, Dest: 1, Src: 1024}.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{0x05, 0x64, 0x05, 0xC0, 0x01, 0x00, 0x00, 0x04, 0xE9, 0x21}
	if !bytes.Equal(b, want) {
		t.Fatalf("% X, want % X", b, want)
	}
}

func TestFrameRoundTrip(t *testing.T) {
	data := make([]byte, 40)
	for i := range data {
		data[i] = byte(i * 7)
	}
	f := Frame{Prm: true, Func: LinkUnconfirmedData, Dest: 3, Src: 10, Data: data}
	b, _ := f.Marshal()
	got, err := ReadFrame(bytes.NewReader(append([]byte{0xAA, 0x05}, b...))) // noise before it
	if err != nil || !bytes.Equal(got.Data, data) || got.Dest != 3 || got.Src != 10 || !got.Prm || got.Dir {
		t.Fatalf("%+v %v", got, err)
	}
	b[12] ^= 0xFF // a damaged data block
	if _, err := ReadFrame(bytes.NewReader(b)); err != ErrCRC {
		t.Errorf("damaged frame: %v", err)
	}
	if _, err := (Frame{Data: make([]byte, 251)}).Marshal(); err == nil {
		t.Error("251 octets of user data")
	}
	// A fragment over several segments is put back together.
	frag := make([]byte, 600)
	for i := range frag {
		frag[i] = byte(i)
	}
	segs, _ := segments(frag, 62)
	var r reassembler
	var out []byte
	for i, s := range segs {
		got, done := r.add(s)
		if done != (i == len(segs)-1) {
			t.Fatalf("segment %d done %v", i, done)
		}
		out = got
	}
	if len(segs) != 3 || !bytes.Equal(out, frag) {
		t.Errorf("%d segments, %d octets", len(segs), len(out))
	}
}

// source is the points' values, as tags would hold them.
type source struct {
	mu   sync.Mutex
	vals map[Point]Value
	ops  []string
}

func (s *source) Read(p Point) (Value, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.vals[Point{Kind: p.Kind, Index: p.Index}]
	return v, ok
}

func (s *source) Operate(p Point, v Value, by string) uint8 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.vals[Point{Kind: p.Kind, Index: p.Index}] = Value{Bool: v.Bool, Number: v.Number, Online: true}
	s.ops = append(s.ops, by)
	return StatusSuccess
}

func (s *source) set(k Kind, i uint16, v Value) {
	s.mu.Lock()
	s.vals[Point{Kind: k, Index: i}] = v
	s.mu.Unlock()
}

func setup(t *testing.T, commands bool) (*Outstation, *source, *Master) {
	t.Helper()
	src := &source{vals: map[Point]Value{
		{Kind: BinaryInput, Index: 0}: {Bool: true, Online: true}, {Kind: BinaryInput, Index: 1}: {Online: true},
		{Kind: AnalogInput, Index: 0}: {Number: 12.5, Online: true}, {Kind: AnalogInt, Index: 5}: {Number: -40, Online: true},
		{Kind: BinaryOutput, Index: 0}: {Online: true}, {Kind: AnalogOutput, Index: 2}: {Number: 1, Online: true},
	}}
	o, err := New(Config{Address: 10, Master: 1, Source: src, Points: []Point{
		{Kind: BinaryInput, Index: 0, Class: 1}, {Kind: BinaryInput, Index: 1, Class: 1},
		{Kind: AnalogInput, Index: 0, Class: 2}, {Kind: AnalogInt, Index: 5, Class: 3},
		{Kind: BinaryOutput, Index: 0, Writable: true}, {Kind: AnalogOutput, Index: 2, Writable: true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	o.Scan()
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		c, err := ln.Accept()
		ln.Close()
		if err == nil {
			o.Serve(ctx, c, commands, "scada")
		}
	}()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return o, src, NewMaster(conn, 1, 10)
}

func find(ms []Measurement, g uint8, idx uint16) (Measurement, bool) {
	for _, m := range ms {
		if m.Group == g && m.Index == idx {
			return m, true
		}
	}
	return Measurement{}, false
}

func TestIntegrityAndEvents(t *testing.T) {
	o, src, m := setup(t, true)
	r, ms, err := m.Integrity()
	if err != nil {
		t.Fatal(err)
	}
	if r.IIN&IINDeviceRestart == 0 {
		t.Error("no restart indication at start")
	}
	for _, c := range []struct {
		g   uint8
		idx uint16
		b   bool
		n   float64
	}{{1, 0, true, 0}, {1, 1, false, 0}, {30, 0, false, 12.5}, {30, 5, false, -40}, {10, 0, false, 0}, {40, 2, false, 1}} {
		got, ok := find(ms, c.g, c.idx)
		if !ok || got.Bool != c.b || got.Number != c.n || !got.Online {
			t.Errorf("g%d[%d] = %+v %v", c.g, c.idx, got, ok)
		}
	}
	if err := m.ClearRestart(); err != nil {
		t.Fatal(err)
	}

	at := time.Date(2026, 11, 10, 12, 0, 0, 123*int(time.Millisecond), time.UTC)
	src.set(BinaryInput, 1, Value{Bool: true, Online: true, Time: at})
	src.set(AnalogInput, 0, Value{Number: 99.5, Online: true, Time: at.Add(time.Second)})
	o.Scan()
	if o.Pending() != 2 {
		t.Fatalf("%d events", o.Pending())
	}
	r, ms, err = m.Events()
	if err != nil {
		t.Fatal(err)
	}
	if r.IIN&IINDeviceRestart != 0 || r.IIN&(IINClass1Events|IINClass2Events) == 0 || !r.Confirm {
		t.Errorf("IIN %04x confirm %v", r.IIN, r.Confirm)
	}
	bi, ok := find(ms, 2, 1)
	if !ok || !bi.Bool || !bi.Time.Equal(at) {
		t.Errorf("binary event %+v", bi)
	}
	ai, ok := find(ms, 32, 0)
	if !ok || ai.Number != 99.5 || !ai.Time.Equal(at.Add(time.Second)) {
		t.Errorf("analog event %+v", ai)
	}
	// Confirmed: the events are gone, and the next read says so.
	deadline := time.Now().Add(2 * time.Second)
	for o.Pending() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if o.Pending() != 0 {
		t.Fatalf("%d events after the confirmation", o.Pending())
	}
	r, ms, _ = m.Events()
	if len(ms) != 0 || r.IIN&(IINClass1Events|IINClass2Events|IINClass3Events) != 0 {
		t.Errorf("after confirming: %v, IIN %04x", ms, r.IIN)
	}
}

// Events a master read but never confirmed are sent again.
func TestUnconfirmedEventsStay(t *testing.T) {
	o, src, m := setup(t, true)
	src.set(BinaryInput, 0, Value{Bool: false, Online: true, Time: time.Now()})
	o.Scan()
	r, err := m.Request(FuncRead, []byte{60, 2, qAll}) // read, then no confirmation:
	if err != nil {
		t.Fatal(err)
	}
	_ = r // Request confirms; read again by hand without it
	deadline := time.Now().Add(2 * time.Second)
	for o.Pending() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if o.Pending() != 0 {
		t.Fatalf("confirmed read left %d", o.Pending())
	}
	src.set(BinaryInput, 0, Value{Bool: true, Online: true, Time: time.Now()})
	o.Scan()
	seq := m.seq
	m.seq = (m.seq + 1) & 0x0F
	m.sendFragment([]byte{acFIR | acFIN | seq, FuncRead, 60, 2, qAll})
	time.Sleep(200 * time.Millisecond) // the response, unconfirmed
	if o.Pending() != 1 {
		t.Fatalf("an unconfirmed event was released: %d left", o.Pending())
	}
	_, ms, err := m.Events() // reads past the unanswered response
	if err != nil {
		t.Fatal(err)
	}
	if ev, ok := find(ms, 2, 0); !ok || !ev.Bool {
		t.Errorf("the unconfirmed event was not sent again: %v", ms)
	}
}

func TestControls(t *testing.T) {
	_, src, m := setup(t, true)
	if st, err := m.Operate(BinaryOutput, 0, Value{Bool: true}, false); err != nil || st != StatusSuccess {
		t.Fatalf("direct operate: %d %v", st, err)
	}
	if v, _ := src.Read(Point{Kind: BinaryOutput, Index: 0}); !v.Bool {
		t.Error("the binary output was not set")
	}
	if st, err := m.Operate(AnalogOutput, 2, Value{Number: 47.25}, true); err != nil || st != StatusSuccess {
		t.Fatalf("select and operate: %d %v", st, err)
	}
	if v, _ := src.Read(Point{Kind: AnalogOutput, Index: 2}); v.Number != 47.25 {
		t.Errorf("the analog output is %v", v.Number)
	}
	// An operate without its select.
	obj := []byte{41, 3, qIndex16, 1, 0, 2, 0}
	obj = append(append(obj, f32(1)...), 0)
	r, err := m.Request(FuncOperate, obj)
	if err != nil || status(r) != StatusNoSelect {
		t.Errorf("operate alone: %d %v", status(r), err)
	}
	if st, _ := m.Operate(BinaryOutput, 9, Value{Bool: true}, false); st != StatusNotSupported {
		t.Errorf("an unknown output: %d", st)
	}
	if len(src.ops) != 2 || src.ops[0] != "scada" {
		t.Errorf("operations %v", src.ops)
	}
}

func TestReadOnlyMaster(t *testing.T) {
	_, src, m := setup(t, false)
	if st, _ := m.Operate(BinaryOutput, 0, Value{Bool: true}, false); st != StatusNotAuthorized {
		t.Errorf("a read-only master's control: %d", st)
	}
	if v, _ := src.Read(Point{Kind: BinaryOutput, Index: 0}); v.Bool {
		t.Error("a read-only master changed an output")
	}
	if _, ms, err := m.Integrity(); err != nil || len(ms) == 0 {
		t.Errorf("a read-only master cannot read: %v", err)
	}
}

func TestEventBufferBounded(t *testing.T) {
	src := &source{vals: map[Point]Value{{Kind: BinaryInput, Index: 0}: {Online: true}}}
	o, _ := New(Config{Address: 1, Source: src, Events: 3, Points: []Point{{Kind: BinaryInput, Index: 0, Class: 1}}})
	o.Scan()
	for i := range 5 {
		src.set(BinaryInput, 0, Value{Bool: i%2 == 0, Online: true})
		o.Scan()
	}
	if o.Pending() != 3 || o.iin()&IINEventOverflow == 0 {
		t.Errorf("%d events, IIN %04x", o.Pending(), o.iin())
	}
}
