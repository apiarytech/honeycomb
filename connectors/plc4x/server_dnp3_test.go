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
	"crypto/tls"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/apiarytech/honeycomb"
	plc "github.com/apiarytech/royaljelly/iec"

	"github.com/apiarytech/honeycomb/connectors/plc4x/dnp3"
)

func startDNP3(t *testing.T, query string) (*honeycomb.TagDatabase, int) {
	t.Helper()
	port := freePort(t)
	db := honeycomb.NewTagDatabase()
	addTag(t, db, "Breaker", honeycomb.TypeBOOL, plc.BOOL(true))
	addTag(t, db, "Volts", honeycomb.TypeREAL, plc.REAL(230.5))
	addTag(t, db, "Taps", honeycomb.TypeDINT, plc.DINT(-3))
	addTag(t, db, "Trip", honeycomb.TypeBOOL, plc.BOOL(false))
	addTag(t, db, "Limit", honeycomb.TypeREAL, plc.REAL(10))
	addTag(t, db, "Fixed", honeycomb.TypeREAL, plc.REAL(5))
	c, err := New(db, []Connection{{Name: "rtu", Mode: Server, URL: fmt.Sprintf("dnp3://127.0.0.1:%d?address=10%s", port, query),
		Bindings: []Binding{out("Breaker", "BI:0"), out("Volts", "AI:0:2"), out("Taps", "AI32:1:3"),
			inout("Trip", "BO:0"), inout("Limit", "AO:0"), out("Fixed", "AO:1")}}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return db, port
}

func dialDNP3(t *testing.T, port int, cfg *tls.Config) *dnp3.Master {
	t.Helper()
	var conn net.Conn
	var err error
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cfg != nil {
			conn, err = tls.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port), cfg)
		} else {
			conn, err = net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		}
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return dnp3.NewMaster(conn, 1, 10)
}

func measurement(ms []dnp3.Measurement, g uint8, idx uint16) (dnp3.Measurement, bool) {
	for _, m := range ms {
		if m.Group == g && m.Index == idx {
			return m, true
		}
	}
	return dnp3.Measurement{}, false
}

// A DNP3 outstation on the tags (beehive R-143): class 0 reads them, a
// change is an event with the tag's own time, and controls write the
// outputs bound in-out.
func TestDNP3Outstation(t *testing.T) {
	db, port := startDNP3(t, "")
	m := dialDNP3(t, port, nil)
	_, ms, err := m.Integrity()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		g   uint8
		idx uint16
		b   bool
		n   float64
	}{{1, 0, true, 0}, {30, 0, false, 230.5}, {30, 1, false, -3}, {10, 0, false, 0}, {40, 0, false, 10}, {40, 1, false, 5}} {
		got, ok := measurement(ms, c.g, c.idx)
		if !ok || got.Bool != c.b || got.Number != c.n || !got.Online {
			t.Errorf("g%d[%d] = %+v %v", c.g, c.idx, got, ok)
		}
	}

	at := time.Date(2026, 11, 10, 14, 0, 0, 500*int(time.Millisecond), time.UTC)
	time.Sleep(300 * time.Millisecond) // the outstation's first scan notes the values
	db.SetTagValueQualityAt("Breaker", plc.BOOL(false), honeycomb.QualityGood, at)
	db.SetTagValueQualityAt("Volts", plc.REAL(231.25), honeycomb.QualityBad, at.Add(time.Second))
	var bi, ai dnp3.Measurement
	waitFor(t, func() bool {
		_, ms, err := m.Events()
		if err != nil {
			return false
		}
		if x, ok := measurement(ms, 2, 0); ok {
			bi = x
		}
		if x, ok := measurement(ms, 32, 0); ok {
			ai = x
		}
		return bi.Group != 0 && ai.Group != 0
	})
	if bi.Bool || !bi.Time.Equal(at) {
		t.Errorf("binary event %+v", bi)
	}
	if ai.Number != 231.25 || ai.Online || !ai.Time.Equal(at.Add(time.Second)) {
		t.Errorf("analog event %+v: a bad tag is reported offline", ai)
	}

	if st, err := m.Operate(dnp3.BinaryOutput, 0, dnp3.Value{Bool: true}, true); err != nil || st != dnp3.StatusSuccess {
		t.Fatalf("trip: %d %v", st, err)
	}
	if v, _ := db.GetTagValue("Trip"); v != plc.BOOL(true) {
		t.Errorf("Trip = %v", v)
	}
	if st, _ := m.Operate(dnp3.AnalogOutput, 0, dnp3.Value{Number: 12.75}, false); st != dnp3.StatusSuccess {
		t.Errorf("limit: %d", st)
	}
	if v, _ := db.GetTagValue("Limit"); v != plc.REAL(12.75) {
		t.Errorf("Limit = %v", v)
	}
	if st, _ := m.Operate(dnp3.AnalogOutput, 1, dnp3.Value{Number: 99}, false); st != dnp3.StatusNotSupported {
		t.Errorf("a read-only output was commanded: %d", st)
	}
}

// Over TLS with roles: a viewer reads, only an operator commands.
func TestDNP3OverTLSRoles(t *testing.T) {
	p := newPKI(t)
	cert, key := p.issue("outstation", true)
	db, port := startDNP3(t, fmt.Sprintf("&tls=true&tls-cert=%s&tls-key=%s&tls-ca=%s", cert, key, p.file))
	vc, vk := p.issue("viewer", false, RoleViewer)
	viewer := dialDNP3(t, port, p.station(vc, vk))
	if _, ms, err := viewer.Integrity(); err != nil || len(ms) == 0 {
		t.Fatalf("the viewer cannot read: %v", err)
	}
	if st, _ := viewer.Operate(dnp3.BinaryOutput, 0, dnp3.Value{Bool: true}, false); st != dnp3.StatusNotAuthorized {
		t.Errorf("the viewer's control: %d", st)
	}
	oc, ok := p.issue("operator", false, RoleOperator)
	op := dialDNP3(t, port, p.station(oc, ok))
	if st, err := op.Operate(dnp3.BinaryOutput, 0, dnp3.Value{Bool: true}, false); err != nil || st != dnp3.StatusSuccess {
		t.Fatalf("the operator's control: %d %v", st, err)
	}
	if v, _ := db.GetTagValue("Trip"); v != plc.BOOL(true) {
		t.Error("Trip not set")
	}
}

func TestDNP3Config(t *testing.T) {
	db := honeycomb.NewTagDatabase()
	addTag(t, db, "A", honeycomb.TypeBOOL, plc.BOOL(false))
	for _, c := range []struct {
		url  string
		bind Binding
	}{
		{"dnp3://0.0.0.0:20000", out("A", "BI:0")},               // no address
		{"dnp3://0.0.0.0:20000?address=70000", out("A", "BI:0")}, // out of range
		{"dnp3://0.0.0.0:20000?address=1", out("A", "XX:0")},     // a kind
		{"dnp3://0.0.0.0:20000?address=1", inout("A", "BI:0")},   // an input commanded
		{"dnp3://0.0.0.0:20000?address=1", out("A", "BO:0:1")},   // an output's class
		{"dnp3://0.0.0.0:20000?address=1&events=0", out("A", "BI:0")},
	} {
		if _, err := New(db, []Connection{{Name: "x", Mode: Server, URL: c.url, Bindings: []Binding{c.bind}}}); err == nil {
			t.Errorf("%s %+v accepted", c.url, c.bind)
		}
	}
	if _, err := New(db, []Connection{{Name: "x", URL: "dnp3://10.0.0.1:20000", Bindings: []Binding{{Tag: "A", Address: "BI:0", Direction: Input}}}}); err == nil {
		t.Error("a DNP3 client connection was accepted")
	}
	found := false
	for _, p := range KnownProtocols() {
		found = found || p == "dnp3"
	}
	if !found {
		t.Error("dnp3 is not a known protocol")
	}
}
