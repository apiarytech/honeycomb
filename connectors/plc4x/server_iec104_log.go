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

// Store and forward in the IEC 60870-5-104 outstation (beehive R-142). With
// buffer=N on the URL the outstation keeps every change of its points, with
// its time tag, in a log of up to N events, whether a controlling station
// is connected or not. A station that starts data transfer receives the
// events no station has acknowledged yet, in order and with their original
// times, then the current values; from then on, each new event as it
// comes. An event leaves the log only when a station acknowledges the
// frame that carried it (its receive sequence number), so a link that
// drops in the middle loses nothing: the next station gets it again.
//
// Past N events the oldest are dropped and counted (Status.Dropped... in
// the connector's error report): the station should interrogate after a
// long outage. Buffered events always carry time tags.

import (
	"fmt"
	"sync"

	iec "github.com/apache/plc4x/plc4go/protocols/iec608705104/readwrite/model"
)

// iecEvent is one change, ready to send.
type iecEvent struct {
	seq    uint64
	ca     uint16
	typeID iec.TypeIdentification
	obj    iec.InformationObject
}

// iecLog is the outstation's event log, shared by its stations.
type iecLog struct {
	size int

	mu      sync.Mutex
	events  []iecEvent // in order, oldest first
	next    uint64     // the next event's sequence number, from 1
	acked   uint64     // every event up to this one was acknowledged
	dropped uint64
	last    map[string]string // the last value recorded per point
}

func newIECLog(size int) *iecLog {
	return &iecLog{size: size, next: 1, last: map[string]string{}}
}

// record takes the points' changes since the last call. The first call
// only notes the values: there is no change before it.
func (c *Connector) recordIEC(s *iecServer) {
	l := s.log
	for i := range s.points {
		p := &s.points[i]
		_, fp, ok := c.iecValue(p)
		if !ok {
			continue
		}
		l.mu.Lock()
		prev, seen := l.last[fp.key]
		l.last[fp.key] = fp.value
		l.mu.Unlock()
		if !seen || prev == fp.value {
			continue
		}
		obj, ok := c.iecTimedValue(p)
		if !ok {
			continue
		}
		l.add(p.ca, p.kind.timed, obj)
	}
}

func (l *iecLog) add(ca uint16, typeID iec.TypeIdentification, obj iec.InformationObject) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, iecEvent{seq: l.next, ca: ca, typeID: typeID, obj: obj})
	l.next++
	if over := len(l.events) - l.size; over > 0 {
		l.dropped += uint64(over)
		if last := l.events[over-1].seq; last > l.acked {
			l.acked = last // dropped: no station will get them
		}
		l.events = append(l.events[:0:0], l.events[over:]...)
	}
}

// after returns the events after seq, oldest first, up to max.
func (l *iecLog) after(seq uint64, max int) []iecEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []iecEvent
	for _, e := range l.events {
		if e.seq > seq {
			out = append(out, e)
			if len(out) == max {
				break
			}
		}
	}
	return out
}

// ack records that a station received every event up to seq: they leave
// the log.
func (l *iecLog) ack(seq uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if seq <= l.acked {
		return
	}
	l.acked = seq
	i := 0
	for i < len(l.events) && l.events[i].seq <= seq {
		i++
	}
	l.events = append(l.events[:0:0], l.events[i:]...)
}

// position is where a new station starts: after the acknowledged events.
func (l *iecLog) position() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.acked
}

// Len is the events waiting for a station, and those dropped.
func (l *iecLog) Len() (waiting int, dropped uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.events), l.dropped
}

// pendingFrame is a sent frame that carried a logged event.
type pendingFrame struct {
	frame uint16 // the I-frame's send sequence number
	event uint64
}

// sendLog sends the session's next logged events within the send window;
// once the backlog is sent it sends the current values, once.
func (s *iecSession) sendLog() {
	for _, e := range s.s.log.after(s.cursor, int(s.s.k)) {
		frame, ok := s.sendASDUSeq(iec.NewASDU(e.typeID, false, false, false, cotSpontaneous, 0, e.ca, []iec.InformationObject{e.obj}))
		if !ok {
			return // the window is full: the rest next scan
		}
		s.mu.Lock()
		s.pending = append(s.pending, pendingFrame{frame: frame, event: e.seq})
		s.cursor = e.seq
		s.mu.Unlock()
	}
	s.mu.Lock()
	current := s.needCurrent
	s.needCurrent = false
	s.mu.Unlock()
	if current {
		s.sendAll(cotSpontaneous)
	}
}

// ackLog takes the station's receive sequence number: the frames before it
// arrived, and the events they carried leave the log.
func (s *iecSession) ackLog(recvSeqNo uint16) {
	s.mu.Lock()
	var upTo uint64
	keep := s.pending[:0]
	for _, p := range s.pending {
		if d := (recvSeqNo - p.frame) & 0x7FFF; d >= 1 && d < 0x4000 {
			upTo = max(upTo, p.event)
			continue
		}
		keep = append(keep, p)
	}
	s.pending = keep
	s.mu.Unlock()
	if upTo > 0 {
		s.s.log.ack(upTo)
	}
}

// parseBuffer reads buffer=N: 0 (none) to 100000 events.
func parseBuffer(v string) (int, error) {
	if v == "" {
		return 0, nil
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil || n < 0 || n > 100000 || fmt.Sprint(n) != v {
		return 0, fmt.Errorf("buffer %q: the events kept for the controlling station, 0 to 100000", v)
	}
	return n, nil
}
