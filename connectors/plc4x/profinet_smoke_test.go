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
	"testing"
)

func TestProfinetRegistered(t *testing.T) {
	if _, err := NewDriverManagerFor("profinet"); err != nil {
		t.Fatal(err)
	}
	c := CapabilitiesOf("profinet")
	if !c.Subscribe || !c.Events || !c.Discover || c.Note == "" {
		t.Fatalf("capabilities %+v", c)
	}
}
