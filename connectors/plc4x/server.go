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

// Server mode: the connector answers Modbus masters, serving tags as
// coils, discrete inputs, input registers and holding registers (drone
// D-66), as a Modbus RTU slave on a serial port or a Modbus TCP server.
// PLC4X's Go library has no Modbus slave, so this is built from its public
// parts: the serial port package its serial transport uses, and the Modbus
// protocol model its drivers use to parse requests and encode responses and
// values. A value is therefore encoded and decoded exactly as PLC4X's own
// client does.
//
// A server connection's URL names where it answers and its own address,
// default-unit-identifier (unit-identifier is taken too):
//
//	modbus-rtu:serial:///dev/ttyAMA2?serial.baud-rate=19200&serial.data-bits=8&serial.parity=even&serial.stop-bits=1&default-unit-identifier=1
//	modbus-tcp://0.0.0.0:502?default-unit-identifier=1&read-only-interfaces=eth1
//
// Its bindings are the register map, in PLC4X's Modbus address form
// ("holding-register:1:REAL", "coil:5", "input-register:10[4]:INT"). An
// Output binding is read only: the master reads the tag. An InOut binding
// the master may also write (function codes 5, 6, 15 and 16): a coil or a
// holding register, one value (not an array), written whole. A TCP server
// answers writes only on a connection whose local address is not on one of
// read-only-interfaces: a master there reads and nothing more. Every other
// function gets exception 01.
//
// Modbus ASCII is served too, on a serial port like RTU: the same requests,
// framed as ':' and hex characters, an LRC and CR LF (server_ascii.go):
//
//	modbus-ascii:serial:///dev/ttyAMA2?serial.baud-rate=9600&serial.data-bits=7&serial.parity=even&default-unit-identifier=1
//
// IEC 60870-5-104 (server_iec104.go) and S7 (server_s7.go) servers share
// this file's URL handling, Status and write path.

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	apiValues "github.com/apache/plc4x/plc4go/pkg/api/values"
	"github.com/apache/plc4x/plc4go/protocols/modbus/readwrite/model"
	"github.com/apache/plc4x/plc4go/spi/transports/serial/serialport"
	spiValues "github.com/apache/plc4x/plc4go/spi/values"
)

// Modbus areas, by the function code that reads them.
const (
	areaCoil          = 1 // FC 1, 0xxxx
	areaDiscreteInput = 2 // FC 2, 1xxxx
	areaHolding       = 3 // FC 3, 4xxxx
	areaInput         = 4 // FC 4, 3xxxx
)

var areaNames = map[string]int{"coil": areaCoil, "discrete-input": areaDiscreteInput, "holding-register": areaHolding, "input-register": areaInput}

// numeric prefixes: 0xxxx coils, 1xxxx discrete inputs, 3xxxx input
// registers, 4xxxx holding registers.
var numericAreas = map[byte]int{'0': areaCoil, '1': areaDiscreteInput, '3': areaInput, '4': areaHolding}

var (
	namedAddress   = regexp.MustCompile(`^(coil|discrete-input|holding-register|input-register):(\d+)(?:\[(\d+)\])?(?::([A-Za-z_]+))?$`)
	numericAddress = regexp.MustCompile(`^([0134])[xX]?(\d{4,5})(?:\[(\d+)\])?(?::([A-Za-z_]+))?$`)
)

// Limits of one request (Modbus application protocol 1.1b3).
const (
	maxReadBits       = 2000
	maxReadRegisters  = 125
	maxWriteBits      = 1968
	maxWriteRegisters = 123
)

// serverPoint is one binding of a server's register map.
type serverPoint struct {
	tag      string
	area     int
	start    uint16 // wire address of the first bit or register
	elems    uint16 // 1 for a scalar, n for an array
	words    uint16 // registers per element (registers only)
	dataType model.ModbusDataType
	writable bool // an InOut binding: the master may write it
}

// span is how many bits or registers the point covers.
func (p serverPoint) span() int {
	if p.area == areaCoil || p.area == areaDiscreteInput {
		return int(p.elems)
	}
	return int(p.elems) * int(p.words)
}

// server is a server connection: where it answers, its unit address and its
// register map.
type server struct {
	proto    string // the URL's protocol
	tcp      bool
	ascii    bool // Modbus ASCII framing on the serial port
	port     string // serial port
	config   serialport.Config
	listen   string   // TCP address, "0.0.0.0:502"
	readOnly []string // TCP: interfaces whose masters only read
	unit     uint8
	points   map[int][]serverPoint // by area, sorted by start
	mu       sync.Mutex            // one request at a time
	iec      *iecServer            // an IEC 60870-5-104 server's points
	s7       *s7Server             // an S7 server's areas
}

// IsServerURL says whether url is one a server connection can take: Modbus
// RTU or ASCII on a serial port, Modbus TCP, IEC 60870-5-104 or S7 over TCP:
// the protocols whose Capabilities say Server.
func IsServerURL(u string) bool {
	return CapabilitiesOf(ProtocolOf(u)).Server
}

// serverPorts are the TCP servers' default ports.
var serverPorts = map[string]string{"modbus-tcp": "502", "iec-60870-5-104": "2404", "s7": "102"}

// newServer reads a server connection: its URL and its bindings.
func newServer(conn Connection) (*server, error) {
	s := &server{points: map[int][]serverPoint{}}
	if err := s.parseURL(conn.URL); err != nil {
		return nil, fmt.Errorf("plc4x: server %q: %w", conn.Name, err)
	}
	for _, b := range conn.Bindings {
		if b.Direction != Output && b.Direction != InOut {
			return nil, fmt.Errorf("plc4x: server %q: tag %q: a server's bindings are outputs (read only) or in-outs (the client may write)", conn.Name, b.Tag)
		}
	}
	var err error
	switch s.proto {
	case "iec-60870-5-104":
		s.iec, err = newIECServer(conn)
		return s, err
	case "s7":
		s.s7, err = newS7Server(conn)
		return s, err
	}
	for _, b := range conn.Bindings {
		p, err := parseServerAddress(b.Address)
		if err != nil {
			return nil, fmt.Errorf("plc4x: server %q: tag %q: %w", conn.Name, b.Tag, err)
		}
		p.tag = b.Tag
		if b.Direction == InOut {
			switch {
			case p.area != areaCoil && p.area != areaHolding:
				return nil, fmt.Errorf("plc4x: server %q: tag %q: %s is read only in Modbus: only coils and holding registers are written", conn.Name, b.Tag, b.Address)
			case p.elems != 1:
				return nil, fmt.Errorf("plc4x: server %q: tag %q: an array is served read only", conn.Name, b.Tag)
			}
			p.writable = true
		}
		for _, q := range s.points[p.area] {
			if int(p.start) < int(q.start)+q.span() && int(q.start) < int(p.start)+p.span() {
				return nil, fmt.Errorf("plc4x: server %q: %s (%s) overlaps %s", conn.Name, b.Address, b.Tag, q.tag)
			}
		}
		s.points[p.area] = append(s.points[p.area], p)
	}
	for a := range s.points {
		slices.SortFunc(s.points[a], func(x, y serverPoint) int { return int(x.start) - int(y.start) })
	}
	return s, nil
}

// parseURL reads where the server answers, its settings and its unit
// address. A serial port's settings are those of PLC4X's serial transport.
func (s *server) parseURL(raw string) error {
	if !IsServerURL(raw) {
		return fmt.Errorf("a server is Modbus RTU or ASCII on a serial port (modbus-rtu:serial://...), or Modbus TCP (modbus-tcp://0.0.0.0:502), IEC 60870-5-104 (iec-60870-5-104://0.0.0.0:2404) or S7 (s7://0.0.0.0:102), not %q", ProtocolOf(raw))
	}
	s.proto = ProtocolOf(raw)
	var q url.Values
	if def, ok := serverPorts[s.proto]; ok {
		u, err := url.Parse(raw)
		if err != nil {
			return err
		}
		s.tcp = true
		host, port := u.Hostname(), u.Port()
		if port == "" {
			port = def
		}
		s.listen = net.JoinHostPort(host, port)
		q = u.Query()
		s.readOnly = readOnlyInterfaces(q)
		if s.proto != "modbus-tcp" {
			return nil // the unit address is Modbus's
		}
	} else {
		s.ascii = s.proto == "modbus-ascii"
		rest := strings.TrimPrefix(raw, s.proto+":")
		rest = strings.TrimPrefix(rest, "//") // "modbus-rtu://..." names the default transport, serial
		if !strings.Contains(rest, "://") {
			rest = "serial://" + rest
		}
		u, err := url.Parse(rest)
		if err != nil {
			return err
		}
		if u.Scheme != "serial" {
			return fmt.Errorf("a Modbus RTU or ASCII server needs the serial transport, not %q", u.Scheme)
		}
		s.port = u.Host + u.Path
		if s.port == "" {
			return errors.New("no serial port in the URL")
		}
		q = u.Query()
		if err := s.parseSerial(q); err != nil {
			return err
		}
	}
	unit := q.Get("default-unit-identifier")
	if unit == "" {
		unit = q.Get("unit-identifier")
	}
	if unit == "" {
		return errors.New("no default-unit-identifier: the unit's own Modbus address, 1 to 247")
	}
	n, err := strconv.ParseUint(unit, 10, 8)
	if err != nil || n < 1 || n > 247 {
		return fmt.Errorf("default-unit-identifier %q: the unit's own address, 1 to 247", unit)
	}
	s.unit = uint8(n)
	return nil
}

// parseSerial reads a serial port's settings.
func (s *server) parseSerial(q url.Values) error {
	s.config = serialport.Config{BaudRate: 19200, DataBits: 8, Parity: serialport.ParityEven, StopBits: serialport.StopBitsOne}
	if v := q.Get("serial.baud-rate"); v != "" {
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil || n == 0 {
			return fmt.Errorf("serial.baud-rate %q", v)
		}
		s.config.BaudRate = uint(n)
	}
	if v := q.Get("serial.data-bits"); v != "" {
		n, err := strconv.ParseUint(v, 10, 8)
		if err != nil || n < 5 || n > 8 {
			return fmt.Errorf("serial.data-bits %q: 5 to 8", v)
		}
		s.config.DataBits = uint(n)
	}
	switch v := q.Get("serial.stop-bits"); v {
	case "", "1":
	case "2":
		s.config.StopBits = serialport.StopBitsTwo
	default:
		return fmt.Errorf("serial.stop-bits %q: 1 or 2", v)
	}
	if v := q.Get("serial.parity"); v != "" {
		p, ok := map[string]serialport.Parity{"none": serialport.ParityNone, "odd": serialport.ParityOdd, "even": serialport.ParityEven}[v]
		if !ok {
			return fmt.Errorf("serial.parity %q: none, odd or even", v)
		}
		s.config.Parity = p
	}
	if q.Get("serial.reuse-port") == "true" {
		return errors.New("a server owns its port: serial.reuse-port is not allowed")
	}
	return nil
}

// parseServerAddress reads one binding's address, in PLC4X's Modbus forms.
func parseServerAddress(addr string) (serverPoint, error) {
	var area int
	var number, count, typeName string
	if m := namedAddress.FindStringSubmatch(addr); m != nil {
		area, number, count, typeName = areaNames[m[1]], m[2], m[3], m[4]
	} else if m := numericAddress.FindStringSubmatch(addr); m != nil {
		area, number, count, typeName = numericAreas[m[1][0]], m[2], m[3], m[4]
	} else {
		return serverPoint{}, fmt.Errorf("address %q: want coil:, discrete-input:, input-register: or holding-register: and a number (1 is the first), e.g. holding-register:1:REAL", addr)
	}
	n, err := strconv.ParseUint(number, 10, 32)
	if err != nil || n < 1 || n > 65536 {
		return serverPoint{}, fmt.Errorf("address %q: the number is 1 to 65536 (1 is the first)", addr)
	}
	p := serverPoint{area: area, start: uint16(n - 1), elems: 1}
	if count != "" {
		c, err := strconv.ParseUint(count, 10, 16)
		if err != nil || c < 1 {
			return serverPoint{}, fmt.Errorf("address %q: array length", addr)
		}
		p.elems = uint16(c)
	}
	bits := area == areaCoil || area == areaDiscreteInput
	if typeName == "" {
		typeName = "INT"
		if bits {
			typeName = "BOOL"
		}
	}
	dt, ok := model.ModbusDataTypeByName(strings.ToUpper(typeName))
	if !ok {
		return serverPoint{}, fmt.Errorf("address %q: no Modbus type %q", addr, typeName)
	}
	switch {
	case bits && dt != model.ModbusDataType_BOOL:
		return serverPoint{}, fmt.Errorf("address %q: coils and discrete inputs are BOOL", addr)
	case slices.Contains([]model.ModbusDataType{model.ModbusDataType_STRING, model.ModbusDataType_WSTRING, model.ModbusDataType_CHAR, model.ModbusDataType_WCHAR}, dt):
		return serverPoint{}, fmt.Errorf("address %q: the server does not serve %s yet", addr, dt)
	}
	p.dataType = dt
	if !bits {
		p.words = (uint16(dt.DataTypeSize()) + 1) / 2
		if p.words == 0 {
			p.words = 1
		}
	}
	if int(p.start)+p.span() > 65536 {
		return serverPoint{}, fmt.Errorf("address %q: runs past the last address", addr)
	}
	return p, nil
}

// Modbus exception codes.
const (
	exIllegalFunction = 0x01
	exIllegalAddress  = 0x02
	exIllegalValue    = 0x03
	exDeviceFailure   = 0x04
)

// answer returns the response frame to one RTU request frame, or nil when
// the request is not for this unit, is a broadcast, or is not a whole
// frame. A serial line's master may write the writable points.
func (s *server) answer(c *Connector, name string, frame []byte) []byte {
	if len(frame) < 4 || frame[0] != s.unit || !crcOK(frame) {
		return nil
	}
	out := append([]byte{s.unit}, s.handle(c, name, frame[1:len(frame)-2], true, "serial")...)
	return binary.LittleEndian.AppendUint16(out, rtuCRC(out))
}

// handle answers one request PDU with a response PDU: the reply, or an
// exception. writes says whether this master may write at all.
func (s *server) handle(c *Connector, name string, pdu []byte, writes bool, from string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	reply, ex := s.reply(c, name, pdu, writes, from)
	if ex != 0 {
		return []byte{pdu[0] | 0x80, ex}
	}
	return reply
}

// reply answers one request PDU: the response PDU, or an exception code.
func (s *server) reply(c *Connector, name string, pdu []byte, writes bool, from string) ([]byte, byte) {
	if len(pdu) == 0 {
		return nil, exIllegalFunction
	}
	fc := pdu[0]
	switch fc {
	case 1, 2, 3, 4:
	case 5, 6, 15, 16:
		if !writes {
			return nil, exIllegalFunction // read only here (D-66)
		}
	default:
		return nil, exIllegalFunction
	}
	req, err := model.ModbusPDUParse[model.ModbusPDU](context.Background(), pdu, false)
	if err != nil {
		return nil, exIllegalValue
	}
	var start, qty uint16
	switch r := req.(type) {
	case model.ModbusPDUReadCoilsRequest:
		start, qty = r.GetStartingAddress(), r.GetQuantity()
	case model.ModbusPDUReadDiscreteInputsRequest:
		start, qty = r.GetStartingAddress(), r.GetQuantity()
	case model.ModbusPDUReadHoldingRegistersRequest:
		start, qty = r.GetStartingAddress(), r.GetQuantity()
	case model.ModbusPDUReadInputRegistersRequest:
		start, qty = r.GetStartingAddress(), r.GetQuantity()
	case model.ModbusPDUWriteSingleCoilRequest:
		v := r.GetValue()
		if v != 0xFF00 && v != 0 {
			return nil, exIllegalValue
		}
		if ex := s.writeBits(c, name, from, r.GetAddress(), []bool{v == 0xFF00}); ex != 0 {
			return nil, ex
		}
		return pdu, 0 // the request, echoed
	case model.ModbusPDUWriteSingleRegisterRequest:
		if ex := s.writeRegisters(c, name, from, r.GetAddress(), binary.BigEndian.AppendUint16(nil, r.GetValue())); ex != 0 {
			return nil, ex
		}
		return pdu, 0
	case model.ModbusPDUWriteMultipleCoilsRequest:
		n := r.GetQuantity()
		if n < 1 || n > maxWriteBits || len(r.GetValue()) != (int(n)+7)/8 {
			return nil, exIllegalValue
		}
		bits := make([]bool, n)
		for i := range bits {
			bits[i] = r.GetValue()[i/8]&(1<<(i%8)) != 0
		}
		if ex := s.writeBits(c, name, from, r.GetStartingAddress(), bits); ex != 0 {
			return nil, ex
		}
		return serialize(model.NewModbusPDUWriteMultipleCoilsResponse(r.GetStartingAddress(), n))
	case model.ModbusPDUWriteMultipleHoldingRegistersRequest:
		n := r.GetQuantity()
		if n < 1 || n > maxWriteRegisters || len(r.GetValue()) != 2*int(n) {
			return nil, exIllegalValue
		}
		if ex := s.writeRegisters(c, name, from, r.GetStartingAddress(), r.GetValue()); ex != 0 {
			return nil, ex
		}
		return serialize(model.NewModbusPDUWriteMultipleHoldingRegistersResponse(r.GetStartingAddress(), n))
	default:
		return nil, exIllegalFunction
	}
	area := int(fc)
	var value []byte
	var ex byte
	if area == areaCoil || area == areaDiscreteInput {
		value, ex = s.bits(c, area, start, qty)
	} else {
		value, ex = s.registers(c, area, start, qty)
	}
	if ex != 0 {
		return nil, ex
	}
	switch area {
	case areaCoil:
		return serialize(model.NewModbusPDUReadCoilsResponse(value))
	case areaDiscreteInput:
		return serialize(model.NewModbusPDUReadDiscreteInputsResponse(value))
	case areaHolding:
		return serialize(model.NewModbusPDUReadHoldingRegistersResponse(value))
	default:
		return serialize(model.NewModbusPDUReadInputRegistersResponse(value))
	}
}

func serialize(pdu interface{ Serialize() ([]byte, error) }) ([]byte, byte) {
	b, err := pdu.Serialize()
	if err != nil {
		return nil, exDeviceFailure
	}
	return b, 0
}

// covering returns the points of area that overlap [start, start+qty).
func (s *server) covering(area int, start, qty uint16) []serverPoint {
	var out []serverPoint
	end := int(start) + int(qty)
	for _, p := range s.points[area] {
		if int(p.start) < end && int(start) < int(p.start)+p.span() {
			out = append(out, p)
		}
	}
	return out
}

// pointAt returns the writable point that starts at addr in area.
func (s *server) pointAt(area int, addr int) (serverPoint, bool) {
	for _, p := range s.points[area] {
		if int(p.start) == addr && p.writable {
			return p, true
		}
	}
	return serverPoint{}, false
}

// bits reads qty coils or discrete inputs from start, packed as Modbus packs
// them: the first bit in the low bit of the first byte.
func (s *server) bits(c *Connector, area int, start, qty uint16) ([]byte, byte) {
	if qty < 1 || qty > maxReadBits {
		return nil, exIllegalValue
	}
	if int(start)+int(qty) > 65536 {
		return nil, exIllegalAddress
	}
	points := s.covering(area, start, qty)
	if len(points) == 0 {
		return nil, exIllegalAddress
	}
	out := make([]byte, (int(qty)+7)/8)
	for _, p := range points {
		values := c.serverValues(p)
		for i := range int(p.elems) {
			bit := int(p.start) + i - int(start)
			if bit < 0 || bit >= int(qty) || i >= len(values) || !asBool(values[i]) {
				continue
			}
			out[bit/8] |= 1 << (bit % 8)
		}
	}
	return out, 0
}

// registers reads qty registers from start, each big-endian.
func (s *server) registers(c *Connector, area int, start, qty uint16) ([]byte, byte) {
	if qty < 1 || qty > maxReadRegisters {
		return nil, exIllegalValue
	}
	if int(start)+int(qty) > 65536 {
		return nil, exIllegalAddress
	}
	points := s.covering(area, start, qty)
	if len(points) == 0 {
		return nil, exIllegalAddress
	}
	out := make([]byte, 2*int(qty))
	for _, p := range points {
		values := c.serverValues(p)
		for i := range int(p.elems) {
			var b []byte
			if i < len(values) {
				b = encode(values[i], p.dataType)
			}
			for w := range int(p.words) {
				reg := int(p.start) + i*int(p.words) + w - int(start)
				if reg < 0 || reg >= int(qty) || 2*w+1 >= len(b) {
					continue
				}
				out[2*reg], out[2*reg+1] = b[2*w], b[2*w+1]
			}
		}
	}
	return out, 0
}

// pending is one tag a write sets, its value converted.
type pending struct {
	tag   string
	value any
}

// writeBits writes coils from start: each must be a writable coil. Nothing
// is written unless all are.
func (s *server) writeBits(c *Connector, name, from string, start uint16, bits []bool) byte {
	if int(start)+len(bits) > 65536 {
		return exIllegalAddress
	}
	var set []pending
	for i, b := range bits {
		p, ok := s.pointAt(areaCoil, int(start)+i)
		if !ok {
			return exIllegalAddress
		}
		v, ex := c.serverValue(p, spiValues.NewPlcBOOL(b))
		if ex != 0 {
			return ex
		}
		set = append(set, pending{p.tag, v})
	}
	return c.serverSet(name, from, set)
}

// writeRegisters writes holding registers from start, big-endian: they must
// cover writable holding points whole, one after another. Nothing is
// written unless all are.
func (s *server) writeRegisters(c *Connector, name, from string, start uint16, data []byte) byte {
	end := int(start) + len(data)/2
	if end > 65536 {
		return exIllegalAddress
	}
	var set []pending
	for addr := int(start); addr < end; {
		p, ok := s.pointAt(areaHolding, addr)
		if !ok || addr+int(p.words) > end {
			return exIllegalAddress
		}
		off := 2 * (addr - int(start))
		raw := data[off : off+2*int(p.words)]
		if p.dataType.DataTypeSize() == 1 { // a byte-sized type is the low byte of its register
			raw = raw[1:]
		}
		pv, err := model.DataItemParse(context.Background(), raw, p.dataType, 0)
		if err != nil {
			return exIllegalValue
		}
		v, ex := c.serverValue(p, pv)
		if ex != 0 {
			return ex
		}
		set = append(set, pending{p.tag, v})
		addr += int(p.words)
	}
	return c.serverSet(name, from, set)
}

// serverValue converts a written value to the tag's own type.
func (c *Connector) serverValue(p serverPoint, pv apiValues.PlcValue) (v any, ex byte) {
	defer func() {
		if recover() != nil {
			v, ex = nil, exIllegalValue
		}
	}()
	cur, err := c.db.GetTagValue(p.tag)
	if err != nil || cur == nil {
		return nil, exDeviceFailure
	}
	out, err := convert(reflect.TypeOf(cur), pv)
	if err != nil {
		return nil, exIllegalValue
	}
	return out, 0
}

// serverSet sets the tags a write named, and reports each.
func (c *Connector) serverSet(name, from string, set []pending) byte {
	for _, w := range set {
		if err := c.db.SetTagValue(w.tag, w.value); err != nil {
			c.onError(name, fmt.Errorf("write %s from %s: %w", w.tag, from, err))
			return exDeviceFailure
		}
		c.onServerWrite(name, w.tag, w.value, from)
	}
	c.record(name, func(st *Status) { st.Writes++; st.LastWrite = time.Now() }, nil)
	return 0
}

// serverValues returns a point's tag value, element by element: one value
// for a scalar. A tag without a value gives none, which reads as zero.
func (c *Connector) serverValues(p serverPoint) []any {
	v, err := c.db.GetTagValue(p.tag)
	if err != nil || v == nil {
		return nil
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array {
		out := make([]any, rv.Len())
		for i := range out {
			out[i] = rv.Index(i).Interface()
		}
		return out
	}
	return []any{v}
}

// encode is one value as PLC4X encodes dataType in registers, padded to
// whole registers; nil when it cannot be encoded, which reads as zero.
func encode(v any, dt model.ModbusDataType) (out []byte) {
	defer func() {
		if recover() != nil {
			out = nil
		}
	}()
	pv, err := toPlcValue(reflect.ValueOf(v))
	if err != nil {
		return nil
	}
	b, err := model.DataItemSerialize(pv, dt, 0)
	if err != nil {
		return nil
	}
	if len(b)%2 == 1 { // a byte-sized type takes the low byte of its register
		b = append([]byte{0}, b...)
	}
	return b
}

// asBool reads a tag value as a bit: true, or any number other than zero.
func asBool(v any) bool {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Bool:
		return rv.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int() != 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return rv.Uint() != 0
	case reflect.Float32, reflect.Float64:
		return rv.Float() != 0
	}
	return false
}

// runServer answers masters until ctx ends, starting again with backoff
// after an error (a port that cannot be opened, an address in use).
func (c *Connector) runServer(ctx context.Context, cs *connState) {
	backoff := minBackoff
	for ctx.Err() == nil {
		var err error
		switch {
		case cs.server.iec != nil:
			err = c.serveIEC(ctx, cs)
		case cs.server.s7 != nil:
			err = c.serveS7(ctx, cs)
		case cs.server.tcp:
			err = c.serveTCP(ctx, cs)
		case cs.server.ascii:
			err = c.serveASCII(ctx, cs)
		default:
			err = c.serve(ctx, cs)
		}

		if ctx.Err() != nil {
			return
		}
		c.record(cs.Name, func(s *Status) { s.Connected = false }, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, maxBackoff)
	}
}

// frameGap is the silence that ends an RTU frame: 3.5 characters, and
// 1.75 ms above 19200 baud (Modbus over serial line 1.02, 2.5.1.1).
func frameGap(cfg serialport.Config) time.Duration {
	if cfg.BaudRate > 19200 {
		return 1750 * time.Microsecond
	}
	bits := 1 + float64(cfg.DataBits) + 1
	if cfg.Parity != serialport.ParityNone {
		bits++
	}
	if cfg.StopBits == serialport.StopBitsTwo {
		bits++
	}
	return time.Duration(math.Ceil(3.5 * bits / float64(cfg.BaudRate) * float64(time.Second)))
}

// serve opens the serial port and answers requests until ctx ends or the
// port fails. A frame ends at a silence of frameGap.
func (c *Connector) serve(ctx context.Context, cs *connState) error {
	s := cs.server
	port, err := serialport.Open(s.port, s.config)
	if err != nil {
		return fmt.Errorf("open %s: %w", s.port, err)
	}
	stop := context.AfterFunc(ctx, func() { port.Close() })
	defer func() {
		if stop() {
			port.Close()
		}
	}()
	c.record(cs.Name, func(st *Status) { st.Connected = true }, nil)
	gap := frameGap(s.config)
	buf := make([]byte, 0, 256)
	chunk := make([]byte, 256)
	for {
		deadline := time.Time{}
		if len(buf) > 0 {
			deadline = time.Now().Add(gap)
		}
		if err := port.SetReadDeadline(deadline); err != nil {
			return err
		}
		n, err := port.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
			if len(buf) > 256 { // longer than any RTU frame: noise
				buf = buf[:0]
			}
			continue
		}
		switch {
		case errors.Is(err, os.ErrDeadlineExceeded):
			if reply := s.answerAny(c, cs.Name, buf); reply != nil {
				if _, err := port.Write(reply); err != nil {
					return fmt.Errorf("write %s: %w", s.port, err)
				}
				c.record(cs.Name, func(st *Status) { st.Reads++; st.LastRead = time.Now() }, nil)
			}
			buf = buf[:0]
		case err != nil:
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("read %s: %w", s.port, err)
		}
	}
}

// answerAny answers the frame, or, when it does not check out, a request
// for this unit at its end: one that followed another device's reply too
// closely to be told apart by the silence alone. Only a read request
// (8 bytes) is looked for there.
func (s *server) answerAny(c *Connector, name string, buf []byte) []byte {
	if reply := s.answer(c, name, buf); reply != nil {
		return reply
	}
	const readRequest = 8 // address, function, start, quantity, CRC
	if len(buf) > readRequest && buf[len(buf)-readRequest+1] <= 4 {
		return s.answer(c, name, buf[len(buf)-readRequest:])
	}
	return nil
}

// Modbus TCP limits.
const (
	maxTCPClients = 8
	tcpIdle       = 2 * time.Minute // a client silent this long is closed
)

// serveTCP listens and answers each client until ctx ends or the listener
// fails.
func (c *Connector) serveTCP(ctx context.Context, cs *connState) error {
	s := cs.server
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
			conn.Close() // too many clients
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			c.serveTCPConn(ctx, cs, conn)
		}()
	}
}

// serveTCPConn answers one client's requests (MBAP framing) until it goes,
// falls silent for tcpIdle or ctx ends.
func (c *Connector) serveTCPConn(ctx context.Context, cs *connState, conn net.Conn) {
	s := cs.server
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer func() {
		stop()
		conn.Close()
	}()
	writes := s.writesFrom(conn.LocalAddr())
	from := conn.RemoteAddr().String()
	var head [7]byte
	for {
		conn.SetReadDeadline(time.Now().Add(tcpIdle))
		if _, err := io.ReadFull(conn, head[:]); err != nil {
			return
		}
		length := int(binary.BigEndian.Uint16(head[4:6]))
		if binary.BigEndian.Uint16(head[2:4]) != 0 || length < 2 || length > 254 {
			return // not Modbus: close
		}
		pdu := make([]byte, length-1)
		if _, err := io.ReadFull(conn, pdu); err != nil {
			return
		}
		var reply []byte
		if unit := head[6]; unit != s.unit && unit != 0 && unit != 255 {
			reply = []byte{pdu[0] | 0x80, 0x0B} // gateway target device failed to respond
		} else {
			reply = s.handle(c, cs.Name, pdu, writes, from)
		}
		// MBAP length: the unit id and the PDU, which is at most 253 bytes
		// (handle never makes more).
		n := len(reply) + 1
		if n < 2 || n > 254 {
			return
		}
		out := append(head[:4:4], 0, 0, head[6])
		binary.BigEndian.PutUint16(out[4:6], uint16(n))
		conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if _, err := conn.Write(append(out, reply...)); err != nil {
			return
		}
		c.record(cs.Name, func(st *Status) { st.Reads++; st.LastRead = time.Now() }, nil)
	}
}

// writesFrom says whether a client that reached the server at local may
// write: not when local is an address of a read-only interface.
func (s *server) writesFrom(local net.Addr) bool {
	return writesFromReadOnly(s.readOnly, local)
}

// readOnlyInterfaces reads the read-only-interfaces query parameter: the
// interface names whose clients may read but not write.
func readOnlyInterfaces(q url.Values) []string {
	var out []string
	for _, name := range strings.Split(q.Get("read-only-interfaces"), ",") {
		if name = strings.TrimSpace(name); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// parseServerTCPURL reads a TCP server's URL: its host, its port (defaultPort
// when none is given) and its query parameters.
func parseServerTCPURL(raw, defaultPort string) (*url.URL, string, string, url.Values, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, "", "", nil, err
	}
	port := u.Port()
	if port == "" {
		port = defaultPort
	}
	return u, u.Hostname(), port, u.Query(), nil
}

// writesFromReadOnly says whether a client that reached a TCP server at local
// may write: not when local is an address of one of the read-only interfaces.
func writesFromReadOnly(readOnly []string, local net.Addr) bool {
	a, ok := local.(*net.TCPAddr)
	if !ok {
		return false
	}
	for _, name := range readOnly {
		iface, err := net.InterfaceByName(name)
		if err != nil {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			return false // cannot tell: read only
		}
		for _, x := range addrs {
			if n, ok := x.(*net.IPNet); ok && n.IP.Equal(a.IP) {
				return false
			}
		}
	}
	return true
}


// rtuCRC is the Modbus RTU CRC, sent low byte first.
func rtuCRC(b []byte) uint16 {
	crc := uint16(0xFFFF)
	for _, x := range b {
		crc ^= uint16(x)
		for range 8 {
			if crc&1 != 0 {
				crc = crc>>1 ^ 0xA001
			} else {
				crc >>= 1
			}
		}
	}
	return crc
}

// crcOK checks a frame's last two bytes against the CRC of the rest.
func crcOK(frame []byte) bool {
	n := len(frame)
	return binary.LittleEndian.Uint16(frame[n-2:]) == rtuCRC(frame[:n-2])
}
