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

// S7 server (beehive R-133): the server side of the S7comm protocol PLC4X
// only has a client for. It listens on ISO-on-TCP (TPKT/COTP, default port
// 102) and answers an S7 client.
//
// Everything on the wire is parsed and serialized by PLC4X's s7 model, so a
// value is encoded exactly as PLC4X's own client decodes it, and a binding's
// address is the client's S7 address form ("%DB1.DBW0:INT", "%M4.0:BOOL",
// "%DB2.DBD4:REAL"). It answers:
//
//   - the COTP connection request with a connection response, echoing the
//     TSAP and TPDU-size parameters;
//   - the S7 setup-communication job with a setup-communication acknowledgement
//     (the client's negotiated PDU size and window);
//   - read-var jobs: each addressed item that a binding covers is returned
//     with its tag's current value, others with "not found";
//   - write-var jobs: each addressed item that an in-out binding covers is
//     written through serverSet, others refused. A client reaching the server
//     over a read-only interface may read but not write.
//
// User-data services (the SZL identification probe, alarms, cyclic reads) are
// not answered; a client that is not pinned to a controller type simply finds
// them unavailable, exactly as it does on a PLC without them.

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	s7 "github.com/apache/plc4x/plc4go/protocols/s7/readwrite/model"
)

// s7Point is one binding of an S7 server.
type s7Point struct {
	tag      string
	area     s7.MemoryArea
	db       uint16
	byteAddr uint16
	bitAddr  uint8
	size     s7.TransportSize
	writable bool
}

// s7Server is an S7 server: where it listens, whether some interfaces read
// only, and its points.
type s7Server struct {
	listen   string
	readOnly []string
	points   []s7Point
}

var (
	s7AddrDB   = regexp.MustCompile(`^%DB(\d+)\.DB([XBWD]?)(\d+)(?:\.([0-7]))?:([A-Za-z0-9_]+)$`)
	s7AddrArea = regexp.MustCompile(`^%([IQMEA])([XBWD]?)(\d+)(?:\.([0-7]))?:([A-Za-z0-9_]+)$`)
)

var s7Areas = map[string]s7.MemoryArea{
	"I": s7.MemoryArea_INPUTS, "E": s7.MemoryArea_INPUTS,
	"Q": s7.MemoryArea_OUTPUTS, "A": s7.MemoryArea_OUTPUTS,
	"M": s7.MemoryArea_FLAGS_MARKERS,
}

// newS7Server reads an S7 server connection: its URL and its points.
func newS7Server(conn Connection) (*s7Server, error) {
	s := &s7Server{}
	_, host, port, q, err := parseServerTCPURL(conn.URL, "102")
	if err != nil {
		return nil, fmt.Errorf("plc4x: server %q: %w", conn.Name, err)
	}
	s.listen = net.JoinHostPort(host, port)
	s.readOnly = readOnlyInterfaces(q)
	seen := map[string]bool{}
	for _, b := range conn.Bindings {
		p, err := parseS7Address(b.Address)
		if err != nil {
			return nil, fmt.Errorf("plc4x: server %q: tag %q: %w", conn.Name, b.Tag, err)
		}
		p.tag = b.Tag
		p.writable = b.Direction == InOut
		key := fmt.Sprintf("%d/%d/%d.%d", p.area, p.db, p.byteAddr, p.bitAddr)
		if seen[key] {
			return nil, fmt.Errorf("plc4x: server %q: two points at %s", conn.Name, b.Address)
		}
		seen[key] = true
		s.points = append(s.points, p)
	}
	return s, nil
}

// parseS7Address reads one binding's address in the PLC4X S7 form.
func parseS7Address(addr string) (s7Point, error) {
	addr = strings.TrimSpace(addr)
	var p s7Point
	var byteStr, bitStr, typeName string
	if m := s7AddrDB.FindStringSubmatch(addr); m != nil {
		n, err := strconv.ParseUint(m[1], 10, 16)
		if err != nil {
			return p, fmt.Errorf("address %q: the data block number is 0 to 65535", addr)
		}
		p.area, p.db = s7.MemoryArea_DATA_BLOCKS, uint16(n)
		byteStr, bitStr, typeName = m[3], m[4], m[5]
	} else if m := s7AddrArea.FindStringSubmatch(addr); m != nil {
		p.area = s7Areas[m[1]]
		byteStr, bitStr, typeName = m[3], m[4], m[5]
	} else {
		return p, fmt.Errorf("address %q: want an S7 address, e.g. %%DB1.DBW0:INT, %%DB1.DBX0.0:BOOL, %%M4.0:BOOL, %%Q0.0:BOOL", addr)
	}
	n, err := strconv.ParseUint(byteStr, 10, 16)
	if err != nil {
		return p, fmt.Errorf("address %q: the byte offset is 0 to 65535", addr)
	}
	p.byteAddr = uint16(n)
	size, ok := s7.TransportSizeByName(strings.ToUpper(typeName))
	if !ok {
		return p, fmt.Errorf("address %q: no S7 type %q", addr, typeName)
	}
	p.size = size
	if size == s7.TransportSize_BOOL {
		if bitStr == "" {
			return p, fmt.Errorf("address %q: a BOOL needs a bit offset, e.g. %%DB1.DBX0.0:BOOL", addr)
		}
		b, _ := strconv.ParseUint(bitStr, 10, 8)
		p.bitAddr = uint8(b)
	} else if bitStr != "" {
		return p, fmt.Errorf("address %q: only a BOOL has a bit offset", addr)
	}
	return p, nil
}

// serveS7 listens and answers each client until ctx ends or the listener
// fails.
func (c *Connector) serveS7(ctx context.Context, cs *connState) error {
	s := cs.server.s7
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
			c.serveS7Conn(ctx, cs, conn)
		}()
	}
}

// serveS7Conn answers one client's TPKT frames until it goes, falls silent
// for tcpIdle or ctx ends.
func (c *Connector) serveS7Conn(ctx context.Context, cs *connState, conn net.Conn) {
	s := cs.server.s7
	writes := writesFromReadOnly(s.readOnly, conn.LocalAddr())
	from := conn.RemoteAddr().String()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer func() {
		stop()
		conn.Close()
	}()
	var head [4]byte
	for {
		conn.SetReadDeadline(time.Now().Add(tcpIdle))
		if _, err := io.ReadFull(conn, head[:]); err != nil {
			return
		}
		// TPKT header: version 0x03, reserved, 16-bit total length.
		if head[0] != 0x03 {
			return
		}
		total := int(binary.BigEndian.Uint16(head[2:4]))
		if total < 7 || total > 2048 {
			return
		}
		rest := make([]byte, total-4)
		if _, err := io.ReadFull(conn, rest); err != nil {
			return
		}
		packet, err := s7.TPKTPacketParse(ctx, append(head[:], rest...))
		if err != nil {
			return
		}
		reply := c.s7Reply(cs.Name, from, writes, s, packet)
		if reply == nil {
			continue
		}
		raw, err := reply.Serialize()
		if err != nil {
			c.onError(cs.Name, fmt.Errorf("serialize S7 reply: %w", err))
			return
		}
		conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if _, err := conn.Write(raw); err != nil {
			return
		}
		c.record(cs.Name, func(st *Status) { st.Reads++; st.LastRead = time.Now() }, nil)
	}
}

// s7Reply builds the reply to one TPKT packet, or nil when none is due.
func (c *Connector) s7Reply(name, from string, writes bool, s *s7Server, packet s7.TPKTPacket) s7.TPKTPacket {
	switch cotp := packet.GetPayload().(type) {
	case s7.COTPPacketConnectionRequest:
		cc := s7.NewCOTPPacketConnectionResponse(cotp.GetParameters(), nil,
			cotp.GetSourceReference(), cotp.GetDestinationReference(), cotp.GetProtocolClass())
		return s7.NewTPKTPacket(cc)
	case s7.COTPPacketData:
		msg, ok := cotp.GetPayload().(s7.S7MessageRequest)
		if !ok {
			return nil // user data (SZL probe) and the like are not answered
		}
		resp := c.s7Message(name, from, writes, s, msg)
		if resp == nil {
			return nil
		}
		return s7.NewTPKTPacket(s7.NewCOTPPacketData(nil, resp, true, cotp.GetTpduRef()))
	}
	return nil
}

// s7Message answers one S7 request message: setup communication, read var or
// write var.
func (c *Connector) s7Message(name, from string, writes bool, s *s7Server, msg s7.S7MessageRequest) s7.S7Message {
	ref := msg.GetTpduReference()
	switch param := msg.GetParameter().(type) {
	case s7.S7ParameterSetupCommunication:
		ack := s7.NewS7ParameterSetupCommunication(param.GetMaxAmqCaller(), param.GetMaxAmqCallee(), param.GetPduLength())
		return s7.NewS7MessageResponseData(ref, ack, nil, 0, 0)
	case s7.S7ParameterReadVarRequest:
		items := param.GetItems()
		data := make([]s7.S7VarPayloadDataItem, len(items))
		for i, it := range items {
			data[i] = c.s7Read(s, it)
		}
		return s7.NewS7MessageResponseData(ref,
			s7.NewS7ParameterReadVarResponse(uint8(len(items))),
			s7.NewS7PayloadReadVarResponse(data), 0, 0)
	case s7.S7ParameterWriteVarRequest:
		items := param.GetItems()
		var payloadItems []s7.S7VarPayloadDataItem
		if wp, ok := msg.GetPayload().(s7.S7PayloadWriteVarRequest); ok {
			payloadItems = wp.GetItems()
		}
		status := make([]s7.S7VarPayloadStatusItem, len(items))
		for i, it := range items {
			var raw []byte
			if i < len(payloadItems) {
				raw = payloadItems[i].GetData()
			}
			status[i] = s7.NewS7VarPayloadStatusItem(c.s7Write(name, from, writes, s, it, raw))
		}
		return s7.NewS7MessageResponseData(ref,
			s7.NewS7ParameterWriteVarResponse(uint8(len(items))),
			s7.NewS7PayloadWriteVarResponse(status), 0, 0)
	}
	return nil
}

// s7Read answers one read item with its tag's current value.
func (c *Connector) s7Read(s *s7Server, item s7.S7VarRequestParameterItem) s7.S7VarPayloadDataItem {
	notFound := func() s7.S7VarPayloadDataItem {
		return s7.NewS7VarPayloadDataItem(s7.DataTransportErrorCode_NOT_FOUND, s7.DataTransportSize_NULL, nil)
	}
	addr, ok := s7AddressOf(item)
	if !ok {
		return notFound()
	}
	p := s.pointFor(addr)
	if p == nil {
		return notFound()
	}
	raw, err := c.db.GetTagValue(p.tag)
	if err != nil || raw == nil {
		return s7.NewS7VarPayloadDataItem(s7.DataTransportErrorCode_INVALID_ADDRESS, s7.DataTransportSize_NULL, nil)
	}
	pv, err := toPlcValue(reflect.ValueOf(raw))
	if err != nil {
		return notFound()
	}
	data, err := s7.DataItemSerialize(pv, p.size.DataProtocolId(), s7.ControllerType_ANY, 0)
	if err != nil {
		return notFound()
	}
	return s7.NewS7VarPayloadDataItem(s7.DataTransportErrorCode_OK, p.size.DataTransportSize(), data)
}

// s7Write writes one write item's value to its tag and returns the status.
func (c *Connector) s7Write(name, from string, writes bool, s *s7Server, item s7.S7VarRequestParameterItem, raw []byte) s7.DataTransportErrorCode {
	addr, ok := s7AddressOf(item)
	if !ok {
		return s7.DataTransportErrorCode_NOT_FOUND
	}
	p := s.pointFor(addr)
	if p == nil {
		return s7.DataTransportErrorCode_NOT_FOUND
	}
	if !p.writable || !writes {
		return s7.DataTransportErrorCode_ACCESS_DENIED
	}
	pv, err := s7.DataItemParse(context.Background(), raw, p.size.DataProtocolId(), s7.ControllerType_ANY, 0)
	if err != nil {
		return s7.DataTransportErrorCode_DATA_TYPE_NOT_SUPPORTED
	}
	out, ex := c.serverValue(serverPoint{tag: p.tag}, pv)
	if ex != 0 {
		return s7.DataTransportErrorCode_DATA_TYPE_NOT_SUPPORTED
	}
	if ex := c.serverSet(name, from, []pending{{p.tag, out}}); ex != 0 {
		return s7.DataTransportErrorCode_ACCESS_DENIED
	}
	return s7.DataTransportErrorCode_OK
}

// s7AddressOf reads a request item's address, when it is a plain S7 address.
func s7AddressOf(item s7.S7VarRequestParameterItem) (s7.S7AddressAny, bool) {
	a, ok := item.(s7.S7VarRequestParameterItemAddress)
	if !ok {
		return nil, false
	}
	addr, ok := a.GetAddress().(s7.S7AddressAny)
	return addr, ok
}

// pointFor finds the point an addressed item names: same area, data block,
// byte offset, transport size, and, for a BOOL, bit offset.
func (s *s7Server) pointFor(addr s7.S7AddressAny) *s7Point {
	for i := range s.points {
		p := &s.points[i]
		if p.area != addr.GetArea() || p.byteAddr != addr.GetByteAddress() || p.size != addr.GetTransportSize() {
			continue
		}
		if p.area == s7.MemoryArea_DATA_BLOCKS && p.db != addr.GetDbNumber() {
			continue
		}
		if p.size == s7.TransportSize_BOOL && p.bitAddr != addr.GetBitAddress() {
			continue
		}
		return p
	}
	return nil
}
