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
	"reflect"
	"strconv"
	"strings"
)

const (
	statusBadIndexRangeNoData = 0x80370000
)

// indexRange is a NumericRange (OPC UA Part 4, 7.27): "3", "1:4", or for an array of strings
// "1:4,0:2", the second dimension taking characters of each string.
type indexRange struct {
	dims [][2]int // first and last index, inclusive
}

// parseIndexRange reads a range; nil for none. ok is false for one that does not parse.
func parseIndexRange(s string) (r *indexRange, ok bool) {
	if s == "" {
		return nil, true
	}
	r = &indexRange{}
	for _, part := range strings.Split(s, ",") {
		first, last, isRange := strings.Cut(part, ":")
		a, err := strconv.Atoi(first)
		if err != nil || a < 0 {
			return nil, false
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(last); err != nil || b <= a {
				return nil, false
			}
		}
		r.dims = append(r.dims, [2]int{a, b})
	}
	if len(r.dims) > 2 {
		return nil, false
	}
	return r, true
}

// apply is the part of a value the range selects: elements of an array, characters of a
// string, characters of each selected string of an array of strings.
func (r *indexRange) apply(v any) (any, uint32) {
	return r.applyDim(reflect.ValueOf(v), 0)
}

func (r *indexRange) applyDim(v reflect.Value, dim int) (any, uint32) {
	first, last := r.dims[dim][0], r.dims[dim][1]
	switch {
	case v.Kind() == reflect.String:
		if dim != len(r.dims)-1 {
			return nil, statusBadIndexRangeNoData
		}
		runes := []rune(v.String())
		if first >= len(runes) {
			return nil, statusBadIndexRangeNoData
		}
		return string(runes[first : min(last, len(runes)-1)+1]), statusGood
	case v.Kind() == reflect.Slice:
		if first >= v.Len() {
			return nil, statusBadIndexRangeNoData
		}
		part := v.Slice(first, min(last, v.Len()-1)+1)
		if dim == len(r.dims)-1 {
			out := reflect.MakeSlice(part.Type(), part.Len(), part.Len())
			reflect.Copy(out, part)
			return out.Interface(), statusGood
		}
		out := reflect.MakeSlice(part.Type(), part.Len(), part.Len())
		for i := range part.Len() {
			inner, status := r.applyDim(part.Index(i), dim+1)
			if status != statusGood {
				return nil, status
			}
			out.Index(i).Set(reflect.ValueOf(inner))
		}
		return out.Interface(), statusGood
	}
	return nil, statusBadIndexRangeNoData
}

// splice writes part into the range of current: the elements (or characters) the range
// selects become part's, which must have as many as the range.
func (r *indexRange) splice(current, part any) (any, uint32) {
	if len(r.dims) != 1 {
		return nil, statusBadIndexRangeInvalid
	}
	first, last := r.dims[0][0], r.dims[0][1]
	cv, pv := reflect.ValueOf(current), reflect.ValueOf(part)
	switch {
	case cv.Kind() == reflect.String && pv.Kind() == reflect.String:
		runes, with := []rune(cv.String()), []rune(pv.String())
		if last >= len(runes) {
			return nil, statusBadIndexRangeNoData
		}
		if len(with) != last-first+1 {
			return nil, statusBadIndexRangeInvalid
		}
		out := append(append(append([]rune{}, runes[:first]...), with...), runes[last+1:]...)
		return string(out), statusGood
	case cv.Kind() == reflect.Slice && pv.Kind() == reflect.Slice && cv.Type() == pv.Type():
		if last >= cv.Len() {
			return nil, statusBadIndexRangeNoData
		}
		if pv.Len() != last-first+1 {
			return nil, statusBadIndexRangeInvalid
		}
		out := reflect.MakeSlice(cv.Type(), cv.Len(), cv.Len())
		reflect.Copy(out, cv)
		reflect.Copy(out.Slice(first, last+1), pv)
		return out.Interface(), statusGood
	}
	return nil, statusBadTypeMismatch
}
