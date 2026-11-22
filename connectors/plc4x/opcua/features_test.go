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
	"crypto/x509"
	"errors"
	"reflect"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	ua "github.com/apache/plc4x/plc4go/protocols/opcua/readwrite/model"
)

// featureServer serves the test points plus a structure point, methods, an alarm and history.
func featureServer(t *testing.T) (*memory, string, *atomic.Int32) {
	t.Helper()
	source := newMemory()
	source.set("Motor", map[string]any{"Speed": 1500.0, "Running": true, "Name": "pump", "Temps": []int16{40, 41, 42},
		"Config": map[string]any{"MaxSpeed": float32(1800), "Ramp": int64(5)}})
	resets := &atomic.Int32{}
	points := testPoints()
	points[0].Historize = true // Line1.Speed
	points = append(points, Point{Name: "Motor", DataType: TypeStructure, Structure: "MotorData", Writable: true})
	address := startServer(t, Config{
		Points: points, Source: source,
		Structures: []Structure{
			{Name: "MotorData", Fields: []Field{
				{Name: "Speed", DataType: TypeDouble}, {Name: "Running", DataType: TypeBoolean}, {Name: "Name", DataType: TypeString},
				{Name: "Temps", DataType: TypeInt16, ArrayLength: 3}, {Name: "Config", DataType: TypeStructure, Structure: "MotorConfig"},
			}},
			{Name: "MotorConfig", Fields: []Field{{Name: "MaxSpeed", DataType: TypeFloat}, {Name: "Ramp", DataType: TypeInt64}}},
		},
		Methods: []Method{
			{Name: "Line1.Reset", Call: func([]any, string) ([]any, error) { resets.Add(1); return nil, nil }},
			{Name: "Line1.Add", Inputs: []Argument{{Name: "a", DataType: TypeInt32}, {Name: "b", DataType: TypeInt32}},
				Outputs: []Argument{{Name: "sum", DataType: TypeInt32}},
				Call:    func(in []any, _ string) ([]any, error) { return []any{in[0].(int32) + in[1].(int32)}, nil }},
			{Name: "Line1.Fail", Call: func([]any, string) ([]any, error) { return nil, errors.New("no") }},
		},
		Alarms: []Alarm{{Name: "Line1.HighSpeed", Source: "Line1.Speed", Severity: 700, Message: "Speed high",
			Active: func() bool { v, _ := source.get("Line1.Speed").(float64); return v > 50 }}},
		AlarmInterval: 10 * time.Millisecond,
		History:       History{Depth: 100, Interval: 10 * time.Millisecond, Events: 100},
	})
	return source, address, resets
}

// wrap wraps a structure the model knows under its encoding id.
func wrap(definition ua.ExtensionObjectDefinition) ua.ExtensionObject {
	return extensionObject(definition, uint32(definition.GetExtensionId()))
}

func activeClient(t *testing.T, address string) *rawClient {
	t.Helper()
	c := dialRaw(t, address)
	c.hello()
	c.open()
	c.session(true)
	return c
}

func (c *rawClient) readRange(id ua.NodeId, attribute uint32, indexRange string) ua.DataValue {
	c.t.Helper()
	r := str(indexRange)
	if indexRange == "" {
		r = nullString()
	}
	response, ok := c.call(ua.NewReadRequest(c.header(), 0, ua.TimestampsToReturn_timestampsToReturnNeither,
		[]ua.ReadValueId{ua.NewReadValueId(id, attribute, r, ua.NewQualifiedName(0, nullString()))})).(ua.ReadResponse)
	if !ok {
		c.t.Fatal("Read failed")
	}
	return response.GetResults()[0]
}

func statusOf(v ua.DataValue) uint32 {
	if v.GetStatusCode() == nil {
		return statusGood
	}
	return v.GetStatusCode().GetStatusCode()
}

func (c *rawClient) write(id ua.NodeId, indexRange string, variant ua.Variant) uint32 {
	c.t.Helper()
	r := str(indexRange)
	if indexRange == "" {
		r = nullString()
	}
	response := c.call(ua.NewWriteRequest(c.header(), []ua.WriteValue{ua.NewWriteValue(id, attrValue, r,
		dataValue(variant, statusGood, time.Time{}, ua.TimestampsToReturn_timestampsToReturnNeither))})).(ua.WriteResponse)
	return response.GetResults()[0].GetStatusCode()
}

func TestIndexRanges(t *testing.T) {
	source, address, _ := featureServer(t)
	c := activeClient(t, address)
	value := func(v ua.DataValue) any {
		t.Helper()
		got, _, err := valueOf(v.GetValue())
		if err != nil {
			t.Fatalf("%v: %v", v, err)
		}
		return got
	}
	if got := value(c.readRange(nodeOf(1, "Line1.Temps"), attrValue, "1:2")); !reflect.DeepEqual(got, []int16{21, 22}) {
		t.Errorf("Temps[1:2] = %v", got)
	}
	if got := value(c.readRange(nodeOf(1, "Line1.Temps"), attrValue, "2:9")); !reflect.DeepEqual(got, []int16{22}) {
		t.Errorf("Temps[2:9] = %v", got)
	}
	if got := value(c.readRange(nodeOf(1, "Line1.Name"), attrValue, "0:2")); got != "pac" {
		t.Errorf("Name[0:2] = %v", got)
	}
	for r, want := range map[string]uint32{"5": statusBadIndexRangeNoData, "x": statusBadIndexRangeInvalid, "2:1": statusBadIndexRangeInvalid} {
		if got := statusOf(c.readRange(nodeOf(1, "Line1.Temps"), attrValue, r)); got != want {
			t.Errorf("Temps[%s]: 0x%08X", r, got)
		}
	}
	part, _ := variantOf([]int16{99}, TypeInt16)
	if status := c.write(nodeOf(1, "Line1.Temps"), "0", part); status != statusGood {
		t.Fatalf("write Temps[0]: 0x%08X", status)
	}
	if got := source.get("Line1.Temps"); !reflect.DeepEqual(got, []int16{99, 21, 22}) {
		t.Errorf("Temps = %v", got)
	}
	two, _ := variantOf([]int16{1, 2}, TypeInt16)
	if status := c.write(nodeOf(1, "Line1.Temps"), "0", two); status != statusBadIndexRangeInvalid {
		t.Errorf("two values into one place: 0x%08X", status)
	}
}

func TestTypesAndStructures(t *testing.T) {
	source, address, _ := featureServer(t)
	c := activeClient(t, address)

	// Data types are nodes: a client names a variable's type by reading its DataType node.
	dataType := c.readRange(nodeOf(1, "Line1.Speed"), attrDataType, "").GetValue().(ua.VariantNodeId).GetValue()[0]
	name := c.readRange(dataType, attrBrowseName, "").GetValue().(ua.VariantQualifiedName).GetValue()[0]
	if stringOf(name.GetName()) != "Double" {
		t.Errorf("Line1.Speed's data type is %s", stringOf(name.GetName()))
	}

	// The structure's DataType node carries its definition.
	motorType := c.readRange(nodeOf(1, "Motor"), attrDataType, "").GetValue().(ua.VariantNodeId).GetValue()[0]
	if keyOf(motorType) != "ns=1;s=type:MotorData" {
		t.Fatalf("Motor's data type %s", keyOf(motorType))
	}
	definition := c.readRange(motorType, attrDataTypeDefinition, "").GetValue().(ua.VariantExtensionObject).GetValue()[0]
	fields := definition.GetBody().(ua.StructureDefinition).GetFields()
	var names []string
	for _, f := range fields {
		names = append(names, stringOf(f.GetName()))
	}
	if !slices.Equal(names, []string{"Speed", "Running", "Name", "Temps", "Config"}) {
		t.Errorf("MotorData fields %v", names)
	}
	if keyOf(fields[4].GetDataType()) != "ns=1;s=type:MotorConfig" {
		t.Errorf("Config's type %s", keyOf(fields[4].GetDataType()))
	}

	// The value is an ExtensionObject of the structure's encoding.
	srv, _ := New(Config{Source: source, Points: []Point{{Name: "Motor", DataType: TypeStructure, Structure: "MotorData"}},
		Structures: []Structure{
			{Name: "MotorData", Fields: []Field{
				{Name: "Speed", DataType: TypeDouble}, {Name: "Running", DataType: TypeBoolean}, {Name: "Name", DataType: TypeString},
				{Name: "Temps", DataType: TypeInt16, ArrayLength: 3}, {Name: "Config", DataType: TypeStructure, Structure: "MotorConfig"},
			}},
			{Name: "MotorConfig", Fields: []Field{{Name: "MaxSpeed", DataType: TypeFloat}, {Name: "Ramp", DataType: TypeInt64}}},
		}})
	motor := srv.structureByName["MotorData"]
	got, err := motor.valueOf(c.readRange(nodeOf(1, "Motor"), attrValue, "").GetValue())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, source.get("Motor")) {
		t.Errorf("Motor read as %v", got)
	}

	// A variable per field.
	if v, _, _ := valueOf(c.readRange(nodeOf(1, "Motor.Config.MaxSpeed"), attrValue, "").GetValue()); v != float32(1800) {
		t.Errorf("Motor.Config.MaxSpeed = %v", v)
	}
	speed, _ := variantOf(1200, TypeDouble)
	if status := c.write(nodeOf(1, "Motor.Speed"), "", speed); status != statusGood {
		t.Fatalf("write Motor.Speed: 0x%08X", status)
	}
	if v := source.get("Motor").(map[string]any)["Speed"]; v != 1200.0 {
		t.Errorf("Motor.Speed = %v", v)
	}
	if v := source.get("Motor").(map[string]any)["Name"]; v != "pump" {
		t.Errorf("writing a field changed another: Name = %v", v)
	}

	// The whole structure, written.
	whole := map[string]any{"Speed": 10.0, "Running": false, "Name": "fan", "Temps": []int16{1, 2, 3},
		"Config": map[string]any{"MaxSpeed": float32(20), "Ramp": int64(1)}}
	variant, _ := motor.variant(whole)
	if status := c.write(nodeOf(1, "Motor"), "", variant); status != statusGood {
		t.Fatalf("write Motor: 0x%08X", status)
	}
	if got := source.get("Motor"); !reflect.DeepEqual(got, whole) {
		t.Errorf("Motor written as %v", got)
	}
	if status := c.write(nodeOf(1, "Motor"), "", speed); status != statusBadTypeMismatch {
		t.Errorf("a double for a structure: 0x%08X", status)
	}
}

func TestMethods(t *testing.T) {
	_, address, resets := featureServer(t)
	c := activeClient(t, address)
	call := func(object, method ua.NodeId, inputs ...ua.Variant) ua.CallMethodResult {
		t.Helper()
		response, ok := c.call(ua.NewCallRequest(c.header(), []ua.CallMethodRequest{ua.NewCallMethodRequest(object, method, inputs)})).(ua.CallResponse)
		if !ok {
			t.Fatal("Call failed")
		}
		return response.GetResults()[0]
	}
	line := nodeOf(1, "Line1")
	if r := call(line, nodeOf(1, "Line1.Reset")); r.GetStatusCode().GetStatusCode() != statusGood || resets.Load() != 1 {
		t.Errorf("Reset: 0x%08X, %d resets", r.GetStatusCode().GetStatusCode(), resets.Load())
	}
	a, _ := variantOf(2, TypeInt32)
	b, _ := variantOf(3, TypeInt32)
	r := call(line, nodeOf(1, "Line1.Add"), a, b)
	if sum, _, _ := valueOf(r.GetOutputArguments()[0]); r.GetStatusCode().GetStatusCode() != statusGood || sum != int32(5) {
		t.Errorf("Add(2, 3): 0x%08X %v", r.GetStatusCode().GetStatusCode(), r.GetOutputArguments())
	}
	if r := call(line, nodeOf(1, "Line1.Add"), a); r.GetStatusCode().GetStatusCode() != statusBadArgumentsMissing {
		t.Errorf("Add(2): 0x%08X", r.GetStatusCode().GetStatusCode())
	}
	text, _ := variantOf("x", TypeString)
	r = call(line, nodeOf(1, "Line1.Add"), a, text)
	if r.GetStatusCode().GetStatusCode() != statusBadInvalidArgument || r.GetInputArgumentResults()[1].GetStatusCode() != statusBadTypeMismatch {
		t.Errorf("Add(2, \"x\"): 0x%08X %v", r.GetStatusCode().GetStatusCode(), r.GetInputArgumentResults())
	}
	if r := call(nodeOf(0, "85"), nodeOf(1, "Line1.Reset")); r.GetStatusCode().GetStatusCode() != statusBadMethodInvalid {
		t.Errorf("Reset on Objects: 0x%08X", r.GetStatusCode().GetStatusCode())
	}
	if r := call(line, nodeOf(1, "Line1.Fail")); r.GetStatusCode().GetStatusCode() != statusBad {
		t.Errorf("Fail: 0x%08X", r.GetStatusCode().GetStatusCode())
	}
	args := c.readRange(nodeOf(1, "Line1.Add.InputArguments"), attrValue, "").GetValue().(ua.VariantExtensionObject).GetValue()
	if len(args) != 2 || stringOf(args[1].GetBody().(ua.Argument).GetName()) != "b" {
		t.Errorf("InputArguments %v", args)
	}
	if v := c.readRange(nodeOf(1, "Line1.Add"), attrExecutable, "").GetValue(); !reflect.DeepEqual(v, variantBool(true)) {
		t.Errorf("Executable %v", v)
	}
}

func eventSelect(names ...string) ua.SimpleAttributeOperand {
	path := make([]ua.QualifiedName, len(names))
	for i, n := range names {
		path[i] = ua.NewQualifiedName(0, str(n))
	}
	return ua.NewSimpleAttributeOperand(numericNodeId(0, idBaseEventType), path, attrValue, nullString())
}

func TestAlarmsAndHistory(t *testing.T) {
	source, address, _ := featureServer(t)
	c := activeClient(t, address)
	start := time.Now()

	created := c.call(ua.NewCreateSubscriptionRequest(c.header(), 50, 100, 10, 0, true, 0)).(ua.CreateSubscriptionResponse)
	sub := created.GetSubscriptionId()
	selects := []ua.SimpleAttributeOperand{eventSelect("EventId"), eventSelect("EventType"), eventSelect("SourceName"), eventSelect("Message"),
		eventSelect("Severity"), eventSelect("ActiveState", "Id"), eventSelect("AckedState", "Id"), eventSelect("Retain"),
		ua.NewSimpleAttributeOperand(numericNodeId(0, idConditionType), nil, attrNodeId, nullString())}
	filter := wrap(ua.NewEventFilter(selects, ua.NewContentFilter(nil)))
	items := c.call(ua.NewCreateMonitoredItemsRequest(c.header(), sub, ua.TimestampsToReturn_timestampsToReturnBoth, []ua.MonitoredItemCreateRequest{
		ua.NewMonitoredItemCreateRequest(ua.NewReadValueId(nodeOf(0, "2253"), attrEventNotifier, nullString(), ua.NewQualifiedName(0, nullString())),
			ua.MonitoringMode_monitoringModeReporting, ua.NewMonitoringParameters(1, 0, filter, 0, true)),
	})).(ua.CreateMonitoredItemsResponse)
	if got := items.GetResults()[0].GetStatusCode().GetStatusCode(); got != statusGood {
		t.Fatalf("event item: 0x%08X", got)
	}

	type alarmEvent struct {
		eventId   []byte
		eventType string
		message   string
		severity  any
		active    any
		acked     any
		retain    any
		condition string
	}
	var received []alarmEvent // events of a message not yet looked at
	next := func(want func(alarmEvent) bool) alarmEvent {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			for len(received) > 0 {
				ev := received[0]
				received = received[1:]
				if want(ev) {
					return ev
				}
			}
			r := c.call(ua.NewPublishRequest(c.header(), nil)).(ua.PublishResponse)
			for _, data := range r.GetNotificationMessage().GetNotificationData() {
				list, ok := data.GetBody().(ua.EventNotificationList)
				if !ok {
					continue
				}
				for _, e := range list.GetEvents() {
					f := e.GetEventFields()
					id, _ := byteStringOf(f[0])
					message, _ := textOf(f[3])
					ev := alarmEvent{eventId: id, eventType: keyOf(f[1].(ua.VariantNodeId).GetValue()[0]), message: message}
					ev.severity, _, _ = valueOf(f[4])
					ev.active, _, _ = valueOf(f[5])
					ev.acked, _, _ = valueOf(f[6])
					ev.retain, _, _ = valueOf(f[7])
					if ids, ok := f[8].(ua.VariantNodeId); ok {
						ev.condition = keyOf(ids.GetValue()[0])
					}
					received = append(received, ev)
				}
			}
		}
		t.Fatal("no such event")
		return alarmEvent{}
	}

	source.set("Line1.Speed", 60.0)
	raised := next(func(e alarmEvent) bool { return e.active == true })
	if raised.message != "Speed high" || raised.severity != uint16(700) || raised.acked != false || raised.retain != true ||
		raised.condition != "ns=1;s=Line1.HighSpeed" || raised.eventType != numericKey(idAlarmConditionType) {
		t.Errorf("raised %+v", raised)
	}
	if v, _, _ := valueOf(c.readRange(nodeOf(1, "Line1.HighSpeed.ActiveState.Id"), attrValue, "").GetValue()); v != true {
		t.Errorf("ActiveState/Id reads %v", v)
	}

	method := func(object ua.NodeId, methodId uint32, inputs ...ua.Variant) uint32 {
		t.Helper()
		response := c.call(ua.NewCallRequest(c.header(), []ua.CallMethodRequest{ua.NewCallMethodRequest(object, numericNodeId(0, methodId), inputs)})).(ua.CallResponse)
		return response.GetResults()[0].GetStatusCode().GetStatusCode()
	}
	condition := nodeOf(1, "Line1.HighSpeed")
	if status := method(condition, idAcknowledge, variantByteString([]byte("wrong")), variantText("seen")); status != statusBadEventIdUnknown {
		t.Errorf("Acknowledge of another event: 0x%08X", status)
	}
	if status := method(condition, idAcknowledge, variantByteString(raised.eventId), variantText("seen")); status != statusGood {
		t.Fatalf("Acknowledge: 0x%08X", status)
	}
	acked := next(func(e alarmEvent) bool { return e.acked == true })
	if acked.message != "Acknowledged" || acked.active != true {
		t.Errorf("acknowledged %+v", acked)
	}
	if status := method(condition, idAcknowledge, variantByteString(acked.eventId), variantText("")); status != statusBadConditionBranchAlreadyAck {
		t.Errorf("a second Acknowledge: 0x%08X", status)
	}

	// ConditionRefresh brings the alarm again between RefreshStart and RefreshEnd.
	if status := method(nodeOf(0, "2782"), idConditionRefresh, func() ua.Variant { v, _ := variantOf(sub, TypeUInt32); return v }()); status != statusGood {
		t.Fatalf("ConditionRefresh: 0x%08X", status)
	}
	next(func(e alarmEvent) bool { return e.eventType == numericKey(idRefreshStartEventType) })
	again := next(func(e alarmEvent) bool { return e.condition == "ns=1;s=Line1.HighSpeed" })
	if !slices.Equal(again.eventId, acked.eventId) {
		t.Errorf("refresh carried event %x, not the current %x", again.eventId, acked.eventId)
	}
	next(func(e alarmEvent) bool { return e.eventType == numericKey(idRefreshEndEventType) })

	source.set("Line1.Speed", 12.5)
	cleared := next(func(e alarmEvent) bool { return e.active == false })
	if cleared.retain != false {
		t.Errorf("cleared %+v", cleared)
	}

	// History of the speed: 12.5, 60, 12.5.
	read := func(details ua.ExtensionObjectDefinition, encoding uint32, id ua.NodeId, point []byte) ua.HistoryReadResult {
		t.Helper()
		response := c.call(ua.NewHistoryReadRequest(c.header(), extensionObject(details, encoding), ua.TimestampsToReturn_timestampsToReturnSource, false,
			[]ua.HistoryReadValueId{ua.NewHistoryReadValueId(id, nullString(), ua.NewQualifiedName(0, nullString()), byteString(point))})).(ua.HistoryReadResponse)
		return response.GetResults()[0]
	}
	values := func(r ua.HistoryReadResult) []float64 {
		var out []float64
		for _, v := range r.GetHistoryData().GetBody().(ua.HistoryData).GetDataValues() {
			x, _, _ := valueOf(v.GetValue())
			out = append(out, x.(float64))
		}
		return out
	}
	speed := nodeOf(1, "Line1.Speed")
	all := read(ua.NewReadRawModifiedDetails(false, dateTime(start.Add(-time.Hour)), dateTime(time.Now().Add(time.Hour)), 0, false), 649, speed, nil)
	if got := values(all); !slices.Equal(got, []float64{12.5, 60, 12.5}) {
		t.Errorf("history %v (0x%08X)", got, all.GetStatusCode().GetStatusCode())
	}
	backward := read(ua.NewReadRawModifiedDetails(false, dateTime(time.Now().Add(time.Hour)), dateTime(start.Add(-time.Hour)), 2, false), 649, speed, nil)
	point := bytesOf(backward.GetContinuationPoint())
	if got := values(backward); !slices.Equal(got, []float64{12.5, 60}) || point == nil {
		t.Errorf("newest two %v, continuation %x", got, point)
	}
	if got := values(read(ua.NewReadRawModifiedDetails(false, 0, 0, 0, false), 649, speed, point)); !slices.Equal(got, []float64{12.5}) {
		t.Errorf("the rest %v", got)
	}
	if got := read(ua.NewReadRawModifiedDetails(false, dateTime(start), dateTime(time.Now()), 0, false), 649, nodeOf(1, "Line1.Count"), nil); got.GetStatusCode().GetStatusCode() != statusBadHistoryOperationUnsupported {
		t.Errorf("history of a point without: 0x%08X", got.GetStatusCode().GetStatusCode())
	}

	// History of the events, on the Server object.
	events := read(ua.NewReadEventDetails(0, dateTime(start.Add(-time.Hour)), dateTime(time.Now().Add(time.Hour)),
		ua.NewEventFilter([]ua.SimpleAttributeOperand{eventSelect("Message")}, ua.NewContentFilter(nil))), 646, nodeOf(0, "2253"), nil)
	var messages []string
	for _, list := range events.GetHistoryData().GetBody().(ua.HistoryEvent).GetEvents() {
		m, _ := textOf(list.GetEventFields()[0])
		messages = append(messages, m)
	}
	if !slices.Equal(messages, []string{"Speed high", "Acknowledged", "Line1.HighSpeed returned to normal"}) {
		t.Errorf("event history %v", messages)
	}
}

func TestEventWhereClause(t *testing.T) {
	al := &alarm{cfg: Alarm{Name: "A", Severity: 300, Message: "m"}, node: &node{key: "ns=1;s=A", id: stringNodeId(1, "A")}}
	e := al.event("m", alarmState{enabled: true, active: true})
	severity := func(op ua.FilterOperator, value uint16) ua.ContentFilterElement {
		return ua.NewContentFilterElement(op, []ua.ExtensionObject{
			wrap(eventSelect("Severity")), wrap(ua.NewLiteralOperand(variantUInt16(value)))})
	}
	ofType := func(id uint32) ua.ContentFilterElement {
		return ua.NewContentFilterElement(ua.FilterOperator_filterOperatorOfType, []ua.ExtensionObject{
			wrap(ua.NewLiteralOperand(variantNodeId(numericNodeId(0, id))))})
	}
	for name, tc := range map[string]struct {
		elements []ua.ContentFilterElement
		want     bool
	}{
		"none":             {nil, true},
		"severity >= 200":  {[]ua.ContentFilterElement{severity(ua.FilterOperator_filterOperatorGreaterThanOrEqual, 200)}, true},
		"severity > 300":   {[]ua.ContentFilterElement{severity(ua.FilterOperator_filterOperatorGreaterThan, 300)}, false},
		"of ConditionType": {[]ua.ContentFilterElement{ofType(idConditionType)}, true},
		"of RefreshStart":  {[]ua.ContentFilterElement{ofType(idRefreshStartEventType)}, false},
		"and": {[]ua.ContentFilterElement{
			ua.NewContentFilterElement(ua.FilterOperator_filterOperatorAnd, []ua.ExtensionObject{
				wrap(ua.NewElementOperand(1)), wrap(ua.NewElementOperand(2))}),
			ofType(idAlarmConditionType), severity(ua.FilterOperator_filterOperatorLessThan, 500)}, true},
	} {
		f, _, status := newEventFilter(ua.NewEventFilter([]ua.SimpleAttributeOperand{eventSelect("Message")}, ua.NewContentFilter(tc.elements)))
		if status != statusGood {
			t.Errorf("%s: 0x%08X", name, status)
			continue
		}
		if got := f.matches(e); got != tc.want {
			t.Errorf("%s: %v", name, got)
		}
	}
	if _, _, status := newEventFilter(ua.NewEventFilter([]ua.SimpleAttributeOperand{eventSelect("Message")}, ua.NewContentFilter(
		[]ua.ContentFilterElement{ua.NewContentFilterElement(ua.FilterOperator_filterOperatorLike, nil)}))); status != statusBadEventFilterInvalid {
		t.Errorf("Like: 0x%08X", status)
	}
}

func TestGenerateIdentity(t *testing.T) {
	der, key, err := GenerateIdentity("urn:test:honeycomb", "honeycomb test", []string{"plant", "10.0.0.5"}, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	if len(cert.URIs) != 1 || cert.URIs[0].String() != "urn:test:honeycomb" || !slices.Equal(cert.DNSNames, []string{"plant"}) ||
		len(cert.IPAddresses) != 1 || cert.IPAddresses[0].String() != "10.0.0.5" {
		t.Errorf("certificate names %v %v %v", cert.URIs, cert.DNSNames, cert.IPAddresses)
	}
	if _, err := New(Config{Source: newMemory(), Certificate: der, Key: key, Endpoints: []Endpoint{{Policy: "Basic256Sha256", Mode: "Sign"}},
		TrustClient: func([]*x509.Certificate) error { return nil }}); err != nil {
		t.Errorf("a server with the identity: %v", err)
	}
	if _, _, err := GenerateIdentity("not a uri", "x", nil, time.Hour); err == nil {
		t.Error("a bad application URI was taken")
	}
}
