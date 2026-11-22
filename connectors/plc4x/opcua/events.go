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
	"math"
	"strings"
	"sync"
	"time"

	ua "github.com/apache/plc4x/plc4go/protocols/opcua/readwrite/model"
)

// Event and condition status codes.
const (
	statusBadConditionAlreadyDisabled  = 0x80980000
	statusBadConditionAlreadyEnabled   = 0x80CC0000
	statusBadConditionBranchAlreadyAck = 0x80CF0000
	statusBadConditionBranchAlreadyCnf = 0x80D00000
	statusBadEventIdUnknown            = 0x809A0000
	statusBadConditionDisabled         = 0x80990000
	statusBadEventFilterInvalid        = 0x80470000
	statusBadFilterOperatorUnsupported = 0x80C20000
	statusBadFilterOperandInvalid      = 0x80490000
	statusBadContentFilterInvalid      = 0x80480000
)

// event is one event: its type, its fields by browse path ("EventId", "ActiveState/Id"),
// the condition it reports (nil for other events) and the nodes it is reported by.
type event struct {
	typeId      uint32
	fields      map[string]ua.Variant
	conditionId ua.NodeId
	notifiers   []string
	time        time.Time
}

// alarmState is the state of a condition.
type alarmState struct {
	enabled, active, acked, confirmed bool
	comment, user                     string
	eventId                           []byte
	time                              time.Time
}

// retain says a client should show the condition: it is active, or not yet acknowledged.
func (st alarmState) retain() bool { return st.enabled && (st.active || !st.acked) }

// alarm is an Alarm of the Config: an AlarmConditionType condition.
type alarm struct {
	s      *Server
	cfg    Alarm
	node   *node
	source *node

	mutex sync.Mutex
	st    alarmState
}

func (al *alarm) snapshot() alarmState {
	al.mutex.Lock()
	defer al.mutex.Unlock()
	return al.st
}

// changed makes the event of a state change, under al.mutex, with a new event id.
func (al *alarm) changed(message string) *event {
	al.st.eventId = randomBytes(16)
	al.st.time = time.Now()
	return al.event(message, al.st)
}

// event is the event reporting state st.
func (al *alarm) event(message string, st alarmState) *event {
	source, sourceName := al.node.id, al.cfg.Name
	if al.source != nil {
		source, sourceName = al.source.id, al.source.point.Name
	}
	twoState := func(name string, value bool, yes, no string) map[string]ua.Variant {
		label := no
		if value {
			label = yes
		}
		return map[string]ua.Variant{name: variantText(label), name + "/Id": variantBool(value)}
	}
	fields := map[string]ua.Variant{
		"EventId":            variantByteString(st.eventId),
		"EventType":          variantNodeId(numericNodeId(0, idAlarmConditionType)),
		"SourceNode":         variantNodeId(source),
		"SourceName":         variantString(sourceName),
		"Time":               variantDateTime(st.time),
		"ReceiveTime":        variantDateTime(st.time),
		"Message":            variantText(message),
		"Severity":           variantUInt16(al.cfg.Severity),
		"ConditionClassId":   variantNodeId(numericNodeId(0, idBaseConditionClassType)),
		"ConditionClassName": variantText("BaseConditionClass"),
		"ConditionName":      variantString(al.cfg.Name),
		"BranchId":           variantNodeId(nullNodeId()),
		"Retain":             variantBool(st.retain()),
		"Comment":            variantText(st.comment),
		"ClientUserId":       variantString(st.user),
		"LastSeverity":       variantUInt16(al.cfg.Severity),
		"Quality":            variantStatusCode(statusGood),
	}
	for _, m := range []map[string]ua.Variant{
		twoState("EnabledState", st.enabled, "Enabled", "Disabled"),
		twoState("ActiveState", st.active, "Active", "Inactive"),
		twoState("AckedState", st.acked, "Acknowledged", "Unacknowledged"),
		twoState("ConfirmedState", st.confirmed, "Confirmed", "Unconfirmed"),
	} {
		for k, v := range m {
			fields[k] = v
		}
	}
	return &event{typeId: idAlarmConditionType, fields: fields, conditionId: al.node.id,
		notifiers: []string{numericKey(idServer), al.node.key}, time: st.time}
}

// evaluate samples the alarm's condition and reports a change.
func (al *alarm) evaluate() {
	active := al.cfg.Active()
	al.mutex.Lock()
	if active == al.st.active {
		al.mutex.Unlock()
		return
	}
	al.st.active = active
	if !al.st.enabled {
		al.mutex.Unlock()
		return
	}
	var e *event
	if active {
		al.st.acked, al.st.confirmed = false, false
		e = al.changed(al.cfg.Message)
	} else {
		e = al.changed(al.cfg.Name + " returned to normal")
	}
	al.mutex.Unlock()
	al.s.emit(e, nil)
}

func (al *alarm) setEnabled(enabled bool, user string) uint32 {
	al.mutex.Lock()
	if al.st.enabled == enabled {
		al.mutex.Unlock()
		if enabled {
			return statusBadConditionAlreadyEnabled
		}
		return statusBadConditionAlreadyDisabled
	}
	al.st.enabled, al.st.user = enabled, user
	message := "Disabled"
	if enabled {
		message = "Enabled"
	}
	e := al.changed(message)
	al.mutex.Unlock()
	al.s.emit(e, nil)
	return statusGood
}

// act checks an acknowledgement, confirmation or comment against the condition's current
// event and applies it.
func (al *alarm) act(eventId []byte, comment, user, message string, apply func(st *alarmState) uint32) uint32 {
	al.mutex.Lock()
	switch {
	case !al.st.enabled:
		al.mutex.Unlock()
		return statusBadConditionDisabled
	case !bytes.Equal(eventId, al.st.eventId):
		al.mutex.Unlock()
		return statusBadEventIdUnknown
	}
	if status := apply(&al.st); status != statusGood {
		al.mutex.Unlock()
		return status
	}
	al.st.user = user
	if comment != "" {
		al.st.comment = comment
	}
	e := al.changed(message)
	al.mutex.Unlock()
	al.s.emit(e, nil)
	return statusGood
}

func (al *alarm) acknowledge(eventId []byte, comment, user string) uint32 {
	return al.act(eventId, comment, user, "Acknowledged", func(st *alarmState) uint32 {
		if st.acked {
			return statusBadConditionBranchAlreadyAck
		}
		st.acked = true
		return statusGood
	})
}

func (al *alarm) confirm(eventId []byte, comment, user string) uint32 {
	return al.act(eventId, comment, user, "Confirmed", func(st *alarmState) uint32 {
		if st.confirmed {
			return statusBadConditionBranchAlreadyCnf
		}
		st.confirmed = true
		return statusGood
	})
}

func (al *alarm) addComment(eventId []byte, comment, user string) uint32 {
	return al.act(eventId, comment, user, "Comment added", func(*alarmState) uint32 { return statusGood })
}

// systemEvent is a RefreshStart or RefreshEnd event, reported by the Server.
func (s *Server) systemEvent(typeId uint32) *event {
	now := time.Now()
	return &event{typeId: typeId, time: now, notifiers: []string{numericKey(idServer)}, fields: map[string]ua.Variant{
		"EventId":     variantByteString(randomBytes(16)),
		"EventType":   variantNodeId(numericNodeId(0, typeId)),
		"SourceNode":  variantNodeId(numericNodeId(0, idServer)),
		"SourceName":  variantString("Server"),
		"Time":        variantDateTime(now),
		"ReceiveTime": variantDateTime(now),
		"Message":     variantText(""),
		"Severity":    variantUInt16(100),
	}}
}

// refresh answers ConditionRefresh: the subscription's event items get RefreshStart, the
// current state of every condition a client should show, and RefreshEnd.
func (s *Server) refresh(sess *session, subscriptionId uint32) uint32 {
	sess.mutex.Lock()
	sub := sess.subscriptions[subscriptionId]
	sess.mutex.Unlock()
	if sub == nil {
		return statusBadSubscriptionIdInvalid
	}
	s.emit(s.systemEvent(idRefreshStartEventType), sub)
	for _, al := range s.alarms {
		st := al.snapshot()
		if st.retain() && st.eventId != nil {
			s.emit(al.event(al.cfg.Message, st), sub)
		}
	}
	s.emit(s.systemEvent(idRefreshEndEventType), sub)
	return statusGood
}

// emit reports an event to the event items of every subscription, or of only one, and keeps
// it in the event history (unless it is a refresh for one subscription).
func (s *Server) emit(e *event, only *subscription) {
	if only == nil {
		s.history.addEvent(e)
	}
	s.subsMutex.Lock()
	subs := make([]*subscription, 0, len(s.subs))
	for sub := range s.subs {
		if only == nil || sub == only {
			subs = append(subs, sub)
		}
	}
	s.subsMutex.Unlock()
	for _, sub := range subs {
		sub.s.mutex.Lock()
		queued := false
		for _, m := range sub.items {
			if m.filter == nil || m.mode == ua.MonitoringMode_monitoringModeDisabled {
				continue
			}
			reports := false
			for _, key := range e.notifiers {
				reports = reports || key == m.node.key
			}
			if !reports || !m.filter.matches(e) {
				continue
			}
			if len(m.events) >= m.queueSize {
				m.events = m.events[1:]
			}
			m.events = append(m.events, m.filter.fields(e))
			queued = true
		}
		sub.s.mutex.Unlock()
		if queued {
			select {
			case sub.kick <- struct{}{}:
			default:
			}
		}
	}
}

// eventFilter is a checked EventFilter.
type eventFilter struct {
	selects  []ua.SimpleAttributeOperand
	elements []ua.ContentFilterElement
}

// newEventFilter checks a filter: its select clauses and the operators of its where clause.
func newEventFilter(filter ua.EventFilter) (*eventFilter, ua.EventFilterResult, uint32) {
	f := &eventFilter{selects: filter.GetSelectClauses()}
	if where := filter.GetWhereClause(); where != nil {
		f.elements = where.GetElements()
	}
	selectResults := make([]uint32, len(f.selects))
	bad := len(f.selects) == 0
	for i, sel := range f.selects {
		if sel.GetAttributeId() != attrValue && sel.GetAttributeId() != attrNodeId {
			selectResults[i], bad = statusBadAttributeIdInvalid, true
		}
	}
	elementResults := make([]ua.ContentFilterElementResult, len(f.elements))
	for i, el := range f.elements {
		status := uint32(statusGood)
		var operands []uint32
		switch el.GetFilterOperator() {
		case ua.FilterOperator_filterOperatorOfType, ua.FilterOperator_filterOperatorAnd, ua.FilterOperator_filterOperatorOr,
			ua.FilterOperator_filterOperatorNot, ua.FilterOperator_filterOperatorEquals, ua.FilterOperator_filterOperatorGreaterThan,
			ua.FilterOperator_filterOperatorLessThan, ua.FilterOperator_filterOperatorGreaterThanOrEqual,
			ua.FilterOperator_filterOperatorLessThanOrEqual, ua.FilterOperator_filterOperatorIsNull:
			for _, operand := range el.GetFilterOperands() {
				operandStatus := uint32(statusGood)
				switch o := operand.GetBody().(type) {
				case ua.LiteralOperand, ua.SimpleAttributeOperand:
				case ua.ElementOperand:
					if int(o.GetIndex()) <= i || int(o.GetIndex()) >= len(f.elements) {
						operandStatus = statusBadFilterOperandInvalid
					}
				default:
					operandStatus = statusBadFilterOperandInvalid
				}
				if operandStatus != statusGood {
					status = statusBadFilterOperandInvalid
				}
				operands = append(operands, operandStatus)
			}
		default:
			status = statusBadFilterOperatorUnsupported
		}
		if status != statusGood {
			bad = true
		}
		elementResults[i] = ua.NewContentFilterElementResult(ua.NewStatusCode(status), statusCodes(operands), nil)
	}
	result := ua.NewEventFilterResult(statusCodes(selectResults), nil, ua.NewContentFilterResult(elementResults, nil))
	if bad {
		return nil, result, statusBadEventFilterInvalid
	}
	return f, result, statusGood
}

// field is the value a select clause (or a SimpleAttributeOperand) picks from an event.
func field(e *event, sel ua.SimpleAttributeOperand) ua.Variant {
	typeId := uint32(idBaseEventType)
	if t := keyOf(sel.GetTypeDefinitionId()); strings.HasPrefix(t, "ns=0;i=") {
		var id uint32
		for _, ch := range strings.TrimPrefix(t, "ns=0;i=") {
			id = id*10 + uint32(ch-'0')
		}
		if id != 0 {
			typeId = id
		}
	}
	if !isEventSubtype(e.typeId, typeId) {
		return variantNull()
	}
	names := make([]string, len(sel.GetBrowsePath()))
	for i, q := range sel.GetBrowsePath() {
		names[i] = stringOf(q.GetName())
	}
	path := strings.Join(names, "/")
	if sel.GetAttributeId() == attrNodeId {
		if path == "" && e.conditionId != nil {
			return variantNodeId(e.conditionId) // the ConditionId of a condition's event
		}
		return variantNull()
	}
	if v, ok := e.fields[path]; ok {
		return v
	}
	return variantNull()
}

func (f *eventFilter) fields(e *event) []ua.Variant {
	out := make([]ua.Variant, len(f.selects))
	for i, sel := range f.selects {
		out[i] = field(e, sel)
	}
	return out
}

// matches evaluates the where clause; an empty one takes every event.
func (f *eventFilter) matches(e *event) bool {
	if len(f.elements) == 0 {
		return true
	}
	v, _ := f.evaluate(e, 0)
	return v == true
}

// evaluate is the value of a where-clause element: a bool, or for an operand a Go value.
func (f *eventFilter) evaluate(e *event, index int) (any, bool) {
	el := f.elements[index]
	operands := el.GetFilterOperands()
	operand := func(i int) any {
		if i >= len(operands) {
			return nil
		}
		switch o := operands[i].GetBody().(type) {
		case ua.LiteralOperand:
			return goValue(o.GetValue())
		case ua.SimpleAttributeOperand:
			return goValue(field(e, o))
		case ua.ElementOperand:
			v, _ := f.evaluate(e, int(o.GetIndex()))
			return v
		}
		return nil
	}
	switch el.GetFilterOperator() {
	case ua.FilterOperator_filterOperatorOfType:
		if len(operands) != 1 {
			return false, false
		}
		literal, ok := operands[0].GetBody().(ua.LiteralOperand)
		if !ok {
			return false, false
		}
		ids, ok := literal.GetValue().(ua.VariantNodeId)
		if !ok || len(ids.GetValue()) != 1 {
			return false, false
		}
		var typeId uint32
		key := keyOf(ids.GetValue()[0])
		if !strings.HasPrefix(key, "ns=0;i=") {
			return false, true
		}
		for _, ch := range strings.TrimPrefix(key, "ns=0;i=") {
			typeId = typeId*10 + uint32(ch-'0')
		}
		return isEventSubtype(e.typeId, typeId), true
	case ua.FilterOperator_filterOperatorAnd:
		return operand(0) == true && operand(1) == true, true
	case ua.FilterOperator_filterOperatorOr:
		return operand(0) == true || operand(1) == true, true
	case ua.FilterOperator_filterOperatorNot:
		return operand(0) != true, true
	case ua.FilterOperator_filterOperatorIsNull:
		return operand(0) == nil, true
	}
	a, b := operand(0), operand(1)
	if el.GetFilterOperator() == ua.FilterOperator_filterOperatorEquals {
		if x, ok := number(a); ok {
			y, ok := number(b)
			return ok && x == y, true
		}
		return a == b, true
	}
	x, okX := number(a)
	y, okY := number(b)
	if !okX || !okY {
		return false, true
	}
	switch el.GetFilterOperator() {
	case ua.FilterOperator_filterOperatorGreaterThan:
		return x > y, true
	case ua.FilterOperator_filterOperatorLessThan:
		return x < y, true
	case ua.FilterOperator_filterOperatorGreaterThanOrEqual:
		return x >= y, true
	case ua.FilterOperator_filterOperatorLessThanOrEqual:
		return x <= y, true
	}
	return false, false
}

// goValue is a comparable Go value of a Variant: a number, string or bool; nil otherwise.
func goValue(v ua.Variant) any {
	if s, ok := textOf(v); ok {
		if _, isNull := v.(ua.VariantNull); isNull {
			return nil
		}
		return s
	}
	value, _, err := valueOf(v)
	if err != nil {
		return nil
	}
	switch value.(type) {
	case bool, string:
		return value
	}
	if x, ok := number(value); ok {
		return x
	}
	return nil
}

func number(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, !math.IsNaN(x)
	case float32:
		return float64(x), true
	case int8:
		return float64(x), true
	case uint8:
		return float64(x), true
	case int16:
		return float64(x), true
	case uint16:
		return float64(x), true
	case int32:
		return float64(x), true
	case uint32:
		return float64(x), true
	case int64:
		return float64(x), true
	case uint64:
		return float64(x), true
	}
	return 0, false
}
