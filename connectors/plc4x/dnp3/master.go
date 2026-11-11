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
	"net"
	"time"
)

// Master is a DNP3 master on one connection: it reads an outstation's
// points and events and sends controls. One request at a time.
type Master struct {
	conn       net.Conn
	addr, dest uint16
	seq        uint8 // application sequence
	tseq       uint8
	Timeout    time.Duration
}

// NewMaster is a master at addr talking to the outstation at dest.
func NewMaster(conn net.Conn, addr, dest uint16) *Master {
	return &Master{conn: conn, addr: addr, dest: dest, Timeout: 5 * time.Second}
}

// Response is an outstation's answer.
type Response struct {
	IIN     uint16
	Confirm bool // it asked for a confirmation (events)
	Headers []Header
}

// Measurement is one reported value.
type Measurement struct {
	Group, Variation uint8
	Index            uint16
	Online           bool
	Bool             bool
	Number           float64
	Time             time.Time // events only
	Status           uint8     // controls only
}

// Request sends one request and returns the response, confirming it when
// the outstation asks.
func (m *Master) Request(fc uint8, objects []byte) (Response, error) {
	seq := m.seq
	m.seq = (m.seq + 1) & 0x0F
	if err := m.sendFragment(append([]byte{acFIR | acFIN | seq, fc}, objects...)); err != nil {
		return Response{}, err
	}
	if fc == FuncDirectOperateNR {
		return Response{}, nil
	}
	var in reassembler
	deadline := time.Now().Add(m.Timeout)
	for {
		m.conn.SetReadDeadline(deadline)
		f, err := ReadFrame(m.conn)
		if err != nil {
			return Response{}, err
		}
		if f.Dir || !f.Prm || f.Src != m.dest {
			continue
		}
		frag, ok := in.add(f.Data)
		if !ok || len(frag) < 4 || frag[1] != FuncResponse || frag[0]&0x0F != seq {
			continue
		}
		r := Response{IIN: binary.LittleEndian.Uint16(frag[2:4]), Confirm: frag[0]&acCON != 0}
		if r.Headers, err = ParseHeaders(frag[4:], true); err != nil {
			return r, err
		}
		if r.Confirm {
			if err := m.sendFragment([]byte{acFIR | acFIN | seq, FuncConfirm}); err != nil {
				return r, err
			}
		}
		return r, nil
	}
}

func (m *Master) sendFragment(frag []byte) error {
	segs, next := segments(frag, m.tseq)
	m.tseq = next
	for _, seg := range segs {
		b, err := Frame{Dir: true, Prm: true, Func: LinkUnconfirmedData, Dest: m.dest, Src: m.addr, Data: seg}.Marshal()
		if err != nil {
			return err
		}
		m.conn.SetWriteDeadline(time.Now().Add(m.Timeout))
		if _, err := m.conn.Write(b); err != nil {
			return err
		}
	}
	return nil
}

// Integrity reads class 0 (every static point) and classes 1 to 3.
func (m *Master) Integrity() (Response, []Measurement, error) {
	return m.read([]byte{60, 2, qAll, 60, 3, qAll, 60, 4, qAll, 60, 1, qAll})
}

// Events reads classes 1 to 3.
func (m *Master) Events() (Response, []Measurement, error) {
	return m.read([]byte{60, 2, qAll, 60, 3, qAll, 60, 4, qAll})
}

func (m *Master) read(objects []byte) (Response, []Measurement, error) {
	r, err := m.Request(FuncRead, objects)
	if err != nil {
		return r, nil, err
	}
	return r, Measurements(r.Headers), nil
}

// ClearRestart clears the outstation's restart indication.
func (m *Master) ClearRestart() error {
	_, err := m.Request(FuncWrite, []byte{80, 1, qStartStop8, 7, 7, 0})
	return err
}

// Operate commands a binary output (CROB, latch on or off) or an analog
// output (single precision): directly, or select then operate.
func (m *Master) Operate(kind Kind, index uint16, v Value, selectFirst bool) (uint8, error) {
	var obj []byte
	switch kind {
	case BinaryOutput:
		code := uint8(OpLatchOff)
		if v.Bool {
			code = OpLatchOn
		}
		obj = append([]byte{12, 1, qIndex16, 1, 0}, binary.LittleEndian.AppendUint16(nil, index)...)
		obj = append(obj, code, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0)
	case AnalogOutput:
		obj = append([]byte{41, 3, qIndex16, 1, 0}, binary.LittleEndian.AppendUint16(nil, index)...)
		obj = append(append(obj, f32(v.Number)...), 0)
	default:
		return 0, errors.New("dnp3: only outputs are commanded")
	}
	fc := uint8(FuncDirectOperate)
	if selectFirst {
		r, err := m.Request(FuncSelect, obj)
		if err != nil {
			return 0, err
		}
		if st := status(r); st != StatusSuccess {
			return st, nil
		}
		fc = FuncOperate
	}
	r, err := m.Request(fc, obj)
	if err != nil {
		return 0, err
	}
	return status(r), nil
}

func status(r Response) uint8 {
	for _, h := range r.Headers {
		for _, o := range h.Objects {
			return o[len(o)-1]
		}
	}
	return StatusFormatError
}

// Measurements reads the values a response's headers carry.
func Measurements(hs []Header) []Measurement {
	var out []Measurement
	for _, h := range hs {
		for i, o := range h.Objects {
			ms := Measurement{Group: h.Group, Variation: h.Variation, Index: uint16(h.Indexes[i])}
			switch [2]uint8{h.Group, h.Variation} {
			case [2]uint8{1, 2}, [2]uint8{10, 2}:
				ms.Online, ms.Bool = o[0]&flagOnline != 0, o[0]&flagState != 0
			case [2]uint8{2, 2}:
				ms.Online, ms.Bool, ms.Time = o[0]&flagOnline != 0, o[0]&flagState != 0, Time48(o[1:7])
			case [2]uint8{30, 1}, [2]uint8{40, 1}:
				ms.Online, ms.Number = o[0]&flagOnline != 0, float64(int32(binary.LittleEndian.Uint32(o[1:5])))
			case [2]uint8{30, 5}, [2]uint8{40, 3}:
				ms.Online, ms.Number = o[0]&flagOnline != 0, float64(math.Float32frombits(binary.LittleEndian.Uint32(o[1:5])))
			case [2]uint8{32, 3}:
				ms.Online, ms.Number, ms.Time = o[0]&flagOnline != 0, float64(int32(binary.LittleEndian.Uint32(o[1:5]))), Time48(o[5:11])
			case [2]uint8{32, 7}:
				ms.Online, ms.Number, ms.Time = o[0]&flagOnline != 0, float64(math.Float32frombits(binary.LittleEndian.Uint32(o[1:5]))), Time48(o[5:11])
			case [2]uint8{12, 1}, [2]uint8{41, 1}, [2]uint8{41, 3}:
				ms.Status = o[len(o)-1]
			default:
				continue
			}
			out = append(out, ms)
		}
	}
	return out
}

// String is a measurement for messages.
func (ms Measurement) String() string {
	return fmt.Sprintf("g%dv%d[%d]=%v/%v", ms.Group, ms.Variation, ms.Index, ms.Bool, ms.Number)
}
