package honeycomb

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	plc "github.com/apiarytech/royaljelly/iec"
)

// TestNonFiniteValuesIssue2 covers issue #2: a NaN or Inf REAL must not turn
// a response into a 200 with an empty body.
func TestNonFiniteValuesIssue2(t *testing.T) {
	RegisterUDT(&MotorData{})
	db := NewTagDatabase()
	for _, tag := range []*Tag{
		{Name: "R", TypeInfo: &TypeInfo{DataType: TypeREAL}, Value: plc.REAL(1)},
		{Name: "L", TypeInfo: &TypeInfo{DataType: TypeLREAL}, Value: plc.LREAL(2)},
		{Name: "Motor", TypeInfo: &TypeInfo{DataType: "MotorData"}, Value: &MotorData{Speed: 3}},
	} {
		if err := db.AddTag(tag); err != nil {
			t.Fatal(err)
		}
	}
	ts := &tagServer{db: db, validTokens: []string{"k"}}
	start, err := db.Changes(t.Context(), ChangesRequest{}) // a reader's starting position
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetTagValue("R", plc.REAL(math.NaN())); err != nil {
		t.Fatal(err)
	}
	if err := db.SetTagValue("L", plc.LREAL(math.Inf(1))); err != nil {
		t.Fatal(err)
	}
	if err := db.SetTagValue("Motor.Current", plc.REAL(math.Inf(-1))); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/tags/", ts.authMiddleware(http.HandlerFunc(ts.tagHandler)))
	mux.Handle("/tags", ts.authMiddleware(http.HandlerFunc(ts.handleGetAllTags)))
	mux.Handle("/changes", ts.authMiddleware(http.HandlerFunc(ts.handleChanges)))

	get := func(method, path, body string) map[string]any {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer k")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || rec.Body.Len() == 0 {
			t.Fatalf("%s %s = %d with %d bytes", method, path, rec.Code, rec.Body.Len())
		}
		var out any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s %s: invalid JSON: %v\n%s", method, path, err, rec.Body.String())
		}
		m, _ := out.(map[string]any)
		return m
	}
	if v := get(http.MethodGet, "/tags/R", ""); v["value"] != nil {
		t.Errorf("GET /tags/R value = %v, want null", v["value"])
	}
	get(http.MethodGet, "/tags", "")
	get(http.MethodGet, "/tags?names=R,L,Motor", "")
	body, _ := json.Marshal(map[string]any{"since": start.Next, "epoch": start.Epoch})
	batch := get(http.MethodPost, "/changes", string(body))
	if changes, _ := batch["changes"].([]any); len(changes) != 3 {
		t.Errorf("change feed returned %d changes, want 3: %v", len(changes), batch)
	}
	// Finite values are encoded as before.
	if v := get(http.MethodGet, "/tags/Motor", ""); v["value"].(map[string]any)["Speed"] != float64(3) {
		t.Errorf("GET /tags/Motor = %v", v)
	}
}
