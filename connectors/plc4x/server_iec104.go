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

// IEC 60870-5-104 outstation (controlled station), beehive R-133: the
// server side of the protocol PLC4X only has a client (controlling station)
// for. It listens on TCP (default port 2404) and answers a controlling
// station.
//
// The APCI (the six-octet frame header: 0x68, a length, and four control
// octets) is hand-encoded here: it is small and fixed, and PLC4X's model
// packs the control octets into a uint16 "command" that is more awkward to
// drive than the octets themselves. Everything inside the APCI, the ASDU, is
// parsed and serialized by PLC4X's iec608705104 model, so a value is encoded
// exactly as PLC4X's own client decodes it (and the two share the same
// address space: the client's tag is "<ca>/<ioa>", this server's binding is
// the same with the point's type added).
//
// What it does:
//
//   - the connect handshake: it answers a TESTFR activation with a TESTFR
//     confirmation and a STARTDT activation with a STARTDT confirmation,
//     after which it may send data; STOPDT stops it.
//   - I/S/U framing with send and receive sequence numbers, an S-format
//     acknowledgement for received frames, and a k window on frames sent but
//     not yet acknowledged.
//   - general interrogation (C_IC_NA_1): activation confirmation, every
//     monitored point of the addressed common address, then activation
//     termination.
//   - spontaneous transmission: a monitored point whose tag changes is sent
//     with cause "spontaneous".
//   - commands for in-out points (single, double, set-point short float, set-
//     point scaled, set-point normalised), written through serverSet, with an
//     activation confirmation and termination. Select-before-operate is
//     honoured: a select is confirmed without writing, an execute writes.
//
// A controlling station reaching the server over a read-only interface may
// read (receive) but not command, the same rule the Modbus TCP server keeps.

import (
	"context"
	"fmt"
	"io"
	"net"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	apiValues "github.com/apache/plc4x/plc4go/pkg/api/values"
	iec "github.com/apache/plc4x/plc4go/protocols/iec608705104/readwrite/model"
	spiValues "github.com/apache/plc4x/plc4go/spi/values"
)

// IEC 60870-5-104 U-format control octets (first control octet; the other
// three are zero).
const (
	iecStartDTAct = 0x07
	iecStartDTCon = 0x0B
	iecStopDTAct  = 0x13
	iecStopDTCon  = 0x23
	iecTestAct    = 0x43
	iecTestCon    = 0x83
)

// Cause-of-transmission values used by the outstation.
const (
	cotSpontaneous   = iec.CauseOfTransmission_SPONTANEOUS_SPONT
	cotInterrogated  = iec.CauseOfTransmission_INTERROGATED_BY_GENERAL_INTERROGATION_INROGEN
	cotActivationCon = iec.CauseOfTransmission_CONFIRMATION_ACTIVATION_ACTCON
	cotActivationTrm = iec.CauseOfTransmission_TERMINATION_ACTIVATION_ACTTERM
	cotActivation    = iec.CauseOfTransmission_ACTIVATION_ACT
	cotUnknownObject = iec.CauseOfTransmission_INFORMATION_OBJECT_ADDRESS_UNKNOWN_UNKNOWN_OBJECT_ADDRESS
)

// iecKind is how a point is carried on the wire: the monitored ASDU type it
// is reported with, and, for an in-out point, the command type it accepts.
type iecKind struct {
	name string
	mon  iec.TypeIdentification
	cmd  iec.TypeIdentification
}

var iecKinds = map[string]iecKind{
	"SP":     {"SP", iec.TypeIdentification_SINGLE_POINT_INFORMATION, iec.TypeIdentification_SINGLE_COMMAND},
	"DP":     {"DP", iec.TypeIdentification_DOUBLE_POINT_INFORMATION, iec.TypeIdentification_DOUBLE_COMMAND},
	"FLOAT":  {"FLOAT", iec.TypeIdentification_MEASURED_VALUE_SHORT_FLOATING_POINT_NUMBER, iec.TypeIdentification_SET_POINT_COMMAND_SHORT_FLOATING_POINT_NUMBER},
	"SCALED": {"SCALED", iec.TypeIdentification_MEASURED_VALUE_SCALED_VALUE, iec.TypeIdentification_SET_POINT_COMMAND_SCALED_VALUE},
	"NORM":   {"NORM", iec.TypeIdentification_MEASURED_VALUE_NORMALISED_VALUE, iec.TypeIdentification_SET_POINT_COMMAND_NORMALISED_VALUE},
}

// iecPoint is one binding of an IEC 60870-5-104 outstation.
type iecPoint struct {
	tag      string
	ca       uint16 // common address of ASDU
	ioa      uint32 // information object address
	kind     iecKind
	writable bool // an in-out point: the station may command it
}

// iecServer is an IEC 60870-5-104 outstation: where it listens, whether
// some interfaces read only, and its points.
type iecServer struct {
	listen   string
	readOnly []string
	points   []iecPoint
	k, w     uint16 // send window, acknowledgement window
}

var iecAddress = regexp.MustCompile(`^(\d+)/(\d+):([A-Za-z]+)$`)

// newIECServer reads an outstation connection: its URL and its points.
func newIECServer(conn Connection) (*iecServer, error) {
	s := &iecServer{k: 12, w: 8}
	_, host, port, q, err := parseServerTCPURL(conn.URL, "2404")
	if err != nil {
		return nil, fmt.Errorf("plc4x: server %q: %w", conn.Name, err)
	}
	s.listen = net.JoinHostPort(host, port)
	s.readOnly = readOnlyInterfaces(q)
	if v := q.Get("k"); v != "" {
		n, err := strconv.ParseUint(v, 10, 15)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("plc4x: server %q: k %q: the send window, 1 to 32767", conn.Name, v)
		}
		s.k = uint16(n)
	}
	if v := q.Get("w"); v != "" {
		n, err := strconv.ParseUint(v, 10, 15)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("plc4x: server %q: w %q: the acknowledgement window, 1 to 32767", conn.Name, v)
		}
		s.w = uint16(n)
	}
	seen := map[string]bool{}
	for _, b := range conn.Bindings {
		p, err := parseIECAddress(b.Address)
		if err != nil {
			return nil, fmt.Errorf("plc4x: server %q: tag %q: %w", conn.Name, b.Tag, err)
		}
		p.tag = b.Tag
		p.writable = b.Direction == InOut
		key := fmt.Sprintf("%d/%d", p.ca, p.ioa)
		if seen[key] {
			return nil, fmt.Errorf("plc4x: server %q: two points at %s", conn.Name, key)
		}
		seen[key] = true
		s.points = append(s.points, p)
	}
	return s, nil
}

// parseIECAddress reads one binding's address, "<ca>/<ioa>:<type>".
func parseIECAddress(addr string) (iecPoint, error) {
	m := iecAddress.FindStringSubmatch(strings.TrimSpace(addr))
	if m == nil {
		return iecPoint{}, fmt.Errorf("address %q: want <common-address>/<information-object-address>:<type>, e.g. 1/100:FLOAT (types: SP, DP, FLOAT, SCALED, NORM)", addr)
	}
	ca, err := strconv.ParseUint(m[1], 10, 16)
	if err != nil {
		return iecPoint{}, fmt.Errorf("address %q: the common address of ASDU is 0 to 65535", addr)
	}
	ioa, err := strconv.ParseUint(m[2], 10, 24)
	if err != nil {
		return iecPoint{}, fmt.Errorf("address %q: the information object address is 0 to 16777215", addr)
	}
	kind, ok := iecKinds[strings.ToUpper(m[3])]
	if !ok {
		return iecPoint{}, fmt.Errorf("address %q: no type %q (SP, DP, FLOAT, SCALED, NORM)", addr, m[3])
	}
	return iecPoint{ca: uint16(ca), ioa: uint32(ioa), kind: kind}, nil
}

// serveIEC listens and answers each controlling station until ctx ends or
// the listener fails.
func (c *Connector) serveIEC(ctx context.Context, cs *connState) error {
	s := cs.server.iec
	ln, err := net.Listen("tcp", s.listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.listen, err)
	}
	stop := context.AfterFunc(ctx, func() { ln.Close() })
	defer func() {
		if stop() {
			ln.Close()
		}
	}()
	c.record(cs.Name, func(st *Status) { st.Connected = true }, nil)
	var wg sync.WaitGroup
	defer wg.Wait()
	slots := make(chan struct{}, maxTCPClients)
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept on %s: %w", s.listen, err)
		}
		select {
		case slots <- struct{}{}:
		default:
			conn.Close()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			newIECSession(c, cs, conn).run(ctx)
		}()
	}
}

// iecSession is one controlling station's connection.
type iecSession struct {
	c    *Connector
	cs   *connState
	conn net.Conn
	s    *iecServer

	writes bool   // the station may command (not a read-only interface)
	from   string // the station's address, for the record

	mu       sync.Mutex // guards the sequence numbers and the write side
	sendSeq  uint16     // N(S) of the next I-frame sent
	recvSeq  uint16     // N(R): the send sequence number expected next
	ackedSeq uint16     // the lowest unacknowledged send sequence number
	unacked  uint16     // received I-frames not yet acknowledged
	started  bool       // STARTDT confirmed

	last map[string]string // last value sent per point, for change detection
}

func newIECSession(c *Connector, cs *connState, conn net.Conn) *iecSession {
	return &iecSession{c: c, cs: cs, conn: conn, s: cs.server.iec,
		writes: cs.server.iec.writesFrom(conn.LocalAddr()),
		from:   conn.RemoteAddr().String(), last: map[string]string{}}
}

// writesFrom says whether a station reaching the outstation at local may
// command: not when local is an address of a read-only interface.
func (s *iecServer) writesFrom(local net.Addr) bool {
	return writesFromReadOnly(s.readOnly, local)
}

const (
	iecScanInterval = 200 * time.Millisecond // how often points are checked for changes
	iecIdleTest     = 20 * time.Second       // t3: a test frame is sent after this silence
	iecReadIdle     = 5 * time.Minute        // a station silent this long is closed
)

// run drives one station: a reader goroutine takes frames off the wire while
// the main loop sends spontaneous changes and idle test frames.
func (s *iecSession) run(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { s.conn.Close() })
	defer func() {
		stop()
		s.conn.Close()
	}()

	go s.read(ctx, cancel)

	scan := time.NewTicker(iecScanInterval)
	defer scan.Stop()
	idle := time.NewTimer(iecIdleTest)
	defer idle.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-scan.C:
			if s.isStarted() {
				s.sendChanges()
			}
		case <-idle.C:
			s.sendU(iecTestAct)
			idle.Reset(iecIdleTest)
		}
	}
}

func (s *iecSession) isStarted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started
}

// read takes frames off the wire until the station goes, falls silent for
// iecReadIdle or ctx ends; it cancels the session on any of these.
func (s *iecSession) read(ctx context.Context, cancel context.CancelFunc) {
	defer cancel()
	var head [2]byte
	for {
		s.conn.SetReadDeadline(time.Now().Add(iecReadIdle))
		if _, err := io.ReadFull(s.conn, head[:]); err != nil {
			return
		}
		if head[0] != 0x68 {
			return // not an APCI: give up rather than resynchronise on noise
		}
		length := int(head[1])
		if length < 4 || length > 253 {
			return
		}
		body := make([]byte, length)
		if _, err := io.ReadFull(s.conn, body); err != nil {
			return
		}
		if ctx.Err() != nil {
			return
		}
		if !s.handleFrame(ctx, body) {
			return
		}
	}
}

// handleFrame sorts one APDU (its four control octets and any ASDU). It
// returns false to end the session.
func (s *iecSession) handleFrame(ctx context.Context, body []byte) bool {
	ctrl := body[:4]
	switch {
	case ctrl[0]&0x01 == 0: // I-format
		asduBytes := body[4:]
		sendSeqNo := (uint16(ctrl[0]) >> 1) | (uint16(ctrl[1]) << 7)
		s.noteReceived(sendSeqNo)
		if len(asduBytes) > 0 {
			s.handleASDU(ctx, asduBytes)
		}
		s.sendAck()
	case ctrl[0]&0x03 == 0x01: // S-format: the station acknowledging our sends
		s.noteAck(uint16(ctrl[2])>>1 | uint16(ctrl[3])<<7)
	default: // U-format
		switch ctrl[0] {
		case iecStartDTAct:
			s.setStarted(true)
			s.sendU(iecStartDTCon)
			s.sendAll(cotSpontaneous) // the push-only client never interrogates
		case iecStopDTAct:
			s.setStarted(false)
			s.sendU(iecStopDTCon)
		case iecTestAct:
			s.sendU(iecTestCon)
		case iecTestCon, iecStartDTCon, iecStopDTCon:
			// confirmations of frames we sent; nothing to do
		default:
			return false
		}
	}
	return true
}

func (s *iecSession) setStarted(v bool) {
	s.mu.Lock()
	s.started = v
	s.mu.Unlock()
}

// noteReceived records the send sequence number of a received I-frame.
func (s *iecSession) noteReceived(sendSeqNo uint16) {
	s.mu.Lock()
	s.recvSeq = (sendSeqNo + 1) & 0x7FFF
	s.unacked++
	s.mu.Unlock()
}

// noteAck records the station's acknowledgement of frames we sent.
func (s *iecSession) noteAck(recvSeqNo uint16) {
	s.mu.Lock()
	s.ackedSeq = recvSeqNo & 0x7FFF
	s.mu.Unlock()
}

// sendAck sends an S-format acknowledgement when one is due (the w window).
func (s *iecSession) sendAck() {
	s.mu.Lock()
	due := s.unacked >= s.s.w
	if due {
		s.unacked = 0
	}
	recv := s.recvSeq
	s.mu.Unlock()
	if !due {
		return
	}
	s.writeFrame([]byte{0x01, 0x00, byte(recv << 1), byte(recv >> 7)}, nil)
}

// sendU sends a U-format frame (one control command).
func (s *iecSession) sendU(command byte) {
	s.writeFrame([]byte{command, 0, 0, 0}, nil)
}

// sendASDU sends one ASDU in an I-format frame, within the k window. It
// reports whether it was sent (the window may be full).
func (s *iecSession) sendASDU(asdu iec.ASDU) bool {
	asduBytes, err := asdu.Serialize()
	if err != nil {
		s.c.onError(s.cs.Name, fmt.Errorf("serialize ASDU: %w", err))
		return false
	}
	s.mu.Lock()
	if (s.sendSeq-s.ackedSeq)&0x7FFF >= s.s.k {
		s.mu.Unlock()
		return false // the send window is full: the change is re-sent next scan
	}
	send, recv := s.sendSeq, s.recvSeq
	s.sendSeq = (s.sendSeq + 1) & 0x7FFF
	s.unacked = 0 // an I-frame carries the receive sequence number too
	s.mu.Unlock()
	ctrl := []byte{byte(send << 1), byte(send >> 7), byte(recv << 1), byte(recv >> 7)}
	s.writeFrame(ctrl, asduBytes)
	return true
}

// writeFrame puts one APCI frame (its control octets and ASDU) on the wire.
func (s *iecSession) writeFrame(ctrl, asdu []byte) {
	frame := make([]byte, 0, 2+len(ctrl)+len(asdu))
	frame = append(frame, 0x68, byte(len(ctrl)+len(asdu)))
	frame = append(frame, ctrl...)
	frame = append(frame, asdu...)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := s.conn.Write(frame); err != nil {
		s.conn.Close()
		return
	}
	s.c.record(s.cs.Name, func(st *Status) { st.Reads++; st.LastRead = time.Now() }, nil)
}

// sendAll sends every monitored point with the given cause.
func (s *iecSession) sendAll(cot iec.CauseOfTransmission) {
	for i := range s.s.points {
		p := &s.s.points[i]
		value, fp, ok := s.c.iecValue(p)
		if !ok {
			continue
		}
		s.mu.Lock()
		s.last[fp.key] = fp.value
		s.mu.Unlock()
		s.sendASDU(iec.NewASDU(p.kind.mon, false, false, false, cot, 0, p.ca, []iec.InformationObject{value}))
	}
}

// sendChanges sends the monitored points whose tag changed since last sent.
func (s *iecSession) sendChanges() {
	for i := range s.s.points {
		p := &s.s.points[i]
		value, fp, ok := s.c.iecValue(p)
		if !ok {
			continue
		}
		s.mu.Lock()
		changed := s.last[fp.key] != fp.value
		s.mu.Unlock()
		if !changed {
			continue
		}
		if s.sendASDU(iec.NewASDU(p.kind.mon, false, false, false, cotSpontaneous, 0, p.ca, []iec.InformationObject{value})) {
			s.mu.Lock()
			s.last[fp.key] = fp.value
			s.mu.Unlock()
		}
	}
}

// iecFingerprint identifies a point and the value last sent for it.
type iecFingerprint struct {
	key   string
	value string
}

// iecValue reads a point's tag and builds the monitored information object
// to send, with a fingerprint for change detection. ok is false when the tag
// has no value.
func (c *Connector) iecValue(p *iecPoint) (iec.InformationObject, iecFingerprint, bool) {
	raw, err := c.db.GetTagValue(p.tag)
	if err != nil || raw == nil {
		return nil, iecFingerprint{}, false
	}
	key := fmt.Sprintf("%d/%d", p.ca, p.ioa)
	good := iec.NewQualityDescriptor(false, false, false, false, false)
	switch p.kind.name {
	case "SP":
		on := asBool(raw)
		return iec.NewInformationObjectWithoutTime_SINGLE_POINT_INFORMATION(p.ioa, iec.NewSinglePointInformation(false, false, false, false, on)),
			iecFingerprint{key, strconv.FormatBool(on)}, true
	case "DP":
		code := uint8(1) // OFF
		if asBool(raw) {
			code = 2 // ON
		}
		return iec.NewInformationObjectWithoutTime_DOUBLE_POINT_INFORMATION(p.ioa, iec.NewDoublePointInformation(false, false, false, false, code)),
			iecFingerprint{key, strconv.Itoa(int(code))}, true
	case "FLOAT":
		v := float32(asFloat(raw))
		return iec.NewInformationObjectWithoutTime_MEASURED_VALUE_SHORT_FLOATING_POINT_NUMBER(p.ioa, v, good),
			iecFingerprint{key, strconv.FormatFloat(float64(v), 'g', -1, 32)}, true
	case "SCALED":
		v := int16(asFloat(raw))
		return iec.NewInformationObjectWithoutTime_MEASURED_VALUE_SCALED_VALUE(p.ioa, iec.NewScaledValue(v), good),
			iecFingerprint{key, strconv.Itoa(int(v))}, true
	default: // NORM
		v := int16(asFloat(raw))
		return iec.NewInformationObjectWithoutTime_MEASURED_VALUE_NORMALISED_VALUE(p.ioa, iec.NewNormalizedValue(uint16(v)), good),
			iecFingerprint{key, strconv.Itoa(int(v))}, true
	}
}

// handleASDU answers one command or interrogation the station sent.
func (s *iecSession) handleASDU(ctx context.Context, asduBytes []byte) {
	asdu, err := iec.ASDUParse(ctx, asduBytes)
	if err != nil {
		return
	}
	objs := asdu.GetInformationObjects()
	if len(objs) != 1 {
		return
	}
	switch asdu.GetTypeIdentification() {
	case iec.TypeIdentification_INTERROGATION_COMMAND:
		s.generalInterrogation(asdu)
	case iec.TypeIdentification_SINGLE_COMMAND, iec.TypeIdentification_DOUBLE_COMMAND,
		iec.TypeIdentification_SET_POINT_COMMAND_SHORT_FLOATING_POINT_NUMBER,
		iec.TypeIdentification_SET_POINT_COMMAND_SCALED_VALUE,
		iec.TypeIdentification_SET_POINT_COMMAND_NORMALISED_VALUE:
		s.command(asdu, objs[0])
	}
}

// generalInterrogation answers a C_IC_NA_1: an activation confirmation, the
// monitored points of the addressed common address, then an activation
// termination.
func (s *iecSession) generalInterrogation(asdu iec.ASDU) {
	ca := asdu.GetAsduAddressField()
	qoi := iec.NewInformationObjectWithoutTime_INTERROGATION_COMMAND(0, iec.NewQualifierOfInterrogation(20))
	s.sendASDU(iec.NewASDU(iec.TypeIdentification_INTERROGATION_COMMAND, false, false, false, cotActivationCon, 0, ca, []iec.InformationObject{qoi}))
	for i := range s.s.points {
		p := &s.s.points[i]
		if p.ca != ca {
			continue
		}
		value, fp, ok := s.c.iecValue(p)
		if !ok {
			continue
		}
		s.mu.Lock()
		s.last[fp.key] = fp.value
		s.mu.Unlock()
		s.sendASDU(iec.NewASDU(p.kind.mon, false, false, false, cotInterrogated, 0, p.ca, []iec.InformationObject{value}))
	}
	s.sendASDU(iec.NewASDU(iec.TypeIdentification_INTERROGATION_COMMAND, false, false, false, cotActivationTrm, 0, ca, []iec.InformationObject{qoi}))
}

// command answers a control command: it writes the addressed in-out point
// and confirms, or confirms negatively when the point is unknown, read only
// or the station may not command. A select is confirmed without writing.
func (s *iecSession) command(asdu iec.ASDU, obj iec.InformationObject) {
	ca := asdu.GetAsduAddressField()
	typeID := asdu.GetTypeIdentification()
	value, selected, ok := commandValue(obj)
	p := s.s.pointFor(ca, obj.GetAddress(), typeID)

	confirm := func(negative bool) {
		cot := cotActivationCon
		if p == nil {
			cot = cotUnknownObject
		}
		s.sendASDU(iec.NewASDU(typeID, false, false, negative, cot, 0, ca, []iec.InformationObject{obj}))
	}

	if p == nil || !p.writable || !s.writes || !ok {
		confirm(true)
		return
	}
	if selected { // select-before-operate: confirm the selection, write on execute
		confirm(false)
		return
	}
	if ex := s.c.iecWrite(s.cs.Name, s.from, p, value); ex != 0 {
		confirm(true)
		return
	}
	confirm(false)
	s.sendASDU(iec.NewASDU(typeID, false, false, false, cotActivationTrm, 0, ca, []iec.InformationObject{obj}))
}

// pointFor finds the writable point at a command's address whose command
// type the command matches.
func (s *iecServer) pointFor(ca uint16, ioa uint32, cmd iec.TypeIdentification) *iecPoint {
	for i := range s.points {
		p := &s.points[i]
		if p.ca == ca && p.ioa == ioa && p.kind.cmd == cmd {
			return p
		}
	}
	return nil
}

// commandValue reads a command's value as a PLC4X value, whether it carries
// a select, and whether it is a command this server writes.
func commandValue(obj iec.InformationObject) (value apiValues.PlcValue, selected, ok bool) {
	switch o := obj.(type) {
	case iec.InformationObjectWithoutTime_SINGLE_COMMAND:
		return spiValues.NewPlcBOOL(o.GetSco().GetCommandOn()), o.GetSco().GetQoc().GetSelect(), true
	case iec.InformationObjectWithoutTime_DOUBLE_COMMAND:
		return spiValues.NewPlcBOOL(o.GetDco().GetDcs() == 2), o.GetDco().GetQoc().GetSelect(), true
	case iec.InformationObjectWithoutTime_SET_POINT_COMMAND_SHORT_FLOATING_POINT_NUMBER:
		return spiValues.NewPlcREAL(o.GetValue()), o.GetQos().GetSelect(), true
	case iec.InformationObjectWithoutTime_SET_POINT_COMMAND_SCALED_VALUE:
		return spiValues.NewPlcINT(o.GetSva().GetValue()), o.GetQos().GetSelect(), true
	case iec.InformationObjectWithoutTime_SET_POINT_COMMAND_NORMALISED_VALUE:
		return spiValues.NewPlcINT(int16(o.GetNva().GetValue())), o.GetQos().GetSelect(), true
	}
	return nil, false, false
}

// iecWrite writes a commanded value to a point's tag, converted to the tag's
// own type, and records it. It returns an exception code, 0 on success.
func (c *Connector) iecWrite(name, from string, p *iecPoint, v apiValues.PlcValue) byte {
	out, ex := c.serverValue(serverPoint{tag: p.tag}, v)
	if ex != 0 {
		return ex
	}
	return c.serverSet(name, from, []pending{{p.tag, out}})
}

// asFloat reads a tag value as a float64; 0 for a non-number.
func asFloat(v any) float64 {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Bool:
		if rv.Bool() {
			return 1
		}
		return 0
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return float64(rv.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return float64(rv.Uint())
	case reflect.Float32, reflect.Float64:
		return rv.Float()
	}
	return 0
}
