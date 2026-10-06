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
	"testing"

	plc "github.com/apiarytech/royaljelly/iec"
)

func TestTagAt(t *testing.T) {
	db := NewTagDatabase()
	db.AddTag(&Tag{Name: "RIO1.AI0", TypeInfo: &TypeInfo{DataType: TypeWORD}, Value: plc.WORD(0), DirectAddress: "%IW0"})
	db.AddTag(&Tag{Name: "RIO1.DI10", TypeInfo: &TypeInfo{DataType: TypeBOOL}, Value: plc.BOOL(false), DirectAddress: "%IX1.2"})
	db.AddTag(&Tag{Name: "Setpoint", TypeInfo: &TypeInfo{DataType: TypeWORD}, Value: plc.WORD(7)})

	for addr, want := range map[string]string{
		"%IW0": "RIO1.AI0", " %iw0 ": "RIO1.AI0", "%IX1.2": "RIO1.DI10", "%IX10": "RIO1.DI10",
	} {
		tag, ok := db.TagAt(addr)
		if !ok || tag.Name != want {
			t.Errorf("TagAt(%q) = %q %v, want %q", addr, tag.Name, ok, want)
		}
	}
	for _, addr := range []string{"%IW1", "%QW0", "Setpoint", "RIO1.AI0", ""} {
		if tag, ok := db.TagAt(addr); ok {
			t.Errorf("TagAt(%q) found %q", addr, tag.Name)
		}
	}
	// renaming and removing keep the address map right
	if _, err := db.RenameTag("RIO1.AI0", "RIO1.Level"); err != nil {
		t.Fatal(err)
	}
	if tag, ok := db.TagAt("%IW0"); !ok || tag.Name != "RIO1.Level" {
		t.Errorf("after rename: %q %v", tag.Name, ok)
	}
	if err := db.RemoveTag("RIO1.Level"); err != nil {
		t.Fatal(err)
	}
	if _, ok := db.TagAt("%IW0"); ok {
		t.Error("removed tag still at %IW0")
	}
}

func TestCanonicalAddressExported(t *testing.T) {
	for in, want := range map[string]string{
		"%IX1.2": "%IX10", "%ix0.0": "%IX0", " %QW4 ": "%QW4", "%MD2": "%MD2", "Tank1": "TANK1", "%I0.1": "%IX1", "%Q1": "%QX1",
	} {
		if got := CanonicalAddress(in); got != want {
			t.Errorf("CanonicalAddress(%q) = %q, want %q", in, got, want)
		}
	}
	if !IsDirectAddress("%QX0.1") || IsDirectAddress("%XW0") || IsDirectAddress("Tank1") {
		t.Error("IsDirectAddress")
	}
}

// Addresses written in any spelling on a tag are found in any other.
func TestTagAtSpellings(t *testing.T) {
	db := NewTagDatabase()
	db.AddTag(&Tag{Name: "Lower", TypeInfo: &TypeInfo{DataType: TypeBOOL}, Value: plc.BOOL(true), DirectAddress: "%ix0.1"})
	db.AddTag(&Tag{Name: "NoSize", TypeInfo: &TypeInfo{DataType: TypeBOOL}, Value: plc.BOOL(true), DirectAddress: "%Q0.3"})
	for addr, want := range map[string]string{"%IX0.1": "Lower", "%IX1": "Lower", "%QX0.3": "NoSize", "%Q0.3": "NoSize", "%qx3": "NoSize"} {
		if tag, ok := db.TagAt(addr); !ok || tag.Name != want {
			t.Errorf("TagAt(%q) = %q %v, want %q", addr, tag.Name, ok, want)
		}
	}
	// values by address, as SetTagValue and GetTagValue take them
	if v, err := db.GetTagValue("%IX0.1"); err != nil || v != plc.BOOL(true) {
		t.Errorf("GetTagValue: %v %v", v, err)
	}
}
