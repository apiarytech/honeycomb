/*
 * Copyright (C) 2026 Franklin D. Amador
 *
 * This software is dual-licensed under the terms of the GPL v3.0 and
 * a commercial license. You may choose to use this software under either
 * license.
 *
 * See the LICENSE files in the project root for full license text.
 */

package forward

import (
	"fmt"
	"math"
	"path"
	"reflect"

	"github.com/apiarytech/honeycomb"
)

// Deadband keeps a numeric tag's change only when it moves far enough from
// the last value kept. Without a deadband every change is kept.
type Deadband struct {
	// Tags is a pattern of tag names, as path.Match: '*' matches any run of
	// characters except '/', so "Tank1.*" matches "Tank1.Level". The first
	// deadband whose pattern matches a tag applies to it.
	Tags string `json:"tags"`
	// Absolute keeps a change that moves by more than this.
	Absolute float64 `json:"absolute,omitempty"`
	// Percent keeps a change that moves by more than this percentage of the
	// last value kept. With both, a change beyond either is kept. With
	// neither, every change of the matching tags is kept: a way to exempt
	// tags from a wider pattern after it.
	Percent float64 `json:"percent,omitempty"`
}

// checkDeadbands refuses an invalid pattern or a negative band.
func checkDeadbands(ds []Deadband) error {
	for i, d := range ds {
		if _, err := path.Match(d.Tags, ""); err != nil || d.Tags == "" {
			return fmt.Errorf("forward: deadband %d: pattern %q is invalid", i+1, d.Tags)
		}
		if d.Absolute < 0 || d.Percent < 0 || math.IsNaN(d.Absolute) || math.IsNaN(d.Percent) {
			return fmt.Errorf("forward: deadband %d (%s): a band is a positive number", i+1, d.Tags)
		}
	}
	return nil
}

// deadbandFor returns the deadband that applies to tag, if any.
func deadbandFor(ds []Deadband, tag string) (Deadband, bool) {
	for _, d := range ds {
		if ok, _ := path.Match(d.Tags, tag); ok {
			return d, d.Absolute > 0 || d.Percent > 0
		}
	}
	return Deadband{}, false
}

// kept is the last value kept for a tag.
type kept struct {
	num     float64
	numeric bool
	quality honeycomb.Quality
}

// beyond reports whether a change from prev to (v, q) is kept under d. A
// change of quality, and any change of a value that is not a number, is
// always kept.
func (d Deadband) beyond(prev kept, v any, q honeycomb.Quality) bool {
	num, ok := number(v)
	if q != prev.quality || !ok || !prev.numeric {
		return true
	}
	diff := math.Abs(num - prev.num)
	if math.IsNaN(diff) {
		return true // NaN to a number, or back
	}
	if d.Absolute > 0 && diff > d.Absolute {
		return true
	}
	return d.Percent > 0 && diff > d.Percent/100*math.Abs(prev.num)
}

// number returns an integer, float or boolean value (true is 1) as a
// float64, whatever its named type (plc.DINT, plc.LREAL, ...).
func number(v any) (float64, bool) {
	if v == nil {
		return 0, false
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return float64(rv.Int()), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return float64(rv.Uint()), true
	case reflect.Float32, reflect.Float64:
		return rv.Float(), true
	case reflect.Bool:
		if rv.Bool() {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}
