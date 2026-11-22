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
	"reflect"
	"time"

	ua "github.com/apache/plc4x/plc4go/protocols/opcua/readwrite/model"
)

// Built-in data types (OPC UA Part 6, 5.1.2): the numbers of their DataType nodes in
// namespace 0, and of their Variant encodings.
const (
	TypeBoolean  = 1
	TypeSByte    = 2
	TypeByte     = 3
	TypeInt16    = 4
	TypeUInt16   = 5
	TypeInt32    = 6
	TypeUInt32   = 7
	TypeInt64    = 8
	TypeUInt64   = 9
	TypeFloat    = 10
	TypeDouble   = 11
	TypeString   = 12
	TypeDateTime = 13
)

// typeNames names the data types a binding may give.
var typeNames = map[string]uint32{
	"BOOLEAN": TypeBoolean, "BOOL": TypeBoolean, "SBYTE": TypeSByte, "SINT": TypeSByte,
	"BYTE": TypeByte, "USINT": TypeByte, "INT16": TypeInt16, "INT": TypeInt16,
	"UINT16": TypeUInt16, "UINT": TypeUInt16, "WORD": TypeUInt16, "INT32": TypeInt32, "DINT": TypeInt32,
	"UINT32": TypeUInt32, "UDINT": TypeUInt32, "DWORD": TypeUInt32, "INT64": TypeInt64, "LINT": TypeInt64,
	"UINT64": TypeUInt64, "ULINT": TypeUInt64, "LWORD": TypeUInt64, "FLOAT": TypeFloat, "REAL": TypeFloat,
	"DOUBLE": TypeDouble, "LREAL": TypeDouble, "STRING": TypeString, "WSTRING": TypeString, "DATETIME": TypeDateTime, "DATE_AND_TIME": TypeDateTime, "DT": TypeDateTime,
}

// TypeByName is the built-in data type a name stands for: an OPC UA name (DOUBLE) or an
// IEC 61131-3 one (LREAL).
func TypeByName(name string) (uint32, bool) {
	t, ok := typeNames[name]
	return t, ok
}

// TypeOf is the built-in data type of a Go value (a slice counts as its elements), 0 when it
// has none.
func TypeOf(v any) uint32 {
	t := reflect.TypeOf(v)
	if t == nil {
		return 0
	}
	if t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
		t = t.Elem()
	}
	if t.Kind() == reflect.Struct && t.ConvertibleTo(timeType) {
		return TypeDateTime // time.Time, and types made of it (IEC DT)
	}
	switch t.Kind() {
	case reflect.Bool:
		return TypeBoolean
	case reflect.Int8:
		return TypeSByte
	case reflect.Uint8:
		return TypeByte
	case reflect.Int16:
		return TypeInt16
	case reflect.Uint16:
		return TypeUInt16
	case reflect.Int32, reflect.Int:
		return TypeInt32
	case reflect.Uint32, reflect.Uint:
		return TypeUInt32
	case reflect.Int64:
		return TypeInt64
	case reflect.Uint64:
		return TypeUInt64
	case reflect.Float32:
		return TypeFloat
	case reflect.Float64:
		return TypeDouble
	case reflect.String:
		return TypeString
	}
	return 0
}

var timeType = reflect.TypeOf(time.Time{})

// epochOffset is 1601-01-01 to 1970-01-01 in 100 ns ticks, the start of an OPC UA DateTime.
const epochOffset = 116444736000000000

func dateTime(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()/100 + epochOffset
}

func timeOf(ticks int64) time.Time {
	if ticks == 0 {
		return time.Time{}
	}
	return time.Unix(0, (ticks-epochOffset)*100).UTC()
}

// elements is a value as a list: its elements for an array, itself otherwise.
func elements(v any) (list []reflect.Value, array bool) {
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array {
		list = make([]reflect.Value, rv.Len())
		for i := range list {
			list[i] = rv.Index(i)
		}
		return list, true
	}
	return []reflect.Value{rv}, false
}

// variantOf encodes a Go value as a Variant of the built-in type dataType, converting
// numbers between types. A slice is an array.
func variantOf(v any, dataType uint32) (variant ua.Variant, err error) {
	defer func() {
		if r := recover(); r != nil {
			variant, err = nil, fmt.Errorf("cannot encode %T as data type %d: %v", v, dataType, r)
		}
	}()
	list, array := elements(v)
	var length *int32
	if array {
		n := int32(len(list))
		length = &n
	}
	num := func(e reflect.Value) float64 {
		switch e.Kind() {
		case reflect.Bool:
			if e.Bool() {
				return 1
			}
			return 0
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			return float64(e.Int())
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			return float64(e.Uint())
		case reflect.Float32, reflect.Float64:
			return e.Float()
		}
		panic(fmt.Sprintf("%s is not a number", e.Type()))
	}
	signed := func(e reflect.Value) int64 {
		switch e.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			return e.Int()
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			return int64(e.Uint())
		}
		return int64(num(e))
	}
	unsigned := func(e reflect.Value) uint64 {
		switch e.Kind() {
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			return e.Uint()
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			return uint64(e.Int())
		}
		return uint64(num(e))
	}
	switch dataType {
	case TypeBoolean:
		out := make([]byte, len(list))
		for i, e := range list {
			if num(e) != 0 {
				out[i] = 1
			}
		}
		return ua.NewVariantBoolean(array, false, nil, nil, length, out), nil
	case TypeSByte:
		out := make([]byte, len(list))
		for i, e := range list {
			out[i] = byte(int8(signed(e)))
		}
		return ua.NewVariantSByte(array, false, nil, nil, length, out), nil
	case TypeByte:
		out := make([]uint8, len(list))
		for i, e := range list {
			out[i] = uint8(unsigned(e))
		}
		return ua.NewVariantByte(array, false, nil, nil, length, out), nil
	case TypeInt16:
		out := make([]int16, len(list))
		for i, e := range list {
			out[i] = int16(signed(e))
		}
		return ua.NewVariantInt16(array, false, nil, nil, length, out), nil
	case TypeUInt16:
		out := make([]uint16, len(list))
		for i, e := range list {
			out[i] = uint16(unsigned(e))
		}
		return ua.NewVariantUInt16(array, false, nil, nil, length, out), nil
	case TypeInt32:
		out := make([]int32, len(list))
		for i, e := range list {
			out[i] = int32(signed(e))
		}
		return ua.NewVariantInt32(array, false, nil, nil, length, out), nil
	case TypeUInt32:
		out := make([]uint32, len(list))
		for i, e := range list {
			out[i] = uint32(unsigned(e))
		}
		return ua.NewVariantUInt32(array, false, nil, nil, length, out), nil
	case TypeInt64:
		out := make([]int64, len(list))
		for i, e := range list {
			out[i] = signed(e)
		}
		return ua.NewVariantInt64(array, false, nil, nil, length, out), nil
	case TypeUInt64:
		out := make([]uint64, len(list))
		for i, e := range list {
			out[i] = unsigned(e)
		}
		return ua.NewVariantUInt64(array, false, nil, nil, length, out), nil
	case TypeFloat:
		out := make([]float32, len(list))
		for i, e := range list {
			out[i] = float32(num(e))
		}
		return ua.NewVariantFloat(array, false, nil, nil, length, out), nil
	case TypeDouble:
		out := make([]float64, len(list))
		for i, e := range list {
			out[i] = num(e)
		}
		return ua.NewVariantDouble(array, false, nil, nil, length, out), nil
	case TypeString:
		out := make([]ua.PascalString, len(list))
		for i, e := range list {
			var s string
			if e.Kind() == reflect.String {
				s = e.String() // a named string type too, whatever its String method says
			} else {
				s = fmt.Sprint(e.Interface())
			}
			out[i] = ua.NewPascalString(&s)
		}
		return ua.NewVariantString(array, false, nil, nil, length, out), nil
	case TypeDateTime:
		out := make([]int64, len(list))
		for i, e := range list {
			if e.Kind() != reflect.Struct || !e.Type().ConvertibleTo(timeType) {
				return nil, fmt.Errorf("%s is not a time", e.Type())
			}
			out[i] = dateTime(e.Convert(timeType).Interface().(time.Time))
		}
		return ua.NewVariantDateTime(array, false, nil, nil, length, out), nil
	}
	return nil, fmt.Errorf("data type %d is not served", dataType)
}

// valueOf decodes a Variant into a Go value: a scalar, or a slice for an array.
func valueOf(variant ua.Variant) (any, uint32, error) {
	one := func(list any, n int) any {
		if n == 1 && !variant.GetArrayLengthSpecified() {
			return reflect.ValueOf(list).Index(0).Interface()
		}
		return list
	}
	switch v := variant.(type) {
	case ua.VariantBoolean:
		out := make([]bool, len(v.GetValue()))
		for i, b := range v.GetValue() {
			out[i] = b != 0
		}
		return one(out, len(out)), TypeBoolean, nil
	case ua.VariantSByte:
		out := make([]int8, len(v.GetValue()))
		for i, b := range v.GetValue() {
			out[i] = int8(b)
		}
		return one(out, len(out)), TypeSByte, nil
	case ua.VariantByte:
		return one(append([]uint8(nil), v.GetValue()...), len(v.GetValue())), TypeByte, nil
	case ua.VariantInt16:
		return one(v.GetValue(), len(v.GetValue())), TypeInt16, nil
	case ua.VariantUInt16:
		return one(v.GetValue(), len(v.GetValue())), TypeUInt16, nil
	case ua.VariantInt32:
		return one(v.GetValue(), len(v.GetValue())), TypeInt32, nil
	case ua.VariantUInt32:
		return one(v.GetValue(), len(v.GetValue())), TypeUInt32, nil
	case ua.VariantInt64:
		return one(v.GetValue(), len(v.GetValue())), TypeInt64, nil
	case ua.VariantUInt64:
		return one(v.GetValue(), len(v.GetValue())), TypeUInt64, nil
	case ua.VariantFloat:
		return one(v.GetValue(), len(v.GetValue())), TypeFloat, nil
	case ua.VariantDouble:
		return one(v.GetValue(), len(v.GetValue())), TypeDouble, nil
	case ua.VariantString:
		out := make([]string, len(v.GetValue()))
		for i, s := range v.GetValue() {
			if s != nil && s.GetStringValue() != nil {
				out[i] = *s.GetStringValue()
			}
		}
		return one(out, len(out)), TypeString, nil
	case ua.VariantDateTime:
		out := make([]time.Time, len(v.GetValue()))
		for i, t := range v.GetValue() {
			out[i] = timeOf(t)
		}
		return one(out, len(out)), TypeDateTime, nil
	}
	return nil, 0, fmt.Errorf("a %T is not a value this server takes", variant)
}
