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
	"encoding/hex"
	"math"
	"net"
	"testing"
	"time"

	"github.com/apiarytech/honeycomb"
	plc "github.com/apiarytech/royaljelly/iec"
)

const asciiURL = "modbus-ascii:serial:///dev/ttyAMA2?serial.baud-rate=9600&serial.data-bits=7&serial.parity=even&unit-identifier=7"

func newASCIIServer(t *testing.T, bindings ...Binding) (*Connector, *server) {
	t.Helper()
	db := honeycomb.NewTagDatabase()
	addTag(t, db, "Level", honeycomb.TypeREAL, plc.REAL(1.5))
	addTag(t, db, "Count", honeycomb.TypeINT, plc.INT(-2))
	addTag(t, db, "Run", honeycomb.TypeBOOL, plc.BOOL(true))
	c, err := New(db, []Connection{{Name: "SCADA", URL: asciiURL, Mode: Server, Bindings: bindings}})
	if err != nil {
		t.Fatal(err)
	}
	s := c.connections[0].server
	if !s.ascii || s.unit != 7 || s.config.DataBits != 7 {
		t.Fatalf("server %+v", s)
	}
	return c, s
}

// asciiRequest frames an RTU-style PDU as a Modbus ASCII request body: the
// hex characters a client would put between ':' and CR LF (address, PDU, LRC).
func asciiRequest(unit byte, pdu []byte) []byte {
	raw := append([]byte{unit}, pdu...)
	raw = append(raw, lrc(raw))
	text := make([]byte, hex.EncodedLen(len(raw)))
	hex.Encode(text, raw)
	return bytes.ToUpper(text)
}

// asciiReplyPDU decodes an answerASCII reply (':' ... CR LF) to its PDU.
func asciiReplyPDU(t *testing.T, reply []byte) []byte {
	t.Helper()
	if len(reply) < 3 || reply[0] != ':' || reply[len(reply)-2] != '\r' || reply[len(reply)-1] != '\n' {
		t.Fatalf("not an ASCII frame: %q", reply)
	}
	raw := make([]byte, hex.DecodedLen(len(reply)-3))
	if _, err := hex.Decode(raw, reply[1:len(reply)-2]); err != nil {
		t.Fatal(err)
	}
	if lrc(raw[:len(raw)-1]) != raw[len(raw)-1] {
		t.Fatalf("bad LRC in reply %q", reply)
	}
	return raw[1 : len(raw)-1] // drop the address and LRC
}

// TestASCIIServerReadsAndWrites: a Modbus ASCII client reads a holding
// register and writes a writable one; the same Modbus PDU handler as RTU, in
// hex framing with an LRC (beehive R-133).
func TestASCIIServerReadsAndWrites(t *testing.T) {
	c, s := newASCIIServer(t,
		out("Level", "holding-register:1:REAL"),
		inout("Count", "holding-register:3:INT"),
		out("Run", "coil:1"))

	// FC 3: read holding registers 1..3 (Level is a REAL in 1..2, Count in 3).
	read := []byte{3, 0, 0, 0, 3}
	pdu := asciiReplyPDU(t, s.answerASCII(c, "SCADA", asciiRequest(7, read)))
	want := []byte{3, 6}
	want = binary.BigEndian.AppendUint32(want, math.Float32bits(1.5))
	want = binary.BigEndian.AppendUint16(want, 0xFFFE) // -2
	if !bytes.Equal(pdu, want) {
		t.Fatalf("read\n got % x\nwant % x", pdu, want)
	}

	// FC 6: write single register 3 (Count) := 100.
	write := []byte{6, 0, 2, 0, 100}
	pdu = asciiReplyPDU(t, s.answerASCII(c, "SCADA", asciiRequest(7, write)))
	if !bytes.Equal(pdu, write) {
		t.Fatalf("write echo % x", pdu)
	}
	if v, _ := c.db.GetTagValue("Count"); v != plc.INT(100) {
		t.Fatalf("Count %v", v)
	}

	// A bad LRC is not answered; nor is another unit or a broadcast.
	bad := asciiRequest(7, read)
	bad[len(bad)-1] ^= 0x0F
	if got := s.answerASCII(c, "SCADA", bad); got != nil {
		t.Fatalf("answered a bad LRC: %q", got)
	}
	if got := s.answerASCII(c, "SCADA", asciiRequest(8, read)); got != nil {
		t.Fatalf("answered unit 8: %q", got)
	}
	// A write to a read-only coil is refused with exception 02.
	ex := asciiReplyPDU(t, s.answerASCII(c, "SCADA", asciiRequest(7, []byte{5, 0, 0, 0xFF, 0})))
	if len(ex) != 2 || ex[0] != 0x85 || ex[1] != 2 {
		t.Fatalf("write-coil exception % x", ex)
	}
}

// TestASCIIServerServeLoop drives the serial read loop (serveASCIILine) over
// an in-memory pipe: a ':'-framed request in, a framed reply out. net.Pipe
// connections honour read deadlines, which the loop uses to end a frame.
func TestASCIIServerServeLoop(t *testing.T) {
	c, _ := newASCIIServer(t, out("Count", "holding-register:1:INT"))
	cs := c.connections[0]
	client, srv := net.Pipe()
	defer client.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.serveASCIILine(ctx, cs, srv); close(done) }()
	defer func() { cancel(); srv.Close(); <-done }()

	req := asciiRequest(7, []byte{3, 0, 0, 0, 1}) // read holding register 1
	frame := append([]byte{':'}, req...)
	frame = append(frame, '\r', '\n')
	client.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := client.Write(frame); err != nil {
		t.Fatal(err)
	}
	client.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 64)
	n, err := client.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	pdu := asciiReplyPDU(t, buf[:n])
	if !bytes.Equal(pdu, []byte{3, 2, 0xFF, 0xFE}) { // Count = -2
		t.Fatalf("reply PDU % x", pdu)
	}
}
