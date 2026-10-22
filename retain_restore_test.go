package honeycomb

import (
	"math"
	"path/filepath"
	"testing"
	"time"

	plc "github.com/apiarytech/royaljelly/iec"
)

// retainConfig builds the same configuration twice: once to write the retain
// file, once (with different values) to read it back.
func retainConfig(t *testing.T, write bool) *TagDatabase {
	t.Helper()
	RegisterUDT(&MotorData{})
	src := NewTagDatabase()
	if err := src.AddTag(&Tag{Name: "Remote", TypeInfo: &TypeInfo{DataType: TypeDINT}, Value: plc.DINT(1)}); err != nil {
		t.Fatal(err)
	}
	db := NewTagDatabase()
	if err := db.RegisterDatabase("SRC", src); err != nil {
		t.Fatal(err)
	}
	pick := func(w, r interface{}) interface{} {
		if write {
			return w
		}
		return r
	}
	tags := []*Tag{
		{Name: "D", TypeInfo: &TypeInfo{DataType: TypeDINT}, Value: pick(plc.DINT(1000000), plc.DINT(0))},
		{Name: "L", TypeInfo: &TypeInfo{DataType: TypeLINT}, Value: pick(plc.LINT(1<<53+1), plc.LINT(0))},
		{Name: "U", TypeInfo: &TypeInfo{DataType: TypeULINT}, Value: pick(plc.ULINT(math.MaxUint64), plc.ULINT(0))},
		{Name: "B", TypeInfo: &TypeInfo{DataType: TypeBYTE}, Value: pick(plc.BYTE(0xAB), plc.BYTE(0))},
		{Name: "W", TypeInfo: &TypeInfo{DataType: TypeWORD}, Value: pick(plc.WORD(0xBEEF), plc.WORD(0))},
		{Name: "DW", TypeInfo: &TypeInfo{DataType: TypeDWORD}, Value: pick(plc.DWORD(0xDEADBEEF), plc.DWORD(0))},
		{Name: "LW", TypeInfo: &TypeInfo{DataType: TypeLWORD}, Value: pick(plc.LWORD(math.MaxUint64), plc.LWORD(0))},
		{Name: "T", TypeInfo: &TypeInfo{DataType: TypeTIME}, Value: pick(plc.TIME(90*time.Minute), plc.TIME(0))},
		{Name: "Alias", RemoteAlias: &RemoteAliasInfo{DBID: "SRC", TagName: "Remote"}},
		{Name: "Const", TypeInfo: &TypeInfo{DataType: "MotorData"}, Value: pick(&MotorData{Speed: 5}, &MotorData{Speed: 1}), Constant: true},
		{Name: "Motor", TypeInfo: &TypeInfo{DataType: "MotorData"}, Value: pick(&MotorData{Speed: 7, Running: true}, &MotorData{})},
	}
	for _, tag := range tags {
		tag.Retain = true
		if err := db.AddTag(tag); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

// TestRetainRoundTripIssues3And5 covers issues #3 and #5: every retained type
// is restored exactly, remote aliases don't panic, Constants are not
// overwritten, and subscribers see restored values.
func TestRetainRoundTripIssues3And5(t *testing.T) {
	file := filepath.Join(t.TempDir(), "retain.json")
	if err := retainConfig(t, true).WriteTagsToFile(file); err != nil {
		t.Fatal(err)
	}
	db := retainConfig(t, false)
	ch, _, err := db.SubscribeToTag("D")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ReadTagsFromFile(file); err != nil {
		t.Fatalf("ReadTagsFromFile: %v", err)
	}
	for name, want := range map[string]interface{}{
		"D": plc.DINT(1000000), "L": plc.LINT(1<<53 + 1), "U": plc.ULINT(math.MaxUint64),
		"B": plc.BYTE(0xAB), "W": plc.WORD(0xBEEF), "DW": plc.DWORD(0xDEADBEEF),
		"LW": plc.LWORD(math.MaxUint64), "T": plc.TIME(90 * time.Minute),
		"Const.Speed": plc.REAL(1), "Motor.Speed": plc.REAL(7), "Motor.Running": plc.BOOL(true),
	} {
		if got, err := db.GetTagValue(name); err != nil || got != want {
			t.Errorf("%s = %v (%T), %v; want %v (%T)", name, got, got, err, want, want)
		}
	}
	select {
	case u := <-ch:
		if u.Value != plc.DINT(1000000) {
			t.Errorf("subscriber got %v, want the restored 1000000", u.Value)
		}
	case <-time.After(time.Second):
		t.Error("subscriber was not told about the restored value")
	}
}
