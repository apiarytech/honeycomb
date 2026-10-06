//go:build linux

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
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/apiarytech/honeycomb"
	plc "github.com/apiarytech/royaljelly/iec"
)

// TestConnectorModbusRTUOverSerial reads and writes a Modbus RTU device on a
// serial port: a simulated device on a pseudo-terminal, through PLC4X's
// serial transport, as an RS-485 adapter's port would be.
func TestConnectorModbusRTUOverSerial(t *testing.T) {
	master, slave := openPTY(t)
	dev := &rtuDevice{unit: 7}
	dev.regs[0] = 1234 // holding register 1
	go dev.serve(master)

	db := honeycomb.NewTagDatabase()
	addTag(t, db, "RIO1.Level", honeycomb.TypeINT, plc.INT(0))
	addTag(t, db, "RIO1.Setpoint", honeycomb.TypeINT, plc.INT(0))

	url := fmt.Sprintf("modbus-rtu:serial://%s?serial.baud-rate=19200&serial.parity=none&serial.stop-bits=1&default-unit-identifier=7", slave)
	connector, err := New(db, []Connection{{
		Name:     "rio1",
		URL:      url,
		Interval: 50 * time.Millisecond,
		Bindings: []Binding{
			{Tag: "RIO1.Level", Address: "holding-register:1:INT"},
			{Tag: "RIO1.Setpoint", Address: "holding-register:2:INT", Direction: Output},
		},
	}}, WithErrorHandler(func(name string, err error) { t.Logf("%s: %v", name, err) }))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { connector.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	waitFor(t, func() bool { v, _ := db.GetTagValue("RIO1.Level"); return v == plc.INT(1234) })
	if err := db.SetTagValue("RIO1.Setpoint", plc.INT(-5)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return int16(dev.reg(1)) == -5 })
	if s, _ := connector.Status("rio1"); !s.Connected || s.Reads == 0 || s.Writes == 0 {
		t.Errorf("status %+v", s)
	}
	if dev.badCRC() != 0 {
		t.Errorf("%d frames with a bad CRC", dev.badCRC())
	}
}

// openPTY opens a pseudo-terminal pair: the device serves the master, the
// connector opens the slave as its serial port.
func openPTY(t *testing.T) (*os.File, string) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skip("no pseudo-terminals:", err)
	}
	rc, _ := master.SyscallConn()
	var n uint32
	var ierr error
	rc.Control(func(fd uintptr) {
		unlock := int32(0)
		if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(syscall.TIOCSPTLCK), uintptr(unsafe.Pointer(&unlock))); e != 0 {
			ierr = e
			return
		}
		if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(syscall.TIOCGPTN), uintptr(unsafe.Pointer(&n))); e != 0 {
			ierr = e
		}
	})
	if ierr != nil {
		t.Skip("pseudo-terminal:", ierr)
	}
	slave := fmt.Sprintf("/dev/pts/%d", n)
	// keep the slave open, so the master does not fail between the
	// connector's opens and closes
	hold, err := os.OpenFile(slave, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skip("pseudo-terminal slave:", err)
	}
	t.Cleanup(func() { hold.Close(); master.Close() })
	return master, slave
}

// rtuDevice is a Modbus RTU device with 16 holding registers: read holding
// registers (03), write single register (06), write multiple registers (16).
type rtuDevice struct {
	unit byte
	mu   sync.Mutex
	regs [16]uint16
	bad  int
}

func (d *rtuDevice) reg(i int) uint16 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.regs[i]
}

func (d *rtuDevice) badCRC() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.bad
}

func (d *rtuDevice) serve(rw io.ReadWriter) {
	var buf []byte
	chunk := make([]byte, 256)
	for {
		n, err := rw.Read(chunk)
		if err != nil {
			return
		}
		buf = append(buf, chunk[:n]...)
		for {
			size := frameSize(buf)
			if size == 0 || len(buf) < size {
				break
			}
			frame := buf[:size]
			buf = buf[size:]
			if crc16(frame[:size-2]) != binary.LittleEndian.Uint16(frame[size-2:]) {
				d.mu.Lock()
				d.bad++
				d.mu.Unlock()
				buf = nil
				break
			}
			if frame[0] != d.unit {
				continue
			}
			if reply := d.handle(frame[1 : size-2]); reply != nil {
				out := append([]byte{d.unit}, reply...)
				out = binary.LittleEndian.AppendUint16(out, crc16(out))
				rw.Write(out)
			}
		}
	}
}

// frameSize is the length of the request at the start of buf, or 0 when
// not enough has arrived to know it.
func frameSize(buf []byte) int {
	if len(buf) < 2 {
		return 0
	}
	switch buf[1] {
	case 0x03, 0x06:
		return 8
	case 0x10:
		if len(buf) < 7 {
			return 0
		}
		return 9 + int(buf[6])
	}
	return len(buf) // unknown: answered with an exception
}

func (d *rtuDevice) handle(pdu []byte) []byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	fn := pdu[0]
	exception := func(code byte) []byte { return []byte{fn | 0x80, code} }
	switch fn {
	case 0x03:
		start, count := int(binary.BigEndian.Uint16(pdu[1:])), int(binary.BigEndian.Uint16(pdu[3:]))
		if start+count > len(d.regs) {
			return exception(0x02)
		}
		out := []byte{fn, byte(2 * count)}
		for i := range count {
			out = binary.BigEndian.AppendUint16(out, d.regs[start+i])
		}
		return out
	case 0x06:
		reg := int(binary.BigEndian.Uint16(pdu[1:]))
		if reg >= len(d.regs) {
			return exception(0x02)
		}
		d.regs[reg] = binary.BigEndian.Uint16(pdu[3:])
		return append([]byte(nil), pdu[:5]...)
	case 0x10:
		start, count := int(binary.BigEndian.Uint16(pdu[1:])), int(binary.BigEndian.Uint16(pdu[3:]))
		if start+count > len(d.regs) {
			return exception(0x02)
		}
		for i := range count {
			d.regs[start+i] = binary.BigEndian.Uint16(pdu[6+2*i:])
		}
		return append([]byte(nil), pdu[:5]...)
	}
	return exception(0x01)
}

// crc16 is Modbus RTU's CRC (polynomial 0xA001, initial 0xFFFF).
func crc16(b []byte) uint16 {
	crc := uint16(0xFFFF)
	for _, c := range b {
		crc ^= uint16(c)
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

// TestConnectorRS485Bus reads two devices on one bus: two connections on
// one serial port with different unit identifiers, sharing the port
// (serial.reuse-port), as Modbus RTU devices share an RS-485 line.
func TestConnectorRS485Bus(t *testing.T) {
	master, slave := openPTY(t)
	bus := &rtuBus{devices: map[byte]*rtuDevice{1: {unit: 1}, 2: {unit: 2}}}
	bus.devices[1].regs[0] = 111
	bus.devices[2].regs[0] = 222
	go bus.serve(master)

	db := honeycomb.NewTagDatabase()
	addTag(t, db, "RIO1.Level", honeycomb.TypeINT, plc.INT(0))
	addTag(t, db, "RIO2.Level", honeycomb.TypeINT, plc.INT(0))
	conn := func(name string, unit int) Connection {
		return Connection{
			Name:     name,
			URL:      fmt.Sprintf("modbus-rtu:serial://%s?serial.baud-rate=19200&serial.reuse-port=true&default-unit-identifier=%d", slave, unit),
			Interval: 50 * time.Millisecond,
			Bindings: []Binding{{Tag: fmt.Sprintf("RIO%d.Level", unit), Address: "holding-register:1:INT"}},
		}
	}
	connector, err := New(db, []Connection{conn("rio1", 1), conn("rio2", 2)},
		WithErrorHandler(func(name string, err error) { t.Logf("%s: %v", name, err) }))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { connector.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	waitFor(t, func() bool {
		a, _ := db.GetTagValue("RIO1.Level")
		b, _ := db.GetTagValue("RIO2.Level")
		return a == plc.INT(111) && b == plc.INT(222)
	})
}

// rtuBus answers for several devices on one line, each by its unit.
type rtuBus struct{ devices map[byte]*rtuDevice }

func (b *rtuBus) serve(rw io.ReadWriter) {
	var buf []byte
	chunk := make([]byte, 256)
	for {
		n, err := rw.Read(chunk)
		if err != nil {
			return
		}
		buf = append(buf, chunk[:n]...)
		for {
			size := frameSize(buf)
			if size == 0 || len(buf) < size {
				break
			}
			frame := buf[:size]
			buf = buf[size:]
			if crc16(frame[:size-2]) != binary.LittleEndian.Uint16(frame[size-2:]) {
				buf = nil
				break
			}
			d, ok := b.devices[frame[0]]
			if !ok {
				continue
			}
			if reply := d.handle(frame[1 : size-2]); reply != nil {
				out := append([]byte{d.unit}, reply...)
				out = binary.LittleEndian.AppendUint16(out, crc16(out))
				rw.Write(out)
			}
		}
	}
}
