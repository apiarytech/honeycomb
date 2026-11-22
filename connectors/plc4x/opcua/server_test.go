/*
 * Copyright (C) 2026 Franklin D. Amador
 *
 * This software is dual-licensed under the terms of the GPL v3.0 and
 * a commercial license. You may choose to use this software under either
 * license.
 *
 * See the LICENSE files in the project root for full license text.
 */

package opcua

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	plc4go "github.com/apache/plc4x/plc4go/pkg/api"
	"github.com/apache/plc4x/plc4go/pkg/api/drivers"
	apiModel "github.com/apache/plc4x/plc4go/pkg/api/model"
	apiValues "github.com/apache/plc4x/plc4go/pkg/api/values"
)

// memory is a Source over a map.
type memory struct {
	mutex  sync.Mutex
	values map[string]any
	writes []string
}

func (m *memory) Read(p Point) (Value, bool) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	v, ok := m.values[p.Name]
	return Value{Value: v, Good: true, Time: time.Now()}, ok
}

func (m *memory) Write(p Point, value any, by string) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	if p.Name == "Line1.Refused" {
		return errors.New("refused")
	}
	m.values[p.Name] = value
	m.writes = append(m.writes, fmt.Sprintf("%s=%v by %q", p.Name, value, by))
	return nil
}

func (m *memory) set(name string, value any) {
	m.mutex.Lock()
	m.values[name] = value
	m.mutex.Unlock()
}

func (m *memory) get(name string) any {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	return m.values[name]
}

func testPoints() []Point {
	return []Point{
		{Name: "Line1.Speed", DataType: TypeDouble, Description: "belt speed"},
		{Name: "Line1.Setpoint", DataType: TypeFloat, Writable: true},
		{Name: "Line1.Count", DataType: TypeInt32, Writable: true},
		{Name: "Line1.Running", DataType: TypeBoolean, Writable: true},
		{Name: "Line1.Name", DataType: TypeString},
		{Name: "Line1.Temps", DataType: TypeInt16, ArrayLength: 3, Writable: true},
		{Name: "Line1.Refused", DataType: TypeInt32, Writable: true},
		{Name: "Tank", DataType: TypeUInt16},
	}
}

// identity makes a server certificate for applicationURI.
func identity(t *testing.T, applicationURI string) ([]byte, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	uri, _ := url.Parse(applicationURI)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "honeycomb test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageDataEncipherment | x509.KeyUsageContentCommitment | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		URIs:        []*url.URL{uri}, DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return der, key
}

// startServer serves cfg on a loopback port and returns the client URL's base.
func startServer(t *testing.T, cfg Config) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.EndpointURL = "opc.tcp://" + ln.Addr().String()
	var errs []error
	var errMutex sync.Mutex
	cfg.OnError = func(err error) {
		errMutex.Lock()
		errs = append(errs, err)
		errMutex.Unlock()
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.Serve(ctx, ln, nil)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		errMutex.Lock()
		defer errMutex.Unlock()
		for _, err := range errs {
			t.Logf("server: %v", err)
		}
	})
	return ln.Addr().String()
}

func newMemory() *memory {
	return &memory{values: map[string]any{
		"Line1.Speed": 12.5, "Line1.Setpoint": float32(3), "Line1.Count": int32(7), "Line1.Running": true,
		"Line1.Name": "packer", "Line1.Temps": []int16{20, 21, 22}, "Line1.Refused": int32(0),
	}}
}

func connect(t *testing.T, ctx context.Context, url string) plc4go.PlcConnection {
	t.Helper()
	m := plc4go.NewPlcDriverManager()
	drivers.RegisterOpcuaDriver(m)
	conn, err := m.GetConnection(ctx, url)
	if err != nil {
		t.Fatalf("connect %s: %v", url, err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// first is a read value's first element: the driver reads a scalar as a list of one.
func first(v apiValues.PlcValue) apiValues.PlcValue {
	if v != nil && v.IsList() && len(v.GetList()) > 0 {
		return v.GetList()[0]
	}
	return v
}

func TestServerWithPLC4XClient(t *testing.T) {
	const applicationURI = "urn:apiarytech:honeycomb:test"
	certificate, key := identity(t, applicationURI)
	cases := []struct {
		name, policy, mode, options string
	}{
		{"None", "None", "None", ""},
		{"Basic256Sha256", "Basic256Sha256", "SignAndEncrypt", "&security-policy=Basic256Sha256&message-security=SIGN_ENCRYPT"},
		{"Aes128Sign", "Aes128_Sha256_RsaOaep", "Sign", "&security-policy=Aes128_Sha256_RsaOaep&message-security=SIGN"},
		{"Aes256Pss", "Aes256_Sha256_RsaPss", "SignAndEncrypt", "&security-policy=Aes256_Sha256_RsaPss&message-security=SIGN_ENCRYPT"},
		{"Basic128Rsa15", "Basic128Rsa15", "SignAndEncrypt", "&security-policy=Basic128Rsa15&message-security=SIGN_ENCRYPT"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			source := newMemory()
			address := startServer(t, Config{
				ApplicationURI: applicationURI, Points: testPoints(), Source: source,
				Endpoints:   []Endpoint{{Policy: "None", Mode: "None"}, {Policy: tc.policy, Mode: tc.mode}},
				Certificate: certificate, Key: key,
				TrustClient: func([]*x509.Certificate) error { return nil },
			})
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			url := fmt.Sprintf("opcua:tcp://%s?discovery=true&tls.verify=false&allow-unverified-security-policies=true&cert-directory=%s%s",
				address, t.TempDir(), tc.options)
			conn := connect(t, ctx, url)
			exercise(t, ctx, conn, source)
		})
	}
}

// exercise reads, writes, browses and subscribes over a connection.
func exercise(t *testing.T, ctx context.Context, conn plc4go.PlcConnection, source *memory) {
	t.Helper()
	read, err := conn.ReadRequestBuilder().
		AddTagAddress("speed", "ns=1;s=Line1.Speed").
		AddTagAddress("count", "ns=1;s=Line1.Count").
		AddTagAddress("running", "ns=1;s=Line1.Running").
		AddTagAddress("name", "ns=1;s=Line1.Name").
		AddTagAddress("temps", "ns=1;s=Line1.Temps").
		AddTagAddress("state", "ns=0;i=2259").
		AddTagAddress("missing", "ns=1;s=Nope").
		Build()
	if err != nil {
		t.Fatal(err)
	}
	r := <-read.Execute(ctx)
	if r.GetErr() != nil {
		t.Fatal(r.GetErr())
	}
	response := r.GetResponse()
	for _, name := range []string{"speed", "count", "running", "name", "temps", "state"} {
		if code := response.GetResponseCode(name); code != apiModel.PlcResponseCode_OK {
			t.Fatalf("read %s: %s", name, code)
		}
	}
	if v := first(response.GetValue("speed")).GetFloat64(); v != 12.5 {
		t.Errorf("speed %v", v)
	}
	if v := first(response.GetValue("count")).GetInt32(); v != 7 {
		t.Errorf("count %v", v)
	}
	if v := first(response.GetValue("running")).GetBool(); !v {
		t.Errorf("running %v", v)
	}
	if v := first(response.GetValue("name")).GetString(); v != "packer" {
		t.Errorf("name %q", v)
	}
	if v := response.GetValue("temps").GetList(); len(v) != 3 || v[2].GetInt16() != 22 {
		t.Errorf("temps %v", v)
	}
	if v := first(response.GetValue("state")).GetInt32(); v != 0 {
		t.Errorf("server state %v", v)
	}
	if code := response.GetResponseCode("missing"); code != apiModel.PlcResponseCode_NOT_FOUND {
		t.Errorf("missing: %s", code)
	}

	write, err := conn.WriteRequestBuilder().
		AddTagAddress("setpoint", "ns=1;s=Line1.Setpoint;REAL", 4.25).
		AddTagAddress("count", "ns=1;s=Line1.Count;DINT", 42).
		AddTagAddress("speed", "ns=1;s=Line1.Speed;LREAL", 1.0).
		Build()
	if err != nil {
		t.Fatal(err)
	}
	w := <-write.Execute(ctx)
	if w.GetErr() != nil {
		t.Fatal(w.GetErr())
	}
	if code := w.GetResponse().GetResponseCode("setpoint"); code != apiModel.PlcResponseCode_OK {
		t.Errorf("write setpoint: %s", code)
	}
	if code := w.GetResponse().GetResponseCode("count"); code != apiModel.PlcResponseCode_OK {
		t.Errorf("write count: %s", code)
	}
	if code := w.GetResponse().GetResponseCode("speed"); code == apiModel.PlcResponseCode_OK {
		t.Errorf("a read-only point took a write")
	}
	if v := source.get("Line1.Setpoint"); v != float32(4.25) {
		t.Errorf("setpoint written as %v (%T)", v, v)
	}
	if v := source.get("Line1.Count"); v != int32(42) {
		t.Errorf("count written as %v (%T)", v, v)
	}

	browse, err := conn.BrowseRequestBuilder().AddQuery("all", "").Build()
	if err != nil {
		t.Fatal(err)
	}
	b := <-browse.Execute(ctx)
	if b.GetErr() != nil {
		t.Fatal(b.GetErr())
	}
	var names []string
	var walk func(prefix string, items []apiModel.PlcBrowseItem)
	walk = func(prefix string, items []apiModel.PlcBrowseItem) {
		for _, item := range items {
			names = append(names, prefix+item.GetName())
			var children []apiModel.PlcBrowseItem
			for _, child := range item.GetChildren() {
				children = append(children, child)
			}
			walk(prefix+item.GetName()+"/", children)
		}
	}
	walk("", b.GetResponse().GetQueryResults("all"))
	for _, want := range []string{"Speed", "Temps", "Tank", "Server"} {
		if !slices.ContainsFunc(names, func(n string) bool { return n == want || len(n) > len(want) && n[len(n)-len(want)-1:] == "/"+want }) {
			t.Errorf("browse has no %s in %v", want, names)
		}
	}

	events := make(chan float64, 16)
	builder := conn.SubscriptionRequestBuilder()
	builder.AddCyclicTagAddress("speed", "ns=1;s=Line1.Speed", 100*time.Millisecond)
	builder.AddPreRegisteredConsumer("speed", func(e apiModel.PlcSubscriptionEvent) {
		if e.GetResponseCode("speed") == apiModel.PlcResponseCode_OK {
			events <- first(e.GetValue("speed")).GetFloat64()
		}
	})
	subscription, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	s := <-subscription.Execute(ctx)
	if s.GetErr() != nil {
		t.Fatal(s.GetErr())
	}
	if code := s.GetResponse().GetResponseCode("speed"); code != apiModel.PlcResponseCode_OK {
		t.Fatalf("subscribe: %s", code)
	}
	wait := func(want float64) {
		t.Helper()
		for {
			select {
			case v := <-events:
				if v == want {
					return
				}
			case <-ctx.Done():
				t.Fatalf("no notification of %v", want)
			}
		}
	}
	wait(12.5) // the initial value
	source.set("Line1.Speed", 99.0)
	wait(99.0)
}

func TestServerLogin(t *testing.T) {
	const applicationURI = "urn:apiarytech:honeycomb:test"
	certificate, key := identity(t, applicationURI)
	source := newMemory()
	address := startServer(t, Config{
		ApplicationURI: applicationURI, Points: testPoints(), Source: source,
		Certificate: certificate, Key: key,
		Login: func(user, password string) bool { return user == "op" && password == "s3cret" },
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	base := fmt.Sprintf("opcua:tcp://%s?discovery=true&tls.verify=false&cert-directory=%s", address, t.TempDir())
	conn := connect(t, ctx, base+"&username=op&password=s3cret")
	write, _ := conn.WriteRequestBuilder().AddTagAddress("count", "ns=1;s=Line1.Count;DINT", 5).Build()
	if w := <-write.Execute(ctx); w.GetErr() != nil || w.GetResponse().GetResponseCode("count") != apiModel.PlcResponseCode_OK {
		t.Fatalf("write as op: %v", w.GetErr())
	}
	source.mutex.Lock()
	writes := slices.Clone(source.writes)
	source.mutex.Unlock()
	if !slices.Contains(writes, `Line1.Count=5 by "op"`) {
		t.Errorf("writes %v", writes)
	}

	m := plc4go.NewPlcDriverManager()
	drivers.RegisterOpcuaDriver(m)
	for _, url := range []string{base + "&username=op&password=wrong", base} {
		if conn, err := m.GetConnection(ctx, url); err == nil {
			conn.Close()
			t.Errorf("%s connected", url)
		}
	}
}

func TestServerConfig(t *testing.T) {
	source := newMemory()
	cases := []struct {
		name string
		cfg  Config
	}{
		{"no source", Config{}},
		{"secured without certificate", Config{Source: source, Endpoints: []Endpoint{{Policy: "Basic256Sha256", Mode: "Sign"}}}},
		{"None with Sign", Config{Source: source, Endpoints: []Endpoint{{Policy: "None", Mode: "Sign"}}}},
		{"unknown policy", Config{Source: source, Endpoints: []Endpoint{{Policy: "Basic512", Mode: "Sign"}}}},
		{"point and folder", Config{Source: source, Points: []Point{{Name: "A", DataType: TypeInt32}, {Name: "A.B", DataType: TypeInt32}}}},
		{"duplicate", Config{Source: source, Points: []Point{{Name: "A", DataType: TypeInt32}, {Name: "A", DataType: TypeInt32}}}},
		{"empty part", Config{Source: source, Points: []Point{{Name: "A..B", DataType: TypeInt32}}}},
		{"bad type", Config{Source: source, Points: []Point{{Name: "A", DataType: 99}}}},
	}
	for _, tc := range cases {
		if _, err := New(tc.cfg); err == nil {
			t.Errorf("%s: accepted", tc.name)
		}
	}
	if _, err := New(Config{Source: source, Points: testPoints()}); err != nil {
		t.Errorf("valid config: %v", err)
	}
}

func TestVariantRoundTrip(t *testing.T) {
	now := time.Date(2026, 11, 22, 10, 30, 0, 500, time.UTC).Truncate(100 * time.Nanosecond)
	cases := []struct {
		value    any
		dataType uint32
		want     any
	}{
		{true, TypeBoolean, true},
		{int8(-5), TypeSByte, int8(-5)},
		{uint8(200), TypeByte, uint8(200)},
		{int16(-300), TypeInt16, int16(-300)},
		{uint16(60000), TypeUInt16, uint16(60000)},
		{int32(-70000), TypeInt32, int32(-70000)},
		{uint32(4000000000), TypeUInt32, uint32(4000000000)},
		{int64(-1 << 40), TypeInt64, int64(-1 << 40)},
		{uint64(1 << 63), TypeUInt64, uint64(1 << 63)},
		{float32(1.5), TypeFloat, float32(1.5)},
		{2.25, TypeDouble, 2.25},
		{"text", TypeString, "text"},
		{now, TypeDateTime, now},
		{7, TypeDouble, 7.0},
		{[]int{1, 2}, TypeInt16, []int16{1, 2}},
		{[]string{"a"}, TypeString, []string{"a"}},
	}
	for _, tc := range cases {
		variant, err := variantOf(tc.value, tc.dataType)
		if err != nil {
			t.Errorf("%v as %d: %v", tc.value, tc.dataType, err)
			continue
		}
		got, dataType, err := valueOf(variant)
		if err != nil || dataType != tc.dataType || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%v as %d: got %v (%T) of %d, %v", tc.value, tc.dataType, got, got, dataType, err)
		}
	}
	if _, err := variantOf("x", TypeDouble); err == nil {
		t.Error("a string encoded as a double")
	}
}
