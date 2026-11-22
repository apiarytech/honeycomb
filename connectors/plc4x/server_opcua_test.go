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
		"cannot also be a folder": {URL: "opcua:tcp://0.0.0.0:4840", Bindings: []Binding{out("A", "X"), out("A", "X.Y")}},
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
