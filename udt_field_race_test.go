/*
 * Copyright (C) 2026 Franklin D. Amador
 *
 * This software is dual-licensed under:
 * - GPL v3.0
 * - Commercial
 *
 * You may choose to use this software under the terms of either license.
 * See the LICENSE files in the project root for full license text.
 */

package honeycomb

import (
	"sync"
	"testing"

	plc "github.com/apiarytech/royaljelly/iec"
)

// TestUDTFieldReadWhileWritten reads UDT fields, of a tag and of an array
// element, while other goroutines write them in place. Under -race it fails
// if a field is read without the owning tag's lock.
func TestUDTFieldReadWhileWritten(t *testing.T) {
	RegisterUDT(&MotorData{})
	db := NewTagDatabase()
	if err := db.AddTag(&Tag{Name: "Motor", TypeInfo: &TypeInfo{DataType: "MotorData"}, Value: &MotorData{}}); err != nil {
		t.Fatal(err)
	}
	if err := db.AddTag(&Tag{
		Name:     "Motors",
		TypeInfo: &TypeInfo{DataType: TypeARRAY, ElementType: "MotorData", Dimensions: []int{2}},
		Value:    []*MotorData{{}, {}},
	}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			on := plc.BOOL(i%2 == 0)
			for _, name := range []string{"Motor.Running", "Motors[1].Running"} {
				if err := db.SetTagValue(name, on); err != nil {
					t.Error(err)
				}
				if _, err := db.GetTagValue(name); err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()
}
