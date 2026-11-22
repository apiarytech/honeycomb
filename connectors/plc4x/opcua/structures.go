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
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"reflect"
	"time"

	ua "github.com/apache/plc4x/plc4go/protocols/opcua/readwrite/model"
)

// structureType is a Structure of the Config with its nodes' ids: a DataType node with a
// DataTypeDefinition, and its Default Binary encoding.
type structureType struct {
	cfg      Structure
	id       ua.NodeId
	encoding ua.NodeId
	fields   []structField
}

type structField struct {
	name        string
	description string
	dataType    uint32 // a built-in type, or TypeStructure
	structure   *structureType
	arrayLength int
}

// newStructures checks the structures and resolves the ones their fields name.
func newStructures(list []Structure) ([]*structureType, map[string]*structureType, error) {
	byName := map[string]*structureType{}
	var out []*structureType
	for _, s := range list {
		if s.Name == "" {
			return nil, nil, errors.New("a structure without a name")
		}
		if _, dup := byName[s.Name]; dup {
			return nil, nil, fmt.Errorf("two structures named %s", s.Name)
		}
		if len(s.Fields) == 0 {
			return nil, nil, fmt.Errorf("structure %s has no fields", s.Name)
		}
		t := &structureType{cfg: s, id: stringNodeId(1, "type:"+s.Name), encoding: stringNodeId(1, "encoding:"+s.Name)}
		byName[s.Name] = t
		out = append(out, t)
	}
	for _, t := range out {
		seen := map[string]bool{}
		for _, f := range t.cfg.Fields {
			if f.Name == "" || seen[f.Name] {
				return nil, nil, fmt.Errorf("structure %s: field %q is empty or repeated", t.cfg.Name, f.Name)
			}
			seen[f.Name] = true
			sf := structField{name: f.Name, description: f.Description, dataType: f.DataType, arrayLength: f.ArrayLength}
			switch {
			case f.DataType == TypeStructure:
				sf.structure = byName[f.Structure]
				if sf.structure == nil {
					return nil, nil, fmt.Errorf("structure %s: field %s is of structure %q, which is not given", t.cfg.Name, f.Name, f.Structure)
				}
				if f.ArrayLength > 0 {
					return nil, nil, fmt.Errorf("structure %s: field %s: arrays of structures are not served", t.cfg.Name, f.Name)
				}
			case f.DataType < TypeBoolean || f.DataType > TypeDateTime:
				return nil, nil, fmt.Errorf("structure %s: field %s: data type %d is not served", t.cfg.Name, f.Name, f.DataType)
			}
			t.fields = append(t.fields, sf)
		}
	}
	for _, t := range out {
		if err := t.checkCycles(nil); err != nil {
			return nil, nil, err
		}
	}
	return out, byName, nil
}

func (t *structureType) checkCycles(path []string) error {
	for _, p := range path {
		if p == t.cfg.Name {
			return fmt.Errorf("structure %s contains itself", t.cfg.Name)
		}
	}
	for _, f := range t.fields {
		if f.structure != nil {
			if err := f.structure.checkCycles(append(path, t.cfg.Name)); err != nil {
				return err
			}
		}
	}
	return nil
}

// definitionVariant is the DataTypeDefinition attribute: a StructureDefinition.
func (t *structureType) definitionVariant() ua.Variant {
	fields := make([]ua.StructureField, len(t.fields))
	for i, f := range t.fields {
		dataType := numericNodeId(0, f.dataType)
		if f.structure != nil {
			dataType = f.structure.id
		}
		valueRank, dims := int32(-1), []uint32(nil)
		if f.arrayLength > 0 {
			valueRank, dims = 1, []uint32{uint32(f.arrayLength)}
		}
		fields[i] = ua.NewStructureField(str(f.name), text(f.description), dataType, valueRank, dims, 0, false)
	}
	definition := ua.NewStructureDefinition(t.encoding, numericNodeId(0, idStructure), ua.StructureType_structureTypeStructure, fields)
	return ua.NewVariantExtensionObject(false, false, nil, nil, nil, []ua.ExtensionObject{extensionObject(definition, idStructureDefinitionBin)})
}

// variant encodes a structure value, a map of field names to values, as an ExtensionObject
// of the structure's binary encoding.
func (t *structureType) variant(v any) (ua.Variant, error) {
	body, err := t.encode(nil, v)
	if err != nil {
		return nil, err
	}
	object := ua.NewRawBinaryExtensionObjectWithMask(ua.NewExpandedNodeId(false, false, t.encoding.GetNodeId(), nil, nil),
		ua.NewExtensionObjectEncodingMask(false, false, true), body)
	return ua.NewVariantExtensionObject(false, false, nil, nil, nil, []ua.ExtensionObject{object}), nil
}

// valueOf decodes a written Variant of the structure.
func (t *structureType) valueOf(variant ua.Variant) (map[string]any, error) {
	objects, ok := variant.(ua.VariantExtensionObject)
	if !ok || len(objects.GetValue()) != 1 {
		return nil, errors.New("not one ExtensionObject")
	}
	raw, ok := objects.GetValue()[0].(ua.RawBinaryExtensionObjectWithMask)
	if !ok || keyOfDefinition(raw.GetTypeId().GetNodeId()) != keyOf(t.encoding) {
		return nil, fmt.Errorf("not a %s", t.cfg.Name)
	}
	value, rest, err := t.decode(raw.GetRawBody())
	if err != nil {
		return nil, err
	}
	if len(rest) != 0 {
		return nil, fmt.Errorf("%d bytes past the end of a %s", len(rest), t.cfg.Name)
	}
	return value, nil
}

func (t *structureType) encode(out []byte, v any) ([]byte, error) {
	m, ok := v.(map[string]any)
	if !ok && v != nil {
		return nil, fmt.Errorf("a %s is a map of its fields, not %T", t.cfg.Name, v)
	}
	for _, f := range t.fields {
		value := m[f.name]
		var err error
		switch {
		case f.structure != nil:
			out, err = f.structure.encode(out, value)
		case f.arrayLength > 0:
			list := reflect.ValueOf(value)
			if value == nil {
				list = reflect.ValueOf([]any{})
			}
			if list.Kind() != reflect.Slice && list.Kind() != reflect.Array {
				return nil, fmt.Errorf("%s.%s is an array, not %T", t.cfg.Name, f.name, value)
			}
			out = binary.LittleEndian.AppendUint32(out, uint32(list.Len()))
			for i := range list.Len() {
				if out, err = encodeScalar(out, list.Index(i).Interface(), f.dataType); err != nil {
					break
				}
			}
		default:
			out, err = encodeScalar(out, value, f.dataType)
		}
		if err != nil {
			return nil, fmt.Errorf("%s.%s: %w", t.cfg.Name, f.name, err)
		}
	}
	return out, nil
}

func (t *structureType) decode(in []byte) (map[string]any, []byte, error) {
	m := make(map[string]any, len(t.fields))
	for _, f := range t.fields {
		var err error
		switch {
		case f.structure != nil:
			m[f.name], in, err = f.structure.decode(in)
		case f.arrayLength > 0:
			if len(in) < 4 {
				return nil, nil, fmt.Errorf("%s.%s: too short", t.cfg.Name, f.name)
			}
			n := int32(binary.LittleEndian.Uint32(in))
			in = in[4:]
			if n < 0 {
				n = 0
			}
			if int(n) > len(in) {
				return nil, nil, fmt.Errorf("%s.%s: %d elements in %d bytes", t.cfg.Name, f.name, n, len(in))
			}
			list := reflect.MakeSlice(reflect.SliceOf(goType(f.dataType)), int(n), int(n))
			for i := range int(n) {
				var e any
				if e, in, err = decodeScalar(in, f.dataType); err != nil {
					break
				}
				list.Index(i).Set(reflect.ValueOf(e))
			}
			m[f.name] = list.Interface()
		default:
			m[f.name], in, err = decodeScalar(in, f.dataType)
		}
		if err != nil {
			return nil, nil, fmt.Errorf("%s.%s: %w", t.cfg.Name, f.name, err)
		}
	}
	return m, in, nil
}

// goType is the Go type a built-in type decodes to.
func goType(dataType uint32) reflect.Type {
	switch dataType {
	case TypeBoolean:
		return reflect.TypeOf(false)
	case TypeSByte:
		return reflect.TypeOf(int8(0))
	case TypeByte:
		return reflect.TypeOf(uint8(0))
	case TypeInt16:
		return reflect.TypeOf(int16(0))
	case TypeUInt16:
		return reflect.TypeOf(uint16(0))
	case TypeInt32:
		return reflect.TypeOf(int32(0))
	case TypeUInt32:
		return reflect.TypeOf(uint32(0))
	case TypeInt64:
		return reflect.TypeOf(int64(0))
	case TypeUInt64:
		return reflect.TypeOf(uint64(0))
	case TypeFloat:
		return reflect.TypeOf(float32(0))
	case TypeDouble:
		return reflect.TypeOf(float64(0))
	case TypeString:
		return reflect.TypeOf("")
	case TypeDateTime:
		return timeType
	}
	return reflect.TypeOf((*any)(nil)).Elem()
}

// encodeScalar appends the binary encoding of a built-in scalar (OPC UA Part 6, 5.2.2),
// converting numbers as variantOf does.
func encodeScalar(out []byte, v any, dataType uint32) ([]byte, error) {
	if v == nil {
		v = reflect.Zero(goType(dataType)).Interface()
	}
	variant, err := variantOf(v, dataType)
	if err != nil {
		return nil, err
	}
	value, _, err := valueOf(variant)
	if err != nil {
		return nil, err
	}
	le := binary.LittleEndian
	switch x := value.(type) {
	case bool:
		if x {
			return append(out, 1), nil
		}
		return append(out, 0), nil
	case int8:
		return append(out, byte(x)), nil
	case uint8:
		return append(out, x), nil
	case int16:
		return le.AppendUint16(out, uint16(x)), nil
	case uint16:
		return le.AppendUint16(out, x), nil
	case int32:
		return le.AppendUint32(out, uint32(x)), nil
	case uint32:
		return le.AppendUint32(out, x), nil
	case int64:
		return le.AppendUint64(out, uint64(x)), nil
	case uint64:
		return le.AppendUint64(out, x), nil
	case float32:
		return le.AppendUint32(out, math.Float32bits(x)), nil
	case float64:
		return le.AppendUint64(out, math.Float64bits(x)), nil
	case string:
		return append(le.AppendUint32(out, uint32(len(x))), x...), nil
	case time.Time:
		return le.AppendUint64(out, uint64(dateTime(x))), nil
	}
	return nil, fmt.Errorf("a %T is not a scalar of data type %d", value, dataType)
}

func decodeScalar(in []byte, dataType uint32) (any, []byte, error) {
	need := map[uint32]int{TypeBoolean: 1, TypeSByte: 1, TypeByte: 1, TypeInt16: 2, TypeUInt16: 2, TypeInt32: 4, TypeUInt32: 4,
		TypeInt64: 8, TypeUInt64: 8, TypeFloat: 4, TypeDouble: 8, TypeString: 4, TypeDateTime: 8}[dataType]
	if need == 0 {
		return nil, nil, fmt.Errorf("data type %d", dataType)
	}
	if len(in) < need {
		return nil, nil, errors.New("too short")
	}
	le := binary.LittleEndian
	switch dataType {
	case TypeBoolean:
		return in[0] != 0, in[1:], nil
	case TypeSByte:
		return int8(in[0]), in[1:], nil
	case TypeByte:
		return in[0], in[1:], nil
	case TypeInt16:
		return int16(le.Uint16(in)), in[2:], nil
	case TypeUInt16:
		return le.Uint16(in), in[2:], nil
	case TypeInt32:
		return int32(le.Uint32(in)), in[4:], nil
	case TypeUInt32:
		return le.Uint32(in), in[4:], nil
	case TypeInt64:
		return int64(le.Uint64(in)), in[8:], nil
	case TypeUInt64:
		return le.Uint64(in), in[8:], nil
	case TypeFloat:
		return math.Float32frombits(le.Uint32(in)), in[4:], nil
	case TypeDouble:
		return math.Float64frombits(le.Uint64(in)), in[8:], nil
	case TypeString:
		n := int32(le.Uint32(in))
		in = in[4:]
		if n < 0 {
			return "", in, nil
		}
		if int(n) > len(in) {
			return nil, nil, errors.New("string past the end")
		}
		return string(in[:n]), in[n:], nil
	}
	return timeOf(int64(le.Uint64(in))), in[8:], nil
}

// member is the value at a path of fields in a structure value; ok is false when it is not
// there.
func member(v any, path []string) (any, bool) {
	for _, name := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, false
		}
		if v, ok = m[name]; !ok {
			return nil, false
		}
	}
	return v, true
}

// withMember is a copy of a structure value with the value at path replaced.
func withMember(v any, path []string, value any) (map[string]any, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("not a structure value")
	}
	out := make(map[string]any, len(m))
	for k, x := range m {
		out[k] = x
	}
	if len(path) == 1 {
		out[path[0]] = value
		return out, nil
	}
	inner, err := withMember(m[path[0]], path[1:], value)
	if err != nil {
		return nil, err
	}
	out[path[0]] = inner
	return out, nil
}
