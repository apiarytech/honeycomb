/*
 * Copyright (C) 2026 Franklin D. Amador
 *
 * This software is dual-licensed under the terms of the GPL v3.0 and
 * a commercial license. You may choose to use this software under either
 * license.
 *
 * See the LICENSE files in the project root for full license text.
 */

// Package dnp3 is a DNP3 (IEEE 1815) outstation and a small master over
// TCP, written for beehive (R-143): PLC4X has no DNP3 driver. It covers the
// subset a substation data concentrator needs: the data link layer with
// its CRCs, transport segments, the application layer's read, write,
// select, operate and direct operate, confirmations, static data (class
// 0) and time-stamped events (classes 1 to 3) of binary and analog inputs,
// binary and analog output status, and control relay output blocks and
// analog output blocks.
//
// Not in it yet: unsolicited responses, multi-fragment responses (a
// response is one fragment of at most 2048 octets; more events wait for
// the next read), counters, files, Secure Authentication (IEC 62351-5, SAv5
// and SAv6). For security over a network it runs over TLS (IEC 62351-3) as
// the IEC 60870-5-104 connection does.
package dnp3

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// The data link layer (IEEE 1815 clause 9).

// Link function codes.
const (
	// Primary (from the station that starts the exchange).
	LinkResetLinkStates = 0x0
	LinkTestLinkStates  = 0x2
	LinkConfirmedData   = 0x3
	LinkUnconfirmedData = 0x4
	LinkRequestStatus   = 0x9
	// Secondary (the answer).
	LinkAck          = 0x0
	LinkNack         = 0x1
	LinkStatus       = 0xB
	LinkNotSupported = 0xF
)

// Control octet bits.
const (
	ctlDir = 0x80 // from the master
	ctlPrm = 0x40 // a primary frame
	ctlFCB = 0x20
	ctlFCV = 0x10
)

// maxUserData is a link frame's user data at most: 250 octets.
const maxUserData = 250

var crcTable = func() [256]uint16 {
	var t [256]uint16
	for i := range 256 {
		c := uint16(i)
		for range 8 {
			if c&1 != 0 {
				c = c>>1 ^ 0xA6BC // 0x3D65, reflected
			} else {
				c >>= 1
			}
		}
		t[i] = c
	}
	return t
}()

// CRC is DNP3's CRC-16 over b, as it goes on the wire (low octet first).
func CRC(b []byte) uint16 {
	var c uint16
	for _, x := range b {
		c = c>>8 ^ crcTable[byte(c)^x]
	}
	return ^c
}

// Frame is one link layer frame.
type Frame struct {
	Dir, Prm, FCB, FCV bool
	Func               uint8
	Dest, Src          uint16
	Data               []byte // user data, 0 to 250 octets
}

// Marshal writes the frame with its header CRC and one CRC per 16 octets
// of user data.
func (f Frame) Marshal() ([]byte, error) {
	if len(f.Data) > maxUserData {
		return nil, fmt.Errorf("dnp3: %d octets of user data, at most %d", len(f.Data), maxUserData)
	}
	ctl := f.Func & 0x0F
	for _, b := range []struct {
		on  bool
		bit uint8
	}{{f.Dir, ctlDir}, {f.Prm, ctlPrm}, {f.FCB, ctlFCB}, {f.FCV, ctlFCV}} {
		if b.on {
			ctl |= b.bit
		}
	}
	out := make([]byte, 0, 10+len(f.Data)+2*((len(f.Data)+15)/16))
	out = append(out, 0x05, 0x64, byte(5+len(f.Data)), ctl)
	out = binary.LittleEndian.AppendUint16(out, f.Dest)
	out = binary.LittleEndian.AppendUint16(out, f.Src)
	out = binary.LittleEndian.AppendUint16(out, CRC(out[:8]))
	for d := f.Data; len(d) > 0; {
		n := min(16, len(d))
		out = append(out, d[:n]...)
		out = binary.LittleEndian.AppendUint16(out, CRC(d[:n]))
		d = d[n:]
	}
	return out, nil
}

// Errors of ReadFrame. A frame with a bad CRC is dropped by the caller and
// the stream resynchronises on the next start octets.
var (
	ErrCRC    = errors.New("dnp3: bad CRC")
	ErrLength = errors.New("dnp3: bad length")
)

// ReadFrame reads one frame from r, skipping octets until the start octets.
func ReadFrame(r io.Reader) (Frame, error) {
	var f Frame
	var b [1]byte
	prev := byte(0)
	for {
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return f, err
		}
		if prev == 0x05 && b[0] == 0x64 {
			break
		}
		prev = b[0]
	}
	var head [8]byte
	head[0], head[1] = 0x05, 0x64
	if _, err := io.ReadFull(r, head[2:]); err != nil {
		return f, err
	}
	var crc [2]byte
	if _, err := io.ReadFull(r, crc[:]); err != nil {
		return f, err
	}
	if CRC(head[:]) != binary.LittleEndian.Uint16(crc[:]) {
		return f, ErrCRC
	}
	length := int(head[2])
	if length < 5 {
		return f, ErrLength
	}
	ctl := head[3]
	f = Frame{Dir: ctl&ctlDir != 0, Prm: ctl&ctlPrm != 0, FCB: ctl&ctlFCB != 0, FCV: ctl&ctlFCV != 0, Func: ctl & 0x0F,
		Dest: binary.LittleEndian.Uint16(head[4:6]), Src: binary.LittleEndian.Uint16(head[6:8])}
	n := length - 5
	for n > 0 {
		k := min(16, n)
		block := make([]byte, k+2)
		if _, err := io.ReadFull(r, block); err != nil {
			return f, err
		}
		if CRC(block[:k]) != binary.LittleEndian.Uint16(block[k:]) {
			return f, ErrCRC
		}
		f.Data = append(f.Data, block[:k]...)
		n -= k
	}
	return f, nil
}

// The transport function (clause 8): one octet before each segment.
const (
	tpFIN = 0x80
	tpFIR = 0x40
)

// maxSegment is a transport segment's application data at most.
const maxSegment = maxUserData - 1

// segments splits an application fragment into transport segments,
// numbered from seq.
func segments(fragment []byte, seq uint8) ([][]byte, uint8) {
	var out [][]byte
	first := true
	for len(fragment) > 0 || first {
		n := min(maxSegment, len(fragment))
		h := seq & 0x3F
		if first {
			h |= tpFIR
		}
		if n == len(fragment) {
			h |= tpFIN
		}
		out = append(out, append([]byte{h}, fragment[:n]...))
		fragment = fragment[n:]
		seq = (seq + 1) & 0x3F
		first = false
	}
	return out, seq
}

// reassembler joins transport segments into a fragment.
type reassembler struct {
	buf     []byte
	next    uint8
	running bool
}

// maxFragment is an application fragment at most.
const maxFragment = 2048

// add takes one segment; it returns the fragment when FIN completes it.
func (r *reassembler) add(seg []byte) ([]byte, bool) {
	if len(seg) == 0 {
		return nil, false
	}
	h, data := seg[0], seg[1:]
	if h&tpFIR != 0 {
		r.buf, r.running = append([]byte(nil), data...), true
	} else if !r.running || h&0x3F != r.next {
		r.running = false // out of order: drop the fragment
		return nil, false
	} else {
		r.buf = append(r.buf, data...)
	}
	if len(r.buf) > maxFragment {
		r.running = false
		return nil, false
	}
	r.next = (h + 1) & 0x3F
	if h&tpFIN != 0 {
		r.running = false
		return r.buf, true
	}
	return nil, false
}
