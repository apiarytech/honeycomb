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
	"regexp"
	"strings"
)

// noSize matches a bit address written without its size, %I0.1, which IEC
// 61131-3 allows for %IX0.1.
var noSize = regexp.MustCompile(`^%([IQM])(\d)`)

// CanonicalAddress normalizes an IEC 61131-3 direct address so that its
// spellings compare equal: upper case, a bit without its size (%I0.1) with
// it (%IX0.1), and a bit in byte.bit form (%IX1.2) as the flat bit index
// (%IX10). Anything that is not a direct address is returned trimmed and
// upper-cased.
func CanonicalAddress(addr string) string {
	a := strings.ToUpper(strings.TrimSpace(addr))
	a = noSize.ReplaceAllString(a, "%${1}X${2}")
	return canonicalAddress(a)
}

// IsDirectAddress reports whether addr is an IEC 61131-3 direct address
// such as %IX0.0, %QW10 or %MD20.
func IsDirectAddress(addr string) bool {
	return directAddressRegex.MatchString(CanonicalAddress(addr))
}

// TagAt returns the tag whose direct address is addr, in any spelling
// (%IX1.2 or %IX10), and true; an empty Tag and false when no tag has it.
// It is how a program's located variable (`level AT %IW0 : WORD`) finds the
// tag of the I/O point the address is mapped to.
func (db *TagDatabase) TagAt(addr string) (Tag, bool) {
	if !IsDirectAddress(addr) {
		return Tag{}, false
	}
	name, ok := db.directAddressMap.Load(CanonicalAddress(addr))
	if !ok {
		return Tag{}, false
	}
	return db.GetTag(name.(string))
}
