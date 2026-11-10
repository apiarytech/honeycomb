/*
 * Copyright (C) 2026 Franklin D. Amador
 *
 * This software is dual-licensed under the terms of the GPL v3.0 and
 * a commercial license. You may choose to use this software under either
 * license.
 *
 * See the LICENSE files in the project root for full license text.
 */

package plc4x

import (
	"context"
	"testing"
	"time"
)

func TestDiscoverProtocols(t *testing.T) {
	got := DiscoverProtocols()
	for _, want := range []string{"ads", "bacnet-ip", "eip", "knxnet-ip", "logix"} {
		if !discovers(want) {
			t.Errorf("%s does not discover; protocols that do: %v", want, got)
		}
	}
	for _, not := range []string{"modbus-tcp", "s7", "opcua"} {
		if discovers(not) || CapabilitiesOf(not).Discover {
			t.Errorf("%s discovers", not)
		}
	}
	if !CapabilitiesOf("eip").Discover {
		t.Error("eip capabilities say it does not discover")
	}
}

func TestDiscoverRefusesWhatCannot(t *testing.T) {
	if _, err := Discover(context.Background(), DiscoverOptions{Protocols: []string{"modbus-tcp"}}); err == nil {
		t.Error("Modbus discovered")
	}
}

// A discovery on this machine's networks ends at its wait and returns what
// answered, which on a test machine is usually nothing.
func TestDiscoverEndsAtItsWait(t *testing.T) {
	if testing.Short() {
		t.Skip("listens on the network")
	}
	start := time.Now()
	res, err := Discover(context.Background(), DiscoverOptions{Protocols: []string{"eip"}, Wait: 1500 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	found := res.Found
	if d := time.Since(start); d < time.Second || d > 20*time.Second {
		t.Errorf("discovery took %s for a 1.5 s wait", d)
	}
	for _, d := range found {
		if d.URL == "" || d.Protocol != "eip" {
			t.Errorf("found %+v", d)
		}
	}
	t.Logf("found %d, problems %v", len(found), res.Problems)
}
