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
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// replyFor is the line's devices: unit 3 answers, unit 7 answers with
// exception 2, unit 9 sends a frame with a bad CRC, the others are silent.
func replyFor(unit byte) []byte {
	switch unit {
	case 3:
		return []byte{0x03, 2, 0, 42}
	case 7:
		return []byte{0x83, 0x02}
	}
	return nil
}

// fakeLine is a serial line with devices on it.
type fakeLine struct {
	mu       sync.Mutex
	pending  []byte
	ready    chan struct{}
	deadline time.Time
}

func newFakeLine() *fakeLine { return &fakeLine{ready: make(chan struct{}, 1)} }

func (l *fakeLine) Write(b []byte) (int, error) {
	unit := b[0]
	var out []byte
	if r := replyFor(unit); r != nil {
		out = append([]byte{unit}, r...)
		out = binary.LittleEndian.AppendUint16(out, rtuCRC(out))
	} else if unit == 9 {
		out = []byte{9, 0x03, 2, 0, 1, 0xDE, 0xAD}
	}
	if out != nil {
		l.mu.Lock()
		l.pending = append(l.pending, out...)
		l.mu.Unlock()
		select {
		case l.ready <- struct{}{}:
		default:
		}
	}
	return len(b), nil
}

func (l *fakeLine) Read(b []byte) (int, error) {
	for {
		l.mu.Lock()
		if len(l.pending) > 0 {
			n := copy(b, l.pending)
			l.pending = l.pending[n:]
			l.mu.Unlock()
			return n, nil
		}
		d := l.deadline
		l.mu.Unlock()
		wait := time.Until(d)
		if d.IsZero() {
			wait = time.Hour
		}
		if wait <= 0 {
			return 0, os.ErrDeadlineExceeded
		}
		select {
		case <-l.ready:
		case <-time.After(wait):
			return 0, os.ErrDeadlineExceeded
		}
	}
}

func (l *fakeLine) Close() error { return nil }

func (l *fakeLine) SetReadDeadline(t time.Time) error {
	l.mu.Lock()
	l.deadline = t
	l.mu.Unlock()
	return nil
}

func (l *fakeLine) SetWriteDeadline(time.Time) error { return nil }

func TestScanRTU(t *testing.T) {
	line := newFakeLine()
	var found []ScannedUnit
	for unit := 1; unit <= 10; unit++ {
		ok, exc, err := askRTU(line, 2*time.Millisecond, byte(unit), 0, 30*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			found = append(found, ScannedUnit{Unit: unit, Exception: exc})
		}
	}
	if len(found) != 2 || found[0] != (ScannedUnit{Unit: 3}) || found[1] != (ScannedUnit{Unit: 7, Exception: 2}) {
		t.Fatalf("found %+v", found)
	}
}

// fakeGateway is a Modbus TCP gateway with the same devices behind it; a
// unit without one gets exception 0x0B.
func fakeGateway(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			head := make([]byte, 7)
			if _, err := io.ReadFull(conn, head); err != nil {
				return
			}
			body := make([]byte, int(binary.BigEndian.Uint16(head[4:6]))-1)
			if _, err := io.ReadFull(conn, body); err != nil {
				return
			}
			unit := head[6]
			r := replyFor(unit)
			switch {
			case unit == 5:
				continue // silence: the scan's timeout passes
			case r == nil:
				r = []byte{0x83, 0x0B}
			}
			out := append(head[:4:4], 0, 0)
			binary.BigEndian.PutUint16(out[4:6], uint16(len(r)+1))
			out = append(append(out, unit), r...)
			conn.Write(out)
		}
	}()
	return l.Addr().String()
}

func TestScanTCPGateway(t *testing.T) {
	addr := fakeGateway(t)
	var seen []int
	res, err := ScanUnits(context.Background(), "modbus-tcp://"+addr, ScanOptions{From: 1, To: 8, Timeout: 100 * time.Millisecond,
		Progress: func(u int) { seen = append(seen, u) }})
	if err != nil {
		t.Fatal(err)
	}
	if res.Tried != 8 || len(seen) != 8 || len(res.Units) != 2 || res.Units[0].Unit != 3 || res.Units[1] != (ScannedUnit{Unit: 7, Exception: 2}) {
		t.Fatalf("scan %+v (seen %v)", res, seen)
	}
}

func TestScanRefusesOthers(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		url  string
		o    ScanOptions
		want string
	}{
		{"s7://10.0.0.1", ScanOptions{}, "Modbus RTU and Modbus TCP"},
		{"modbus-rtu:tcp://10.0.0.1:502", ScanOptions{}, "serial transport"},
		{"modbus-tcp://127.0.0.1:1", ScanOptions{From: 0, To: 300}, "1 to 247"},
	} {
		if _, err := ScanUnits(ctx, c.url, c.o); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", c.url, err)
		}
	}
}
