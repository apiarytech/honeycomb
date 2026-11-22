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
//   - Browse, BrowseNext, TranslateBrowsePathsToNodeIds, RegisterNodes and UnregisterNodes,
//     over its points, the Server object and a type tree;
//   - Read of the attributes of its nodes, Write of the value of a writable point, with index
//     ranges on arrays and strings;
//   - structures: points whose values are structures, a DataType node with a
//     DataTypeDefinition each, and a variable per field;
//   - Call of its methods;
//   - subscriptions: CreateSubscription, ModifySubscription, SetPublishingMode,
//     DeleteSubscriptions, CreateMonitoredItems, ModifyMonitoredItems, SetMonitoringMode,
//     DeleteMonitoredItems, Publish and Republish, reporting data changes of Value, and
//     events with an EventFilter;
//   - alarms as AlarmConditionType conditions: Acknowledge, Confirm, AddComment, Enable,
//     Disable and ConditionRefresh;
//   - HistoryRead of the raw values of points and of events, kept in memory.
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

	ua "github.com/apache/plc4x/plc4go/protocols/opcua/readwrite/model"
)

// TypeStructure is the DataType of a point (or field) whose value is a structure: a
// map[string]any of its fields' values, nested maps for fields that are structures.
const TypeStructure = idStructure

// Point is one variable of the server.
type Point struct {
	// Name is the variable's place in the tree: dot-separated folders and the variable's own
	// name ("Line1.Speed"). Its node id is ns=1;s=<Name>.
	Name string
	// DataType is the built-in type the variable has (TypeDouble, ...), or TypeStructure.
	DataType uint32
	// Structure names the structure of a TypeStructure point.
	Structure string
	// ArrayLength is the number of elements of an array; 0 for a scalar.
	ArrayLength int
	// Writable says a client may write the value.
	Writable bool
	// Historize keeps the point's values for HistoryRead, when the Config keeps history.
	Historize   bool
	Description string
}

// Structure is a structured data type: a DataType node clients decode its values by.
type Structure struct {
	Name        string
	Description string
	Fields      []Field
}

// Field is a field of a Structure.
type Field struct {
	Name        string
	DataType    uint32 // a built-in type, or TypeStructure
	Structure   string // of a TypeStructure field
	ArrayLength int    // of an array of a built-in type
	Description string
}

// Argument is an input or output of a Method: a built-in type, scalar or array.
type Argument struct {
	Name        string
	DataType    uint32
	ArrayLength int
	Description string
}

// Method is a method clients call (OPC UA Part 4, 5.11). Its Name places it like a point's:
// "Tank.Reset" is the method Reset of the object (folder) Tank.
type Method struct {
	Name        string
	Description string
	Inputs      []Argument
	Outputs     []Argument
	// Call runs the method with the inputs (of the Inputs' types) for the user (empty for an
	// anonymous session) and returns the outputs. A non-nil error fails the call.
	Call func(inputs []any, by string) ([]any, error)
}

// Alarm is an alarm clients see as an AlarmConditionType condition (OPC UA Part 9): events
// when it becomes active, returns to normal, is acknowledged, confirmed, commented, enabled or
// disabled.
type Alarm struct {
	// Name places the condition in the tree like a point's name.
	Name string
	// Source is the point the alarm is about; empty for the alarm itself.
	Source   string
	Severity uint16 // 1 to 1000
	Message  string
	// Active says whether the alarm condition holds; the server asks every AlarmInterval.
	Active func() bool
}

// History is what the server keeps for HistoryRead (OPC UA Part 11), in memory.
type History struct {
	Depth    int           // values kept per point that historizes; 0 keeps none
	Interval time.Duration // how often the points are sampled; 0 means a second
	Events   int           // events kept, readable on the Server object; 0 keeps none
}

// Value is a point's value as the Source has it.
type Value struct {
	Value any       // the Go value: bool, a number, string, time.Time, a slice of them, or a structure's map
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
	Structures  []Structure
	Methods     []Method
	Alarms      []Alarm
	// AlarmInterval is how often the alarms are evaluated; 0 means 250 ms.
	AlarmInterval time.Duration
	History       History
	Source        Source
	// Endpoints are the security policies and modes; none means None only.
	Endpoints []Endpoint
	// Certificate (DER) and Key are the server's, needed for a policy other than None and for
	// encrypted passwords. GenerateIdentity makes them.
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
	cfg             Config
	endpoints       []endpoint
	space           *addressSpace
	started         time.Time
	certChain       []*x509.Certificate
	structures      []*structureType
	structureByName map[string]*structureType
	methods         []*method
	alarms          []*alarm
	history         *historyStore

	mutex      sync.Mutex
	sessions   map[string]*session // by authentication token key
	sessionIds uint32
	channels   atomic.Uint32 // the last secure channel id
	tokens     atomic.Uint32 // the last security token id

	subscriptionIds atomic.Uint32
	subsMutex       sync.Mutex
	subs            map[*subscription]struct{} // for events

	watchOnce sync.Once
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
	if cfg.AlarmInterval <= 0 {
		cfg.AlarmInterval = 250 * time.Millisecond
	}
	if cfg.History.Interval <= 0 {
		cfg.History.Interval = time.Second
	}
	if len(cfg.Endpoints) == 0 {
		cfg.Endpoints = []Endpoint{{Policy: "None", Mode: "None"}}
	}
	s := &Server{cfg: cfg, sessions: map[string]*session{}, started: time.Now(), subs: map[*subscription]struct{}{},
		history: newHistoryStore(cfg.History)}
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
	var err error
	if s.structures, s.structureByName, err = newStructures(cfg.Structures); err != nil {
		return nil, fmt.Errorf("opcua: %w", err)
	}
	for _, p := range cfg.Points {
		switch {
		case p.DataType == TypeStructure:
			if s.structureByName[p.Structure] == nil {
				return nil, fmt.Errorf("opcua: point %q: structure %q is not given", p.Name, p.Structure)
			}
			if p.ArrayLength > 0 {
				return nil, fmt.Errorf("opcua: point %q: arrays of structures are not served", p.Name)
			}
		case p.DataType < TypeBoolean || p.DataType > TypeDateTime:
			return nil, fmt.Errorf("opcua: point %q: data type %d is not served", p.Name, p.DataType)
		}
	}
	for _, m := range cfg.Methods {
		if m.Call == nil {
			return nil, fmt.Errorf("opcua: method %q has no Call", m.Name)
		}
		for _, a := range append(append([]Argument{}, m.Inputs...), m.Outputs...) {
			if a.DataType < TypeBoolean || a.DataType > TypeDateTime || a.Name == "" {
				return nil, fmt.Errorf("opcua: method %q: argument %q: a name and a built-in data type", m.Name, a.Name)
			}
		}
		s.methods = append(s.methods, &method{cfg: m, executable: true})
	}
	for _, a := range cfg.Alarms {
		if a.Active == nil {
			return nil, fmt.Errorf("opcua: alarm %q has no Active", a.Name)
		}
		if a.Severity == 0 || a.Severity > 1000 {
			return nil, fmt.Errorf("opcua: alarm %q: severity %d is not 1 to 1000", a.Name, a.Severity)
		}
		s.alarms = append(s.alarms, &alarm{s: s, cfg: a, st: alarmState{enabled: true, acked: true, confirmed: true}})
	}
	if s.space, err = newAddressSpace(s); err != nil {
		return nil, fmt.Errorf("opcua: %w", err)
	}
	return s, nil
}

// serverValue is the value of one of the server's own variables.
func (s *Server) serverValue(id uint32) ua.Variant {
	var v any
	t := uint32(TypeString)
	switch id {
	case idNamespaceArray:
		v = []string{"http://opcfoundation.org/UA/", s.cfg.NamespaceURI}
	case idServerArray:
		v = []string{s.cfg.ApplicationURI}
	case idServerStatusStartTime:
		v, t = s.started, TypeDateTime
	case idServerCurrentTime:
		v, t = time.Now(), TypeDateTime
	case idServerState:
		v, t = int32(0), TypeInt32 // Running
	default:
		return nil
	}
	variant, _ := variantOf(v, t)
	return variant
}

// watch evaluates the alarms and samples the points that historize, until ctx ends.
func (s *Server) watch(ctx context.Context) {
	alarms := time.NewTicker(s.cfg.AlarmInterval)
	defer alarms.Stop()
	history := time.NewTicker(s.cfg.History.Interval)
	defer history.Stop()
	record := func() {
		if s.cfg.History.Depth <= 0 {
			return
		}
		for i := range s.cfg.Points {
			if p := &s.cfg.Points[i]; p.Historize {
				n := s.space.nodes["ns=1;s="+p.Name]
				s.history.record(n.key, s.sample(n))
			}
		}
	}
	record()
	for {
		select {
		case <-ctx.Done():
			return
		case <-alarms.C:
			for _, al := range s.alarms {
				al.evaluate()
			}
		case <-history.C:
			record()
		}
	}
}

func (s *Server) onError(err error) {
	if s.cfg.OnError != nil && err != nil {
		s.cfg.OnError(err)
	}
}

// Serve answers clients that connect to ln until ctx ends or ln fails. writes says whether a
// client that reached the server at a local address may write; nil means all may.
func (s *Server) Serve(ctx context.Context, ln net.Listener, writes func(local net.Addr) bool) error {
	s.watchOnce.Do(func() { go s.watch(ctx) })
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
