package honeycomb

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	plc "github.com/apiarytech/royaljelly/iec"
)

func securityDB(t *testing.T) *TagDatabase {
	t.Helper()
	db := NewTagDatabase()
	if err := db.AddTag(&Tag{Name: "Valve", TypeInfo: &TypeInfo{DataType: TypeINT}, Value: plc.INT(0)}); err != nil {
		t.Fatal(err)
	}
	return db
}

func do(t *testing.T, h http.Handler, method, path, token, body string) int {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestTokenValidConstantTime(t *testing.T) {
	valid := []string{"alpha-token", "bravo-token"}
	for token, want := range map[string]bool{
		"alpha-token": true, "bravo-token": true, "alpha-toke": false, "alpha-tokenX": false, "": false,
	} {
		if got := tokenValid(token, valid); got != want {
			t.Errorf("tokenValid(%q) = %v, want %v", token, got, want)
		}
	}
	if tokenValid("x", nil) {
		t.Error("no tokens accepted a token")
	}
}

func TestServerHardening(t *testing.T) {
	srv, err := NewServer(securityDB(t), ServerOptions{Addr: "127.0.0.1:0", Tokens: []string{"secret"}})
	if err != nil {
		t.Fatal(err)
	}
	if srv.Addr != "127.0.0.1:0" {
		t.Errorf("Addr = %q", srv.Addr)
	}
	if srv.ReadHeaderTimeout == 0 || srv.ReadTimeout == 0 || srv.WriteTimeout == 0 || srv.IdleTimeout == 0 {
		t.Errorf("timeouts not set: %+v", srv)
	}
	if srv.WriteTimeout <= MaxChangesWait {
		t.Errorf("WriteTimeout %v cuts the change-feed long poll (%v)", srv.WriteTimeout, MaxChangesWait)
	}
	if _, err := NewServer(securityDB(t), ServerOptions{Addr: "127.0.0.1:0"}); err == nil {
		t.Error("a server without tokens or Authorize was accepted")
	}

	h := srv.Handler
	if code := do(t, h, http.MethodGet, "/tags/Valve", "", ""); code != http.StatusUnauthorized {
		t.Errorf("no token: %d", code)
	}
	if code := do(t, h, http.MethodGet, "/tags/Valve", "wrong", ""); code != http.StatusUnauthorized {
		t.Errorf("wrong token: %d", code)
	}
	if code := do(t, h, http.MethodPut, "/tags/Valve", "secret", `{"value": 7}`); code != http.StatusOK {
		t.Errorf("write: %d", code)
	}
	big := `{"value": 7, "pad": "` + strings.Repeat("x", DefaultMaxBodyBytes) + `"}`
	if code := do(t, h, http.MethodPut, "/tags/Valve", "secret", big); code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body: %d, want 413", code)
	}
}

func TestReadOnly(t *testing.T) {
	db := securityDB(t)
	srv, _ := NewServer(db, ServerOptions{Addr: "127.0.0.1:0", Tokens: []string{"secret"}, ReadOnly: true})
	if code := do(t, srv.Handler, http.MethodGet, "/tags/Valve", "secret", ""); code != http.StatusOK {
		t.Errorf("read: %d", code)
	}
	if code := do(t, srv.Handler, http.MethodPut, "/tags/Valve", "secret", `{"value": 7}`); code != http.StatusForbidden {
		t.Errorf("write to a read-only server: %d, want 403", code)
	}
	if v, _ := db.GetTagValue("Valve"); v != plc.INT(0) {
		t.Errorf("value changed to %v", v)
	}
}

func TestAuthorizeAndOnWrite(t *testing.T) {
	db := securityDB(t)
	var mu sync.Mutex
	var writes []string
	srv, err := NewServer(db, ServerOptions{
		Addr: "127.0.0.1:0",
		Authorize: func(r *http.Request, access Access) (string, error) {
			switch r.Header.Get("Authorization") {
			case "Bearer operator":
				return "olga", nil
			case "Bearer viewer":
				if access == AccessWrite {
					return "", ErrForbidden
				}
				return "vera", nil
			}
			return "", errors.New("unknown")
		},
		OnWrite: func(_ *http.Request, subject, tag string) {
			mu.Lock()
			defer mu.Unlock()
			writes = append(writes, subject+":"+tag)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	h := srv.Handler
	if code := do(t, h, http.MethodGet, "/tags/Valve", "viewer", ""); code != http.StatusOK {
		t.Errorf("viewer read: %d", code)
	}
	if code := do(t, h, http.MethodPut, "/tags/Valve", "viewer", `{"value": 3}`); code != http.StatusForbidden {
		t.Errorf("viewer write: %d, want 403", code)
	}
	if code := do(t, h, http.MethodGet, "/tags/Valve", "nobody", ""); code != http.StatusUnauthorized {
		t.Errorf("unknown caller: %d, want 401", code)
	}
	if code := do(t, h, http.MethodPost, "/changes", "nobody", `{}`); code != http.StatusUnauthorized {
		t.Errorf("unknown caller on the change feed: %d, want 401", code)
	}
	if code := do(t, h, http.MethodPut, "/tags/Valve", "operator", `{"value": 5}`); code != http.StatusOK {
		t.Errorf("operator write: %d", code)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(writes) != 1 || writes[0] != "olga:Valve" {
		t.Errorf("OnWrite saw %v, want [olga:Valve]", writes)
	}
}

// TestWriteTopLevelTagWithDot is a regression test: a PUT to a top-level tag
// whose name contains a dot used to be taken for a UDT field of the part
// before the dot, and failed with 404.
func TestWriteTopLevelTagWithDot(t *testing.T) {
	db := NewTagDatabase()
	db.AddTag(&Tag{Name: "Guard1.Temp", TypeInfo: &TypeInfo{DataType: TypeREAL}, Value: plc.REAL(0)})
	srv, _ := NewServer(db, ServerOptions{Addr: "127.0.0.1:0", Tokens: []string{"secret"}})
	if code := do(t, srv.Handler, http.MethodPut, "/tags/Guard1.Temp", "secret", `{"value": 21.5}`); code != http.StatusOK {
		t.Fatalf("PUT /tags/Guard1.Temp: %d, want 200", code)
	}
	if v, _ := db.GetTagValue("Guard1.Temp"); v != plc.REAL(21.5) {
		t.Fatalf("value = %v, want 21.5", v)
	}
}
