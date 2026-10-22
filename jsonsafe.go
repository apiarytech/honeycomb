/*
 * Copyright (C) 2026 Franklin D. Amador
 *
 * This software is dual-licensed under the terms of the GPL v3.0 and
 * a commercial license. You may choose to use this software under either
 * license.
 *
 * See the LICENSE files in the project root for full license text.
 */

package honeycomb

import (
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"reflect"
	"strings"
)

// writeJSON encodes v before writing anything, so an encoding failure can
// still be reported with a proper status instead of a 200 with an empty body.
//
// encoding/json can't encode NaN or ±Inf. When v holds such a value (a REAL
// tag set to NaN, for example), v is encoded again with every non-finite
// float written as null, so clients still receive valid JSON and a remote
// change-feed poll doesn't fail until the value scrolls out of the feed.
func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	var unsupported *json.UnsupportedValueError
	if errors.As(err, &unsupported) {
		body, err = json.Marshal(jsonSafe(reflect.ValueOf(v)))
	}
	if err != nil {
		http.Error(w, "could not encode the response: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}

var (
	jsonMarshalerType = reflect.TypeOf((*json.Marshaler)(nil)).Elem()
	textMarshalerType = reflect.TypeOf((*encoding.TextMarshaler)(nil)).Elem()
)

// jsonSafe returns a copy of v, built from maps, slices and plain values,
// that encodes as encoding/json would encode v except that every NaN or ±Inf
// float becomes null. Types with their own JSON or text encoding are kept.
func jsonSafe(v reflect.Value) any {
	if !v.IsValid() {
		return nil
	}
	if (v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface) && v.IsNil() {
		return nil
	}
	if v.CanInterface() && v.Kind() != reflect.Interface &&
		(v.Type().Implements(jsonMarshalerType) || v.Type().Implements(textMarshalerType)) {
		return v.Interface()
	}
	switch v.Kind() {
	case reflect.Float32, reflect.Float64:
		if f := v.Float(); math.IsNaN(f) || math.IsInf(f, 0) {
			return nil
		}
		return v.Interface()
	case reflect.Pointer, reflect.Interface:
		return jsonSafe(v.Elem())
	case reflect.Slice:
		if v.IsNil() {
			return nil
		}
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return v.Interface() // []byte encodes as base64
		}
		fallthrough
	case reflect.Array:
		out := make([]any, v.Len())
		for i := range out {
			out[i] = jsonSafe(v.Index(i))
		}
		return out
	case reflect.Map:
		if v.IsNil() {
			return nil
		}
		out := make(map[string]any, v.Len())
		for iter := v.MapRange(); iter.Next(); {
			out[fmt.Sprint(iter.Key().Interface())] = jsonSafe(iter.Value())
		}
		return out
	case reflect.Struct:
		out := map[string]any{}
		addStructFields(out, v)
		return out
	}
	if v.CanInterface() {
		return v.Interface()
	}
	return nil
}

// addStructFields adds v's exported fields to out under their JSON names,
// following the json tag's name, "-" and omitempty, and flattening embedded
// structs that have no tag.
func addStructFields(out map[string]any, v reflect.Value) {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		tag := field.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		fv := v.Field(i)
		if field.Anonymous && name == "" {
			ft := field.Type
			if ft.Kind() == reflect.Pointer {
				if fv.IsNil() {
					continue
				}
				fv, ft = fv.Elem(), ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				addStructFields(out, fv)
				continue
			}
		}
		if !field.IsExported() {
			continue
		}
		if name == "" {
			name = field.Name
		}
		if strings.Contains(","+opts+",", ",omitempty,") && isEmptyJSONValue(fv) {
			continue
		}
		if strings.Contains(","+opts+",", ",omitzero,") && fv.IsZero() {
			continue
		}
		out[name] = jsonSafe(fv)
	}
}

// isEmptyJSONValue reports whether encoding/json's omitempty would drop v.
func isEmptyJSONValue(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Array, reflect.Map, reflect.Slice, reflect.String:
		return v.Len() == 0
	case reflect.Bool:
		return !v.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int() == 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return v.Uint() == 0
	case reflect.Float32, reflect.Float64:
		return v.Float() == 0
	case reflect.Interface, reflect.Pointer:
		return v.IsNil()
	}
	return false
}
