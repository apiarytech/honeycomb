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
	"fmt"
	"testing"
	"time"

	apiModel "github.com/apache/plc4x/plc4go/pkg/api/model"
	"github.com/apiarytech/honeycomb"
	plc "github.com/apiarytech/royaljelly/iec"
)

// TestS7ServerByPLC4XClient: PLC4X's own S7 client connects over ISO-on-TCP,
// reads DB and marker points, and writes the writable ones, which the server
// serves and accepts through the S7 model (beehive R-133). The client is
// pinned to a controller type so it does not probe for user-data services the
// server does not offer.
func TestS7ServerByPLC4XClient(t *testing.T) {
	port := freePort(t)
	db := honeycomb.NewTagDatabase()
	addTag(t, db, "Level", honeycomb.TypeREAL, plc.REAL(12.5))
	addTag(t, db, "Count", honeycomb.TypeINT, plc.INT(-7))
	addTag(t, db, "Run", honeycomb.TypeBOOL, plc.BOOL(true))
	addTag(t, db, "Setpoint", honeycomb.TypeINT, plc.INT(0))
	c, err := New(db, []Connection{{
		Name: "plc", URL: fmt.Sprintf("s7://127.0.0.1:%d", port), Mode: Server,
		Bindings: []Binding{
			out("Level", "%DB1.DBD0:REAL"),
			out("Count", "%DB1.DBW4:INT"),
			out("Run", "%M0.0:BOOL"),
			inout("Setpoint", "%DB1.DBW6:INT"),
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	m, err := NewDriverManagerFor("s7")
	if err != nil {
		t.Fatal(err)
	}
	url := fmt.Sprintf("s7://127.0.0.1:%d?controller-type=S7_1500&remote-rack=0&remote-slot=1", port)
	conn := connectClient(t, ctx, m, url)
	defer conn.Close()

	// Read the four points.
	rr, err := conn.ReadRequestBuilder().
		AddTagAddress("level", "%DB1.DBD0:REAL").
		AddTagAddress("count", "%DB1.DBW4:INT").
		AddTagAddress("run", "%M0.0:BOOL").
		Build()
	if err != nil {
		t.Fatal(err)
	}
	rres := <-rr.Execute(ctx)
	if rres.GetErr() != nil {
		t.Fatal(rres.GetErr())
	}
	resp := rres.GetResponse()
	if code := resp.GetResponseCode("level"); code != apiModel.PlcResponseCode_OK {
		t.Fatalf("level code %s", code)
	}
	if v := resp.GetValue("level").GetFloat32(); v != 12.5 {
		t.Fatalf("level %v", v)
	}
	if v := resp.GetValue("count").GetInt16(); v != -7 {
		t.Fatalf("count %v", v)
	}
	if !resp.GetValue("run").GetBool() {
		t.Fatalf("run false")
	}

	// Write the writable set-point.
	wr, err := conn.WriteRequestBuilder().AddTagAddress("sp", "%DB1.DBW6:INT", int16(1234)).Build()
	if err != nil {
		t.Fatal(err)
	}
	wres := <-wr.Execute(ctx)
	if wres.GetErr() != nil {
		t.Fatal(wres.GetErr())
	}
	if code := wres.GetResponse().GetResponseCode("sp"); code != apiModel.PlcResponseCode_OK {
		t.Fatalf("write code %s", code)
	}
	waitFor(t, func() bool { v, _ := db.GetTagValue("Setpoint"); return v == plc.INT(1234) })

	// A write to a read-only point (Count) is refused and changes nothing.
	wr2, err := conn.WriteRequestBuilder().AddTagAddress("ro", "%DB1.DBW4:INT", int16(5)).Build()
	if err != nil {
		t.Fatal(err)
	}
	wres2 := <-wr2.Execute(ctx)
	if wres2.GetErr() != nil {
		t.Fatal(wres2.GetErr())
	}
	if code := wres2.GetResponse().GetResponseCode("ro"); code == apiModel.PlcResponseCode_OK {
		t.Fatalf("read-only point accepted a write")
	}
	if v, _ := db.GetTagValue("Count"); v != plc.INT(-7) {
		t.Fatalf("read-only point changed: %v", v)
	}
	if st, _ := c.Status("plc"); !st.Connected || st.Reads == 0 || st.Writes == 0 {
		t.Fatalf("server status %+v", st)
	}
}
