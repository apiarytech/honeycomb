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
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	apiModel "github.com/apache/plc4x/plc4go/pkg/api/model"
	"github.com/apiarytech/honeycomb"
	plc "github.com/apiarytech/royaljelly/iec"
)

// startOPCUAServer runs a connector serving the plant's tags over OPC UA.
func startOPCUAServer(t *testing.T, query string, opts ...Option) (*honeycomb.TagDatabase, int) {
	t.Helper()
	port := freePort(t)
	db := honeycomb.NewTagDatabase()
	addTag(t, db, "Level", honeycomb.TypeREAL, plc.REAL(12.5))
	addTag(t, db, "Run", honeycomb.TypeBOOL, plc.BOOL(false))
	addTag(t, db, "Setpoint", honeycomb.TypeLREAL, plc.LREAL(0))
	addTag(t, db, "Mode", honeycomb.TypeINT, plc.INT(1))
	addTag(t, db, "Name", honeycomb.TypeSTRING, plc.STRING("tank 1"))
	c, err := New(db, []Connection{{
		Name: "plant", URL: fmt.Sprintf("opcua:tcp://127.0.0.1:%d?%s", port, query), Mode: Server,
		Bindings: []Binding{out("Level", "Tank.Level"), inout("Run", "Tank.Run"), inout("Setpoint", "ns=1;s=Tank.Setpoint"),
			inout("Mode", "Tank.Mode;DINT"), out("Name", "Name")},
	}}, opts...)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return db, port
}

func opcuaClient(t *testing.T, ctx context.Context, url string) interface {
	ReadRequestBuilder() apiModel.PlcReadRequestBuilder
	WriteRequestBuilder() apiModel.PlcWriteRequestBuilder
} {
	t.Helper()
	m, err := NewDriverManagerFor("opcua")
	if err != nil {
		t.Fatal(err)
	}
	for {
		conn, err := m.GetConnection(ctx, url)
		if err == nil {
			t.Cleanup(func() { conn.Close() })
			return conn
		}
		select {
		case <-ctx.Done():
			t.Fatalf("connect %s: %v", url, err)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// TestOPCUAServerByPLC4XClient: PLC4X's OPC UA client reads the served tags
// and writes the InOut ones through to the database.
func TestOPCUAServerByPLC4XClient(t *testing.T) {
	var mutex sync.Mutex
	var writes []string
	db, port := startOPCUAServer(t, "", WithServerWrites(func(connection, tag string, value any, from string) {
		mutex.Lock()
		writes = append(writes, fmt.Sprintf("%s %s=%v from %s", connection, tag, value, from))
		mutex.Unlock()
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn := opcuaClient(t, ctx, fmt.Sprintf("opcua:tcp://127.0.0.1:%d?discovery=false", port))

	read, _ := conn.ReadRequestBuilder().AddTagAddress("level", "ns=1;s=Tank.Level").AddTagAddress("name", "ns=1;s=Name").
		AddTagAddress("mode", "ns=1;s=Tank.Mode").Build()
	r := <-read.Execute(ctx)
	if r.GetErr() != nil {
		t.Fatal(r.GetErr())
	}
	if v := r.GetResponse().GetValue("level").GetList()[0].GetFloat32(); v != 12.5 {
		t.Errorf("level %v", v)
	}
	if v := r.GetResponse().GetValue("name").GetList()[0].GetString(); v != "tank 1" {
		t.Errorf("name %q", v)
	}
	if v := r.GetResponse().GetValue("mode").GetList()[0].GetInt32(); v != 1 {
		t.Errorf("mode %v", v)
	}

	write, _ := conn.WriteRequestBuilder().
		AddTagAddress("setpoint", "ns=1;s=Tank.Setpoint;LREAL", 7.5).
		AddTagAddress("mode", "ns=1;s=Tank.Mode;DINT", 3).
		AddTagAddress("run", "ns=1;s=Tank.Run;BOOL", true).
		AddTagAddress("level", "ns=1;s=Tank.Level;REAL", 1.0).
		Build()
	w := <-write.Execute(ctx)
	if w.GetErr() != nil {
		t.Fatal(w.GetErr())
	}
	for name, want := range map[string]apiModel.PlcResponseCode{"setpoint": apiModel.PlcResponseCode_OK, "mode": apiModel.PlcResponseCode_OK,
		"run": apiModel.PlcResponseCode_OK, "level": apiModel.PlcResponseCode_ACCESS_DENIED} {
		if got := w.GetResponse().GetResponseCode(name); got != want {
			t.Errorf("write %s: %s, want %s", name, got, want)
		}
	}
	for tag, want := range map[string]any{"Setpoint": plc.LREAL(7.5), "Mode": plc.INT(3), "Run": plc.BOOL(true), "Level": plc.REAL(12.5)} {
		if got, _ := db.GetTagValue(tag); got != want {
			t.Errorf("%s = %v (%T), want %v", tag, got, got, want)
		}
	}
	mutex.Lock()
	defer mutex.Unlock()
	if len(writes) != 3 || !strings.Contains(writes[0], "from opcua client") {
		t.Errorf("writes reported %v", writes)
	}
}

// TestOPCUAServerToClientConnector: one connector serves a tag over OPC UA,
// another subscribes to it with a secured channel and a login.
func TestOPCUAServerToClientConnector(t *testing.T) {
	dir := t.TempDir()
	cert, key := writeOPCUAIdentity(t, dir)
	users := filepath.Join(dir, "users.txt")
	if err := os.WriteFile(users, []byte("# operators\nop:s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	serverDB, port := startOPCUAServer(t, fmt.Sprintf("security=Basic256Sha256&cert=%s&key=%s&trust=*&users=%s", cert, key, users))

	clientDB := honeycomb.NewTagDatabase()
	addTag(t, clientDB, "Level", honeycomb.TypeREAL, plc.REAL(0))
	q := url.Values{"security-policy": {"Basic256Sha256"}, "message-security": {"SIGN_ENCRYPT"}, "tls.verify": {"false"},
		"cert-directory": {t.TempDir()}, "username": {"op"}, "password": {"s3cret"}}
	c, err := New(clientDB, []Connection{{
		Name: "scada", URL: fmt.Sprintf("opcua:tcp://127.0.0.1:%d?%s", port, q.Encode()), Mode: ChangeOfState, Interval: 100 * time.Millisecond,
		Bindings: []Binding{{Tag: "Level", Address: "ns=1;s=Tank.Level;REAL"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	wait := func(want plc.REAL) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			if v, _ := clientDB.GetTagValue("Level"); v == want {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		st, _ := c.Status("scada")
		t.Fatalf("Level never became %v; status %+v", want, st)
	}
	wait(12.5)
	if err := serverDB.SetTagValue("Level", plc.REAL(-3.25)); err != nil {
		t.Fatal(err)
	}
	wait(-3.25)
	if st, _ := c.Status("scada"); !st.Subscribed {
		t.Errorf("the client polled: %+v", st)
	}
}

func TestOPCUAServerConfig(t *testing.T) {
	dir := t.TempDir()
	cert, key := writeOPCUAIdentity(t, dir)
	db := honeycomb.NewTagDatabase()
	addTag(t, db, "A", honeycomb.TypeINT, plc.INT(0))
	addTag(t, db, "U", honeycomb.DataType("MotorData"), plc.INT(0))
	for want, conn := range map[string]Connection{
		"needs the server's cert": {URL: "opcua:tcp://0.0.0.0:4840?security=Basic256Sha256", Bindings: []Binding{out("A", "A")}},
		"needs trust":             {URL: "opcua:tcp://0.0.0.0:4840?security=Basic256&cert=" + cert + "&key=" + key, Bindings: []Binding{out("A", "A")}},
		"go together":             {URL: "opcua:tcp://0.0.0.0:4840?cert=" + cert, Bindings: []Binding{out("A", "A")}},
		"Sign or SignAndEncrypt":  {URL: "opcua:tcp://0.0.0.0:4840?security=Basic256:Encrypt&cert=" + cert + "&key=" + key + "&trust=*", Bindings: []Binding{out("A", "A")}},
		"listens on TCP":          {URL: "opcua:udp://0.0.0.0:4840", Bindings: []Binding{out("A", "A")}},
		"dotted name":             {URL: "opcua:tcp://0.0.0.0:4840", Bindings: []Binding{out("A", "A;INT;x")}},
		"is not served":           {URL: "opcua:tcp://0.0.0.0:4840", Bindings: []Binding{out("A", "A;TIME")}},
		"MotorData is not served": {URL: "opcua:tcp://0.0.0.0:4840", Bindings: []Binding{out("U", "U")}},
		"two variables":           {URL: "opcua:tcp://0.0.0.0:4840", Bindings: []Binding{out("A", "X"), out("A", "ns=1;s=X")}},
		"is not a folder":         {URL: "opcua:tcp://0.0.0.0:4840", Bindings: []Binding{out("A", "X"), out("A", "X.Y")}},
		"no such file":            {URL: "opcua:tcp://0.0.0.0:4840?users=" + filepath.Join(dir, "nope"), Bindings: []Binding{out("A", "A")}},
		"anonymous":               {URL: "opcua:tcp://0.0.0.0:4840?anonymous=maybe", Bindings: []Binding{out("A", "A")}},
	} {
		conn.Name, conn.Mode = "S", Server
		if _, err := New(db, []Connection{conn}); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", want, err)
		}
	}
	if !IsServerURL("opcua:tcp://0.0.0.0:4840") {
		t.Error("opcua is not a server protocol")
	}
}

// writeOPCUAIdentity writes a self-signed server certificate and key.
func writeOPCUAIdentity(t *testing.T, dir string) (string, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	uri, _ := url.Parse("urn:test:honeycomb")
	template := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "honeycomb"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageDataEncipherment | x509.KeyUsageContentCommitment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		URIs:        []*url.URL{uri}, IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, keyFile := filepath.Join(dir, "server.pem"), filepath.Join(dir, "server-key.pem")
	if err := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0o600); err != nil {
		t.Fatal(err)
	}
	return cert, keyFile
}

// opcuaMotor is a UDT served as an OPC UA structure.
type opcuaMotor struct {
	Speed   plc.REAL
	Running plc.BOOL
	Temps   [3]plc.INT
	Config  *opcuaMotorConfig
}

func (*opcuaMotor) TypeName() honeycomb.DataType { return "OPCUAMotor" }

type opcuaMotorConfig struct {
	MaxSpeed plc.LREAL
	Label    plc.STRING
}

func (*opcuaMotorConfig) TypeName() honeycomb.DataType { return "OPCUAMotorConfig" }

func init() {
	honeycomb.RegisterUDT(&opcuaMotor{})
	honeycomb.RegisterUDT(&opcuaMotorConfig{})
}

// TestOPCUAServerUDTMethodsAlarms: a UDT tag is a structure whose fields a
// client reads and writes one by one; a Name() binding is a method that sets
// its tag; an ;ALARM binding follows its tag.
func TestOPCUAServerUDTMethodsAlarms(t *testing.T) {
	port := freePort(t)
	db := honeycomb.NewTagDatabase()
	addTag(t, db, "Motor1", "OPCUAMotor", &opcuaMotor{Speed: 3.5, Running: true, Temps: [3]plc.INT{1, 2, 3},
		Config: &opcuaMotorConfig{MaxSpeed: 90, Label: "m1"}})
	addTag(t, db, "Reset", honeycomb.TypeBOOL, plc.BOOL(false))
	addTag(t, db, "Target", honeycomb.TypeREAL, plc.REAL(0))
	addTag(t, db, "High", honeycomb.TypeBOOL, plc.BOOL(false))
	conn := Connection{Name: "plant", URL: fmt.Sprintf("opcua:tcp://127.0.0.1:%d?history=10&history-interval=50ms", port), Mode: Server,
		Bindings: []Binding{inout("Motor1", "Motor1"), inout("Reset", "Line.Reset()"), inout("Target", "Line.SetTarget(LREAL)"),
			out("High", "Line.High;ALARM[800]")}}
	c, err := New(db, []Connection{conn})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done }) // after the client closes

	cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer ccancel()
	client := opcuaClient(t, cctx, fmt.Sprintf("opcua:tcp://127.0.0.1:%d?discovery=false", port))
	read, _ := client.ReadRequestBuilder().AddTagAddress("speed", "ns=1;s=Motor1.Speed").AddTagAddress("label", "ns=1;s=Motor1.Config.Label").
		AddTagAddress("temps", "ns=1;s=Motor1.Temps").Build()
	r := <-read.Execute(cctx)
	if r.GetErr() != nil {
		t.Fatal(r.GetErr())
	}
	if v := r.GetResponse().GetValue("speed").GetList()[0].GetFloat32(); v != 3.5 {
		t.Errorf("Motor1.Speed %v", v)
	}
	if v := r.GetResponse().GetValue("label").GetList()[0].GetString(); v != "m1" {
		t.Errorf("Motor1.Config.Label %q", v)
	}
	if v := r.GetResponse().GetValue("temps").GetList(); len(v) != 3 || v[2].GetInt16() != 3 {
		t.Errorf("Motor1.Temps %v", v)
	}
	write, _ := client.WriteRequestBuilder().AddTagAddress("max", "ns=1;s=Motor1.Config.MaxSpeed;LREAL", 120.0).Build()
	if w := <-write.Execute(cctx); w.GetErr() != nil || w.GetResponse().GetResponseCode("max") != apiModel.PlcResponseCode_OK {
		t.Fatalf("write Motor1.Config.MaxSpeed: %v %v", w.GetErr(), w.GetResponse().GetResponseCode("max"))
	}
	v, _ := db.GetTagValue("Motor1")
	motor := v.(*opcuaMotor)
	if motor.Config.MaxSpeed != 120 || motor.Speed != 3.5 || motor.Config.Label != "m1" {
		t.Errorf("Motor1 after the write: %+v %+v", motor, motor.Config)
	}

	// The methods and the alarm, as the server got them.
	if c.connections[0].server.opcua == nil {
		t.Fatal("no OPC UA server")
	}
	src := &opcuaSource{c: c, name: "plant"}
	reset, err := src.method(inout("Reset", ""), "Line.Reset", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reset.Call(nil, "op"); err != nil {
		t.Fatal(err)
	}
	if v, _ := db.GetTagValue("Reset"); v != plc.BOOL(true) {
		t.Errorf("Reset = %v", v)
	}
	setTarget, err := src.method(inout("Target", ""), "Line.SetTarget", "LREAL")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := setTarget.Call([]any{42.5}, ""); err != nil {
		t.Fatal(err)
	}
	if v, _ := db.GetTagValue("Target"); v != plc.REAL(42.5) {
		t.Errorf("Target = %v", v)
	}
	al, err := src.alarm(out("High", ""), "Line.High", "800")
	if err != nil {
		t.Fatal(err)
	}
	if al.Active() || al.Severity != 800 {
		t.Errorf("alarm %+v active %v", al, al.Active())
	}
	_ = db.SetTagValue("High", plc.BOOL(true))
	if !al.Active() {
		t.Error("the alarm did not follow its tag")
	}
}

// TestOPCUAServerCertDirectory: with cert-directory the server makes its
// certificate once and keeps it.
func TestOPCUAServerCertDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pki")
	_, port := startOPCUAServer(t, "security=Basic256Sha256&trust=*&cert-directory="+url.QueryEscape(dir))
	first, err := os.ReadFile(filepath.Join(dir, "server.pem"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	q := url.Values{"security-policy": {"Basic256Sha256"}, "message-security": {"SIGN_ENCRYPT"}, "tls.verify": {"false"}, "cert-directory": {t.TempDir()}}
	client := opcuaClient(t, ctx, fmt.Sprintf("opcua:tcp://127.0.0.1:%d?%s", port, q.Encode()))
	read, _ := client.ReadRequestBuilder().AddTagAddress("level", "ns=1;s=Tank.Level").Build()
	if r := <-read.Execute(ctx); r.GetErr() != nil || r.GetResponse().GetResponseCode("level") != apiModel.PlcResponseCode_OK {
		t.Fatalf("read over the made certificate: %v", r.GetErr())
	}
	db := honeycomb.NewTagDatabase()
	addTag(t, db, "A", honeycomb.TypeINT, plc.INT(0))
	if _, err := New(db, []Connection{{Name: "again", Mode: Server, URL: "opcua:tcp://127.0.0.1:0?security=Basic256Sha256&trust=*&cert-directory=" + url.QueryEscape(dir),
		Bindings: []Binding{out("A", "A")}}}); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(filepath.Join(dir, "server.pem")); string(again) != string(first) {
		t.Error("the certificate was made again")
	}
}

func TestOPCUAServerBindingErrors(t *testing.T) {
	db := honeycomb.NewTagDatabase()
	addTag(t, db, "A", honeycomb.TypeINT, plc.INT(0))
	addTag(t, db, "B", honeycomb.TypeBOOL, plc.BOOL(false))
	for want, b := range map[string]Binding{
		"is InOut":        out("B", "X()"),
		"sets a BOOL tag": inout("A", "X()"),
		"FOO is not":      inout("A", "X(FOO)"),
		"not 1 to 1000":   out("B", "X;ALARM[2000]"),
		"is not served":   out("A", "X;TIME"),
	} {
		_, err := New(db, []Connection{{Name: "S", Mode: Server, URL: "opcua:tcp://0.0.0.0:4840", Bindings: []Binding{b}}})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", want, err)
		}
	}
	for want, query := range map[string]string{
		"history":          "history=-1",
		"history-interval": "history-interval=soon",
		"alarm-interval":   "alarm-interval=0s",
	} {
		_, err := New(db, []Connection{{Name: "S", Mode: Server, URL: "opcua:tcp://0.0.0.0:4840?" + query, Bindings: []Binding{out("A", "A")}}})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", want, err)
		}
	}
}
