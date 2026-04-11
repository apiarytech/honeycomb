/*
 * Copyright (C) 2026 Franklin D. Amador
 *
 * This software is dual-licensed under:
 * - GPL v2.0
 * - Commercial
 *
 * You may choose to use this software under the terms of either license.
 * See the LICENSE files in the project root for full license text.
 */

package honeycomb

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	plc "github.com/apiarytech/royaljelly"
)

// setupTestServer creates a tagServer instance with a pre-populated database for testing.
func setupTestServer() *tagServer {
	db := NewTagDatabase()
	db.AddTag(&Tag{Name: "TestDINT", TypeInfo: &TypeInfo{DataType: TypeDINT}, Value: plc.DINT(100)})
	db.AddTag(&Tag{Name: "TestREAL", TypeInfo: &TypeInfo{DataType: TypeREAL}, Value: plc.REAL(123.45)})
	// Add a UDT for testing JSON unmarshaling on the server
	RegisterUDT(&MotorData{})
	db.AddTag(&Tag{
		Name: "MotorLine",
		TypeInfo: &TypeInfo{
			DataType:    TypeARRAY,
			ElementType: "MotorData",
		},
		Value: []*MotorData{{Speed: 1800.5}, {Speed: 0.0}}})
	udtTag := &Tag{Name: "TestUDT", TypeInfo: &TypeInfo{DataType: "MotorData"}, Value: &MotorData{Speed: 100}}
	db.AddTag(udtTag)

	return &tagServer{
		db:          db,
		validTokens: []string{"test-token"},
	}
}

// TestHandleGetTagValue tests the server's GET /tags/{tagName} handler.
func TestHandleGetTagValue(t *testing.T) {
	ts := setupTestServer()

	t.Run("Success", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, "/tags/TestDINT", nil)
		rr := httptest.NewRecorder()
		ts.handleGetTagValue(rr, req, "TestDINT")

		if status := rr.Code; status != http.StatusOK {
			t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusOK)
		}

		var response map[string]interface{}
		if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
			t.Fatalf("Failed to unmarshal response: %v", err)
		}

		if val, ok := response["value"].(float64); !ok || val != 100 {
			t.Errorf("handler returned unexpected body: got %v want 100", response["value"])
		}
	})

	t.Run("NotFound", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, "/tags/NotFoundTag", nil)
		rr := httptest.NewRecorder()
		ts.handleGetTagValue(rr, req, "NotFoundTag")

		if status := rr.Code; status != http.StatusNotFound {
			t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusNotFound)
		}
	})
}

// TestHandleGetAllTags tests the server's GET /tags handler for listing all tags.
func TestHandleGetAllTags(t *testing.T) {
	ts := setupTestServer()

	t.Run("Success", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, "/tags", nil)
		rr := httptest.NewRecorder()
		ts.handleGetAllTags(rr, req)

		if status := rr.Code; status != http.StatusOK {
			t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusOK)
		}

		var response []Tag
		if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
			t.Fatalf("Failed to unmarshal response: %v", err)
		}

		// The test server adds 4 tags in setupTestServer()
		if len(response) != 4 {
			t.Errorf("handler returned wrong number of tags: got %d want 4", len(response))
		}

		// Sort by name for predictable checking
		sort.Slice(response, func(i, j int) bool {
			return response[i].Name < response[j].Name
		})

		expectedNames := []string{"MotorLine", "TestDINT", "TestREAL", "TestUDT"}
		for i, name := range expectedNames {
			if response[i].Name != name {
				t.Errorf("Expected tag name '%s' at index %d, but got '%s'", name, i, response[i].Name)
			}
		}
	})

	t.Run("EmptyDatabase", func(t *testing.T) {
		// Create a server with a completely empty database
		emptyDB := NewTagDatabase()
		emptyTS := &tagServer{
			db:          emptyDB,
			validTokens: []string{"test-token"},
		}

		req, _ := http.NewRequest(http.MethodGet, "/tags", nil)
		rr := httptest.NewRecorder()
		emptyTS.handleGetAllTags(rr, req)

		if status := rr.Code; status != http.StatusOK {
			t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusOK)
		}

		// The response for an empty database should be an empty JSON array "[]".
		if body := strings.TrimSpace(rr.Body.String()); body != "[]" {
			t.Errorf("handler returned wrong body for empty db: got %s want []", body)
		}
	})

	t.Run("UnsupportedMethod", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, "/tags", nil)
		rr := httptest.NewRecorder()
		ts.handleGetAllTags(rr, req)
	})
}

// TestHandleSetTagValue tests the server's PUT /tags/{tagName} handler.
func TestHandleSetTagValue(t *testing.T) {
	ts := setupTestServer()

	t.Run("Success", func(t *testing.T) {
		payload := `{"value": 200}`
		req, _ := http.NewRequest(http.MethodPut, "/tags/TestDINT", bytes.NewBufferString(payload))
		rr := httptest.NewRecorder()
		ts.handleSetTagValue(rr, req, "TestDINT")

		if status := rr.Code; status != http.StatusOK {
			t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusOK)
		}

		// Verify the tag value was actually updated
		val, _ := ts.db.GetTagValue("TestDINT")
		if val != plc.DINT(200) {
			t.Errorf("tag value was not updated: got %v want 200", val)
		}
	})

	t.Run("TagNotFound", func(t *testing.T) {
		payload := `{"value": 200}`
		req, _ := http.NewRequest(http.MethodPut, "/tags/NotFoundTag", bytes.NewBufferString(payload))
		rr := httptest.NewRecorder()
		ts.handleSetTagValue(rr, req, "NotFoundTag")

		if status := rr.Code; status != http.StatusNotFound {
			t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusNotFound)
		}
	})

	t.Run("SetUDTValue", func(t *testing.T) {
		// This payload represents the new state of the MotorData UDT.
		payload := `{"value": {"Speed": 555.5, "Current": 7.7, "Running": true}}`
		req, _ := http.NewRequest(http.MethodPut, "/tags/TestUDT", bytes.NewBufferString(payload))
		rr := httptest.NewRecorder()
		ts.handleSetTagValue(rr, req, "TestUDT")

		status := rr.Code
		if status != http.StatusOK {
			t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusOK)
		}

		// Verify the tag value was actually updated by checking a field.
		val, _ := ts.db.GetTagValue("TestUDT.Speed")
		if val != plc.REAL(555.5) {
			t.Errorf("UDT field value was not updated: got %v want 555.5", val)
		}
	})

	t.Run("SetNestedUDTField", func(t *testing.T) {
		// This payload represents setting just a single field on the UDT.
		payload := `{"value": 999.9}`
		nestedTagName := "TestUDT.Speed"
		req, _ := http.NewRequest(http.MethodPut, "/tags/"+nestedTagName, bytes.NewBufferString(payload))
		rr := httptest.NewRecorder()
		ts.handleSetTagValue(rr, req, nestedTagName)

		if status := rr.Code; status != http.StatusOK {
			t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusOK)
		}

		// Verify the tag value was actually updated by checking the field.
		val, _ := ts.db.GetTagValue(nestedTagName)
		if val != plc.REAL(999.9) {
			t.Errorf("UDT field value was not updated: got %v want 999.9", val)
		}
	})

	t.Run("SetNestedFieldInArray", func(t *testing.T) {
		// This payload represents setting a single field on a UDT inside an array.
		payload := `{"value": 1999.9}`
		nestedTagName := "MotorLine[0].Speed"
		req, _ := http.NewRequest(http.MethodPut, "/tags/"+nestedTagName, bytes.NewBufferString(payload))
		rr := httptest.NewRecorder()
		ts.handleSetTagValue(rr, req, nestedTagName)

		status := rr.Code
		if status != http.StatusOK {
			t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusOK)
		}

		// Verify the tag value was actually updated by checking the field.
		val, err := ts.db.GetTagValue(nestedTagName)
		if err != nil || val != plc.REAL(1999.9) {
			t.Errorf("UDT field value in array was not updated: got %v, want 1999.9 (err: %v)", val, err)
		}
	})

	t.Run("BadJSON", func(t *testing.T) {
		payload := `{"value": 200,}` // Invalid JSON with trailing comma
		req, _ := http.NewRequest(http.MethodPut, "/tags/TestDINT", bytes.NewBufferString(payload))
		rr := httptest.NewRecorder()
		ts.handleSetTagValue(rr, req, "TestDINT")

		if status := rr.Code; status != http.StatusBadRequest {
			t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusBadRequest)
		}
	})

	t.Run("TypeMismatch", func(t *testing.T) {
		// TestDINT expects an integer, but we send a string.
		payload := `{"value": "hello"}`
		req, _ := http.NewRequest(http.MethodPut, "/tags/TestDINT", bytes.NewBufferString(payload))
		rr := httptest.NewRecorder()
		ts.handleSetTagValue(rr, req, "TestDINT")

		if status := rr.Code; status != http.StatusBadRequest {
			t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusBadRequest)
		}
	})
}

// TestAuthMiddleware tests the server's authentication middleware.
func TestAuthMiddleware(t *testing.T) {
	ts := setupTestServer()
	// Create a simple handler that the middleware will call if authentication succeeds.
	nextHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	authHandler := ts.authMiddleware(nextHandler)

	t.Run("ValidToken", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", "Bearer test-token")
		rr := httptest.NewRecorder()
		authHandler.ServeHTTP(rr, req)

		if status := rr.Code; status != http.StatusOK {
			t.Errorf("middleware returned wrong status code: got %v want %v", status, http.StatusOK)
		}
	})

	t.Run("InvalidToken", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", "Bearer wrong-token")
		rr := httptest.NewRecorder()
		authHandler.ServeHTTP(rr, req)

		if status := rr.Code; status != http.StatusUnauthorized {
			t.Errorf("middleware returned wrong status code: got %v want %v", status, http.StatusUnauthorized)
		}
	})

	t.Run("MissingToken", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, "/", nil)
		rr := httptest.NewRecorder()
		authHandler.ServeHTTP(rr, req)

		if status := rr.Code; status != http.StatusUnauthorized {
			t.Errorf("middleware returned wrong status code: got %v want %v", status, http.StatusUnauthorized)
		}
	})

	t.Run("MalformedHeader", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", "Token test-token") // "Token" instead of "Bearer"
		rr := httptest.NewRecorder()
		authHandler.ServeHTTP(rr, req)

		if status := rr.Code; status != http.StatusUnauthorized {
			t.Errorf("middleware returned wrong status code: got %v want %v", status, http.StatusUnauthorized)
		}
		if !strings.Contains(rr.Body.String(), "Bearer {token}") {
			t.Errorf("Expected error message about header format, but got: %s", rr.Body.String())
		}
	})
}

// TestTagHandler_Routing tests the main tag handler's routing logic.
func TestTagHandler_Routing(t *testing.T) {
	ts := setupTestServer()

	t.Run("UnsupportedMethod", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, "/tags/TestDINT", nil)
		rr := httptest.NewRecorder()
		ts.tagHandler(rr, req)

		if status := rr.Code; status != http.StatusMethodNotAllowed {
			t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusMethodNotAllowed)
		}
	})

	t.Run("MissingTagName", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, "/tags/", nil)
		rr := httptest.NewRecorder()
		ts.tagHandler(rr, req)

		if status := rr.Code; status != http.StatusBadRequest {
			t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusBadRequest)
		}
	})
}
