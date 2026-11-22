/*
 * Copyright (C) 2026 Franklin D. Amador
 *
 * This software is dual-licensed under the terms of the GPL v3.0 and
 * a commercial license. You may choose to use this software under either
 * license.
 *
 * See the LICENSE files in the project root for full license text.
 */

// Package opcua is an OPC UA server (OPC UA Parts 4 and 6, UA-TCP binary): the server side
// PLC4X only has a client for. The service messages are parsed and serialized by PLC4X's opcua
// model, as the connector's other servers use their protocols' models; the transport's
// framing, chunking and security are done here on the bytes.
//
// It serves points (a tag each) as variables under Objects, in namespace 1: a point named
// "Line1.Speed" is the variable ns=1;s=Line1.Speed in the folder ns=1;s=Line1. It answers:
//
//   - Hello, OpenSecureChannel (Issue and Renew) and CloseSecureChannel, with the security
//     policies None, Basic128Rsa15, Basic256, Basic256Sha256, Aes128_Sha256_RsaOaep and
//     Aes256_Sha256_RsaPss in the modes Sign and SignAndEncrypt, and messages in chunks;
//   - GetEndpoints and FindServers;
//   - CreateSession, ActivateSession (anonymous, or a user name and password the Config
//     checks) and CloseSession;
//   - Browse, BrowseNext, TranslateBrowsePathsToNodeIds, RegisterNodes and UnregisterNodes;
//   - Read of the attributes of its nodes, Write of the value of a writable point;
//   - subscriptions: CreateSubscription, ModifySubscription, SetPublishingMode,
//     DeleteSubscriptions, CreateMonitoredItems, ModifyMonitoredItems, SetMonitoringMode,
//     DeleteMonitoredItems, Publish and Republish, reporting data changes of Value.
//
// Other services are answered with BadServiceUnsupported.
package opcua

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Point is one variable of the server.
type Point struct {
	// Name is the variable's place in the tree: dot-separated folders and the variable's own
	// name ("Line1.Speed"). Its node id is ns=1;s=<Name>.
	Name string
	// DataType is the built-in type the variable has (TypeDouble, ...).
	DataType uint32
	// ArrayLength is the number of elements of an array; 0 for a scalar.
	ArrayLength int
	// Writable says a client may write the value.
	Writable    bool
	Description string
}

// Value is a point's value as the Source has it.
type Value struct {
	Value any       // the Go value: bool, a number, string, time.Time, or a slice of them
	Good  bool      // false reports the value with a Bad status
	Time  time.Time // when the value was taken; zero for unknown
}

// Source gives the points their values and takes the values clients write.
type Source interface {
	Read(p Point) (Value, bool)
	// Write sets a point's value; by names the client. A non-nil error refuses the write.
	Write(p Point, value any, by string) error
}

// Endpoint is a security policy and mode the server accepts: ("None", "None"),
// ("Basic256Sha256", "SignAndEncrypt"), ...
type Endpoint struct {
	Policy string
	Mode   string // None, Sign or SignAndEncrypt
}

// Config is what a Server serves and how.
type Config struct {
	ApplicationURI  string // the server's identity, also in its certificate: "urn:..."
	ApplicationName string
	NamespaceURI    string // namespace 1, the points'
	// EndpointURL is the URL the endpoints carry; clients connect to it.
	EndpointURL string
	Points      []Point
	Source      Source
	// Endpoints are the security policies and modes; none means None only.
	Endpoints []Endpoint
	// Certificate (DER) and Key are the server's, needed for a policy other than None and for
	// encrypted passwords.
	Certificate []byte
	Key         *rsa.PrivateKey
	// TrustClient decides whether a client's certificate chain (leaf first) is trusted, on a
	// channel with security. Without it no secured channel opens.
	TrustClient func(chain []*x509.Certificate) error
	// Login checks a user name and password; without it only anonymous sessions are allowed.
	Login func(user, password string) bool
	// Anonymous allows sessions without a user; true by default when Login is nil.
	Anonymous bool
	// MaxSessions bounds the sessions of the server; 0 means 100.
	MaxSessions int
	OnError     func(error)
}

type endpoint struct {
	policy *securityPolicy
	mode   uint32 // MessageSecurityMode
}

// Server is an OPC UA server.
type Server struct {
	cfg       Config
	endpoints []endpoint
	space     *addressSpace
	started   time.Time
	certChain []*x509.Certificate

	mutex      sync.Mutex
	sessions   map[string]*session // by authentication token key
	sessionIds uint32
	channels   atomic.Uint32 // the last secure channel id
	tokens     atomic.Uint32 // the last security token id

	subscriptionIds atomic.Uint32
}

// New checks a Config and makes its server.
func New(cfg Config) (*Server, error) {
	if cfg.Source == nil {
		return nil, errors.New("opcua: no Source")
	}
	if cfg.ApplicationURI == "" {
		cfg.ApplicationURI = "urn:apiarytech:honeycomb:opcua"
	}
	if cfg.ApplicationName == "" {
		cfg.ApplicationName = "honeycomb OPC UA server"
	}
	if cfg.NamespaceURI == "" {
		cfg.NamespaceURI = cfg.ApplicationURI
	}
	if cfg.Login == nil {
		cfg.Anonymous = true
	}
	if cfg.MaxSessions == 0 {
		cfg.MaxSessions = 100
	}
	if len(cfg.Endpoints) == 0 {
		cfg.Endpoints = []Endpoint{{Policy: "None", Mode: "None"}}
	}
	s := &Server{cfg: cfg, sessions: map[string]*session{}, started: time.Now()}
	for _, e := range cfg.Endpoints {
		policy, err := securityPolicyByName(e.Policy)
		if err != nil {
			return nil, fmt.Errorf("opcua: %w", err)
		}
		mode, ok := map[string]uint32{"None": 1, "Sign": 2, "SignAndEncrypt": 3}[e.Mode]
		if !ok {
			return nil, fmt.Errorf("opcua: message security %q: None, Sign or SignAndEncrypt", e.Mode)
		}
		if policy.isNone() != (mode == 1) {
			return nil, fmt.Errorf("opcua: security policy %s with mode %s: None goes with None only", e.Policy, e.Mode)
		}
		if !policy.isNone() && (cfg.Key == nil || len(cfg.Certificate) == 0) {
			return nil, fmt.Errorf("opcua: security policy %s needs the server's certificate and key", e.Policy)
		}
		if !policy.isNone() && cfg.TrustClient == nil {
			return nil, fmt.Errorf("opcua: security policy %s needs a way to trust clients' certificates", e.Policy)
		}
		s.endpoints = append(s.endpoints, endpoint{policy: policy, mode: mode})
	}
	if len(cfg.Certificate) > 0 {
		chain, err := x509.ParseCertificates(cfg.Certificate)
		if err != nil || len(chain) == 0 {
			return nil, fmt.Errorf("opcua: the server certificate: %w", err)
		}
		if cfg.Key != nil && !cfg.Key.PublicKey.Equal(chain[0].PublicKey) {
			return nil, errors.New("opcua: the key does not belong to the server certificate")
		}
		s.certChain = chain
	}
	for _, p := range cfg.Points {
		if p.DataType < TypeBoolean || p.DataType > TypeDateTime {
			return nil, fmt.Errorf("opcua: point %q: data type %d is not served", p.Name, p.DataType)
		}
	}
	space, err := newAddressSpace(cfg.Points, []string{"http://opcfoundation.org/UA/", cfg.NamespaceURI}, s.serverValue)
	if err != nil {
		return nil, fmt.Errorf("opcua: %w", err)
	}
	s.space = space
	return s, nil
}

// serverValue is the value of one of the server's own variables.
func (s *Server) serverValue(id uint32) (any, uint32) {
	switch id {
	case idNamespaceArray:
		return []string{"http://opcfoundation.org/UA/", s.cfg.NamespaceURI}, TypeString
	case idServerArray:
		return []string{s.cfg.ApplicationURI}, TypeString
	case idServerStatusStartTime:
		return s.started, TypeDateTime
	case idServerCurrentTime:
		return time.Now(), TypeDateTime
	case idServerState:
		return int32(0), TypeInt32 // Running
	}
	return nil, 0
}

func (s *Server) onError(err error) {
	if s.cfg.OnError != nil && err != nil {
		s.cfg.OnError(err)
	}
}

// Serve answers clients that connect to ln until ctx ends or ln fails. writes says whether a
// client that reached the server at a local address may write; nil means all may.
func (s *Server) Serve(ctx context.Context, ln net.Listener, writes func(local net.Addr) bool) error {
	stop := context.AfterFunc(ctx, func() { _ = ln.Close() })
	defer stop()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		mayWrite := writes == nil || writes(conn.LocalAddr())
		wg.Go(func() { s.ServeConn(ctx, conn, mayWrite) })
	}
}

// randomBytes makes n random bytes, for nonces and tokens.
func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}
