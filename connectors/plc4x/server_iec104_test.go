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
	"net"
	"testing"
	"time"

	plc4go "github.com/apache/plc4x/plc4go/pkg/api"
	apiModel "github.com/apache/plc4x/plc4go/pkg/api/model"
	iec "github.com/apache/plc4x/plc4go/protocols/iec608705104/readwrite/model"
	"github.com/apiarytech/honeycomb"
	plc "github.com/apiarytech/royaljelly/iec"
)

// startIECServer runs an outstation on a free loopback port and returns the
// connector, the database and the port.
func startIECServer(t *testing.T, bindings ...Binding) (*Connector, *honeycomb.TagDatabase, int) {
	t.Helper()
	port := freePort(t)
	db := honeycomb.NewTagDatabase()
	addTag(t, db, "Level", honeycomb.TypeREAL, plc.REAL(12.5))
	addTag(t, db, "Run", honeycomb.TypeBOOL, plc.BOOL(false))
	addTag(t, db, "Setpoint", honeycomb.TypeREAL, plc.REAL(0))
	addTag(t, db, "Mode", honeycomb.TypeINT, plc.INT(0))
	c, err := New(db, []Connection{{
		Name: "outstation", URL: fmt.Sprintf("iec-60870-5-104://127.0.0.1:%d", port), Mode: Server,
		Bindings: bindings,
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return c, db, port
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return p
}

// TestIECServerByPLC4XClient: PLC4X's own IEC 60870-5-104 client connects to
// the outstation, subscribes, and receives the spontaneous update sent when a
// monitored tag changes (beehive R-133).
func TestIECServerByPLC4XClient(t *testing.T) {
	c, db, port := startIECServer(t,
		out("Level", "1/100:FLOAT"),
		inout("Run", "1/200:SP"))

	m, err := NewDriverManagerFor("iec-60870-5-104")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn := connectClient(t, ctx, m, fmt.Sprintf("iec-60870-5-104://127.0.0.1:%d", port))
	defer conn.Close()

	events := make(chan apiModel.PlcSubscriptionEvent, 16)
	builder := conn.SubscriptionRequestBuilder()
	builder.AddChangeOfStateTagAddress("level", "1/100")
	builder.AddPreRegisteredConsumer("level", func(e apiModel.PlcSubscriptionEvent) { events <- e })
	req, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	if r := <-req.Execute(ctx); r.GetErr() != nil {
		t.Fatal(r.GetErr())
	}

	// A change to the tag is sent spontaneously and reaches the subscriber.
	// The outstation also sends the current value once when data transfer
	// starts, so the changed value is read past any initial report.
	if err := db.SetTagValue("Level", plc.REAL(-7.5)); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case e := <-events:
			if code := e.GetResponseCode("level"); code != apiModel.PlcResponseCode_OK {
				t.Fatalf("response code %s", code)
			}
			if v := e.GetValue("level").GetStruct()["value"]; v != nil && v.GetFloat32() == -7.5 {
				goto got
			}
		case <-ctx.Done():
			t.Fatal("no spontaneous update reached the client")
		}
	}
got:
	if st, _ := c.Status("outstation"); !st.Connected || st.Reads == 0 {
		t.Fatalf("server status %+v", st)
	}
}

// connectClient opens a PLC4X client connection, retrying until the server's
// listener is up or ctx ends.
func connectClient(t *testing.T, ctx context.Context, m plc4go.PlcDriverManager, url string) plc4go.PlcConnection {
	t.Helper()
	for {
		conn, err := m.GetConnection(ctx, url)
		if err == nil {
			return conn
		}
		select {
		case <-ctx.Done():
			t.Fatalf("connect %s: %v", url, err)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// --- hand-built controlling station, for the paths the push-only PLC4X client cannot drive ---

// iecClient is a minimal controlling station over a raw TCP connection.
type iecClient struct {
	t    *testing.T
	conn net.Conn
	send uint16
	recv uint16 // the send sequence number expected next, for S-frame acks
}

func dialIEC(t *testing.T, port int) *iecClient {
	t.Helper()
	var conn net.Conn
	var err error
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		conn, err = net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	return &iecClient{t: t, conn: conn}
}

func (cl *iecClient) close() { cl.conn.Close() }

func (cl *iecClient) writeU(cmd byte) { cl.write([]byte{cmd, 0, 0, 0}, nil) }

func (cl *iecClient) writeI(asdu iec.ASDU) {
	raw, err := asdu.Serialize()
	if err != nil {
		cl.t.Fatal(err)
	}
	cl.write([]byte{byte(cl.send << 1), byte(cl.send >> 7), 0, 0}, raw)
	cl.send = (cl.send + 1) & 0x7FFF
}

func (cl *iecClient) write(ctrl, asdu []byte) {
	frame := append([]byte{0x68, byte(len(ctrl) + len(asdu))}, ctrl...)
	frame = append(frame, asdu...)
	cl.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := cl.conn.Write(frame); err != nil {
		cl.t.Fatal(err)
	}
}

// readFrame reads one APDU; it returns the four control octets and the ASDU
// bytes (nil for U/S frames).
func (cl *iecClient) readFrame() ([]byte, []byte) {
	var head [2]byte
	cl.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(cl.conn, head[:]); err != nil {
		cl.t.Fatalf("read frame: %v", err)
	}
	if head[0] != 0x68 {
		cl.t.Fatalf("not an APCI start: %x", head[0])
	}
	body := make([]byte, head[1])
	if _, err := io.ReadFull(cl.conn, body); err != nil {
		cl.t.Fatalf("read body: %v", err)
	}
	return body[:4], body[4:]
}

// expectASDU reads frames until an I-frame arrives, answering test frames,
// and returns its parsed ASDU.
func (cl *iecClient) expectASDU() iec.ASDU {
	for {
		ctrl, asduBytes := cl.readFrame()
		switch {
		case ctrl[0]&0x01 == 0: // I-format
			asdu, err := iec.ASDUParse(context.Background(), asduBytes)
			if err != nil {
				cl.t.Fatalf("parse ASDU: %v", err)
			}
			// Acknowledge it with an S-frame so the outstation's send window
			// does not fill, the way a real controlling station does.
			sendSeqNo := (uint16(ctrl[0]) >> 1) | (uint16(ctrl[1]) << 7)
			cl.recv = (sendSeqNo + 1) & 0x7FFF
			cl.write([]byte{0x01, 0x00, byte(cl.recv << 1), byte(cl.recv >> 7)}, nil)
			return asdu
		case ctrl[0] == iecTestAct:
			cl.writeU(iecTestCon)
		default:
			// S-frame or other U-frame: keep reading
		}
	}
}

func (cl *iecClient) handshake() {
	cl.writeU(iecTestAct)
	if ctrl, _ := cl.readFrame(); ctrl[0] != iecTestCon {
		cl.t.Fatalf("expected test confirmation, got %x", ctrl[0])
	}
	cl.writeU(iecStartDTAct)
	if ctrl, _ := cl.readFrame(); ctrl[0] != iecStartDTCon {
		cl.t.Fatalf("expected start confirmation, got %x", ctrl[0])
	}
}

// TestIECServerInterrogationAndCommands: a controlling station runs the
// connect handshake, a general interrogation, and single/set-point commands,
// which the outstation confirms and writes (beehive R-133).
func TestIECServerInterrogationAndCommands(t *testing.T) {
	c, db, port := startIECServer(t,
		out("Level", "1/100:FLOAT"),
		inout("Run", "1/200:SP"),
		inout("Setpoint", "1/300:FLOAT"))

	cl := dialIEC(t, port)
	defer cl.close()
	cl.handshake()
	// After STARTDT the outstation sends the current values spontaneously;
	// drain them (three points).
	for range 3 {
		cl.expectASDU()
	}

	// General interrogation of common address 1.
	gi := iec.NewInformationObjectWithoutTime_INTERROGATION_COMMAND(0, iec.NewQualifierOfInterrogation(20))
	cl.writeI(iec.NewASDU(iec.TypeIdentification_INTERROGATION_COMMAND, false, false, false, cotActivation, 0, 1, []iec.InformationObject{gi}))
	first := cl.expectASDU()
	if first.GetTypeIdentification() != iec.TypeIdentification_INTERROGATION_COMMAND || first.GetCauseOfTransmission() != cotActivationCon {
		t.Fatalf("expected GI activation confirmation, got %s/%s", first.GetTypeIdentification(), first.GetCauseOfTransmission())
	}
	var points int
	for {
		a := cl.expectASDU()
		if a.GetTypeIdentification() == iec.TypeIdentification_INTERROGATION_COMMAND && a.GetCauseOfTransmission() == cotActivationTrm {
			break
		}
		if a.GetCauseOfTransmission() != cotInterrogated {
			t.Fatalf("unexpected cause during GI: %s", a.GetCauseOfTransmission())
		}
		points++
	}
	if points != 3 {
		t.Fatalf("GI returned %d points, want 3", points)
	}

	// Single command: turn Run on.
	sc := iec.NewInformationObjectWithoutTime_SINGLE_COMMAND(200, iec.NewSingleCommand(iec.NewQualifierOfCommand(false, 0), true))
	cl.writeI(iec.NewASDU(iec.TypeIdentification_SINGLE_COMMAND, false, false, false, cotActivation, 0, 1, []iec.InformationObject{sc}))
	con := cl.expectASDU()
	if con.GetCauseOfTransmission() != cotActivationCon || con.GetNegative() {
		t.Fatalf("single command not confirmed: %s negative=%v", con.GetCauseOfTransmission(), con.GetNegative())
	}
	if term := cl.expectASDU(); term.GetCauseOfTransmission() != cotActivationTrm {
		t.Fatalf("single command not terminated: %s", term.GetCauseOfTransmission())
	}
	waitFor(t, func() bool { v, _ := db.GetTagValue("Run"); return v == plc.BOOL(true) })

	// Set-point short float command: Setpoint := 42.5.
	sp := iec.NewInformationObjectWithoutTime_SET_POINT_COMMAND_SHORT_FLOATING_POINT_NUMBER(300, 42.5, iec.NewQualifierOfSetPointCommand(false, 0))
	cl.writeI(iec.NewASDU(iec.TypeIdentification_SET_POINT_COMMAND_SHORT_FLOATING_POINT_NUMBER, false, false, false, cotActivation, 0, 1, []iec.InformationObject{sp}))
	if con := cl.expectASDU(); con.GetCauseOfTransmission() != cotActivationCon || con.GetNegative() {
		t.Fatalf("set-point not confirmed: %s negative=%v", con.GetCauseOfTransmission(), con.GetNegative())
	}
	if term := cl.expectASDU(); term.GetCauseOfTransmission() != cotActivationTrm {
		t.Fatalf("set-point not terminated: %s", term.GetCauseOfTransmission())
	}
	waitFor(t, func() bool { v, _ := db.GetTagValue("Setpoint"); return v == plc.REAL(42.5) })

	// A command to an unknown point is confirmed negatively.
	bad := iec.NewInformationObjectWithoutTime_SINGLE_COMMAND(999, iec.NewSingleCommand(iec.NewQualifierOfCommand(false, 0), true))
	cl.writeI(iec.NewASDU(iec.TypeIdentification_SINGLE_COMMAND, false, false, false, cotActivation, 0, 1, []iec.InformationObject{bad}))
	if con := cl.expectASDU(); !con.GetNegative() && con.GetCauseOfTransmission() != cotUnknownObject {
		t.Fatalf("unknown point was not refused: %s negative=%v", con.GetCauseOfTransmission(), con.GetNegative())
	}
	if st, _ := c.Status("outstation"); st.Writes == 0 {
		t.Fatalf("no writes recorded: %+v", st)
	}
}
