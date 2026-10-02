package plc4x

import (
	"slices"
	"testing"

	"github.com/apiarytech/honeycomb"
)

func TestKnownProtocols(t *testing.T) {
	known := KnownProtocols()
	if len(known) != len(allDrivers) {
		t.Fatalf("%d protocols for %d drivers: %v", len(known), len(allDrivers), known)
	}
	for _, want := range []string{"modbus-tcp", "s7", "opcua"} {
		if !slices.Contains(known, want) {
			t.Errorf("%s missing from %v", want, known)
		}
	}
}

func TestProtocolOf(t *testing.T) {
	for url, want := range map[string]string{
		"modbus-tcp://10.0.0.5:502": "modbus-tcp",
		"opcua:tcp://host:4840":     "opcua",
		"S7://192.168.0.10":         "s7",
		"nonsense":                  "nonsense",
	} {
		if got := ProtocolOf(url); got != want {
			t.Errorf("ProtocolOf(%q) = %q, want %q", url, got, want)
		}
	}
	got := Protocols([]Connection{{URL: "s7://a"}, {URL: "modbus-tcp://b"}, {URL: "s7://c"}})
	if !slices.Equal(got, []string{"modbus-tcp", "s7"}) {
		t.Errorf("Protocols = %v", got)
	}
}

func TestNewDriverManagerFor(t *testing.T) {
	m, err := NewDriverManagerFor("modbus-tcp")
	if err != nil {
		t.Fatal(err)
	}
	if names := m.ListDriverNames(); !slices.Equal(names, []string{"modbus-tcp"}) {
		t.Errorf("registered drivers %v, want only modbus-tcp", names)
	}
	if _, err := m.GetDriver("s7"); err == nil {
		t.Error("s7 is reachable through a modbus-tcp-only manager")
	}
	if _, err := NewDriverManagerFor("modbus-tcp", "telnet"); err == nil {
		t.Error("an unknown protocol was accepted")
	}
}

func TestNewRegistersOnlyUsedDrivers(t *testing.T) {
	db := honeycomb.NewTagDatabase()
	if _, err := New(db, []Connection{{Name: "x", URL: "telnet://host"}}); err == nil {
		t.Fatal("New accepted a connection with an unknown protocol")
	}
	c, err := New(db, []Connection{{Name: "x", URL: "modbus-tcp://127.0.0.1:1"}})
	if err != nil {
		t.Fatal(err)
	}
	if names := c.manager.ListDriverNames(); !slices.Equal(names, []string{"modbus-tcp"}) {
		t.Errorf("connector registered %v, want only modbus-tcp", names)
	}
}
