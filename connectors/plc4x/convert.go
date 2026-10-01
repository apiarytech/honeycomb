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
	"fmt"
	"reflect"
	"strings"
	"time"

	apiValues "github.com/apache/plc4x/plc4go/pkg/api/values"
	spiValues "github.com/apache/plc4x/plc4go/spi/values"
	"github.com/apiarytech/honeycomb"
	plc "github.com/apiarytech/royaljelly/iec"
)

// converters turn a PLC4X value into the royaljelly type a tag holds.
var converters = map[reflect.Type]func(apiValues.PlcValue) any{
	reflect.TypeFor[plc.BOOL]():    func(v apiValues.PlcValue) any { return plc.BOOL(v.GetBool()) },
	reflect.TypeFor[plc.BYTE]():    func(v apiValues.PlcValue) any { return plc.BYTE(v.GetUint8()) },
	reflect.TypeFor[plc.WORD]():    func(v apiValues.PlcValue) any { return plc.WORD(v.GetUint16()) },
	reflect.TypeFor[plc.DWORD]():   func(v apiValues.PlcValue) any { return plc.DWORD(v.GetUint32()) },
	reflect.TypeFor[plc.LWORD]():   func(v apiValues.PlcValue) any { return plc.LWORD(v.GetUint64()) },
	reflect.TypeFor[plc.SINT]():    func(v apiValues.PlcValue) any { return plc.SINT(v.GetInt8()) },
	reflect.TypeFor[plc.INT]():     func(v apiValues.PlcValue) any { return plc.INT(v.GetInt16()) },
	reflect.TypeFor[plc.DINT]():    func(v apiValues.PlcValue) any { return plc.DINT(v.GetInt32()) },
	reflect.TypeFor[plc.LINT]():    func(v apiValues.PlcValue) any { return plc.LINT(v.GetInt64()) },
	reflect.TypeFor[plc.USINT]():   func(v apiValues.PlcValue) any { return plc.USINT(v.GetUint8()) },
	reflect.TypeFor[plc.UINT]():    func(v apiValues.PlcValue) any { return plc.UINT(v.GetUint16()) },
	reflect.TypeFor[plc.UDINT]():   func(v apiValues.PlcValue) any { return plc.UDINT(v.GetUint32()) },
	reflect.TypeFor[plc.ULINT]():   func(v apiValues.PlcValue) any { return plc.ULINT(v.GetUint64()) },
	reflect.TypeFor[plc.REAL]():    func(v apiValues.PlcValue) any { return plc.REAL(v.GetFloat32()) },
	reflect.TypeFor[plc.LREAL]():   func(v apiValues.PlcValue) any { return plc.LREAL(v.GetFloat64()) },
	reflect.TypeFor[plc.STRING]():  func(v apiValues.PlcValue) any { return plc.STRING(v.GetString()) },
	reflect.TypeFor[plc.WSTRING](): func(v apiValues.PlcValue) any { return plc.WSTRING(v.GetString()) },
	reflect.TypeFor[plc.TIME]():    func(v apiValues.PlcValue) any { return plc.TIME(v.GetDuration()) },
	reflect.TypeFor[plc.DATE]():    func(v apiValues.PlcValue) any { return plc.DATE(v.GetDate()) },
	reflect.TypeFor[plc.TOD]():     func(v apiValues.PlcValue) any { return plc.TOD(v.GetTime()) },
	reflect.TypeFor[plc.DT]():      func(v apiValues.PlcValue) any { return plc.DT(v.GetDateTime()) },
	reflect.TypeFor[string]():      func(v apiValues.PlcValue) any { return v.GetString() }, // ENUM tags
}

// deviceValues turn a royaljelly value into the PLC4X value written to a device.
var deviceValues = map[reflect.Type]func(reflect.Value) apiValues.PlcValue{
	reflect.TypeFor[plc.BOOL]():    func(v reflect.Value) apiValues.PlcValue { return spiValues.NewPlcBOOL(v.Bool()) },
	reflect.TypeFor[plc.BYTE]():    func(v reflect.Value) apiValues.PlcValue { return spiValues.NewPlcBYTE(uint8(v.Uint())) },
	reflect.TypeFor[plc.WORD]():    func(v reflect.Value) apiValues.PlcValue { return spiValues.NewPlcWORD(uint16(v.Uint())) },
	reflect.TypeFor[plc.DWORD]():   func(v reflect.Value) apiValues.PlcValue { return spiValues.NewPlcDWORD(uint32(v.Uint())) },
	reflect.TypeFor[plc.LWORD]():   func(v reflect.Value) apiValues.PlcValue { return spiValues.NewPlcLWORD(v.Uint()) },
	reflect.TypeFor[plc.SINT]():    func(v reflect.Value) apiValues.PlcValue { return spiValues.NewPlcSINT(int8(v.Int())) },
	reflect.TypeFor[plc.INT]():     func(v reflect.Value) apiValues.PlcValue { return spiValues.NewPlcINT(int16(v.Int())) },
	reflect.TypeFor[plc.DINT]():    func(v reflect.Value) apiValues.PlcValue { return spiValues.NewPlcDINT(int32(v.Int())) },
	reflect.TypeFor[plc.LINT]():    func(v reflect.Value) apiValues.PlcValue { return spiValues.NewPlcLINT(v.Int()) },
	reflect.TypeFor[plc.USINT]():   func(v reflect.Value) apiValues.PlcValue { return spiValues.NewPlcUSINT(uint8(v.Uint())) },
	reflect.TypeFor[plc.UINT]():    func(v reflect.Value) apiValues.PlcValue { return spiValues.NewPlcUINT(uint16(v.Uint())) },
	reflect.TypeFor[plc.UDINT]():   func(v reflect.Value) apiValues.PlcValue { return spiValues.NewPlcUDINT(uint32(v.Uint())) },
	reflect.TypeFor[plc.ULINT]():   func(v reflect.Value) apiValues.PlcValue { return spiValues.NewPlcULINT(v.Uint()) },
	reflect.TypeFor[plc.REAL]():    func(v reflect.Value) apiValues.PlcValue { return spiValues.NewPlcREAL(float32(v.Float())) },
	reflect.TypeFor[plc.LREAL]():   func(v reflect.Value) apiValues.PlcValue { return spiValues.NewPlcLREAL(v.Float()) },
	reflect.TypeFor[plc.STRING]():  func(v reflect.Value) apiValues.PlcValue { return spiValues.NewPlcSTRING(v.String()) },
	reflect.TypeFor[plc.WSTRING](): func(v reflect.Value) apiValues.PlcValue { return spiValues.NewPlcWSTRING(v.String()) },
	reflect.TypeFor[plc.TIME]():    func(v reflect.Value) apiValues.PlcValue { return spiValues.NewPlcTIME(time.Duration(v.Int())) },
	reflect.TypeFor[plc.DATE]():    func(v reflect.Value) apiValues.PlcValue { return spiValues.NewPlcDATE(asTime(v)) },
	reflect.TypeFor[plc.TOD]():     func(v reflect.Value) apiValues.PlcValue { return spiValues.NewPlcTIME_OF_DAY(asTime(v)) },
	reflect.TypeFor[plc.DT]():      func(v reflect.Value) apiValues.PlcValue { return spiValues.NewPlcDATE_AND_TIME(asTime(v)) },
	reflect.TypeFor[string]():      func(v reflect.Value) apiValues.PlcValue { return spiValues.NewPlcSTRING(v.String()) },
}

var timeType = reflect.TypeFor[time.Time]()

func asTime(v reflect.Value) time.Time { return v.Convert(timeType).Interface().(time.Time) }

// store converts value to the Go type of the tag's current value and writes it
// with Good quality. A read of an InOut tag whose change is waiting to be
// written to the device is dropped, so the change is not lost.
func (c *Connector) store(cs *connState, tagName string, value apiValues.PlcValue) error {
	current, err := c.db.GetTagValue(tagName)
	if err != nil {
		return err
	}
	if current == nil {
		return fmt.Errorf("tag has no value to infer its type from")
	}
	converted, err := convert(reflect.TypeOf(current), value)
	if err != nil {
		return err
	}
	if cs.byTag[tagName].Direction == InOut && !cs.link.accept(tagName, converted) {
		return nil
	}
	return c.db.SetTagValueQuality(tagName, converted, honeycomb.QualityGood)
}

// convert turns a PLC4X value into a value of type target. Slices map to PLC4X
// lists, and UDTs (structs or pointers to structs) to PLC4X structs.
func convert(target reflect.Type, value apiValues.PlcValue) (any, error) {
	if value == nil || value.IsNull() {
		return nil, fmt.Errorf("device returned no value")
	}
	if fn, ok := converters[target]; ok {
		return fn(value), nil
	}
	switch {
	case target.Kind() == reflect.Slice:
		var items []apiValues.PlcValue
		if value.IsList() {
			items = value.GetList()
		} else {
			items = []apiValues.PlcValue{value}
		}
		slice := reflect.MakeSlice(target, len(items), len(items))
		for i, item := range items {
			elem, err := convert(target.Elem(), item)
			if err != nil {
				return nil, fmt.Errorf("element %d: %w", i, err)
			}
			slice.Index(i).Set(reflect.ValueOf(elem))
		}
		return slice.Interface(), nil
	case target.Kind() == reflect.Pointer && target.Elem().Kind() == reflect.Struct:
		ptr := reflect.New(target.Elem())
		if err := convertStruct(ptr.Elem(), value); err != nil {
			return nil, err
		}
		return ptr.Interface(), nil
	case target.Kind() == reflect.Struct:
		out := reflect.New(target).Elem()
		if err := convertStruct(out, value); err != nil {
			return nil, err
		}
		return out.Interface(), nil
	}
	return nil, fmt.Errorf("no conversion from PLC4X %s to %s", value.GetPlcValueType(), target)
}

// convertStruct fills the exported fields of out from the members of a PLC4X
// struct. Every field must have a member; see memberName for the matching rules.
func convertStruct(out reflect.Value, value apiValues.PlcValue) error {
	if !value.IsStruct() {
		return fmt.Errorf("device returned %s for UDT %s, want a struct", value.GetPlcValueType(), out.Type())
	}
	members := value.GetStruct()
	for _, f := range structFields(out.Type()) {
		member, ok := lookupMember(members, f.member)
		if !ok {
			return fmt.Errorf("UDT %s: device struct has no member %q", out.Type(), f.member)
		}
		converted, err := convert(f.field.Type, member)
		if err != nil {
			return fmt.Errorf("UDT %s: member %q: %w", out.Type(), f.member, err)
		}
		out.FieldByIndex(f.field.Index).Set(reflect.ValueOf(converted))
	}
	return nil
}

type udtField struct {
	field  reflect.StructField
	member string
}

// structFields lists the exported fields of a UDT with their device member
// names: the `plc4x:"name"` struct tag if present, else the field name.
// Fields tagged `plc4x:"-"` are not exchanged with the device.
func structFields(t reflect.Type) []udtField {
	var fields []udtField
	for _, f := range reflect.VisibleFields(t) {
		if !f.IsExported() || f.Anonymous {
			continue
		}
		name := f.Name
		if tag, ok := f.Tag.Lookup("plc4x"); ok {
			if tag == "-" {
				continue
			}
			name = tag
		}
		fields = append(fields, udtField{field: f, member: name})
	}
	return fields
}

// lookupMember finds a struct member by exact name, then case-insensitively,
// since devices differ in how they capitalize member names.
func lookupMember(members map[string]apiValues.PlcValue, name string) (apiValues.PlcValue, bool) {
	if v, ok := members[name]; ok {
		return v, true
	}
	for key, v := range members {
		if strings.EqualFold(key, name) {
			return v, true
		}
	}
	return nil, false
}

// toDevice converts a tag value into the value handed to a PLC4X write request.
// Arrays become Go slices of PLC4X values, which PLC4X expects for array addresses.
func toDevice(value any) (any, error) {
	v := reflect.ValueOf(value)
	if _, simple := deviceValues[v.Type()]; !simple && v.Kind() == reflect.Slice {
		items := make([]any, v.Len())
		for i := range items {
			item, err := toPlcValue(v.Index(i))
			if err != nil {
				return nil, fmt.Errorf("element %d: %w", i, err)
			}
			items[i] = item
		}
		return items, nil
	}
	return toPlcValue(v)
}

// toPlcValue converts a royaljelly value, array or UDT into a PLC4X value.
func toPlcValue(v reflect.Value) (apiValues.PlcValue, error) {
	if fn, ok := deviceValues[v.Type()]; ok {
		return fn(v), nil
	}
	switch v.Kind() {
	case reflect.Slice:
		items := make([]apiValues.PlcValue, v.Len())
		for i := range items {
			item, err := toPlcValue(v.Index(i))
			if err != nil {
				return nil, fmt.Errorf("element %d: %w", i, err)
			}
			items[i] = item
		}
		return spiValues.NewPlcList(items), nil
	case reflect.Pointer:
		if v.IsNil() {
			return nil, fmt.Errorf("nil %s", v.Type())
		}
		return toPlcValue(v.Elem())
	case reflect.Struct:
		members := make(map[string]apiValues.PlcValue)
		var order []string
		for _, f := range structFields(v.Type()) {
			member, err := toPlcValue(v.FieldByIndex(f.field.Index))
			if err != nil {
				return nil, fmt.Errorf("UDT %s: member %q: %w", v.Type(), f.member, err)
			}
			members[f.member] = member
			order = append(order, f.member)
		}
		return spiValues.NewPlcStructOrdered(members, order), nil
	}
	return nil, fmt.Errorf("no conversion from %s to a PLC4X value", v.Type())
}
