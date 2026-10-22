package honeycomb

import (
	"testing"

	plc "github.com/apiarytech/royaljelly/iec"
)

// TestSnapshotsAreDeepCopiesIssue1 covers issue #1: values handed out by
// GetTag, GetAllTags, GetTagValue and Tag.GetValue must not share array or
// UDT memory with the live tag.
func TestSnapshotsAreDeepCopiesIssue1(t *testing.T) {
	RegisterUDT(&MotorData{})
	db := NewTagDatabase()
	if err := db.AddTag(&Tag{Name: "Arr", TypeInfo: &TypeInfo{DataType: TypeARRAY, ElementType: TypeINT}, Value: []plc.INT{1, 2, 3}}); err != nil {
		t.Fatal(err)
	}
	if err := db.AddTag(&Tag{Name: "Motor", TypeInfo: &TypeInfo{DataType: "MotorData"}, Value: &MotorData{Speed: 1}}); err != nil {
		t.Fatal(err)
	}

	tag, _ := db.GetTag("Arr")
	tag.Value.([]plc.INT)[0] = 99
	v, _ := db.GetTagValue("Arr")
	v.([]plc.INT)[1] = 99
	for _, all := range db.GetAllTags() {
		if a, ok := all.Value.([]plc.INT); ok {
			a[2] = 99
		}
	}
	if got, _ := db.GetTagValue("Arr"); got.([]plc.INT)[0] != 1 || got.([]plc.INT)[1] != 2 || got.([]plc.INT)[2] != 3 {
		t.Errorf("live array changed through a copy: %v", got)
	}

	m, _ := db.GetTag("Motor")
	m.Value.(*MotorData).Speed = 99
	mv, _ := db.GetTagValue("Motor")
	mv.(*MotorData).Speed = 98
	if got, _ := db.GetTagValue("Motor.Speed"); got != plc.REAL(1) {
		t.Errorf("live UDT changed through a copy: Speed = %v", got)
	}

	// A write still reaches the live tag and the next copy.
	if err := db.SetTagValue("Arr[0]", plc.INT(7)); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.GetTagValue("Arr[0]"); got != plc.INT(7) {
		t.Errorf("Arr[0] after write = %v", got)
	}
}
