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

// OPC UA server, the connector's own code (package opcua): PLC4X has an OPC
// UA client only. A server connection
//
//	opcua:tcp://0.0.0.0:4840?security=Basic256Sha256&cert=server.pem&key=server-key.pem&trust=clients.pem
//
// answers OPC UA clients over UA-TCP (port 4840 by default). Its bindings
// are variables in namespace 1, named by a dotted path that makes the
// folders under Objects, with an optional data type:
//
//	Line1.Speed            the tag's own type
//	ns=1;s=Line1.Speed     the same, as a node id
//	Line1.Temps;INT[3]     an array of 3 INT
//	Motor1                 a UDT tag: a structure, with a variable per field
//
// The data type is an IEC 61131-3 or OPC UA name (BOOL, INT, DINT, REAL,
// LREAL, STRING, DT, ..., or Boolean, Int16, Double, ...). Without one the
// tag's declared type is used. Output bindings are read only; InOut ones a
// client may write. Clients may subscribe to every variable.
//
// Two other forms of binding:
//
//	Line1.Reset()          a method that sets the (BOOL) tag to TRUE; InOut
//	Line1.SetSpeed(REAL)   a method that writes its argument to the tag; InOut
//	Line1.HighLevel;ALARM[700]  an alarm, active while the tag is TRUE (or not
//	                       zero), severity 1 to 1000 (500 by default), the
//	                       tag's description its message
//
// The options:
//
//   - security: the endpoints, a comma-separated list of policy[:mode], the
//     mode Sign or SignAndEncrypt (the default). None (the default) and
//     Basic128Rsa15, Basic256, Basic256Sha256, Aes128_Sha256_RsaOaep,
//     Aes256_Sha256_RsaPss. A list without None refuses unsecured clients.
//   - cert, key: the server's certificate and RSA key (PEM files), needed by
//     every policy but None and to encrypt passwords. The certificate should
//     carry the application URI as a URI subject alternative name.
//   - trust: a PEM file of the client certificates, or the CAs of them, the
//     server trusts on a secured channel. trust=* trusts every client (for
//     tests only).
//   - users: a file of "name:password" or "name:sha256:<hex>" lines. With
//     users a client logs in with a user name; anonymous=true lets clients
//     in without one as well.
//   - uri, name: the application URI and name (default urn:<host>:honeycomb:
//     <connection>, honeycomb <connection>).
//   - endpoint: the URL the server gives clients (default opc.tcp://<this
//     host's name>:<port>).
//   - cert-directory: where the server keeps a certificate and key it makes
//     (server.pem, server-key.pem) when cert and key are not given.
//   - history: values kept per variable for HistoryRead (in memory), and as
//     many events; history-events changes the events', history-interval
//     (1s) how often the variables are sampled.
//   - alarm-interval: how often the alarms' tags are looked at (250ms).
//   - read-only-interfaces: clients that reach the server on these
//     interfaces only read; they cannot call methods or acknowledge alarms.

import (
	"bufio"
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	apiValues "github.com/apache/plc4x/plc4go/pkg/api/values"
	spiValues "github.com/apache/plc4x/plc4go/spi/values"
	"github.com/apiarytech/honeycomb"

	"github.com/apiarytech/honeycomb/connectors/plc4x/opcua"
)

// opcuaServer is an OPC UA server connection.
type opcuaServer struct {
	listen   string
	readOnly []string
	server   *opcua.Server
}

// opcuaSource reads and writes the bindings' tags.
type opcuaSource struct {
	c    *Connector
	name string
	tags map[string]string       // point name -> tag
	udts map[string]reflect.Type // point name -> struct type, for UDT tags
}

var (
	opcuaAddress = regexp.MustCompile(`^(?:ns=1;s=)?([^;\[\]()]+)(?:;([A-Za-z_]+)(?:\[(\d{1,6})\])?)?$`)
	opcuaMethod  = regexp.MustCompile(`^(?:ns=1;s=)?([^;\[\]()]+)\(([A-Za-z_]*)\)$`)
)

func newOPCUAServer(c *Connector, conn Connection) (*opcuaServer, error) {
	_, host, port, q, err := parseOPCUAURL(conn.URL)
	if err != nil {
		return nil, fmt.Errorf("plc4x: server %q: %w", conn.Name, err)
	}
	fail := func(format string, args ...any) error {
		return fmt.Errorf("plc4x: server %q: "+format, append([]any{conn.Name}, args...)...)
	}
	s := &opcuaServer{listen: net.JoinHostPort(host, port), readOnly: readOnlyInterfaces(q)}
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "localhost"
	}
	cfg := opcua.Config{
		ApplicationURI:  q.Get("uri"),
		ApplicationName: q.Get("name"),
		EndpointURL:     q.Get("endpoint"),
		OnError:         func(err error) { c.onError(conn.Name, err) },
	}
	if cfg.ApplicationURI == "" {
		cfg.ApplicationURI = "urn:" + hostname + ":honeycomb:" + url.PathEscape(conn.Name)
	}
	if cfg.ApplicationName == "" {
		cfg.ApplicationName = "honeycomb " + conn.Name
	}
	if cfg.EndpointURL == "" {
		advertised := host
		if ip := net.ParseIP(host); host == "" || ip != nil && ip.IsUnspecified() {
			advertised = hostname
		}
		cfg.EndpointURL = "opc.tcp://" + net.JoinHostPort(advertised, port)
	}

	secured := false
	for _, item := range strings.Split(q.Get("security"), ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		policy, mode, _ := strings.Cut(item, ":")
		switch {
		case policy == "None" && mode == "":
			mode = "None"
		case mode == "":
			mode = "SignAndEncrypt"
		}
		if policy != "None" {
			secured = true
		}
		cfg.Endpoints = append(cfg.Endpoints, opcua.Endpoint{Policy: policy, Mode: mode})
	}
	cert, key := q.Get("cert"), q.Get("key")
	if dir := q.Get("cert-directory"); dir != "" && cert == "" && key == "" {
		cert, key = filepath.Join(dir, "server.pem"), filepath.Join(dir, "server-key.pem")
		if err := ensureOPCUAIdentity(dir, cert, key, cfg.ApplicationURI, cfg.ApplicationName, []string{hostname, host}); err != nil {
			return nil, fail("cert-directory: %w", err)
		}
	}
	if cert != "" || key != "" {
		if cert == "" || key == "" {
			return nil, fail("cert and key go together")
		}
		if cfg.Certificate, cfg.Key, err = loadOPCUAIdentity(cert, key); err != nil {
			return nil, fail("%w", err)
		}
	} else if secured {
		return nil, fail("a security policy other than None needs the server's cert and key, or a cert-directory to make them in")
	}
	switch trust := q.Get("trust"); {
	case trust == "*":
		cfg.TrustClient = func([]*x509.Certificate) error { return nil }
	case trust != "":
		if cfg.TrustClient, err = opcuaTrust(trust); err != nil {
			return nil, fail("trust: %w", err)
		}
	case secured:
		return nil, fail("a security policy other than None needs trust: the clients' certificates or their CAs")
	}
	if users := q.Get("users"); users != "" {
		if cfg.Login, err = opcuaUsers(users); err != nil {
			return nil, fail("users: %w", err)
		}
	}
	switch q.Get("anonymous") {
	case "", "false":
	case "true":
		cfg.Anonymous = true
	default:
		return nil, fail("anonymous %q: true or false", q.Get("anonymous"))
	}

	if v := q.Get("history"); v != "" {
		if cfg.History.Depth, err = strconv.Atoi(v); err != nil || cfg.History.Depth < 0 {
			return nil, fail("history %q: the number of values kept per variable", v)
		}
		cfg.History.Events = cfg.History.Depth
	}
	if v := q.Get("history-events"); v != "" {
		if cfg.History.Events, err = strconv.Atoi(v); err != nil || cfg.History.Events < 0 {
			return nil, fail("history-events %q: the number of events kept", v)
		}
	}
	for option, target := range map[string]*time.Duration{"history-interval": &cfg.History.Interval, "alarm-interval": &cfg.AlarmInterval} {
		if v := q.Get(option); v != "" {
			if *target, err = time.ParseDuration(v); err != nil || *target <= 0 {
				return nil, fail("%s %q: a duration such as 1s", option, v)
			}
		}
	}

	src := &opcuaSource{c: c, name: conn.Name, tags: map[string]string{}, udts: map[string]reflect.Type{}}
	structures := map[string]opcua.Structure{}
	for _, b := range conn.Bindings {
		address := strings.TrimSpace(b.Address)
		if m := opcuaMethod.FindStringSubmatch(address); m != nil {
			method, err := src.method(b, m[1], m[2])
			if err != nil {
				return nil, fail("tag %q: %w", b.Tag, err)
			}
			cfg.Methods = append(cfg.Methods, method)
			continue
		}
		if m := opcuaAddress.FindStringSubmatch(address); m != nil && strings.EqualFold(m[2], "ALARM") {
			al, err := src.alarm(b, m[1], m[3])
			if err != nil {
				return nil, fail("tag %q: %w", b.Tag, err)
			}
			cfg.Alarms = append(cfg.Alarms, al)
			continue
		}
		p, err := opcuaPoint(c.db, b)
		if err != nil {
			return nil, fail("tag %q: %w", b.Tag, err)
		}
		if _, dup := src.tags[p.Name]; dup {
			return nil, fail("two variables named %s", p.Name)
		}
		if p.DataType == opcua.TypeStructure {
			tag, _ := c.db.GetTag(b.Tag)
			t, _ := udtStruct(tag.Value)
			if p.Structure, err = udtStructures(t, structures); err != nil {
				return nil, fail("tag %q: %w", b.Tag, err)
			}
			src.udts[p.Name] = t
		}
		p.Historize = cfg.History.Depth > 0
		src.tags[p.Name] = b.Tag
		cfg.Points = append(cfg.Points, p)
	}
	for _, st := range structures {
		cfg.Structures = append(cfg.Structures, st)
	}
	sort.Slice(cfg.Structures, func(i, j int) bool { return cfg.Structures[i].Name < cfg.Structures[j].Name })
	cfg.Source = src
	if s.server, err = opcua.New(cfg); err != nil {
		return nil, fail("%w", err)
	}
	return s, nil
}

// parseOPCUAURL reads opcua:tcp://host:port?..., as PLC4X spells an OPC UA
// connection, or opcua://host:port?....
func parseOPCUAURL(raw string) (*url.URL, string, string, url.Values, error) {
	rest := strings.TrimPrefix(raw[len("opcua"):], ":")
	if !strings.Contains(rest, "://") {
		rest = "tcp:" + rest
	}
	if !strings.HasPrefix(rest, "tcp://") && !strings.HasPrefix(rest, "//") {
		return nil, "", "", nil, fmt.Errorf("an OPC UA server listens on TCP (opcua:tcp://0.0.0.0:4840), not %q", raw)
	}
	return parseServerTCPURL(strings.TrimPrefix(rest, "tcp:"), "4840")
}

// opcuaPoint is the variable of a binding.
func opcuaPoint(db *honeycomb.TagDatabase, b Binding) (opcua.Point, error) {
	m := opcuaAddress.FindStringSubmatch(strings.TrimSpace(b.Address))
	if m == nil {
		return opcua.Point{}, fmt.Errorf("address %q: a dotted name with an optional type, e.g. Line1.Speed or Line1.Temps;INT[3]", b.Address)
	}
	p := opcua.Point{Name: m[1], Writable: b.Direction == InOut}
	tag, known := db.GetTag(b.Tag)
	if known {
		p.Description = tag.Description
	}
	switch {
	case m[2] == "" && known && isUDT(tag.Value):
		p.DataType = opcua.TypeStructure
	case m[2] != "":
		t, ok := opcua.TypeByName(strings.ToUpper(m[2]))
		if !ok {
			return p, fmt.Errorf("data type %s is not served: BOOL, SINT, USINT, INT, UINT, DINT, UDINT, LINT, ULINT, REAL, LREAL, STRING or DT", m[2])
		}
		p.DataType = t
		if m[3] != "" {
			p.ArrayLength, _ = strconv.Atoi(m[3])
			if p.ArrayLength == 0 {
				return p, errors.New("an array has elements")
			}
		}
	case known && tag.TypeInfo != nil:
		name := string(tag.TypeInfo.DataType)
		if tag.TypeInfo.DataType == honeycomb.TypeARRAY {
			if len(tag.TypeInfo.Dimensions) != 1 {
				return p, errors.New("only one-dimensional arrays are served")
			}
			name, p.ArrayLength = string(tag.TypeInfo.ElementType), tag.TypeInfo.Dimensions[0]
		}
		t, ok := opcua.TypeByName(name)
		if !ok {
			return p, fmt.Errorf("data type %s is not served", name)
		}
		p.DataType = t
	case known && tag.Value != nil:
		p.DataType = opcua.TypeOf(tag.Value)
		if rv := reflect.ValueOf(tag.Value); rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array {
			p.ArrayLength = rv.Len()
		}
		if p.DataType == 0 {
			return p, fmt.Errorf("a %T is not served", tag.Value)
		}
	default:
		return p, errors.New("no data type: give one in the address (Line1.Speed;REAL) or declare the tag's type")
	}
	return p, nil
}

func isUDT(v any) bool {
	_, ok := udtStruct(v)
	return ok
}

func loadOPCUAIdentity(certFile, keyFile string) ([]byte, *rsa.PrivateKey, error) {
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, nil, err
	}
	key, ok := pair.PrivateKey.(*rsa.PrivateKey)
	if !ok {
		return nil, nil, fmt.Errorf("%s: OPC UA needs an RSA key, not %T", keyFile, pair.PrivateKey)
	}
	var der []byte
	for _, c := range pair.Certificate {
		der = append(der, c...)
	}
	return der, key, nil
}

// opcuaTrust trusts a client whose certificate is in the PEM file, or is
// issued by a CA in it.
func opcuaTrust(file string) (func([]*x509.Certificate) error, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	var exact []*x509.Certificate
	for block, rest := pem.Decode(data); block != nil; block, rest = pem.Decode(rest) {
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		pool.AddCert(cert)
		exact = append(exact, cert)
	}
	if len(exact) == 0 {
		return nil, fmt.Errorf("%s has no certificates", file)
	}
	return func(chain []*x509.Certificate) error {
		for _, c := range exact {
			if c.Equal(chain[0]) {
				return nil
			}
		}
		intermediates := x509.NewCertPool()
		for _, c := range chain[1:] {
			intermediates.AddCert(c)
		}
		_, err := chain[0].Verify(x509.VerifyOptions{Roots: pool, Intermediates: intermediates, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}})
		return err
	}, nil
}

// opcuaUsers reads the users file: "name:password" or "name:sha256:<hex>".
func opcuaUsers(file string) (func(user, password string) bool, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	users := map[string][]byte{} // name -> SHA-256 of the password
	scanner := bufio.NewScanner(f)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		name, secret, ok := strings.Cut(text, ":")
		if !ok || name == "" {
			return nil, fmt.Errorf("%s:%d: name:password", file, line)
		}
		if digest, ok := strings.CutPrefix(secret, "sha256:"); ok {
			sum, err := hex.DecodeString(digest)
			if err != nil || len(sum) != sha256.Size {
				return nil, fmt.Errorf("%s:%d: sha256: needs 64 hex digits", file, line)
			}
			users[name] = sum
		} else {
			sum := sha256.Sum256([]byte(secret))
			users[name] = sum[:]
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return func(user, password string) bool {
		want, ok := users[user]
		sum := sha256.Sum256([]byte(password))
		return ok && subtle.ConstantTimeCompare(want, sum[:]) == 1
	}, nil
}

// Read is a variable's tag: its value, whether it is good, its time. A UDT
// is a map of its fields.
func (s *opcuaSource) Read(p opcua.Point) (opcua.Value, bool) {
	t, ok := s.c.db.GetTag(s.tags[p.Name])
	if !ok || t.Value == nil {
		return opcua.Value{}, false
	}
	v := t.Value
	if _, isUDT := s.udts[p.Name]; isUDT {
		v = udtMap(reflect.ValueOf(v))
	}
	return opcua.Value{Value: v, Good: t.Quality != honeycomb.QualityBad, Time: t.Timestamp}, true
}

// Write sets a variable's tag to what a client wrote.
func (s *opcuaSource) Write(p opcua.Point, value any, by string) error {
	tag := s.tags[p.Name]
	var typed any
	if t, isUDT := s.udts[p.Name]; isUDT {
		m, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("%s is a structure, not %T", tag, value)
		}
		var err error
		if typed, err = udtValue(t, m); err != nil {
			return fmt.Errorf("%s: %w", tag, err)
		}
	} else {
		var err error
		if typed, err = s.typed(tag, value); err != nil {
			return err
		}
	}
	return s.set(tag, typed, by)
}

// typed is a decoded value of the tag's own type.
func (s *opcuaSource) typed(tag string, value any) (any, error) {
	pv, err := uaPlcValue(reflect.ValueOf(value))
	if err != nil {
		return nil, err
	}
	typed, ex := s.c.serverValue(serverPoint{tag: tag}, pv)
	if ex != 0 {
		return nil, fmt.Errorf("%v does not convert to %s's type", value, tag)
	}
	return typed, nil
}

// set writes a tag for a client.
func (s *opcuaSource) set(tag string, value any, by string) error {
	from := "opcua client"
	if by != "" {
		from += " " + by
	}
	if s.c.serverSet(s.name, from, []pending{{tag: tag, value: value}}) != 0 {
		return fmt.Errorf("%s was not written", tag)
	}
	return nil
}

// method is the method of a binding Name() or Name(TYPE): calling it sets
// the tag, a BOOL to TRUE without an argument, any tag to the argument.
func (s *opcuaSource) method(b Binding, name, typeName string) (opcua.Method, error) {
	if b.Direction != InOut {
		return opcua.Method{}, errors.New("a method writes its tag: the binding is InOut")
	}
	tag := b.Tag
	m := opcua.Method{Name: name}
	if t, ok := s.c.db.GetTag(tag); ok {
		m.Description = t.Description
	}
	if typeName == "" {
		if t, ok := s.c.db.GetTag(tag); !ok || opcua.TypeOf(t.Value) != opcua.TypeBoolean {
			return m, fmt.Errorf("%s() sets a BOOL tag; give the argument's type, %s(REAL), for another", name, name)
		}
		m.Call = func(_ []any, by string) ([]any, error) {
			typed, err := s.typed(tag, true)
			if err != nil {
				return nil, err
			}
			return nil, s.set(tag, typed, by)
		}
		return m, nil
	}
	dataType, ok := opcua.TypeByName(strings.ToUpper(typeName))
	if !ok {
		return m, fmt.Errorf("data type %s is not served", typeName)
	}
	m.Inputs = []opcua.Argument{{Name: "Value", DataType: dataType, Description: "the value " + tag + " takes"}}
	m.Call = func(in []any, by string) ([]any, error) {
		typed, err := s.typed(tag, in[0])
		if err != nil {
			return nil, err
		}
		return nil, s.set(tag, typed, by)
	}
	return m, nil
}

// alarm is the alarm of a binding Name;ALARM[severity]: active while the tag
// is TRUE (or not zero).
func (s *opcuaSource) alarm(b Binding, name, severity string) (opcua.Alarm, error) {
	al := opcua.Alarm{Name: name, Severity: 500, Message: name + " is active"}
	if t, ok := s.c.db.GetTag(b.Tag); ok && t.Description != "" {
		al.Message = t.Description
	}
	if severity != "" {
		n, err := strconv.Atoi(severity)
		if err != nil || n < 1 || n > 1000 {
			return al, fmt.Errorf("severity %s is not 1 to 1000", severity)
		}
		al.Severity = uint16(n)
	}
	tag := b.Tag
	al.Active = func() bool {
		v, err := s.c.db.GetTagValue(tag)
		if err != nil || v == nil {
			return false
		}
		return truthy(reflect.ValueOf(v))
	}
	return al, nil
}

// truthy is TRUE for a true bool and a number that is not zero.
func truthy(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Bool:
		return v.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int() != 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return v.Uint() != 0
	case reflect.Float32, reflect.Float64:
		return v.Float() != 0
	}
	return false
}

// ensureOPCUAIdentity makes a self-signed certificate and key in dir unless
// both files are there.
func ensureOPCUAIdentity(dir, certFile, keyFile, uri, name string, hosts []string) error {
	_, certErr := os.Stat(certFile)
	_, keyErr := os.Stat(keyFile)
	if certErr == nil && keyErr == nil {
		return nil
	}
	if certErr == nil || keyErr == nil {
		return fmt.Errorf("%s or %s is missing: remove the other to make both again", certFile, keyFile)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	der, key, err := opcua.GenerateIdentity(uri, name, hosts, 5*365*24*time.Hour)
	if err != nil {
		return err
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0o600); err != nil {
		return err
	}
	return os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}

// uaPlcValue is a value the OPC UA server decoded as a PLC4X value.
func uaPlcValue(v reflect.Value) (apiValues.PlcValue, error) {
	if t, ok := v.Interface().(time.Time); ok {
		return spiValues.NewPlcDATE_AND_TIME(t), nil
	}
	switch v.Kind() {
	case reflect.Bool:
		return spiValues.NewPlcBOOL(v.Bool()), nil
	case reflect.Int8:
		return spiValues.NewPlcSINT(int8(v.Int())), nil
	case reflect.Uint8:
		return spiValues.NewPlcUSINT(uint8(v.Uint())), nil
	case reflect.Int16:
		return spiValues.NewPlcINT(int16(v.Int())), nil
	case reflect.Uint16:
		return spiValues.NewPlcUINT(uint16(v.Uint())), nil
	case reflect.Int32:
		return spiValues.NewPlcDINT(int32(v.Int())), nil
	case reflect.Uint32:
		return spiValues.NewPlcUDINT(uint32(v.Uint())), nil
	case reflect.Int64:
		return spiValues.NewPlcLINT(v.Int()), nil
	case reflect.Uint64:
		return spiValues.NewPlcULINT(v.Uint()), nil
	case reflect.Float32:
		return spiValues.NewPlcREAL(float32(v.Float())), nil
	case reflect.Float64:
		return spiValues.NewPlcLREAL(v.Float()), nil
	case reflect.String:
		return spiValues.NewPlcSTRING(v.String()), nil
	case reflect.Slice:
		items := make([]apiValues.PlcValue, v.Len())
		for i := range items {
			item, err := uaPlcValue(v.Index(i))
			if err != nil {
				return nil, err
			}
			items[i] = item
		}
		return spiValues.NewPlcList(items), nil
	}
	return nil, fmt.Errorf("no conversion from %s", v.Type())
}

// serveOPCUA answers clients until ctx ends or the listener fails.
func (c *Connector) serveOPCUA(ctx context.Context, cs *connState) error {
	s := cs.server.opcua
	ln, err := net.Listen("tcp", s.listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.listen, err)
	}
	c.record(cs.Name, func(st *Status) { st.Connected = true }, nil)
	err = s.server.Serve(ctx, &limitedListener{Listener: ln, slots: make(chan struct{}, maxTCPClients)}, func(local net.Addr) bool {
		return writesFromReadOnly(s.readOnly, local)
	})
	if err != nil {
		return fmt.Errorf("accept on %s: %w", s.listen, err)
	}
	return nil
}

// limitedListener accepts at most cap(slots) connections at a time, closing
// the ones beyond.
type limitedListener struct {
	net.Listener
	slots chan struct{}
}

func (l *limitedListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		select {
		case l.slots <- struct{}{}:
			return &slotConn{Conn: conn, slots: l.slots}, nil
		default:
			_ = conn.Close()
		}
	}
}

// slotConn gives its slot back when it closes.
type slotConn struct {
	net.Conn
	slots chan struct{}
	once  sync.Once
}

func (c *slotConn) Close() error {
	c.once.Do(func() { <-c.slots })
	return c.Conn.Close()
}
