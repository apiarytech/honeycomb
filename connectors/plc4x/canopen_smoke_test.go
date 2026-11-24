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

func TestCanopenRegistered(t *testing.T) {
	m, err := NewDriverManagerFor("canopen")
	if err != nil {
		t.Fatal(err)
	}
	connection, err := m.GetConnection(t.Context(), "canopen:virtualcan://connector-smoke")
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if !connection.GetMetadata().CanSubscribe() {
		t.Fatal("canopen connection does not report subscriptions")
	}
	if c := CapabilitiesOf("canopen"); !c.Events || c.Note == "" {
		t.Fatalf("capabilities %+v", c)
	}
}
