package plc4x

import (
	"testing"
)

func TestProfinetRegistered(t *testing.T) {
	if _, err := NewDriverManagerFor("profinet"); err != nil {
		t.Fatal(err)
	}
	c := CapabilitiesOf("profinet")
	if !c.Subscribe || !c.Events || !c.Discover || c.Note == "" {
		t.Fatalf("capabilities %+v", c)
	}
}
