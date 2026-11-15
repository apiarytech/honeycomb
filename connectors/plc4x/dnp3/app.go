/*
 * Copyright (C) 2026 Franklin D. Amador
 *
 * This software is dual-licensed under the terms of the GPL v3.0 and
 * a commercial license. You may choose to use this software under either
 * license.
 *
 * See the LICENSE files in the project root for full license text.
 */

package dnp3

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"time"
)

// The application layer (clause 4).

// Function codes.
const (
	FuncConfirm         = 0
	FuncRead            = 1
	FuncWrite           = 2
	FuncSelect          = 3
	FuncOperate         = 4
	FuncDirectOperate   = 5
	FuncDirectOperateNR = 6
	FuncResponse        = 129
)

// Application control octet.
const (
	acFIR = 0x80
	acFIN = 0x40
	acCON = 0x20
	acUNS = 0x10
)

// Internal indications: IIN1 in the low octet, IIN2 in the high one.
const (
	IINAllStations      uint16 = 0x0001
	IINClass1Events     uint16 = 0x0002
	IINClass2Events     uint16 = 0x0004
	IINClass3Events     uint16 = 0x0008
	IINNeedTime         uint16 = 0x0010
	IINDeviceRestart    uint16 = 0x0080
	IINNoFuncSupport    uint16 = 0x0100
	IINObjectUnknown    uint16 = 0x0200
	IINParameterError   uint16 = 0x0400
	IINEventOverflow    uint16 = 0x0800
	IINAlreadyExecuting uint16 = 0x1000
)

// Command statuses (a control's answer).
const (
	StatusSuccess       = 0
	StatusTimeout       = 1
	StatusNoSelect      = 2
	StatusFormatError   = 3
	StatusNotSupported  = 4
	StatusNotAuthorized = 9
)

// Qualifiers used here.
const (
	qStartStop8  = 0x00
	qStartStop16 = 0x01
	qAll         = 0x06
	qCount8      = 0x07
	qCount16     = 0x08
	qIndex8      = 0x17
	qIndex16     = 0x28
)

// Point flags: online, and the state bit of a binary.
const (
	flagOnline = 0x01
	flagState  = 0x80
)

// Objects (group, variation) handled, and each one's size in octets.
var objectSize = map[[2]uint8]int{
	{1, 2}: 1, {10, 2}: 1, // binary input, binary output status, with flags
	{2, 2}:  7,  // binary input event with absolute time
	{30, 1}: 5,  // 32-bit analog input with flag
	{30, 5}: 5,  // single-precision analog input with flag
	{32, 3}: 11, // 32-bit analog input event with time
	{32, 7}: 11, // single-precision analog input event with time
	{40, 1}: 5,  // 32-bit analog output status with flag
	{40, 3}: 5,  // single-precision analog output status with flag
	{12, 1}: 11, // control relay output block
	{41, 1}: 5,  // 32-bit analog output block
	{41, 3}: 5,  // single-precision analog output block
	{50, 1}: 6,  // time and date
}

// Header is one object header and its objects: the indexes they carry and
// each object's octets.
type Header struct {
	Group, Variation, Qualifier uint8
	Indexes                     []uint32
	Objects                     [][]byte
	All                         bool // qualifier 0x06: every object of the kind
}

var errShort = errors.New("dnp3: object headers cut short")

// ParseHeaders reads a fragment's object headers. With data false (a READ)
// the headers carry no objects but the indexes a prefix names.
func ParseHeaders(b []byte, data bool) ([]Header, error) {
	var out []Header
	for len(b) > 0 {
		if len(b) < 3 {
			return out, errShort
		}
		h := Header{Group: b[0], Variation: b[1], Qualifier: b[2]}
		b = b[3:]
		size := 0
		if data {
			if h.Group == 80 && h.Variation == 1 {
				size = -1 // packed bits
			} else if s, ok := objectSize[[2]uint8{h.Group, h.Variation}]; ok {
				size = s
			} else {
				return out, fmt.Errorf("dnp3: object g%dv%d not supported", h.Group, h.Variation)
			}
		}
		take := func(n int) ([]byte, error) {
			if len(b) < n {
				return nil, errShort
			}
			v := b[:n]
			b = b[n:]
			return v, nil
		}
		switch h.Qualifier {
		case qAll:
			h.All = true
		case qStartStop8, qStartStop16:
			w := 1
			if h.Qualifier == qStartStop16 {
				w = 2
			}
			r, err := take(2 * w)
			if err != nil {
				return out, err
			}
			start, stop := uint32(r[0]), uint32(r[1])
			if w == 2 {
				start, stop = uint32(binary.LittleEndian.Uint16(r[0:2])), uint32(binary.LittleEndian.Uint16(r[2:4]))
			}
			if stop < start || stop-start > 4096 {
				return out, fmt.Errorf("dnp3: range %d to %d", start, stop)
			}
			for i := start; i <= stop; i++ {
				h.Indexes = append(h.Indexes, i)
			}
			if size == -1 {
				bits, err := take((len(h.Indexes) + 7) / 8)
				if err != nil {
					return out, err
				}
				for i := range h.Indexes {
					h.Objects = append(h.Objects, []byte{bits[i/8] >> (i % 8) & 1})
				}
			} else if size > 0 {
				for range h.Indexes {
					o, err := take(size)
					if err != nil {
						return out, err
					}
					h.Objects = append(h.Objects, o)
				}
			}
		case qCount8, qCount16, qIndex8, qIndex16:
			w := 1
			if h.Qualifier == qCount16 || h.Qualifier == qIndex16 {
				w = 2
			}
			r, err := take(w)
			if err != nil {
				return out, err
			}
			n := int(r[0])
			if w == 2 {
				n = int(binary.LittleEndian.Uint16(r))
			}
			if n > 4096 {
				return out, fmt.Errorf("dnp3: count %d", n)
			}
			prefixed := h.Qualifier == qIndex8 || h.Qualifier == qIndex16
			if size == -1 && prefixed {
				return out, fmt.Errorf("dnp3: packed bits with an index prefix")
			}
			for i := range n {
				idx := uint32(i)
				if prefixed {
					p, err := take(w)
					if err != nil {
						return out, err
					}
					idx = uint32(p[0])
					if w == 2 {
						idx = uint32(binary.LittleEndian.Uint16(p))
					}
				}
				h.Indexes = append(h.Indexes, idx)
				if size > 0 {
					o, err := take(size)
					if err != nil {
						return out, err
					}
					h.Objects = append(h.Objects, o)
				}
			}
			if size == -1 {
				bits, err := take((n + 7) / 8)
				if err != nil {
					return out, err
				}
				for i := range n {
					h.Objects = append(h.Objects, []byte{bits[i/8] >> (i % 8) & 1})
				}
			}
		default:
			return out, fmt.Errorf("dnp3: qualifier 0x%02x not supported", h.Qualifier)
		}
		out = append(out, h)
	}
	return out, nil
}

// time48 is DNP3's absolute time: milliseconds since 1970 UTC, 6 octets.
func time48(t time.Time) []byte {
	ms := uint64(t.UnixMilli())
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, ms)
	return b[:6]
}

// Time48 reads an absolute time.
func Time48(b []byte) time.Time {
	var x [8]byte
	copy(x[:], b[:6])
	return time.UnixMilli(int64(binary.LittleEndian.Uint64(x[:]))).UTC()
}

func f32(v float64) []byte {
	return binary.LittleEndian.AppendUint32(nil, math.Float32bits(float32(v)))
}

func i32(v float64) []byte {
	v = math.Round(v)
	switch {
	case v > math.MaxInt32:
		v = math.MaxInt32
	case v < math.MinInt32:
		v = math.MinInt32
	}
	return binary.LittleEndian.AppendUint32(nil, uint32(int32(v)))
}

// Control relay output block (g12v1) codes.
const (
	OpPulseOn  = 0x01
	OpPulseOff = 0x02
	OpLatchOn  = 0x03
	OpLatchOff = 0x04
	TCClose    = 0x40
	TCTrip     = 0x80
)

// CROB is a control relay output block's value: on for latch on, close or
// pulse on; off for latch off, trip or pulse off. ok is false for a code
// that names nothing.
func CROB(code uint8) (on, ok bool) {
	switch {
	case code&0xC0 == TCClose:
		return true, true
	case code&0xC0 == TCTrip:
		return false, true
	}
	switch code & 0x0F {
	case OpLatchOn, OpPulseOn:
		return true, true
	case OpLatchOff, OpPulseOff:
		return false, true
	}
	return false, false
}
