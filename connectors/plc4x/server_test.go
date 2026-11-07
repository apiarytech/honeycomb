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
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/apiarytech/honeycomb"
	plc "github.com/apiarytech/royaljelly/iec"

	"github.com/apache/plc4x/plc4go/spi/transports/serial/serialport"
)

const serverURL = "modbus-rtu:serial:///dev/ttyAMA2?serial.baud-rate=19200&serial.data-bits=8&serial.parity=even&serial.stop-bits=1&unit-identifier=7"

// request is an RTU request frame for unit 7: function, start (wire), qty.
func request(unit, fc byte, start, qty uint16) []byte {
	f := []byte{unit, fc, byte(start >> 8), byte(start), byte(qty >> 8), byte(qty)}
	crc := rtuCRC(f)
	return append(f, byte(crc), byte(crc>>8))
}

func newTestServer(t *testing.T, bindings ...Binding) (*Connector, *server) {
	t.Helper()
	db := honeycomb.NewTagDatabase()
	addTag(t, db, "Level", honeycomb.TypeREAL, plc.REAL(1.5))
	addTag(t, db, "Count", honeycomb.TypeINT, plc.INT(-2))
	addTag(t, db, "Total", honeycomb.TypeDINT, plc.DINT(70000))
	addTag(t, db, "Run", honeycomb.TypeBOOL, plc.BOOL(true))
	addTag(t, db, "Trip", honeycomb.TypeBOOL, plc.BOOL(false))
	addTag(t, db, "Alarm", honeycomb.TypeBOOL, plc.BOOL(true))
	c, err := New(db, []Connection{{Name: "SCADA", URL: serverURL, Mode: Server, Bindings: bindings}})
	if err != nil {
		t.Fatal(err)
	}
	return c, c.connections[0].server
}

func out(tag, addr string) Binding { return Binding{Tag: tag, Address: addr, Direction: Output} }

// TestServerAnswersReads: registers in PLC4X's encoding (big-endian, REAL
// in two registers, high word first), coils packed low bit first, 0 where no
// binding is, and the frame's unit and CRC (drone D-66).
func TestServerAnswersReads(t *testing.T) {
	c, s := newTestServer(t,
		out("Level", "holding-register:1:REAL"), out("Count", "holding-register:3:INT"),
		out("Total", "40010:DINT"),
		out("Run", "coil:1"), out("Alarm", "coil:3"), out("Trip", "discrete-input:2"),
		out("Count", "input-register:5:INT"))
	if s.unit != 7 || s.port != "/dev/ttyAMA2" || s.config.BaudRate != 19200 || s.config.Parity != serialport.ParityEven {
		t.Fatalf("server %+v", s)
	}

	// Holding registers 1 to 4: Level (2), Count, and an unbound one.
	got := s.answer(c, "SCADA", request(7, 3, 0, 4))
	want := []byte{7, 3, 8}
	want = binary.BigEndian.AppendUint32(want, math.Float32bits(1.5))
	want = binary.BigEndian.AppendUint16(want, uint16(0xFFFE)) // -2
	want = append(want, 0, 0)
	crc := rtuCRC(want)
	want = append(want, byte(crc), byte(crc>>8))
	if !bytes.Equal(got, want) {
		t.Fatalf("holding registers\n got % x\nwant % x", got, want)
	}

	// 40010 is wire address 9: a DINT in two registers.
	if got := s.answer(c, "SCADA", request(7, 3, 9, 2)); !bytes.Equal(got[3:7], binary.BigEndian.AppendUint32(nil, 70000)) {
		t.Fatalf("DINT % x", got)
	}
	// Coils 1 to 3: Run, unbound, Alarm -> 0b101.
	if got := s.answer(c, "SCADA", request(7, 1, 0, 3)); len(got) != 6 || got[2] != 1 || got[3] != 0b101 {
		t.Fatalf("coils % x", got)
	}
	// Discrete input 2 is Trip, false.
	if got := s.answer(c, "SCADA", request(7, 2, 1, 1)); len(got) != 6 || got[3] != 0 {
		t.Fatalf("discrete input % x", got)
	}
	// Input register 5 (wire 4).
	if got := s.answer(c, "SCADA", request(7, 4, 4, 1)); !bytes.Equal(got[2:5], []byte{2, 0xFF, 0xFE}) {
		t.Fatalf("input register % x", got)
	}
	// The value now, not a copy: a change reads at once.
	if err := c.db.SetTagValue("Count", plc.INT(12)); err != nil {
		t.Fatal(err)
	}
	if got := s.answer(c, "SCADA", request(7, 3, 2, 1)); !bytes.Equal(got[3:5], []byte{0, 12}) {
		t.Fatalf("after a change % x", got)
	}
}

// TestServerIsReadOnly: a write to a read-only (Output) binding gets
// exception 02, as an address the master may not write; other functions
// get 01; a range with nothing bound gets 02; too long a read gets 03;
// another unit, a broadcast or a bad CRC gets no answer at all.
func TestServerIsReadOnly(t *testing.T) {
	c, s := newTestServer(t, out("Count", "holding-register:1:INT"), out("Run", "coil:1"))
	exception := func(frame []byte) (byte, byte) {
		t.Helper()
		got := s.answer(c, "SCADA", frame)
		if len(got) != 5 || !crcOK(got) {
			t.Fatalf("not an exception: % x", got)
		}
		return got[1], got[2]
	}
	writeOne := request(7, 6, 0, 99) // write single register 0 := 99
	if fc, code := exception(writeOne); fc != 0x86 || code != 2 {
		t.Fatalf("write single register: %x %d", fc, code)
	}
	if fc, code := exception(request(7, 5, 0, 0xFF00)); fc != 0x85 || code != 2 {
		t.Fatalf("write single coil: %x %d", fc, code)
	}
	if fc, code := exception(request(7, 8, 0, 0)); fc != 0x88 || code != 1 {
		t.Fatalf("diagnostics: %x %d", fc, code)
	}
	if v, _ := c.db.GetTagValue("Count"); v != plc.INT(-2) {
		t.Fatalf("a refused write changed the tag: %v", v)
	}
	if fc, code := exception(request(7, 3, 100, 2)); fc != 0x83 || code != 2 {
		t.Fatalf("unbound range: %x %d", fc, code)
	}
	if fc, code := exception(request(7, 3, 0, 126)); fc != 0x83 || code != 3 {
		t.Fatalf("126 registers: %x %d", fc, code)
	}
	if fc, code := exception(request(7, 1, 0, 2001)); fc != 0x81 || code != 3 {
		t.Fatalf("2001 coils: %x %d", fc, code)
	}
	if got := s.answer(c, "SCADA", request(8, 3, 0, 1)); got != nil {
		t.Fatalf("answered unit 8: % x", got)
	}
	if got := s.answer(c, "SCADA", request(0, 3, 0, 1)); got != nil {
		t.Fatalf("answered a broadcast: % x", got)
	}
	bad := request(7, 3, 0, 1)
	bad[len(bad)-1] ^= 0xFF
	if got := s.answer(c, "SCADA", bad); got != nil {
		t.Fatalf("answered a bad CRC: % x", got)
	}
	// A request run together with another device's reply is still found.
	other := append([]byte{3, 3, 2, 0, 5}, 0, 0)
	if got := s.answerAny(c, "SCADA", append(other, request(7, 3, 0, 1)...)); len(got) != 7 {
		t.Fatalf("request after another reply: % x", got)
	}
}

// TestServerConfig: what a server connection refuses.
func TestServerConfig(t *testing.T) {
	db := honeycomb.NewTagDatabase()
	addTag(t, db, "A", honeycomb.TypeINT, plc.INT(0))
	addTag(t, db, "B", honeycomb.TypeREAL, plc.REAL(0))
	for want, conn := range map[string]Connection{
		"or Modbus TCP": {URL: "opcua:tcp://10.0.0.5", Bindings: []Binding{out("A", "holding-register:1")}}, // not a server protocol

		"unit-identifier":      {URL: strings.Replace(serverURL, "unit-identifier=7", "unit-identifier=0", 1)},
		"owns its port":        {URL: serverURL + "&serial.reuse-port=true"},
		"bindings are outputs": {URL: serverURL, Bindings: []Binding{{Tag: "A", Address: "holding-register:1"}}},
		"overlaps":             {URL: serverURL, Bindings: []Binding{out("B", "holding-register:1:REAL"), out("A", "holding-register:2:INT")}},
		"are BOOL":             {URL: serverURL, Bindings: []Binding{out("A", "coil:1:INT")}},
		"1 is the first":       {URL: serverURL, Bindings: []Binding{out("A", "holding-register:0")}},
		"does not serve":       {URL: serverURL, Bindings: []Binding{out("A", "holding-register:1:STRING")}},
	} {
		conn.Name, conn.Mode = "S", Server
		if _, err := New(db, []Connection{conn}); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", want, err)
		}
	}
	// A server beside a client needs no driver for itself.
	c, err := New(db, []Connection{{Name: "S", URL: serverURL, Mode: Server, Bindings: []Binding{out("A", "holding-register:1"), out("B", "input-register:1:REAL")}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(c.connections[0].server.points[areaInput]); got != 1 {
		t.Fatalf("input points %d", got)
	}
	if m, err := ParseConfig(strings.NewReader(`{"connections":[{"name":"S","url":"` + serverURL + `","mode":"server","bindings":[{"tag":"A","address":"holding-register:1","direction":"output"}]}]}`)); err != nil || m.Connections[0].Mode != Server {
		t.Fatalf("config: %v %+v", err, m)
	}
	if g := frameGap(serialport.Config{BaudRate: 19200, DataBits: 8, Parity: serialport.ParityEven}); g < 1900*time.Microsecond || g > 2100*time.Microsecond {
		t.Fatalf("gap at 19200 8E1: %v", g)
	}
	if g := frameGap(serialport.Config{BaudRate: 115200, DataBits: 8}); g != 1750*time.Microsecond {
		t.Fatalf("gap at 115200: %v", g)
	}
}

// writeFrame is an RTU write request for unit 7: FC 16 with registers, or
// FC 15 with bits packed.
func writeFrame(fc byte, start, qty uint16, data []byte) []byte {
	f := []byte{7, fc, byte(start >> 8), byte(start), byte(qty >> 8), byte(qty), byte(len(data))}
	f = append(f, data...)
	crc := rtuCRC(f)
	return append(f, byte(crc), byte(crc>>8))
}

func inout(tag, addr string) Binding { return Binding{Tag: tag, Address: addr, Direction: InOut} }

// TestServerWrites: on a serial line the master writes the InOut bindings
// (D-66): whole values, converted to the tag's own type, all or nothing.
func TestServerWrites(t *testing.T) {
	c, s := newTestServer(t, inout("Level", "holding-register:1:REAL"), inout("Count", "holding-register:3:INT"),
		out("Total", "holding-register:4:DINT"), inout("Run", "coil:1"), inout("Trip", "coil:2"))
	var wrote []string
	c.onServerWrite = func(_, tag string, v any, from string) { wrote = append(wrote, fmt.Sprintf("%s=%v %s", tag, v, from)) }

	// FC 16: Level and Count together.
	data := binary.BigEndian.AppendUint32(nil, math.Float32bits(7.25))
	data = binary.BigEndian.AppendUint16(data, 0xFFF6) // -10
	if got := s.answer(c, "SCADA", writeFrame(16, 0, 3, data)); len(got) != 8 || got[1] != 16 {
		t.Fatalf("FC 16: % x", got)
	}
	if l, _ := c.db.GetTagValue("Level"); l != plc.REAL(7.25) {
		t.Fatalf("Level %v", l)
	}
	if n, _ := c.db.GetTagValue("Count"); n != plc.INT(-10) {
		t.Fatalf("Count %v", n)
	}
	// FC 6 on Count; FC 5 on Run; FC 15 on Run and Trip.
	if got := s.answer(c, "SCADA", request(7, 6, 2, 300)); len(got) != 8 || got[1] != 6 {
		t.Fatalf("FC 6: % x", got)
	}
	if got := s.answer(c, "SCADA", request(7, 5, 0, 0)); len(got) != 8 || got[1] != 5 {
		t.Fatalf("FC 5: % x", got)
	}
	if r, _ := c.db.GetTagValue("Run"); r != plc.BOOL(false) {
		t.Fatalf("Run %v", r)
	}
	if got := s.answer(c, "SCADA", writeFrame(15, 0, 2, []byte{0b10})); len(got) != 8 || got[1] != 15 {
		t.Fatalf("FC 15: % x", got)
	}
	if r, _ := c.db.GetTagValue("Trip"); r != plc.BOOL(true) {
		t.Fatalf("Trip %v", r)
	}
	// Half a REAL, a read-only DINT, or a range reaching one: refused, and
	// nothing written, not even the writable part.
	for name, f := range map[string][]byte{
		"half a REAL":      request(7, 6, 0, 1),
		"read-only DINT":   writeFrame(16, 3, 2, []byte{0, 0, 0, 1}),
		"Count then Total": writeFrame(16, 2, 3, []byte{0, 1, 0, 0, 0, 1}),
	} {
		if got := s.answer(c, "SCADA", f); len(got) != 5 || got[2] != 2 {
			t.Errorf("%s: % x", name, got)
		}
	}
	if n, _ := c.db.GetTagValue("Count"); n != plc.INT(300) {
		t.Fatalf("a refused write changed Count: %v", n)
	}
	if len(wrote) != 6 || wrote[0] != "Level=7.25 serial" { // FC 16 and FC 15 set two tags each
		t.Fatalf("writes reported %q", wrote)
	}
	if st, _ := c.Status("SCADA"); st.Writes != 4 {
		t.Fatalf("writes counted %+v", st)
	}
}

// TestServerTCP: PLC4X's Modbus TCP client reads the server and, on an
// interface that is not read only, writes a writable tag; on a read-only
// interface its write is refused (D-66: LAN 2 reads only).
func TestServerTCP(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		t.Run(fmt.Sprintf("read-only=%v", readOnly), func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			port := ln.Addr().(*net.TCPAddr).Port
			ln.Close()
			ro := ""
			if readOnly {
				ro = "&read-only-interfaces=" + url.QueryEscape(loopbackName(t))
			}
			db := honeycomb.NewTagDatabase()
			addTag(t, db, "Level", honeycomb.TypeREAL, plc.REAL(12.5))
			addTag(t, db, "Set", honeycomb.TypeINT, plc.INT(1))
			addTag(t, db, "Read.Level", honeycomb.TypeREAL, plc.REAL(0))
			addTag(t, db, "Read.Set", honeycomb.TypeINT, plc.INT(0))
			c, err := New(db, []Connection{
				{Name: "server", URL: fmt.Sprintf("modbus-tcp://127.0.0.1:%d?default-unit-identifier=1%s", port, ro), Mode: Server,
					Bindings: []Binding{out("Level", "holding-register:1:REAL"), inout("Set", "holding-register:10:INT")}},
				{Name: "master", URL: fmt.Sprintf("modbus-tcp://127.0.0.1:%d?default-unit-identifier=1", port), Interval: 100 * time.Millisecond,
					Bindings: []Binding{{Tag: "Read.Level", Address: "holding-register:1:REAL"}, {Tag: "Read.Set", Address: "holding-register:10:INT", Direction: Output}}},
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { c.Run(ctx); close(done) }()
			defer func() { cancel(); <-done }()
			waitFor(t, func() bool { v, _ := db.GetTagValue("Read.Level"); return v == plc.REAL(12.5) })
			if err := db.SetTagValue("Read.Set", plc.INT(42)); err != nil {
				t.Fatal(err)
			}
			if readOnly {
				time.Sleep(500 * time.Millisecond)
				if v, _ := db.GetTagValue("Set"); v != plc.INT(1) {
					t.Fatalf("a write on a read-only interface changed Set: %v", v)
				}
				if m, _ := c.Status("master"); m.Errors == 0 {
					t.Fatalf("the write was not refused: %+v", m)
				}
				return
			}
			waitFor(t, func() bool { v, _ := db.GetTagValue("Set"); return v == plc.INT(42) })
			if st, _ := c.Status("server"); !st.Connected || st.Writes == 0 {
				t.Fatalf("server %+v", st)
			}
		})
	}
}

// loopbackName is the interface holding 127.0.0.1 ("lo" on Linux).
func loopbackName(t *testing.T) string {
	ifaces, _ := net.Interfaces()
	for _, i := range ifaces {
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.IsLoopback() && n.IP.To4() != nil {
				return i.Name
			}
		}
	}
	t.Skip("no loopback interface with 127.0.0.1")
	return ""
}
