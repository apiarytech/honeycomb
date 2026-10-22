package honeycomb

import (
	"net/http"
	"testing"

	plc "github.com/apiarytech/royaljelly/iec"
)

// TestPutDecodesTargetTypeIssue6 covers issue #6: PUT decodes the JSON value
// into the type of the tag, element, field or remote tag being written, and
// refuses null.
func TestPutDecodesTargetTypeIssue6(t *testing.T) {
	RegisterUDT(&MotorData{})
	src := NewTagDatabase()
	if err := src.AddTag(&Tag{Name: "Remote", TypeInfo: &TypeInfo{DataType: TypeDINT}, Value: plc.DINT(1)}); err != nil {
		t.Fatal(err)
	}
	db := NewTagDatabase()
	if err := db.RegisterDatabase("SRC", src); err != nil {
		t.Fatal(err)
	}
	for _, tag := range []*Tag{
		{Name: "Arr", TypeInfo: &TypeInfo{DataType: TypeARRAY, ElementType: TypeINT}, Value: []plc.INT{1, 2, 3}},
		{Name: "Motor", TypeInfo: &TypeInfo{DataType: "MotorData"}, Value: &MotorData{Speed: 1}},
		{Name: "N", TypeInfo: &TypeInfo{DataType: TypeINT}, Value: plc.INT(5)},
		{Name: "Alias", RemoteAlias: &RemoteAliasInfo{DBID: "SRC", TagName: "Remote"}},
		{Name: "Dangling", RemoteAlias: &RemoteAliasInfo{DBID: "SRC", TagName: "Missing"}},
	} {
		if err := db.AddTag(tag); err != nil {
			t.Fatal(err)
		}
	}
	h := (&tagServer{db: db, validTokens: []string{"k"}}).authMiddleware(http.HandlerFunc((&tagServer{db: db}).tagHandler))
	put := func(path, body string) int { return do(t, h, http.MethodPut, path, "k", body) }

	if c := put("/tags/Arr", `{"value": [7, 8, 9]}`); c != http.StatusOK {
		t.Errorf("PUT whole array = %d", c)
	}
	if v, _ := db.GetTagValue("Arr[1]"); v != plc.INT(8) {
		t.Errorf("Arr[1] = %v (%T), want INT 8", v, v)
	}
	if c := put("/tags/Arr[0]", `{"value": 42}`); c != http.StatusOK {
		t.Errorf("PUT Arr[0] = %d", c)
	}
	if v, _ := db.GetTagValue("Arr[0]"); v != plc.INT(42) {
		t.Errorf("Arr[0] = %v (%T), want INT 42", v, v)
	}
	if c := put("/tags/Motor.Running", `{"value": true}`); c != http.StatusOK {
		t.Errorf("PUT Motor.Running = %d", c)
	}
	if v, _ := db.GetTagValue("Motor.Running"); v != plc.BOOL(true) {
		t.Errorf("Motor.Running = %v (%T), want TRUE", v, v)
	}
	if c := put("/tags/N", `{"value": null}`); c != http.StatusBadRequest {
		t.Errorf("PUT null = %d, want 400", c)
	}
	if v, _ := db.GetTagValue("N"); v != plc.INT(5) {
		t.Errorf("N after PUT null = %v, want 5", v)
	}
	if c := put("/tags/N", `{"value": "text"}`); c != http.StatusBadRequest {
		t.Errorf("PUT string into INT = %d, want 400", c)
	}
	if c := put("/tags/Alias", `{"value": 77}`); c != http.StatusOK {
		t.Errorf("PUT alias = %d", c)
	}
	if v, _ := src.GetTagValue("Remote"); v != plc.DINT(77) {
		t.Errorf("remote tag after alias PUT = %v (%T), want DINT 77", v, v)
	}
	if c := put("/tags/Dangling", `{"value": 1}`); c < 400 || c >= 500 {
		t.Errorf("PUT dangling alias = %d, want a 4xx", c)
	}
}
