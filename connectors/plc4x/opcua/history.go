/*
 * Copyright (C) 2026 Franklin D. Amador
 *
 * This software is dual-licensed under the terms of the GPL v3.0 and
 * a commercial license. You may choose to use this software under either
 * license.
 *
 * See the LICENSE files in the project root for full license text.
 */

package opcua

import (
	"bytes"
	"sort"
	"sync"
	"time"

	ua "github.com/apache/plc4x/plc4go/protocols/opcua/readwrite/model"
)

// History status codes.
const (
	statusGoodNoData                     = 0x00A50000
	statusBadHistoryOperationUnsupported = 0x80720000
	statusBadHistoryOperationInvalid     = 0x80710000
)

// historyStore keeps the values of the points that historize, and the events, in memory:
// the last cfg.Depth values per point and cfg.Events events, oldest first.
type historyStore struct {
	cfg    History
	mutex  sync.Mutex
	values map[string][]historyValue // by node key
	events []*event
}

type historyValue struct {
	sample
	raw []byte // the encoded value, to keep only changes
}

func newHistoryStore(cfg History) *historyStore {
	return &historyStore{cfg: cfg, values: map[string][]historyValue{}}
}

// record keeps a sample of a point, when its value or status changed.
func (h *historyStore) record(key string, got sample) {
	if h.cfg.Depth <= 0 {
		return
	}
	var raw []byte
	if got.variant != nil {
		raw, _ = got.variant.Serialize()
	}
	if got.time.IsZero() {
		got.time = time.Now()
	}
	h.mutex.Lock()
	defer h.mutex.Unlock()
	list := h.values[key]
	if n := len(list); n > 0 && list[n-1].status == got.status && bytes.Equal(list[n-1].raw, raw) {
		return
	}
	if n := len(list); n > 0 && got.time.Before(list[n-1].time) {
		got.time = list[n-1].time // keep the list in time order
	}
	list = append(list, historyValue{sample: got, raw: raw})
	if len(list) > h.cfg.Depth {
		list = list[len(list)-h.cfg.Depth:]
	}
	h.values[key] = list
}

func (h *historyStore) addEvent(e *event) {
	if h.cfg.Events <= 0 {
		return
	}
	h.mutex.Lock()
	defer h.mutex.Unlock()
	h.events = append(h.events, e)
	if len(h.events) > h.cfg.Events {
		h.events = h.events[len(h.events)-h.cfg.Events:]
	}
}

// window is the indexes of the times between start and end (either may be zero), in the
// order the request asks for: backward when end is before start.
func window(n int, at func(int) time.Time, start, end time.Time) (from, to int, backward bool) {
	backward = !start.IsZero() && !end.IsZero() && end.Before(start) || start.IsZero() && !end.IsZero()
	lo, hi := start, end
	if backward {
		lo, hi = end, start
	}
	from = 0
	if !lo.IsZero() {
		from = sort.Search(n, func(i int) bool { return !at(i).Before(lo) })
	}
	to = n
	if !hi.IsZero() {
		to = sort.Search(n, func(i int) bool { return at(i).After(hi) })
	}
	if to < from {
		to = from
	}
	return from, to, backward
}

// historyCursor is where a HistoryRead left off, for its continuation point.
type historyCursor struct {
	key       string
	events    bool
	start     time.Time
	end       time.Time
	skip      int // values already returned
	per       int
	bounds    bool
	timestamp ua.TimestampsToReturn
	filter    *eventFilter
}

func (c *conn) historyRead(s *session, request ua.HistoryReadRequest) ua.ExtensionObjectDefinition {
	header := request.GetRequestHeader()
	nodes := request.GetNodesToRead()
	if status := tooMany(len(nodes)); status != statusGood {
		return c.fault(header, status)
	}
	var details ua.ExtensionObjectDefinition
	if d := request.GetHistoryReadDetails(); d != nil {
		details = d.GetBody()
	}
	timestamps := request.GetTimestampsToReturn()
	if timestamps > ua.TimestampsToReturn_timestampsToReturnNeither {
		return c.fault(header, statusBadTimestampsToReturnInvalid)
	}
	results := make([]ua.HistoryReadResult, len(nodes))
	for i, r := range nodes {
		results[i] = c.historyReadNode(s, details, timestamps, request.GetReleaseContinuationPoints(), r)
	}
	return ua.NewHistoryReadResponse(c.responseHeader(header, statusGood), results, nil)
}

func historyResult(status uint32, point []byte, data ua.ExtensionObjectDefinition, encodingId uint32) ua.HistoryReadResult {
	object := nullExtensionObject()
	if data != nil {
		object = extensionObject(data, encodingId)
	}
	return ua.NewHistoryReadResult(ua.NewStatusCode(status), byteString(point), object)
}

func (c *conn) historyReadNode(s *session, details ua.ExtensionObjectDefinition, timestamps ua.TimestampsToReturn, release bool, r ua.HistoryReadValueId) ua.HistoryReadResult {
	if point := bytesOf(r.GetContinuationPoint()); point != nil {
		s.mutex.Lock()
		cursor, ok := s.histories[string(point)]
		delete(s.histories, string(point))
		s.mutex.Unlock()
		switch {
		case !ok:
			return historyResult(statusBadContinuationPointInvalid, nil, nil, 0)
		case release:
			return historyResult(statusGood, nil, nil, 0)
		}
		return c.historyPage(s, cursor)
	}
	n := c.s.space.nodes[keyOf(r.GetNodeId())]
	if n == nil {
		return historyResult(statusBadNodeIdUnknown, nil, nil, 0)
	}
	switch d := details.(type) {
	case ua.ReadRawModifiedDetails:
		if d.GetIsReadModified() {
			return historyResult(statusBadHistoryOperationUnsupported, nil, nil, 0)
		}
		if n.accessLevel(true, c.s.history.cfg.Depth > 0)&4 == 0 {
			return historyResult(statusBadHistoryOperationUnsupported, nil, nil, 0)
		}
		if stringOf(r.GetIndexRange()) != "" {
			return historyResult(statusBadIndexRangeInvalid, nil, nil, 0)
		}
		start, end := timeOf(d.GetStartTime()), timeOf(d.GetEndTime())
		// Two of start, end and a count (Part 11, 6.4.3).
		if start.IsZero() && end.IsZero() || (start.IsZero() || end.IsZero()) && d.GetNumValuesPerNode() == 0 {
			return historyResult(statusBadHistoryOperationInvalid, nil, nil, 0)
		}
		return c.historyPage(s, historyCursor{key: n.key, start: start, end: end, per: int(d.GetNumValuesPerNode()),
			bounds: d.GetReturnBounds(), timestamp: timestamps})
	case ua.ReadEventDetails:
		if n.eventNotifier&notifierHistoryRead == 0 {
			return historyResult(statusBadHistoryOperationUnsupported, nil, nil, 0)
		}
		filter, _, status := newEventFilter(d.GetFilter())
		if status != statusGood {
			return historyResult(status, nil, nil, 0)
		}
		start, end := timeOf(d.GetStartTime()), timeOf(d.GetEndTime())
		if start.IsZero() && end.IsZero() || (start.IsZero() || end.IsZero()) && d.GetNumValuesPerNode() == 0 {
			return historyResult(statusBadHistoryOperationInvalid, nil, nil, 0)
		}
		return c.historyPage(s, historyCursor{key: n.key, events: true, start: start, end: end, per: int(d.GetNumValuesPerNode()), filter: filter})
	}
	return historyResult(statusBadHistoryOperationUnsupported, nil, nil, 0)
}

// historyPage reads the next page of a history read, keeping a continuation point when more
// is left than the page takes.
func (c *conn) historyPage(s *session, cursor historyCursor) ua.HistoryReadResult {
	h := c.s.history
	h.mutex.Lock()
	var selected []int
	var values []historyValue
	var events []*event
	if cursor.events {
		events = append(events, h.events...)
	} else {
		values = append(values, h.values[cursor.key]...)
	}
	h.mutex.Unlock()

	at := func(i int) time.Time {
		if cursor.events {
			return events[i].time
		}
		return values[i].time
	}
	n := len(values)
	if cursor.events {
		n = len(events)
	}
	from, to, backward := window(n, at, cursor.start, cursor.end)
	if cursor.bounds && !cursor.events {
		if from > 0 {
			from--
		}
		if to < n {
			to++
		}
	}
	for i := from; i < to; i++ {
		if cursor.events && (!eventReported(events[i], cursor.key) || !cursor.filter.matches(events[i])) {
			continue
		}
		selected = append(selected, i)
	}
	if backward {
		for i, j := 0, len(selected)-1; i < j; i, j = i+1, j-1 {
			selected[i], selected[j] = selected[j], selected[i]
		}
	}
	if cursor.skip > len(selected) {
		cursor.skip = len(selected)
	}
	selected = selected[cursor.skip:]
	var point []byte
	if cursor.per > 0 && len(selected) > cursor.per {
		selected = selected[:cursor.per]
		cursor.skip += cursor.per
		s.mutex.Lock()
		if len(s.histories) < maxContinuationPoints {
			point = randomBytes(16)
			s.histories[string(point)] = cursor
		}
		s.mutex.Unlock()
		if point == nil {
			return historyResult(statusBadNoContinuationPoints, nil, nil, 0)
		}
	}
	status := uint32(statusGood)
	if len(selected) == 0 {
		status = statusGoodNoData
	}
	if cursor.events {
		lists := make([]ua.HistoryEventFieldList, len(selected))
		for i, index := range selected {
			lists[i] = ua.NewHistoryEventFieldList(cursor.filter.fields(events[index]))
		}
		return historyResult(status, point, ua.NewHistoryEvent(lists), 661)
	}
	data := make([]ua.DataValue, len(selected))
	for i, index := range selected {
		v := values[index]
		if v.variant == nil {
			data[i] = dataValue(nil, v.status, v.time, cursor.timestamp)
			continue
		}
		data[i] = dataValue(v.variant, v.status, v.time, sourceTimestamps(cursor.timestamp))
	}
	return historyResult(status, point, ua.NewHistoryData(data), 658)
}

// sourceTimestamps makes sure a history value carries its source time: it is what orders it.
func sourceTimestamps(t ua.TimestampsToReturn) ua.TimestampsToReturn {
	switch t {
	case ua.TimestampsToReturn_timestampsToReturnServer:
		return ua.TimestampsToReturn_timestampsToReturnBoth
	case ua.TimestampsToReturn_timestampsToReturnNeither:
		return ua.TimestampsToReturn_timestampsToReturnSource
	}
	return t
}

func eventReported(e *event, key string) bool {
	for _, n := range e.notifiers {
		if n == key {
			return true
		}
	}
	return false
}
