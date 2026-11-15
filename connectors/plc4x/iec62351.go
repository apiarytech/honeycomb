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

// IEC 60870-5-104 over TLS, as IEC 62351-3 secures it, with the roles of
// IEC 62351-8 (beehive R-140).
//
// A connection takes TLS with options on its URL:
//
//	iec-60870-5-104://10.0.0.5:19998?tls=true&tls-cert=me.pem&tls-key=me.key&tls-ca=ca.pem
//
//   - tls-cert, tls-key: this end's certificate and key (PEM). Both ends
//     present one: TLS is mutual.
//   - tls-ca: the CA certificates (PEM) the other end's must chain to.
//   - tls-server-name: the name the outstation's certificate must carry;
//     default the URL's host.
//   - tls-roles (outstation only): "on" (default) or "off". On, a
//     controlling station may command only when its certificate carries
//     the IEC 62351-8 role OPERATOR; a certificate without roles, or with
//     others only, reads. Off, any authenticated station may command, as
//     over plain TCP. A read-only interface stays read only either way.
//
// TLS is 1.2 or 1.3; with 1.2 only ECDHE key exchange with AES-GCM or
// ChaCha20-Poly1305 is offered, and renegotiation is refused (Go's
// default). The usual port is 19998.
//
// The controlling station's side is PLC4X's client, which speaks plain TCP:
// the connector runs a tunnel for it, a listener on loopback whose
// connections it carries over TLS to the outstation, and points PLC4X at
// the tunnel. Nothing leaves the machine in clear.

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/asn1"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// IEC 62351-8 roles (its mandatory profile), by role number.
const (
	RoleViewer    = 0
	RoleOperator  = 1
	RoleEngineer  = 2
	RoleInstaller = 3
	RoleSecAdm    = 4
	RoleSecAud    = 5
	RoleRBACMnt   = 6
)

// RoleNames names the roles of IEC 62351-8's mandatory profile.
var RoleNames = map[int]string{RoleViewer: "VIEWER", RoleOperator: "OPERATOR", RoleEngineer: "ENGINEER",
	RoleInstaller: "INSTALLER", RoleSecAdm: "SECADM", RoleSecAud: "SECAUD", RoleRBACMnt: "RBACMNT"}

// OIDRoles is the X.509 extension that carries IEC 62351-8 roles in a
// certificate (id-IEC62351-8-ext-roles).
var OIDRoles = asn1.ObjectIdentifier{1, 2, 840, 10070, 8, 1}

// roleInformation is one role in the extension: its number, area of
// responsibility, revision and validity (from and to, both or neither).
// Only the number and the validity are used.
type roleInformation struct {
	RoleID    int
	AoR       string `asn1:"utf8"`
	Revision  int
	ValidFrom time.Time `asn1:"generalized"`
	ValidTo   time.Time `asn1:"generalized"`
}

// parseRole reads one role entry element by element: the number, then
// what follows in order, the two times both present or both absent.
func parseRole(raw asn1.RawValue) (roleInformation, error) {
	var r roleInformation
	var elems []asn1.RawValue
	rest := raw.Bytes
	for len(rest) > 0 {
		var e asn1.RawValue
		var err error
		if rest, err = asn1.Unmarshal(rest, &e); err != nil {
			return r, err
		}
		elems = append(elems, e)
	}
	if len(elems) == 0 {
		return r, errors.New("an empty role")
	}
	if _, err := asn1.Unmarshal(elems[0].FullBytes, &r.RoleID); err != nil {
		return r, err
	}
	var times []time.Time
	for _, e := range elems[1:] {
		switch e.Tag {
		case asn1.TagUTF8String:
			r.AoR = string(e.Bytes)
		case asn1.TagInteger:
			if _, err := asn1.Unmarshal(e.FullBytes, &r.Revision); err != nil {
				return r, err
			}
		case asn1.TagGeneralizedTime:
			var t time.Time
			if _, err := asn1.UnmarshalWithParams(e.FullBytes, &t, "generalized"); err != nil {
				return r, err
			}
			times = append(times, t)
		}
	}
	switch len(times) {
	case 0:
	case 2:
		r.ValidFrom, r.ValidTo = times[0], times[1]
	default:
		return r, errors.New("a role's validity is from and to")
	}
	return r, nil
}

// RolesOf reads the IEC 62351-8 roles a certificate carries that are valid
// at now. A certificate without the extension carries none; a malformed
// extension is an error, so it is never read as fewer roles than meant.
func RolesOf(cert *x509.Certificate, now time.Time) ([]int, error) {
	for _, ext := range cert.Extensions {
		if !ext.Id.Equal(OIDRoles) {
			continue
		}
		var raws []asn1.RawValue
		rest, err := asn1.Unmarshal(ext.Value, &raws)
		if err != nil || len(rest) > 0 {
			return nil, fmt.Errorf("iec 62351-8 roles: malformed extension")
		}
		var roles []int
		for _, raw := range raws {
			r, err := parseRole(raw)
			if err != nil {
				return nil, fmt.Errorf("iec 62351-8 roles: malformed role: %w", err)
			}
			if (!r.ValidFrom.IsZero() && now.Before(r.ValidFrom)) || (!r.ValidTo.IsZero() && now.After(r.ValidTo)) {
				continue
			}
			roles = append(roles, r.RoleID)
		}
		return roles, nil
	}
	return nil, nil
}

// RolesExtension encodes roles as the certificate extension, valid from
// from to to (for tests and for issuing a station's certificate).
func RolesExtension(from, to time.Time, roles ...int) (x509Ext []byte, err error) {
	infos := make([]roleInformation, len(roles))
	for i, r := range roles {
		infos[i] = roleInformation{RoleID: r, AoR: "beehive", Revision: 1, ValidFrom: from.UTC(), ValidTo: to.UTC()}
	}
	return asn1.Marshal(infos)
}

// tlsOptions are a connection's TLS settings, from its URL.
type tlsOptions struct {
	on            bool
	cert, key, ca string
	serverName    string
	roles         bool // outstation: commands need OPERATOR
}

// parseTLS reads the TLS options of a URL's query; off when tls is not
// "true".
func parseTLS(q url.Values, host string) (tlsOptions, error) {
	o := tlsOptions{roles: true}
	switch strings.ToLower(q.Get("tls")) {
	case "", "false":
		for _, k := range []string{"tls-cert", "tls-key", "tls-ca", "tls-server-name", "tls-roles"} {
			if q.Get(k) != "" {
				return o, fmt.Errorf("%s without tls=true", k)
			}
		}
		return o, nil
	case "true":
	default:
		return o, fmt.Errorf("tls=%q: true or false", q.Get("tls"))
	}
	o.on = true
	o.cert, o.key, o.ca = q.Get("tls-cert"), q.Get("tls-key"), q.Get("tls-ca")
	if o.cert == "" || o.key == "" || o.ca == "" {
		return o, errors.New("tls=true needs tls-cert, tls-key and tls-ca: TLS is mutual (IEC 62351-3)")
	}
	o.serverName = q.Get("tls-server-name")
	if o.serverName == "" {
		o.serverName = host
	}
	switch strings.ToLower(q.Get("tls-roles")) {
	case "", "on":
	case "off":
		o.roles = false
	default:
		return o, fmt.Errorf("tls-roles=%q: on or off", q.Get("tls-roles"))
	}
	return o, nil
}

// tls12Suites are the TLS 1.2 suites offered: ECDHE (forward secrecy)
// with an AEAD. TLS 1.3's are Go's own, all AEAD.
var tls12Suites = []uint16{
	tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384, tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
	tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256, tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
	tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256, tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
}

// config builds the TLS configuration of one end.
func (o tlsOptions) config(server bool) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(o.cert, o.key)
	if err != nil {
		return nil, fmt.Errorf("tls-cert, tls-key: %w", err)
	}
	b, err := os.ReadFile(o.ca)
	if err != nil {
		return nil, fmt.Errorf("tls-ca: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(b) {
		return nil, errors.New("tls-ca: no certificate in it")
	}
	c := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12, CipherSuites: tls12Suites}
	if server {
		c.ClientAuth, c.ClientCAs = tls.RequireAndVerifyClientCert, pool
	} else {
		c.RootCAs, c.ServerName = pool, o.serverName
	}
	return c, nil
}

// handshakeTimeout bounds a TLS handshake with a station.
const handshakeTimeout = 10 * time.Second

// stationRights completes a station's handshake and says whether it may
// command, by its certificate's roles, and who it is for the record.
func stationRights(conn net.Conn, roles bool) (commands bool, who string, err error) {
	tc, ok := conn.(*tls.Conn)
	if !ok {
		return true, "", nil
	}
	tc.SetDeadline(time.Now().Add(handshakeTimeout))
	if err := tc.Handshake(); err != nil {
		return false, "", err
	}
	tc.SetDeadline(time.Time{})
	certs := tc.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return false, "", errors.New("no client certificate")
	}
	who = certs[0].Subject.CommonName
	if !roles {
		return true, who, nil
	}
	rs, err := RolesOf(certs[0], time.Now())
	if err != nil {
		return false, who, err
	}
	var names []string
	for _, r := range rs {
		if r == RoleOperator {
			commands = true
		}
		if n, ok := RoleNames[r]; ok {
			names = append(names, n)
		}
	}
	if len(names) > 0 {
		who += " (" + strings.Join(names, ", ") + ")"
	}
	return commands, who, nil
}

// tlsTunnel carries PLC4X's plain TCP to an outstation over TLS: a loopback
// listener whose every connection it dials out with TLS.
type tlsTunnel struct {
	ln     net.Listener
	remote string
	cfg    *tls.Config

	mu    sync.Mutex
	conns map[net.Conn]bool
	done  bool
}

// newTLSTunnel starts a tunnel to remote (host:port).
func newTLSTunnel(remote string, cfg *tls.Config) (*tlsTunnel, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	t := &tlsTunnel{ln: ln, remote: remote, cfg: cfg, conns: map[net.Conn]bool{}}
	go t.accept()
	return t, nil
}

// Addr is the tunnel's loopback address, for PLC4X's URL.
func (t *tlsTunnel) Addr() string { return t.ln.Addr().String() }

func (t *tlsTunnel) accept() {
	for {
		local, err := t.ln.Accept()
		if err != nil {
			return
		}
		go t.carry(local)
	}
}

// carry dials the outstation with TLS and copies both ways until either
// side closes. A failed dial closes the local side: PLC4X sees a refused
// connection, as it would from the outstation.
func (t *tlsTunnel) carry(local net.Conn) {
	d := &tls.Dialer{NetDialer: &net.Dialer{Timeout: handshakeTimeout}, Config: t.cfg}
	remote, err := d.Dial("tcp", t.remote)
	if err != nil {
		local.Close()
		return
	}
	if !t.track(local, remote) {
		local.Close()
		remote.Close()
		return
	}
	var once sync.Once
	end := func() {
		once.Do(func() {
			local.Close()
			remote.Close()
			t.untrack(local, remote)
		})
	}
	go func() { io.Copy(remote, local); end() }()
	io.Copy(local, remote)
	end()
}

func (t *tlsTunnel) track(cs ...net.Conn) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return false
	}
	for _, c := range cs {
		t.conns[c] = true
	}
	return true
}

func (t *tlsTunnel) untrack(cs ...net.Conn) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, c := range cs {
		delete(t.conns, c)
	}
}

// Close stops the tunnel and every connection through it.
func (t *tlsTunnel) Close() {
	t.mu.Lock()
	t.done = true
	conns := t.conns
	t.conns = map[net.Conn]bool{}
	t.mu.Unlock()
	t.ln.Close()
	for c := range conns {
		c.Close()
	}
}

// startTunnel starts the TLS tunnel of an IEC 60870-5-104 client whose URL
// asks for TLS; stop is nil when it does not.
func startTunnel(cs *connState) (stop func(), err error) {
	if ProtocolOf(cs.URL) != "iec-60870-5-104" {
		return nil, nil
	}
	plain, remote, opts, err := tunneledURL(cs.URL)
	if err != nil || !opts.on {
		return nil, err
	}
	cfg, err := opts.config(false)
	if err != nil {
		return nil, err
	}
	t, err := newTLSTunnel(remote, cfg)
	if err != nil {
		return nil, err
	}
	u, _ := url.Parse(plain)
	u.Host = t.Addr()
	dial := u.String()
	cs.dialURL.Store(&dial)
	return func() { cs.dialURL.Store(nil); t.Close() }, nil
}

// tunneledURL is a client URL with its TLS options taken off and its host
// replaced by the tunnel's, for PLC4X; tunnel is "" when the URL has no
// TLS. remote is where the tunnel goes.
func tunneledURL(raw string) (plain, remote string, opts tlsOptions, err error) {
	u, err := url.Parse(raw)
	if err != nil {
		return raw, "", opts, err
	}
	q := u.Query()
	opts, err = parseTLS(q, u.Hostname())
	if err != nil || !opts.on {
		return raw, "", opts, err
	}
	if q.Get("tls-roles") != "" {
		return raw, "", opts, errors.New("tls-roles is the outstation's setting; a controlling station's roles are in its certificate")
	}
	remote = u.Host
	if u.Port() == "" {
		remote = net.JoinHostPort(u.Hostname(), "19998")
	}
	for _, k := range []string{"tls", "tls-cert", "tls-key", "tls-ca", "tls-server-name"} {
		q.Del(k)
	}
	u.RawQuery = q.Encode()
	return u.String(), remote, opts, nil
}
