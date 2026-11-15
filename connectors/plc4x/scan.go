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
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/apache/plc4x/plc4go/spi/transports/serial/serialport"
)

// A Modbus unit scan (R-138): which unit IDs answer on a serial line, or
// behind a Modbus TCP gateway. Each unit in the range is asked for one
// holding register with a short timeout; a normal answer or a Modbus
// exception both mean a device is there. The scan opens the port itself:
// nothing else may use it meanwhile.

// ScanOptions bound a scan.
type ScanOptions struct {
	// From and To are the unit IDs to try, 1 to 247; 0 and 0 are all.
	From, To int
	// Timeout is how long each unit has to answer: 0 is 200 ms.
	Timeout time.Duration
	// Register is the holding register asked for (0-based).
	Register uint16
	// Progress, if set, is told each unit before it is tried.
	Progress func(unit int)
}

// ScannedUnit is a unit that answered.
type ScannedUnit struct {
	Unit int
	// Exception is the Modbus exception code it answered with; 0 for a
	// normal answer.
	Exception uint8
}

// ScanResult is what a scan found.
type ScanResult struct {
	Units []ScannedUnit
	Tried int
}

// ScanUnits tries the units of a Modbus RTU serial line
// (modbus-rtu:serial:///dev/ttyAMA2?serial.baud-rate=...) or of a Modbus
// TCP gateway (modbus-tcp://host:502).
func ScanUnits(ctx context.Context, rawURL string, o ScanOptions) (ScanResult, error) {
	if o.From == 0 && o.To == 0 {
		o.From, o.To = 1, 247
	}
	if o.From < 1 || o.To > 247 || o.From > o.To {
		return ScanResult{}, fmt.Errorf("plc4x: scan units %d to %d: 1 to 247", o.From, o.To)
	}
	if o.Timeout <= 0 {
		o.Timeout = 200 * time.Millisecond
	}
	var ask func(unit byte) (bool, uint8, error)
	switch proto := ProtocolOf(rawURL); proto {
	case "modbus-rtu":
		s := &server{proto: proto}
		rest := strings.TrimPrefix(strings.TrimPrefix(rawURL, proto+":"), "//")
		if !strings.Contains(rest, "://") {
			rest = "serial://" + rest
		}
		u, err := url.Parse(rest)
		if err != nil {
			return ScanResult{}, err
		}
		if u.Scheme != "serial" {
			return ScanResult{}, fmt.Errorf("plc4x: a Modbus RTU scan needs the serial transport, not %q", u.Scheme)
		}
		s.port = u.Host + u.Path
		q := u.Query()
		q.Del("serial.reuse-port")
		if err := s.parseSerial(q); err != nil {
			return ScanResult{}, err
		}
		port, err := serialport.Open(s.port, s.config)
		if err != nil {
			return ScanResult{}, fmt.Errorf("plc4x: open %s: %w", s.port, err)
		}
		defer port.Close()
		gap := frameGap(s.config)
		ask = func(unit byte) (bool, uint8, error) { return askRTU(port, gap, unit, o.Register, o.Timeout) }
	case "modbus-tcp":
		u, err := url.Parse(rawURL)
		if err != nil || u.Host == "" {
			return ScanResult{}, fmt.Errorf("plc4x: scan: %q is not a Modbus TCP address", rawURL)
		}
		host := u.Host
		if u.Port() == "" {
			host = net.JoinHostPort(u.Hostname(), "502")
		}
		var d net.Dialer
		conn, err := d.DialContext(ctx, "tcp", host)
		if err != nil {
			return ScanResult{}, fmt.Errorf("plc4x: connect %s: %w", host, err)
		}
		defer conn.Close()
		var tid uint16
		ask = func(unit byte) (bool, uint8, error) {
			tid++
			return askTCP(conn, tid, unit, o.Register, o.Timeout)
		}
	default:
		return ScanResult{}, fmt.Errorf("plc4x: protocol %q: a unit scan is for Modbus RTU and Modbus TCP gateways", proto)
	}
	var res ScanResult
	for unit := o.From; unit <= o.To; unit++ {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if o.Progress != nil {
			o.Progress(unit)
		}
		res.Tried++
		ok, exc, err := ask(byte(unit))
		if err != nil {
			return res, err
		}
		if ok {
			res.Units = append(res.Units, ScannedUnit{Unit: unit, Exception: exc})
		}
	}
	return res, nil
}

// readRequest is "read holding registers", one register, as a PDU.
func readRequest(register uint16) []byte {
	return []byte{0x03, byte(register >> 8), byte(register), 0, 1}
}

// answer reads a response PDU: a normal answer to function 3 or an
// exception.
func answer(pdu []byte) (ok bool, exception uint8) {
	switch {
	case len(pdu) >= 2 && pdu[0] == 0x83:
		return true, pdu[1]
	case len(pdu) >= 2 && pdu[0] == 0x03:
		return true, 0
	}
	return false, 0
}

// askRTU sends one request on a serial line and waits for its unit's
// answer. Silence, noise or another unit's frame is no answer.
func askRTU(port serialport.Port, gap time.Duration, unit byte, register uint16, timeout time.Duration) (bool, uint8, error) {
	frame := append([]byte{unit}, readRequest(register)...)
	frame = binary.LittleEndian.AppendUint16(frame, rtuCRC(frame))
	if _, err := port.Write(frame); err != nil {
		return false, 0, fmt.Errorf("plc4x: scan: write: %w", err)
	}
	buf := make([]byte, 0, 256)
	chunk := make([]byte, 256)
	deadline := time.Now().Add(timeout)
	for {
		d := deadline
		if len(buf) > 0 {
			d = time.Now().Add(max(gap, 2*time.Millisecond))
		}
		if err := port.SetReadDeadline(d); err != nil {
			return false, 0, err
		}
		n, err := port.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
			if len(buf) > 256 {
				buf = buf[:0]
			}
			continue
		}
		if err != nil && !errors.Is(err, os.ErrDeadlineExceeded) {
			return false, 0, fmt.Errorf("plc4x: scan: read: %w", err)
		}
		// Silence: the frame, if any, is complete. Wait out the rest of a
		// slow unit's time only if nothing came.
		if len(buf) == 0 {
			if time.Now().Before(deadline) {
				continue
			}
			return false, 0, nil
		}
		if len(buf) < 5 || buf[0] != unit || !crcOK(buf) {
			return false, 0, nil
		}
		ok, exc := answer(buf[1 : len(buf)-2])
		return ok, exc, nil
	}
}

// askTCP sends one request through a Modbus TCP gateway and waits for its
// answer. A gateway that answers for a unit behind it with exception 0x0B
// (the target did not respond) has no device there.
func askTCP(conn net.Conn, tid uint16, unit byte, register uint16, timeout time.Duration) (bool, uint8, error) {
	pdu := readRequest(register)
	req := binary.BigEndian.AppendUint16(nil, tid)
	req = append(req, 0, 0)
	req = binary.BigEndian.AppendUint16(req, uint16(len(pdu)+1))
	req = append(append(req, unit), pdu...)
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return false, 0, err
	}
	if _, err := conn.Write(req); err != nil {
		return false, 0, fmt.Errorf("plc4x: scan: write: %w", err)
	}
	for {
		head := make([]byte, 7)
		if _, err := io.ReadFull(conn, head); err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				return false, 0, nil
			}
			return false, 0, fmt.Errorf("plc4x: scan: read: %w", err)
		}
		n := int(binary.BigEndian.Uint16(head[4:6]))
		if n < 2 || n > 260 {
			return false, 0, fmt.Errorf("plc4x: scan: a frame of %d bytes", n)
		}
		body := make([]byte, n-1)
		if _, err := io.ReadFull(conn, body); err != nil {
			return false, 0, fmt.Errorf("plc4x: scan: read: %w", err)
		}
		if binary.BigEndian.Uint16(head[0:2]) != tid || head[6] != unit {
			continue // a late answer to an earlier unit
		}
		ok, exc := answer(body)
		if ok && exc == 0x0B {
			return false, 0, nil
		}
		return ok, exc, nil
	}
}
