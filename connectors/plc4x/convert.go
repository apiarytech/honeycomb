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

	apiValues "github.com/apache/plc4x/plc4go/pkg/api/values"
	plc "github.com/apiarytech/royaljelly"
)

// converters turn a PLC4X value into the royaljelly type a tag holds.
var converters = map[reflect.Type]func(apiValues.PlcValue) any{
	reflect.TypeFor[plc.BOOL]():   func(v apiValues.PlcValue) any { return plc.BOOL(v.GetBool()) },
	reflect.TypeFor[plc.BYTE]():   func(v apiValues.PlcValue) any { return plc.BYTE(v.GetUint8()) },
	reflect.TypeFor[plc.WORD]():   func(v apiValues.PlcValue) any { return plc.WORD(v.GetUint16()) },
	reflect.TypeFor[plc.DWORD]():  func(v apiValues.PlcValue) any { return plc.DWORD(v.GetUint32()) },
	reflect.TypeFor[plc.LWORD]():  func(v apiValues.PlcValue) any { return plc.LWORD(v.GetUint64()) },
	reflect.TypeFor[plc.SINT]():   func(v apiValues.PlcValue) any { return plc.SINT(v.GetInt8()) },
	reflect.TypeFor[plc.INT]():    func(v apiValues.PlcValue) any { return plc.INT(v.GetInt16()) },
	reflect.TypeFor[plc.DINT]():   func(v apiValues.PlcValue) any { return plc.DINT(v.GetInt32()) },
	reflect.TypeFor[plc.LINT]():   func(v apiValues.PlcValue) any { return plc.LINT(v.GetInt64()) },
	reflect.TypeFor[plc.USINT]():  func(v apiValues.PlcValue) any { return plc.USINT(v.GetUint8()) },
	reflect.TypeFor[plc.UINT]():   func(v apiValues.PlcValue) any { return plc.UINT(v.GetUint16()) },
	reflect.TypeFor[plc.UDINT]():  func(v apiValues.PlcValue) any { return plc.UDINT(v.GetUint32()) },
	reflect.TypeFor[plc.ULINT]():  func(v apiValues.PlcValue) any { return plc.ULINT(v.GetUint64()) },
	reflect.TypeFor[plc.REAL]():   func(v apiValues.PlcValue) any { return plc.REAL(v.GetFloat32()) },
	reflect.TypeFor[plc.LREAL]():  func(v apiValues.PlcValue) any { return plc.LREAL(v.GetFloat64()) },
	reflect.TypeFor[plc.STRING](): func(v apiValues.PlcValue) any { return plc.STRING(v.GetString()) },
	reflect.TypeFor[plc.TIME]():   func(v apiValues.PlcValue) any { return plc.TIME(v.GetDuration()) },
	reflect.TypeFor[plc.DATE]():   func(v apiValues.PlcValue) any { return plc.DATE(v.GetDate()) },
	reflect.TypeFor[plc.TOD]():    func(v apiValues.PlcValue) any { return plc.TOD(v.GetTime()) },
	reflect.TypeFor[plc.DT]():     func(v apiValues.PlcValue) any { return plc.DT(v.GetDateTime()) },
	reflect.TypeFor[string]():     func(v apiValues.PlcValue) any { return v.GetString() }, // ENUM tags
}

// store converts value to the Go type of the tag's current value and writes it.
func (c *Connector) store(tagName string, value apiValues.PlcValue) error {
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
	return c.db.SetTagValue(tagName, converted)
}

// convert turns a PLC4X value into a value of type target. Slices map to PLC4X lists.
func convert(target reflect.Type, value apiValues.PlcValue) (any, error) {
	if value == nil || value.IsNull() {
		return nil, fmt.Errorf("device returned no value")
	}
	if fn, ok := converters[target]; ok {
		return fn(value), nil
	}
	if target.Kind() == reflect.Slice {
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
	}
	return nil, fmt.Errorf("no conversion from PLC4X %s to %s", value.GetPlcValueType(), target)
}
