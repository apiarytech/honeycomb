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

// UDT tags served as OPC UA structures: the UDT's struct type gives the
// structure's fields, and values cross as map[string]any.

import (
	"fmt"
	"reflect"
	"time"

	"github.com/apiarytech/honeycomb"

	"github.com/apiarytech/honeycomb/connectors/plc4x/opcua"
)

var udtInterface = reflect.TypeOf((*honeycomb.UDT)(nil)).Elem()

// udtStruct is the struct type of a UDT value (a pointer to a struct that
// implements honeycomb.UDT); ok is false for any other value.
func udtStruct(v any) (reflect.Type, bool) {
	t := reflect.TypeOf(v)
	if t == nil || t.Kind() != reflect.Pointer || t.Elem().Kind() != reflect.Struct || !t.Implements(udtInterface) {
		return nil, false
	}
	return t.Elem(), true
}

// udtName is the data type name of a UDT struct type.
func udtName(t reflect.Type) string {
	return string(reflect.New(t).Interface().(honeycomb.UDT).TypeName())
}

// udtStructures adds the structure of a UDT struct type, and of the UDTs its
// fields hold, to structures (by name) and returns its name.
func udtStructures(t reflect.Type, structures map[string]opcua.Structure) (string, error) {
	name := udtName(t)
	if _, done := structures[name]; done {
		return name, nil
	}
	structures[name] = opcua.Structure{} // placeholder: a UDT containing itself stops here
	s := opcua.Structure{Name: name}
	for i := range t.NumField() {
		sf := t.Field(i)
		if !sf.IsExported() {
			continue
		}
		f := opcua.Field{Name: sf.Name}
		ft := sf.Type
		switch {
		case ft.Kind() == reflect.Pointer && ft.Implements(udtInterface) && ft.Elem().Kind() == reflect.Struct:
			nested, err := udtStructures(ft.Elem(), structures)
			if err != nil {
				return "", err
			}
			f.DataType, f.Structure = opcua.TypeStructure, nested
		case ft.Kind() == reflect.Array:
			f.ArrayLength = ft.Len()
			f.DataType = opcua.TypeOf(reflect.Zero(ft.Elem()).Interface())
		default:
			f.DataType = opcua.TypeOf(reflect.Zero(ft).Interface())
		}
		if f.DataType == 0 {
			return "", fmt.Errorf("UDT %s: field %s of type %s is not served", name, sf.Name, ft)
		}
		s.Fields = append(s.Fields, f)
	}
	if len(s.Fields) == 0 {
		return "", fmt.Errorf("UDT %s has no exported fields", name)
	}
	structures[name] = s
	return name, nil
}

// udtMap is a UDT value as a structure value.
func udtMap(v reflect.Value) map[string]any {
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}
	m := map[string]any{}
	for i := range v.NumField() {
		sf := v.Type().Field(i)
		if !sf.IsExported() {
			continue
		}
		f := v.Field(i)
		switch {
		case f.Kind() == reflect.Pointer && f.Type().Implements(udtInterface):
			if nested := udtMap(f); nested != nil {
				m[sf.Name] = nested
			}
		case f.Kind() == reflect.Array:
			list := reflect.MakeSlice(reflect.SliceOf(f.Type().Elem()), f.Len(), f.Len())
			reflect.Copy(list, f)
			m[sf.Name] = list.Interface()
		default:
			m[sf.Name] = f.Interface()
		}
	}
	return m
}

// udtValue is a structure value as a new UDT of struct type t.
func udtValue(t reflect.Type, m map[string]any) (any, error) {
	out := reflect.New(t)
	if err := fillUDT(out.Elem(), m); err != nil {
		return nil, err
	}
	return out.Interface(), nil
}

func fillUDT(v reflect.Value, m map[string]any) error {
	for i := range v.NumField() {
		sf := v.Type().Field(i)
		if !sf.IsExported() {
			continue
		}
		value, ok := m[sf.Name]
		if !ok {
			continue
		}
		f := v.Field(i)
		switch {
		case f.Kind() == reflect.Pointer && f.Type().Implements(udtInterface):
			nested, ok := value.(map[string]any)
			if !ok {
				return fmt.Errorf("%s: not a structure", sf.Name)
			}
			p := reflect.New(f.Type().Elem())
			if err := fillUDT(p.Elem(), nested); err != nil {
				return fmt.Errorf("%s.%w", sf.Name, err)
			}
			f.Set(p)
		case f.Kind() == reflect.Array:
			list := reflect.ValueOf(value)
			if list.Kind() != reflect.Slice || list.Len() != f.Len() {
				return fmt.Errorf("%s: %d elements, not %v", sf.Name, f.Len(), value)
			}
			for j := range f.Len() {
				if err := assign(f.Index(j), list.Index(j)); err != nil {
					return fmt.Errorf("%s[%d]: %w", sf.Name, j, err)
				}
			}
		default:
			if err := assign(f, reflect.ValueOf(value)); err != nil {
				return fmt.Errorf("%s: %w", sf.Name, err)
			}
		}
	}
	return nil
}

var goTimeType = reflect.TypeOf(time.Time{})

// assign sets a field from a decoded value, converting between kinds of
// number, string, bool and time.
func assign(dst, src reflect.Value) error {
	if !src.IsValid() {
		return nil
	}
	if src.Kind() == reflect.Interface {
		src = src.Elem()
	}
	switch {
	case src.Type().ConvertibleTo(dst.Type()) && kindClass(src.Kind()) == kindClass(dst.Kind()):
		dst.Set(src.Convert(dst.Type()))
	case src.Type() == goTimeType && dst.Type().ConvertibleTo(goTimeType):
		dst.Set(src.Convert(dst.Type()))
	default:
		return fmt.Errorf("a %s does not go into a %s", src.Type(), dst.Type())
	}
	return nil
}

// kindClass groups kinds that convert into each other without surprise.
func kindClass(k reflect.Kind) int {
	switch k {
	case reflect.Bool:
		return 1
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Float32, reflect.Float64:
		return 2
	case reflect.String:
		return 3
	case reflect.Struct:
		return 4
	}
	return 0
}
