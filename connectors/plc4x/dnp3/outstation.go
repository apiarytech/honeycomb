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
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"net"
	"slices"
	"sort"
	"sync"
	"time"
)

// Kind is what a point is.
type Kind int

// Point kinds and the objects they are reported with.
const (
	BinaryInput  Kind = iota // g1v2, events g2v2
	AnalogInput              // single precision: g30v5, events g32v7
	AnalogInt                // 32-bit: g30v1, events g32v3
	BinaryOutput             // status g10v2, commanded with g12v1
	AnalogOutput             // status g40v3, commanded with g41v1 or g41v3
)

// Point is one point of the outstation.
type Point struct {
	Kind  Kind
	Index uint16
	// Class is the event class of an input, 1 to 3; 0 reports no events.
	Class uint8
	// Writable: an output a master may command.
	Writable bool
}

// Value is a point's value as the outstation reads it.
type Value struct {
	Bool   bool
	Number float64
	Online bool
	Time   time.Time // the value's own time, for its events
}

// Source is what the outstation reports and commands: the points' tags.
type Source interface {
	// Read returns a point's value; ok is false when it has none.
	Read(p Point) (v Value, ok bool)
	// Operate commands an output; it returns a command status.
	Operate(p Point, v Value, by string) uint8
}

// Config is an outstation.
type Config struct {
	// Address is the outstation's link address; Master the master's, 0 to
	// answer any.
	Address, Master uint16
	Points          []Point
	Source          Source
	// Events is how many events are kept for the master (1000 when zero).
	Events int
	// SelectTimeout bounds select-before-operate (10 s when zero).
	SelectTimeout time.Duration
	// OnError, when set, receives what the outstation refused.
	OnError func(error)
}

// event is one change of an input.
type event struct {
	seq   uint64
	point Point
	value Value
}

// Outstation answers masters on its points.
type Outstation struct {
	cfg    Config
	byKind map[Kind][]Point

	mu       sync.Mutex
	last     map[Point]Value
	events   []event
	next     uint64
	overflow bool
	restart  bool // IIN1.7 until a master clears it
}

// New checks the points and returns an outstation.
func New(cfg Config) (*Outstation, error) {
	if cfg.Source == nil {
		return nil, errors.New("dnp3: no source")
	}
	if cfg.Events == 0 {
		cfg.Events = 1000
	}
	if cfg.SelectTimeout == 0 {
		cfg.SelectTimeout = 10 * time.Second
	}
	o := &Outstation{cfg: cfg, byKind: map[Kind][]Point{}, last: map[Point]Value{}, next: 1, restart: true}
	seen := map[[2]int]bool{}
	for _, p := range cfg.Points {
		k := [2]int{int(p.Kind), int(p.Index)}
		if seen[k] {
			return nil, errors.New("dnp3: two points of one kind at one index")
		}
		seen[k] = true
		if p.Class > 3 {
			return nil, errors.New("dnp3: event class 0 to 3")
		}
		o.byKind[p.Kind] = append(o.byKind[p.Kind], p)
	}
	for k := range o.byKind {
		sort.Slice(o.byKind[k], func(i, j int) bool { return o.byKind[k][i].Index < o.byKind[k][j].Index })
	}
	return o, nil
}

// Scan records the inputs' changes as events; call it periodically. The
// first scan only notes the values.
func (o *Outstation) Scan() {
	for _, k := range []Kind{BinaryInput, AnalogInput, AnalogInt} {
		for _, p := range o.byKind[k] {
			v, ok := o.cfg.Source.Read(p)
			if !ok {
				continue
			}
			o.mu.Lock()
			prev, seen := o.last[p]
			o.last[p] = v
			changed := seen && (prev.Bool != v.Bool || prev.Number != v.Number || prev.Online != v.Online)
			if changed && p.Class > 0 {
				o.events = append(o.events, event{seq: o.next, point: p, value: v})
				o.next++
				if over := len(o.events) - o.cfg.Events; over > 0 {
					o.events = append(o.events[:0:0], o.events[over:]...)
					o.overflow = true
				}
			}
			o.mu.Unlock()
		}
	}
}

// Pending is the events waiting for a master's confirmation.
func (o *Outstation) Pending() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.events)
}

func (o *Outstation) iin() uint16 {
	o.mu.Lock()
	defer o.mu.Unlock()
	var iin uint16
	if o.restart {
		iin |= IINDeviceRestart
	}
	if o.overflow {
		iin |= IINEventOverflow
	}
	for _, e := range o.events {
		iin |= IINClass1Events << (e.point.Class - 1)
	}
	return iin
}

// Serve answers one master on conn until it closes or ctx ends. commands
// is false for a master that may only read (a read-only interface, a
// certificate without the operator role); by names it in the record.
func (o *Outstation) Serve(ctx context.Context, conn net.Conn, commands bool, by string) error {
	s := &session{o: o, conn: conn, commands: commands, by: by, confirmSeq: -1}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	defer conn.Close()
	for {
		conn.SetReadDeadline(time.Now().Add(5 * time.Minute))
		f, err := ReadFrame(conn)
		if errors.Is(err, ErrCRC) || errors.Is(err, ErrLength) {
			continue // dropped; resynchronise on the next frame
		}
		if err != nil {
			return err
		}
		if err := s.frame(f); err != nil {
			return err
		}
	}
}

// session is one master's connection.
type session struct {
	o        *Outstation
	conn     net.Conn
	commands bool
	by       string

	master uint16
	tseq   uint8
	in     reassembler

	confirmSeq int    // the response awaiting a confirmation, -1 for none
	sentUpTo   uint64 // the last event in it
	selected   []byte // the objects of the last select
	selSeq     uint8
	selAt      time.Time
}

func (s *session) frame(f Frame) error {
	if !f.Dir || !f.Prm {
		return nil // not from a master
	}
	if f.Dest != s.o.cfg.Address && f.Dest < 0xFFFD {
		return nil // another outstation's
	}
	if s.o.cfg.Master != 0 && f.Src != s.o.cfg.Master {
		return nil
	}
	s.master = f.Src
	broadcast := f.Dest >= 0xFFFD
	switch f.Func {
	case LinkRequestStatus:
		return s.link(LinkStatus)
	case LinkResetLinkStates, LinkTestLinkStates:
		return s.link(LinkAck)
	case LinkConfirmedData:
		if err := s.link(LinkAck); err != nil {
			return err
		}
		fallthrough
	case LinkUnconfirmedData:
		if frag, ok := s.in.add(f.Data); ok {
			return s.request(frag, broadcast)
		}
	}
	return nil
}

// link answers a link frame.
func (s *session) link(fn uint8) error {
	b, _ := Frame{Func: fn, Dest: s.master, Src: s.o.cfg.Address}.Marshal()
	_, err := s.conn.Write(b)
	return err
}

// send sends an application fragment in transport segments.
func (s *session) send(fragment []byte) error {
	segs, next := segments(fragment, s.tseq)
	s.tseq = next
	for _, seg := range segs {
		b, err := Frame{Prm: true, Func: LinkUnconfirmedData, Dest: s.master, Src: s.o.cfg.Address, Data: seg}.Marshal()
		if err != nil {
			return err
		}
		s.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if _, err := s.conn.Write(b); err != nil {
			return err
		}
	}
	return nil
}

// request answers one application fragment.
func (s *session) request(frag []byte, broadcast bool) error {
	if len(frag) < 2 {
		return nil
	}
	ac, fc := frag[0], frag[1]
	seq := ac & 0x0F
	if fc == FuncConfirm {
		if s.confirmSeq == int(seq) {
			s.o.release(s.sentUpTo)
			s.confirmSeq = -1
		}
		return nil
	}
	var objs bytes.Buffer
	var iin uint16
	con := false
	switch fc {
	case FuncRead:
		hs, err := ParseHeaders(frag[2:], false)
		if err != nil {
			iin |= IINParameterError
			break
		}
		var upTo uint64
		for _, h := range hs {
			n, flags, last := s.read(&objs, h)
			iin |= flags
			if n > 0 && last > upTo {
				upTo, con = last, true
			}
		}
		if con {
			s.confirmSeq, s.sentUpTo = int(seq), upTo
		}
	case FuncWrite:
		iin |= s.write(frag[2:])
	case FuncSelect, FuncOperate, FuncDirectOperate, FuncDirectOperateNR:
		echo, flags := s.control(fc, seq, frag[2:])
		iin |= flags
		if fc == FuncDirectOperateNR {
			return nil
		}
		objs.Write(echo)
	default:
		iin |= IINNoFuncSupport
	}
	if broadcast {
		return nil
	}
	resp := []byte{acFIR | acFIN | seq, FuncResponse, 0, 0}
	if con {
		resp[0] |= acCON
	}
	binary.LittleEndian.PutUint16(resp[2:], s.o.iin()|iin|IINAllStationsIf(broadcast))
	return s.send(append(resp, objs.Bytes()...))
}

// IINAllStationsIf is IIN1.0 for a request sent to all stations.
func IINAllStationsIf(b bool) uint16 {
	if b {
		return IINAllStations
	}
	return 0
}

// read writes the objects one read header asks for; n counts the events
// written, last is the newest.
func (s *session) read(w *bytes.Buffer, hdr Header) (n int, iin uint16, last uint64) {
	h := &hdr
	switch [2]uint8{h.Group, h.Variation} {
	case [2]uint8{60, 1}: // class 0: every static point
		for _, k := range []Kind{BinaryInput, AnalogInput, AnalogInt, BinaryOutput, AnalogOutput} {
			s.static(w, k, nil)
		}
		return 0, 0, 0
	case [2]uint8{60, 2}, [2]uint8{60, 3}, [2]uint8{60, 4}:
		return s.events(w, h.Variation-1)
	case [2]uint8{1, 0}, [2]uint8{1, 2}:
		s.static(w, BinaryInput, h)
	case [2]uint8{30, 0}:
		s.static(w, AnalogInput, h)
		s.static(w, AnalogInt, h)
	case [2]uint8{30, 5}:
		s.static(w, AnalogInput, h)
	case [2]uint8{30, 1}:
		s.static(w, AnalogInt, h)
	case [2]uint8{10, 0}, [2]uint8{10, 2}:
		s.static(w, BinaryOutput, h)
	case [2]uint8{40, 0}, [2]uint8{40, 3}:
		s.static(w, AnalogOutput, h)
	default:
		return 0, IINObjectUnknown, 0
	}
	return 0, 0, 0
}

// static writes the current values of a kind's points (those h names, or
// all), in ranges of consecutive indexes.
func (s *session) static(w *bytes.Buffer, k Kind, h *Header) {
	var pts []Point
	for _, p := range s.o.byKind[k] {
		if h == nil || h.All || slices.Contains(h.Indexes, uint32(p.Index)) {
			pts = append(pts, p)
		}
	}
	gv := staticObject[k]
	group, variation := gv[0], gv[1]
	for i := 0; i < len(pts); {
		j := i
		for j+1 < len(pts) && pts[j+1].Index == pts[j].Index+1 {
			j++
		}
		w.Write([]byte{group, variation, qStartStop16})
		binary.Write(w, binary.LittleEndian, pts[i].Index)
		binary.Write(w, binary.LittleEndian, pts[j].Index)
		for _, p := range pts[i : j+1] {
			v, ok := s.o.cfg.Source.Read(p)
			w.Write(encodeStatic(k, v, ok))
		}
		i = j + 1
	}
}

// staticObject is the object a kind's current value is reported with.
var staticObject = map[Kind][2]uint8{BinaryInput: {1, 2}, AnalogInput: {30, 5}, AnalogInt: {30, 1}, BinaryOutput: {10, 2}, AnalogOutput: {40, 3}}

func flags(v Value, ok bool) byte {
	var f byte
	if ok && v.Online {
		f |= flagOnline
	}
	return f
}

func encodeStatic(k Kind, v Value, ok bool) []byte {
	f := flags(v, ok)
	switch k {
	case BinaryInput, BinaryOutput:
		if v.Bool {
			f |= flagState
		}
		return []byte{f}
	case AnalogInt:
		return append([]byte{f}, i32(v.Number)...)
	default:
		return append([]byte{f}, f32(v.Number)...)
	}
}

// maxEventOctets keeps a response within one fragment.
const maxEventOctets = maxFragment - 64

// events writes the waiting events of a class (0 for every class), oldest
// first, as many as fit in the fragment.
func (s *session) events(w *bytes.Buffer, class uint8) (n int, iin uint16, last uint64) {
	s.o.mu.Lock()
	evs := slices.Clone(s.o.events)
	s.o.mu.Unlock()
	for _, e := range evs {
		if class != 0 && e.point.Class != class {
			continue
		}
		var obj []byte
		switch e.point.Kind {
		case BinaryInput:
			f := flags(e.value, true)
			if e.value.Bool {
				f |= flagState
			}
			obj = append([]byte{2, 2, qIndex16, 1, 0}, binary.LittleEndian.AppendUint16(nil, e.point.Index)...)
			obj = append(append(obj, f), time48(e.value.Time)...)
		case AnalogInput:
			obj = append([]byte{32, 7, qIndex16, 1, 0}, binary.LittleEndian.AppendUint16(nil, e.point.Index)...)
			obj = append(append(append(obj, flags(e.value, true)), f32(e.value.Number)...), time48(e.value.Time)...)
		case AnalogInt:
			obj = append([]byte{32, 3, qIndex16, 1, 0}, binary.LittleEndian.AppendUint16(nil, e.point.Index)...)
			obj = append(append(append(obj, flags(e.value, true)), i32(e.value.Number)...), time48(e.value.Time)...)
		default:
			continue
		}
		if w.Len()+len(obj) > maxEventOctets {
			break // the rest at the next read; the class bits say so
		}
		w.Write(obj)
		n++
		last = e.seq
	}
	return n, 0, last
}

// release drops the events a master confirmed.
func (o *Outstation) release(upTo uint64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	keep := o.events[:0]
	for _, e := range o.events {
		if e.seq > upTo {
			keep = append(keep, e)
		}
	}
	o.events = keep
	o.overflow = false
}

// write takes a WRITE: clearing the restart indication (g80v1 index 7)
// and time (g50v1), which is noted but never sets the machine's clock: the
// unit keeps its time from its own time service.
func (s *session) write(b []byte) uint16 {
	hs, err := ParseHeaders(b, true)
	if err != nil {
		return IINParameterError
	}
	var iin uint16
	for _, h := range hs {
		switch [2]uint8{h.Group, h.Variation} {
		case [2]uint8{80, 1}:
			for i, idx := range h.Indexes {
				if i < len(h.Objects) && idx == 7 && h.Objects[i][0] == 0 {
					s.o.mu.Lock()
					s.o.restart = false
					s.o.mu.Unlock()
				}
			}
		case [2]uint8{50, 1}:
			// noted, not applied
		default:
			iin |= IINObjectUnknown
		}
	}
	return iin
}

// control answers SELECT, OPERATE and DIRECT OPERATE: it echoes the
// objects with each one's status.
func (s *session) control(fc, seq uint8, b []byte) ([]byte, uint16) {
	hs, err := ParseHeaders(b, true)
	if err != nil {
		return nil, IINParameterError
	}
	selectOK := true
	if fc == FuncOperate {
		selectOK = s.selected != nil && bytes.Equal(s.selected, b) && seq == (s.selSeq+1)&0x0F && time.Since(s.selAt) <= s.o.cfg.SelectTimeout
		s.selected = nil
	}
	var echo bytes.Buffer
	for _, h := range hs {
		echo.Write([]byte{h.Group, h.Variation, h.Qualifier})
		count := make([]byte, 0, 2)
		switch h.Qualifier {
		case qIndex8, qCount8:
			count = append(count, byte(len(h.Indexes)))
		case qIndex16, qCount16:
			count = binary.LittleEndian.AppendUint16(count, uint16(len(h.Indexes)))
		default:
			return nil, IINParameterError
		}
		echo.Write(count)
		for i, idx := range h.Indexes {
			obj := slices.Clone(h.Objects[i])
			status := s.command(fc, h, uint16(idx), obj, selectOK)
			obj[len(obj)-1] = status
			switch h.Qualifier {
			case qIndex8:
				echo.WriteByte(byte(idx))
			case qIndex16:
				binary.Write(&echo, binary.LittleEndian, uint16(idx))
			}
			echo.Write(obj)
		}
	}
	if fc == FuncSelect {
		s.selected, s.selSeq, s.selAt = slices.Clone(b), seq, time.Now()
	}
	return echo.Bytes(), 0
}

// command checks and, but for a select, carries out one control.
func (s *session) command(fc uint8, h Header, idx uint16, obj []byte, selectOK bool) uint8 {
	var p Point
	var v Value
	switch [2]uint8{h.Group, h.Variation} {
	case [2]uint8{12, 1}:
		on, ok := CROB(obj[0])
		if !ok {
			return StatusNotSupported
		}
		p, v = Point{Kind: BinaryOutput, Index: idx}, Value{Bool: on, Online: true, Time: time.Now()}
	case [2]uint8{41, 1}:
		p, v = Point{Kind: AnalogOutput, Index: idx}, Value{Number: float64(int32(binary.LittleEndian.Uint32(obj[:4]))), Online: true, Time: time.Now()}
	case [2]uint8{41, 3}:
		p, v = Point{Kind: AnalogOutput, Index: idx}, Value{Number: float64(math.Float32frombits(binary.LittleEndian.Uint32(obj[:4]))), Online: true, Time: time.Now()}
	default:
		return StatusNotSupported
	}
	var found *Point
	for i := range s.o.byKind[p.Kind] {
		if s.o.byKind[p.Kind][i].Index == idx {
			found = &s.o.byKind[p.Kind][i]
		}
	}
	switch {
	case found == nil || !found.Writable:
		return StatusNotSupported
	case !s.commands:
		return StatusNotAuthorized
	case !selectOK:
		return StatusNoSelect
	case fc == FuncSelect:
		return StatusSuccess
	}
	return s.o.cfg.Source.Operate(*found, v, s.by)
}
