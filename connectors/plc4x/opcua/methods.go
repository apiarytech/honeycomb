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
	"fmt"

	ua "github.com/apache/plc4x/plc4go/protocols/opcua/readwrite/model"
)

// Method status codes.
const (
	statusBadMethodInvalid    = 0x80750000
	statusBadArgumentsMissing = 0x80760000
	statusBadInvalidArgument  = 0x80AB0000
	statusBadTooManyArguments = 0x80E50000
)

// method is a Method of the Config, or a method of the condition types (executable, Call
// empty: the server answers it).
type method struct {
	cfg        Method
	parent     string // the key of the object it belongs to
	executable bool
}

func (c *conn) call(s *session, request ua.CallRequest) ua.ExtensionObjectDefinition {
	header := request.GetRequestHeader()
	calls := request.GetMethodsToCall()
	if status := tooMany(len(calls)); status != statusGood {
		return c.fault(header, status)
	}
	s.mutex.Lock()
	user := s.user
	s.mutex.Unlock()
	results := make([]ua.CallMethodResult, len(calls))
	for i, call := range calls {
		results[i] = c.callMethod(s, user, call)
	}
	return ua.NewCallResponse(c.responseHeader(header, statusGood), results, nil)
}

func callResult(status uint32, inputs []uint32, outputs []ua.Variant) ua.CallMethodResult {
	return ua.NewCallMethodResult(ua.NewStatusCode(status), statusCodes(inputs), nil, outputs)
}

func (c *conn) callMethod(s *session, user string, call ua.CallMethodRequest) ua.CallMethodResult {
	object := c.s.space.nodes[keyOf(call.GetObjectId())]
	m := c.s.space.nodes[keyOf(call.GetMethodId())]
	switch {
	case object == nil:
		return callResult(statusBadNodeIdUnknown, nil, nil)
	case m == nil || m.method == nil:
		return callResult(statusBadMethodInvalid, nil, nil)
	}
	inputs := call.GetInputArguments()
	args := m.method.cfg.Inputs
	switch {
	case len(inputs) < len(args):
		return callResult(statusBadArgumentsMissing, nil, nil)
	case len(inputs) > len(args):
		return callResult(statusBadTooManyArguments, nil, nil)
	}
	if m.method.cfg.Call == nil {
		return c.conditionMethod(s, user, object, m, inputs)
	}
	if m.method.parent != object.key {
		return callResult(statusBadMethodInvalid, nil, nil)
	}
	if !c.mayWrite {
		return callResult(statusBadUserAccessDenied, nil, nil)
	}
	values := make([]any, len(args))
	results := make([]uint32, len(args))
	failed := false
	for i, a := range args {
		values[i], results[i] = convertTo(a.DataType, a.ArrayLength, nil, inputs[i])
		failed = failed || results[i] != statusGood
	}
	if failed {
		return callResult(statusBadInvalidArgument, results, nil)
	}
	outputs, err := m.method.cfg.Call(values, user)
	if err != nil {
		c.s.onError(fmt.Errorf("opcua: method %s: %w", m.method.cfg.Name, err))
		return callResult(statusBad, nil, nil)
	}
	if len(outputs) != len(m.method.cfg.Outputs) {
		c.s.onError(fmt.Errorf("opcua: method %s returned %d outputs, not %d", m.method.cfg.Name, len(outputs), len(m.method.cfg.Outputs)))
		return callResult(statusBadInternalError, nil, nil)
	}
	encoded := make([]ua.Variant, len(outputs))
	for i, a := range m.method.cfg.Outputs {
		if encoded[i], err = variantOf(outputs[i], a.DataType); err != nil {
			c.s.onError(fmt.Errorf("opcua: method %s output %s: %w", m.method.cfg.Name, a.Name, err))
			return callResult(statusBadInternalError, nil, nil)
		}
	}
	return callResult(statusGood, nil, encoded)
}

// conditionMethod answers the methods of the condition types: on a condition, Enable,
// Disable, AddComment, Acknowledge and Confirm; on ConditionType or Server, ConditionRefresh.
func (c *conn) conditionMethod(s *session, user string, object, m *node, inputs []ua.Variant) ua.CallMethodResult {
	methodId := keyOf(m.id)
	if methodId == numericKey(idConditionRefresh) {
		if object.key != numericKey(idConditionType) && object.key != numericKey(idServer) {
			return callResult(statusBadMethodInvalid, nil, nil)
		}
		value, status := convertTo(TypeUInt32, 0, nil, inputs[0])
		if status != statusGood {
			return callResult(statusBadInvalidArgument, []uint32{status}, nil)
		}
		return callResult(c.s.refresh(s, value.(uint32)), nil, nil)
	}
	al := object.alarm
	if al == nil {
		return callResult(statusBadMethodInvalid, nil, nil)
	}
	if !c.mayWrite {
		return callResult(statusBadUserAccessDenied, nil, nil)
	}
	switch methodId {
	case numericKey(idEnable):
		return callResult(al.setEnabled(true, user), nil, nil)
	case numericKey(idDisable):
		return callResult(al.setEnabled(false, user), nil, nil)
	}
	eventId, ok := byteStringOf(inputs[0])
	comment, okComment := textOf(inputs[1])
	if !ok || !okComment {
		results := []uint32{statusGood, statusGood}
		if !ok {
			results[0] = statusBadTypeMismatch
		}
		if !okComment {
			results[1] = statusBadTypeMismatch
		}
		return callResult(statusBadInvalidArgument, results, nil)
	}
	var status uint32
	switch methodId {
	case numericKey(idAcknowledge):
		status = al.acknowledge(eventId, comment, user)
	case numericKey(idConfirm):
		status = al.confirm(eventId, comment, user)
	case numericKey(idAddComment):
		status = al.addComment(eventId, comment, user)
	default:
		status = statusBadMethodInvalid
	}
	return callResult(status, nil, nil)
}
