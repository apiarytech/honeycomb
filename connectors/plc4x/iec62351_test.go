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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	apiModel "github.com/apache/plc4x/plc4go/pkg/api/model"
	iec "github.com/apache/plc4x/plc4go/protocols/iec608705104/readwrite/model"
	"github.com/apiarytech/honeycomb"
	plc "github.com/apiarytech/royaljelly/iec"
)

// pki is a test CA and the files of the certificates it issues.
type pki struct {
	t    *testing.T
	dir  string
	ca   *x509.Certificate
	key  *ecdsa.PrivateKey
	n    int64
	file string // the CA's PEM
}

func newPKI(t *testing.T) *pki {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(der)
	p := &pki{t: t, dir: t.TempDir(), ca: ca, key: key, n: 1}
	p.file = filepath.Join(p.dir, "ca.pem")
	os.WriteFile(p.file, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	return p
}

// issue makes a certificate for name, with IEC 62351-8 roles when given,
// and returns its certificate and key files.
func (p *pki) issue(name string, server bool, roles ...int) (certFile, keyFile string) {
	p.t.Helper()
	p.n++
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(p.n), Subject: pkix.Name{CommonName: name}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature}
	if server {
		tmpl.ExtKeyUsage, tmpl.IPAddresses = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, []net.IP{net.ParseIP("127.0.0.1")}
	} else {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	if len(roles) > 0 {
		v, err := RolesExtension(time.Now().Add(-time.Hour), time.Now().Add(time.Hour), roles...)
		if err != nil {
			p.t.Fatal(err)
		}
		tmpl.ExtraExtensions = []pkix.Extension{{Id: OIDRoles, Value: v}}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.ca, &key.PublicKey, p.key)
	if err != nil {
		p.t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(key)
	certFile, keyFile = filepath.Join(p.dir, name+".pem"), filepath.Join(p.dir, name+".key")
	os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600)
	return certFile, keyFile
}

// station is a controlling station's TLS configuration with a certificate.
func (p *pki) station(certFile, keyFile string) *tls.Config {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		p.t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(p.ca)
	return &tls.Config{Certificates: []tls.Certificate{cert}, RootCAs: pool, ServerName: "127.0.0.1"}
}

// startTLSOutstation runs an outstation over TLS.
func startTLSOutstation(t *testing.T, p *pki, extra string) (*honeycomb.TagDatabase, int) {
	t.Helper()
	port := freePort(t)
	cert, key := p.issue("outstation", true)
	db := honeycomb.NewTagDatabase()
	addTag(t, db, "Level", honeycomb.TypeREAL, plc.REAL(12.5))
	addTag(t, db, "Run", honeycomb.TypeBOOL, plc.BOOL(false))
	c, err := New(db, []Connection{{Name: "outstation", Mode: Server,
		URL:      fmt.Sprintf("iec-60870-5-104://127.0.0.1:%d?tls=true&tls-cert=%s&tls-key=%s&tls-ca=%s%s", port, cert, key, p.file, extra),
		Bindings: []Binding{out("Level", "1/100:FLOAT"), inout("Run", "1/200:SP")}}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return db, port
}

// dialTLS is a raw controlling station over TLS; nil when the outstation
// refuses it.
func dialTLS(t *testing.T, port int, cfg *tls.Config) *iecClient {
	t.Helper()
	var conn *tls.Conn
	var err error
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err = tls.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port), cfg)
		if err == nil {
			// TLS 1.3 reports a refused client certificate on the first read.
			conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
			var b [1]byte
			if _, rerr := conn.Read(b[:]); rerr != nil {
				if ne, ok := rerr.(net.Error); !ok || !ne.Timeout() {
					conn.Close()
					return nil
				}
			}
			conn.SetReadDeadline(time.Time{})
			return &iecClient{t: t, conn: conn}
		}
		if _, ok := err.(*net.OpError); !ok {
			return nil // the handshake failed: refused
		}
		time.Sleep(20 * time.Millisecond)
	}
	return nil
}

// command sends a single command to IOA 200 and says whether it was
// confirmed positively.
func command(cl *iecClient, on bool) bool {
	sc := iec.NewInformationObjectWithoutTime_SINGLE_COMMAND(200, iec.NewSingleCommand(iec.NewQualifierOfCommand(false, 0), on))
	cl.writeI(iec.NewASDU(iec.TypeIdentification_SINGLE_COMMAND, false, false, false, cotActivation, 0, 1, []iec.InformationObject{sc}))
	for {
		a := cl.expectASDU()
		if a.GetTypeIdentification() == iec.TypeIdentification_SINGLE_COMMAND {
			return a.GetCauseOfTransmission() == cotActivationCon && !a.GetNegative()
		}
	}
}

// An outstation over TLS (IEC 62351-3) commands only for a station whose
// certificate carries the role OPERATOR (IEC 62351-8); a viewer and a
// station without roles read; a certificate from another CA is refused.
func TestIECOverTLSRoles(t *testing.T) {
	p := newPKI(t)
	db, port := startTLSOutstation(t, p, "")

	opCert, opKey := p.issue("operator", false, RoleOperator)
	op := dialTLS(t, port, p.station(opCert, opKey))
	if op == nil {
		t.Fatal("the operator was refused")
	}
	defer op.close()
	op.handshake()
	if !command(op, true) {
		t.Fatal("the operator's command was refused")
	}
	waitFor(t, func() bool { v, _ := db.GetTagValue("Run"); return v == plc.BOOL(true) })

	for _, who := range []struct {
		name  string
		roles []int
	}{{"viewer", []int{RoleViewer}}, {"engineer", []int{RoleEngineer}}, {"nobody", nil}} {
		cert, key := p.issue(who.name, false, who.roles...)
		cl := dialTLS(t, port, p.station(cert, key))
		if cl == nil {
			t.Fatalf("%s was refused: it may read", who.name)
		}
		cl.handshake()
		if command(cl, false) {
			t.Errorf("%s commanded", who.name)
		}
		cl.close()
	}
	if v, _ := db.GetTagValue("Run"); v != plc.BOOL(true) {
		t.Error("a station without OPERATOR changed Run")
	}

	other := newPKI(t)
	cert, key := other.issue("stranger", false, RoleOperator)
	cfg := other.station(cert, key)
	cfg.RootCAs.AddCert(p.ca) // it trusts the outstation; the outstation does not trust it
	if cl := dialTLS(t, port, cfg); cl != nil {
		cl.close()
		t.Error("a certificate from another CA was accepted")
	}
	if cl := dialIEC(t, port); cl != nil {
		// Plain TCP to a TLS port: the outstation never answers the handshake.
		cl.conn.SetReadDeadline(time.Now().Add(time.Second))
		cl.conn.Write([]byte{0x68, 4, iecStartDTAct, 0, 0, 0})
		var b [6]byte
		if n, err := cl.conn.Read(b[:]); err == nil && n == 6 && b[0] == 0x68 {
			t.Error("a plain TCP station was answered")
		}
		cl.close()
	}
}

// tls-roles=off: any authenticated station commands, as over plain TCP.
func TestIECOverTLSRolesOff(t *testing.T) {
	p := newPKI(t)
	db, port := startTLSOutstation(t, p, "&tls-roles=off")
	cert, key := p.issue("anyone", false)
	cl := dialTLS(t, port, p.station(cert, key))
	if cl == nil {
		t.Fatal("refused")
	}
	defer cl.close()
	cl.handshake()
	if !command(cl, true) {
		t.Fatal("refused with roles off")
	}
	waitFor(t, func() bool { v, _ := db.GetTagValue("Run"); return v == plc.BOOL(true) })
}

// PLC4X's client, as a controlling station over TLS through the
// connector's tunnel, receives the outstation's spontaneous updates.
func TestIECClientThroughTunnel(t *testing.T) {
	p := newPKI(t)
	db, port := startTLSOutstation(t, p, "")
	cert, key := p.issue("scada", false, RoleViewer)
	cs := &connState{Connection: Connection{Name: "rtu", URL: fmt.Sprintf("iec-60870-5-104://127.0.0.1:%d?tls=true&tls-cert=%s&tls-key=%s&tls-ca=%s", port, cert, key, p.file)}}
	stop, err := startTunnel(cs)
	if err != nil || stop == nil {
		t.Fatalf("tunnel: %v", err)
	}
	defer stop()
	if u := cs.plc4xURL(); u == cs.URL || !containsAll(u, "127.0.0.1:") || containsAll(u, "tls") {
		t.Fatalf("PLC4X's URL %q", u)
	}

	m, err := NewDriverManagerFor("iec-60870-5-104")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn := connectClient(t, ctx, m, cs.plc4xURL())
	defer conn.Close()
	events := make(chan apiModel.PlcSubscriptionEvent, 16)
	b := conn.SubscriptionRequestBuilder()
	b.AddChangeOfStateTagAddress("level", "1/100")
	b.AddPreRegisteredConsumer("level", func(e apiModel.PlcSubscriptionEvent) { events <- e })
	req, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	if r := <-req.Execute(ctx); r.GetErr() != nil {
		t.Fatal(r.GetErr())
	}
	db.SetTagValue("Level", plc.REAL(-3.25))
	for {
		select {
		case e := <-events:
			if v := e.GetValue("level").GetStruct()["value"]; v != nil && v.GetFloat32() == -3.25 {
				return
			}
		case <-ctx.Done():
			t.Fatal("no update through the tunnel")
		}
	}
}

func TestIEC62351Options(t *testing.T) {
	for _, bad := range []string{
		"iec-60870-5-104://h:19998?tls=yes",
		"iec-60870-5-104://h:19998?tls=true&tls-cert=a&tls-key=b",
		"iec-60870-5-104://h:19998?tls-cert=a",
		"iec-60870-5-104://h:19998?tls=true&tls-cert=a&tls-key=b&tls-ca=c&tls-roles=maybe",
	} {
		if _, _, _, err := tunneledURL(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	if _, _, _, err := tunneledURL("iec-60870-5-104://h:19998?tls=true&tls-cert=a&tls-key=b&tls-ca=c&tls-roles=off"); err == nil {
		t.Error("a client with tls-roles")
	}
	plain, remote, o, err := tunneledURL("iec-60870-5-104://rtu7?tls=true&tls-cert=a&tls-key=b&tls-ca=c&t1=15")
	if err != nil || !o.on || remote != "rtu7:19998" || o.serverName != "rtu7" || plain != "iec-60870-5-104://rtu7?t1=15" {
		t.Errorf("%q %q %+v %v", plain, remote, o, err)
	}
	if plain, _, o, err := tunneledURL("iec-60870-5-104://rtu7:2404"); err != nil || o.on || plain != "iec-60870-5-104://rtu7:2404" {
		t.Errorf("plain: %q %v %v", plain, o.on, err)
	}

	// Roles, with their validity.
	now := time.Now()
	v, _ := asn1.Marshal([]roleInformation{
		{RoleID: RoleOperator, AoR: "a", Revision: 1, ValidFrom: now.Add(-time.Hour).UTC(), ValidTo: now.Add(-time.Minute).UTC()},
		{RoleID: RoleViewer, AoR: "a", Revision: 1, ValidFrom: now.Add(-time.Hour).UTC(), ValidTo: now.Add(time.Hour).UTC()}})
	cert := &x509.Certificate{Extensions: []pkix.Extension{{Id: OIDRoles, Value: v}}}
	if rs, err := RolesOf(cert, now); err != nil || len(rs) != 1 || rs[0] != RoleViewer {
		t.Errorf("roles %v %v: an expired role counted", rs, err)
	}
	if _, err := RolesOf(&x509.Certificate{Extensions: []pkix.Extension{{Id: OIDRoles, Value: []byte{1, 2, 3}}}}, now); err == nil {
		t.Error("a malformed extension read as no roles")
	}
}

func containsAll(s string, parts ...string) bool {
	for _, p := range parts {
		if !contains(s, p) {
			return false
		}
	}
	return true
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
