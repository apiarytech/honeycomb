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
