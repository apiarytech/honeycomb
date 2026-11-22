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
	"bytes"
	"context"
	"crypto/x509"
	"encoding/binary"
	"fmt"
	"reflect"
	"sync"
	"time"

	ua "github.com/apache/plc4x/plc4go/protocols/opcua/readwrite/model"
)

// Service status codes (OPC UA Part 6, StatusCode.csv).
const (
	statusGood                              = 0
	statusBad                               = 0x80000000
	statusBadInternalError                  = 0x80020000
	statusBadTimeoutService                 = 0x800A0000
	statusBadServiceUnsupported             = 0x800B0000
	statusBadNothingToDo                    = 0x800F0000
	statusBadTooManyOperations              = 0x80100000
	statusBadSecurityChecksFailed           = 0x80130000
	statusBadCertificateUntrusted           = 0x801A0000
	statusBadUserAccessDenied               = 0x801F0000
	statusBadIdentityTokenInvalid           = 0x80200000
	statusBadIdentityTokenRejected          = 0x80210000
	statusBadSessionIdInvalid               = 0x80250000
	statusBadSessionClosed                  = 0x80260000
	statusBadSessionNotActivated            = 0x80270000
	statusBadSubscriptionIdInvalid          = 0x80280000
	statusBadTimestampsToReturnInvalid      = 0x802B0000
	statusBadWaitingForInitialData          = 0x80320000
	statusBadNodeIdUnknown                  = 0x80340000
	statusBadAttributeIdInvalid             = 0x80350000
	statusBadIndexRangeInvalid              = 0x80360000
	statusBadDataEncodingUnsupported        = 0x80390000
	statusBadNotReadable                    = 0x803A0000
	statusBadNotWritable                    = 0x803B0000
	statusBadMonitoringModeInvalid          = 0x80410000
	statusBadMonitoredItemIdInvalid         = 0x80420000
	statusBadMonitoredItemFilterUnsupported = 0x80440000
	statusBadContinuationPointInvalid       = 0x804A0000
	statusBadNoContinuationPoints           = 0x804B0000
	statusBadReferenceTypeIdInvalid         = 0x804C0000
	statusBadBrowseDirectionInvalid         = 0x804D0000
	statusBadSecurityPolicyRejected         = 0x80550000
	statusBadTooManySessions                = 0x80560000
	statusBadApplicationSignatureInvalid    = 0x80580000
	statusBadNoMatch                        = 0x806F0000
	statusBadTypeMismatch                   = 0x80740000
	statusBadTooManySubscriptions           = 0x80770000
	statusBadTooManyPublishRequests         = 0x80780000
	statusBadNoSubscription                 = 0x80790000
	statusBadSequenceNumberUnknown          = 0x807A0000
	statusBadMessageNotAvailable            = 0x807B0000
	statusBadTooManyMonitoredItems          = 0x80DB0000
)

// Bounds of what one session may hold.
const (
	maxOperations         = 10000 // per request
	maxContinuationPoints = 16
	maxSubscriptions      = 64
	maxMonitoredItems     = 10000 // per subscription
	maxPublishRequests    = 16
	sessionTimeoutMin     = 10 * time.Second
	sessionTimeoutMax     = time.Hour
	userTokenAnonymous    = "anonymous"
	userTokenUserName     = "username"
	transportProfile      = "http://opcfoundation.org/UA-Profile/Transport/uatcp-uasc-uabinary"
)

// session is a session of a client, bound to the channel it was created on.
type session struct {
	c        *conn
	id       ua.NodeId
	token    ua.NodeId
	tokenKey string
	nonce    []byte
	timeout  time.Duration

	mutex         sync.Mutex
	activated     bool
	closed        bool
	user          string
	lastUsed      time.Time
	browses       map[string][]ua.ReferenceDescription
	subscriptions map[uint32]*subscription
	publishes     []publishRequest
}

type publishRequest struct {
	requestId     uint32
	requestHandle uint32
	received      time.Time
	timeout       time.Duration // 0 for none
	results       []uint32      // of the acknowledgements it carried
}

type requestWithHeader interface {
	GetRequestHeader() ua.RequestHeader
}

// serveRequest answers one service request; nil means the answer comes later.
func (c *conn) serveRequest(requestId uint32, request ua.ExtensionObjectDefinition) ua.ExtensionObjectDefinition {
	withHeader, ok := request.(requestWithHeader)
	if !ok {
		return c.fault(nil, statusBadServiceUnsupported)
	}
	header := withHeader.GetRequestHeader()
	switch request.(type) {
	case ua.GetEndpointsRequest:
		return ua.NewGetEndpointsResponse(c.responseHeader(header, statusGood), c.s.endpointDescriptions())
	case ua.FindServersRequest:
		return ua.NewFindServersResponse(c.responseHeader(header, statusGood), []ua.ApplicationDescription{c.s.applicationDescription()})
	}
	if c.discoveryOnly {
		return c.fault(header, statusBadSecurityPolicyRejected)
	}
	if request, ok := request.(ua.CreateSessionRequest); ok {
		return c.createSession(request)
	}
	s := c.sessionOf(header)
	if s == nil {
		return c.fault(header, statusBadSessionIdInvalid)
	}
	s.mutex.Lock()
	activated := s.activated
	s.lastUsed = time.Now()
	s.mutex.Unlock()
	switch request := request.(type) {
	case ua.ActivateSessionRequest:
		return c.activateSession(s, request)
	case ua.CloseSessionRequest:
		c.closeSession(s)
		return ua.NewCloseSessionResponse(c.responseHeader(header, statusGood))
	}
	if !activated {
		return c.fault(header, statusBadSessionNotActivated)
	}
	switch request := request.(type) {
	case ua.BrowseRequest:
		return c.browse(s, request)
	case ua.BrowseNextRequest:
		return c.browseNext(s, request)
	case ua.TranslateBrowsePathsToNodeIdsRequest:
		return c.translate(request)
	case ua.RegisterNodesRequest:
		// Registering only promises faster access later: every node is as fast already.
		if status := tooMany(len(request.GetNodesToRegister())); status != statusGood {
			return c.fault(header, status)
		}
		return ua.NewRegisterNodesResponse(c.responseHeader(header, statusGood), request.GetNodesToRegister())
	case ua.UnregisterNodesRequest:
		return ua.NewUnregisterNodesResponse(c.responseHeader(header, statusGood))
	case ua.ReadRequest:
		return c.read(request)
	case ua.WriteRequest:
		return c.writeValues(s, request)
	case ua.CreateSubscriptionRequest:
		return c.createSubscription(s, request)
	case ua.ModifySubscriptionRequest:
		return c.modifySubscription(s, request)
	case ua.SetPublishingModeRequest:
		return c.setPublishingMode(s, request)
	case ua.DeleteSubscriptionsRequest:
		return c.deleteSubscriptions(s, request)
	case ua.CreateMonitoredItemsRequest:
		return c.createMonitoredItems(s, request)
	case ua.ModifyMonitoredItemsRequest:
		return c.modifyMonitoredItems(s, request)
	case ua.SetMonitoringModeRequest:
		return c.setMonitoringMode(s, request)
	case ua.DeleteMonitoredItemsRequest:
		return c.deleteMonitoredItems(s, request)
	case ua.PublishRequest:
		return c.publish(s, requestId, request)
	case ua.RepublishRequest:
		return c.republish(s, request)
	}
	return c.fault(header, statusBadServiceUnsupported)
}

func (c *conn) responseHeader(request ua.RequestHeader, status uint32) ua.ResponseHeader {
	handle := uint32(0)
	if request != nil {
		handle = request.GetRequestHandle()
	}
	return ua.NewResponseHeader(dateTime(time.Now()), handle, ua.NewStatusCode(status), nullDiagnostics(), nil, nullExtensionObject())
}

func (c *conn) fault(request ua.RequestHeader, status uint32) ua.ExtensionObjectDefinition {
	return ua.NewServiceFault(c.responseHeader(request, status))
}

func nullDiagnostics() ua.DiagnosticInfo {
	return ua.NewDiagnosticInfo(false, false, false, false, false, false, false, nil, nil, nil, nil, nil, nil, nil)
}

func nullExtensionObject() ua.ExtensionObject {
	return ua.NewNullExtensionObjectWithMask(ua.NewExpandedNodeId(false, false, ua.NewNodeIdTwoByte(0), nil, nil),
		ua.NewExtensionObjectEncodingMask(false, false, false))
}

func str(s string) ua.PascalString { return ua.NewPascalString(&s) }

func nullString() ua.PascalString { return ua.NewPascalString(nil) }

func stringOf(s ua.PascalString) string {
	if s == nil || s.GetStringValue() == nil {
		return ""
	}
	return *s.GetStringValue()
}

func byteString(b []byte) ua.PascalByteString {
	if b == nil {
		return ua.NewPascalByteString(-1, nil)
	}
	return ua.NewPascalByteString(int32(len(b)), b)
}

func bytesOf(b ua.PascalByteString) []byte {
	if b == nil || b.GetStringLength() < 0 {
		return nil
	}
	return b.GetStringValue()
}

func text(s string) ua.LocalizedText {
	return ua.NewLocalizedText(true, false, nil, str(s))
}

func statusCodes(codes []uint32) []ua.StatusCode {
	out := make([]ua.StatusCode, len(codes))
	for i, code := range codes {
		out[i] = ua.NewStatusCode(code)
	}
	return out
}

// tooMany checks the number of operations of a request.
func tooMany(n int) uint32 {
	switch {
	case n == 0:
		return statusBadNothingToDo
	case n > maxOperations:
		return statusBadTooManyOperations
	}
	return statusGood
}

func (s *Server) applicationDescription() ua.ApplicationDescription {
	var discovery []ua.PascalString
	if s.cfg.EndpointURL != "" {
		discovery = []ua.PascalString{str(s.cfg.EndpointURL)}
	}
	return ua.NewApplicationDescription(str(s.cfg.ApplicationURI), str(s.cfg.ApplicationURI), text(s.cfg.ApplicationName),
		ua.ApplicationType_applicationTypeServer, nullString(), nullString(), discovery)
}

// endpointDescriptions describes an endpoint per security policy and mode. A user name's
// password goes encrypted with the endpoint's policy, or with Basic256Sha256 on an endpoint
// without security when the server has a certificate.
func (s *Server) endpointDescriptions() []ua.EndpointDescription {
	var out []ua.EndpointDescription
	for _, e := range s.endpoints {
		var tokens []ua.UserTokenPolicy
		if s.cfg.Anonymous {
			tokens = append(tokens, ua.NewUserTokenPolicy(str(userTokenAnonymous), ua.UserTokenType_userTokenTypeAnonymous, nullString(), nullString(), nullString()))
		}
		if s.cfg.Login != nil {
			passwordPolicy := e.policy.uri
			if e.policy.isNone() && s.cfg.Key != nil {
				passwordPolicy = SECURITY_POLICY_URI_PREFIX + "Basic256Sha256"
			}
			tokens = append(tokens, ua.NewUserTokenPolicy(str(userTokenUserName), ua.UserTokenType_userTokenTypeUserName, nullString(), nullString(), str(passwordPolicy)))
		}
		var certificate []byte
		if len(s.certChain) > 0 {
			certificate = s.cfg.Certificate
		}
		level := uint8(e.mode-1) * 10 // None 0, Sign 10, SignAndEncrypt 20, then by policy
		if !e.policy.isNone() {
			level += uint8(e.policy.nonceLength / 8)
		}
		out = append(out, ua.NewEndpointDescription(str(s.cfg.EndpointURL), s.applicationDescription(), byteString(certificate),
			ua.MessageSecurityMode(e.mode), str(e.policy.uri), tokens, str(transportProfile), level))
	}
	return out
}

func (c *conn) sessionOf(header ua.RequestHeader) *session {
	key := keyOf(header.GetAuthenticationToken())
	c.s.mutex.Lock()
	s := c.s.sessions[key]
	c.s.mutex.Unlock()
	if s == nil || s.c != c {
		return nil
	}
	return s
}

func (c *conn) createSession(request ua.CreateSessionRequest) ua.ExtensionObjectDefinition {
	header := request.GetRequestHeader()
	if c.security.secured() {
		chain, err := x509.ParseCertificates(bytesOf(request.GetClientCertificate()))
		if err != nil || len(chain) == 0 || !chain[0].Equal(c.security.remoteCertificate) {
			return c.fault(header, statusBadSecurityChecksFailed)
		}
	}
	timeout := time.Duration(request.GetRequestedSessionTimeout() * float64(time.Millisecond))
	timeout = min(max(timeout, sessionTimeoutMin), sessionTimeoutMax)
	s := &session{c: c, nonce: randomBytes(32), timeout: timeout, lastUsed: time.Now(),
		browses: map[string][]ua.ReferenceDescription{}, subscriptions: map[uint32]*subscription{}}
	s.token = ua.NewNodeId(ua.NewNodeIdGuid(1, randomBytes(16)))
	s.tokenKey = keyOf(s.token)
	c.s.mutex.Lock()
	if len(c.s.sessions) >= c.s.cfg.MaxSessions {
		c.s.mutex.Unlock()
		return c.fault(header, statusBadTooManySessions)
	}
	c.s.sessionIds++
	s.id = ua.NewNodeId(ua.NewNodeIdNumeric(1, c.s.sessionIds))
	c.s.sessions[s.tokenKey] = s
	c.s.mutex.Unlock()
	c.sessions = append(c.sessions, s)

	algorithm, signature, err := c.security.signSession(bytesOf(request.GetClientNonce()))
	if err != nil {
		c.closeSession(s)
		return c.fault(header, statusBadInternalError)
	}
	signatureData := ua.NewSignatureData(nullString(), byteString(nil))
	if c.security.secured() {
		signatureData = ua.NewSignatureData(str(algorithm), byteString(signature))
	}
	var certificate []byte
	if len(c.s.certChain) > 0 {
		certificate = c.s.cfg.Certificate
	}
	return ua.NewCreateSessionResponse(c.responseHeader(header, statusGood), s.id, s.token, float64(timeout/time.Millisecond),
		byteString(s.nonce), byteString(certificate), c.s.endpointDescriptions(), nil, signatureData, maxMessageSize)
}

func (c *conn) activateSession(s *session, request ua.ActivateSessionRequest) ua.ExtensionObjectDefinition {
	header := request.GetRequestHeader()
	s.mutex.Lock()
	nonce := s.nonce
	s.mutex.Unlock()
	if c.security.secured() {
		signature := request.GetClientSignature()
		if signature == nil || c.security.verifySession(nonce, stringOf(signature.GetAlgorithm()), bytesOf(signature.GetSignature())) != nil {
			return c.fault(header, statusBadApplicationSignatureInvalid)
		}
	}
	var token ua.ExtensionObjectDefinition
	if object := request.GetUserIdentityToken(); object != nil {
		token = object.GetBody()
	}
	user := ""
	switch token := token.(type) {
	case nil, ua.AnonymousIdentityToken:
		if !c.s.cfg.Anonymous {
			return c.fault(header, statusBadIdentityTokenRejected)
		}
	case ua.UserNameIdentityToken:
		if c.s.cfg.Login == nil {
			return c.fault(header, statusBadIdentityTokenRejected)
		}
		password, status := c.password(token, nonce)
		if status != statusGood {
			return c.fault(header, status)
		}
		user = stringOf(token.GetUserName())
		if !c.s.cfg.Login(user, password) {
			return c.fault(header, statusBadUserAccessDenied)
		}
	default:
		return c.fault(header, statusBadIdentityTokenInvalid)
	}
	s.mutex.Lock()
	s.activated, s.user, s.nonce = true, user, randomBytes(32)
	nonce = s.nonce
	s.mutex.Unlock()
	return ua.NewActivateSessionResponse(c.responseHeader(header, statusGood), byteString(nonce), nil, nil)
}

// password decrypts a user name token's password: length, password and the session nonce,
// encrypted with the server's key. Unencrypted it is taken only on an encrypted channel.
func (c *conn) password(token ua.UserNameIdentityToken, nonce []byte) (string, uint32) {
	secret := bytesOf(token.GetPassword())
	algorithm := stringOf(token.GetEncryptionAlgorithm())
	if algorithm == "" {
		if !c.security.encrypted() {
			return "", statusBadIdentityTokenRejected
		}
		return string(secret), statusGood
	}
	if c.s.cfg.Key == nil {
		return "", statusBadIdentityTokenRejected
	}
	var policy *securityPolicy
	for _, p := range securityPolicies {
		if !p.isNone() && p.asymmetricEncryptionURI() == algorithm {
			policy = p
			break
		}
	}
	size := c.s.cfg.Key.Size()
	if policy == nil || len(secret) == 0 || len(secret)%size != 0 {
		return "", statusBadIdentityTokenInvalid
	}
	var plain []byte
	for start := 0; start < len(secret); start += size {
		block, err := policy.asymmetricDecrypt(c.s.cfg.Key, secret[start:start+size])
		if err != nil {
			return "", statusBadIdentityTokenInvalid
		}
		plain = append(plain, block...)
	}
	if len(plain) < 4 {
		return "", statusBadIdentityTokenInvalid
	}
	length := int(binary.LittleEndian.Uint32(plain))
	if length < len(nonce) || 4+length > len(plain) || !bytes.Equal(plain[4+length-len(nonce):4+length], nonce) {
		return "", statusBadIdentityTokenInvalid
	}
	return string(plain[4 : 4+length-len(nonce)]), statusGood
}

// closeSession ends a session and its subscriptions; waiting Publish requests are answered.
func (c *conn) closeSession(s *session) {
	c.s.mutex.Lock()
	delete(c.s.sessions, s.tokenKey)
	c.s.mutex.Unlock()
	s.mutex.Lock()
	if s.closed {
		s.mutex.Unlock()
		return
	}
	s.closed = true
	for _, sub := range s.subscriptions {
		sub.stop()
	}
	s.subscriptions = nil
	publishes := s.publishes
	s.publishes = nil
	s.mutex.Unlock()
	for _, p := range publishes {
		_ = c.respond(p.requestId, c.fault(handleHeader(p.requestHandle), statusBadSessionClosed))
	}
}

// closeSessions ends the sessions of a channel that closed.
func (c *conn) closeSessions() {
	for _, s := range c.sessions {
		c.closeSession(s)
	}
}

// watchSessions ends sessions the client stopped using, and answers Publish requests that
// waited past their timeout.
func (c *conn) watchSessions(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			for _, s := range c.sessionsNow() {
				s.mutex.Lock()
				idle := now.Sub(s.lastUsed) > s.timeout && len(s.publishes) == 0
				var expired []publishRequest
				kept := s.publishes[:0]
				for _, p := range s.publishes {
					if p.timeout > 0 && now.Sub(p.received) > p.timeout {
						expired = append(expired, p)
					} else {
						kept = append(kept, p)
					}
				}
				s.publishes = kept
				s.mutex.Unlock()
				for _, p := range expired {
					_ = c.respond(p.requestId, c.fault(handleHeader(p.requestHandle), statusBadTimeoutService))
				}
				if idle {
					c.closeSession(s)
				}
			}
		}
	}
}

func (c *conn) sessionsNow() []*session {
	c.s.mutex.Lock()
	defer c.s.mutex.Unlock()
	var out []*session
	for _, s := range c.s.sessions {
		if s.c == c {
			out = append(out, s)
		}
	}
	return out
}

// Browse.

func (c *conn) browse(s *session, request ua.BrowseRequest) ua.ExtensionObjectDefinition {
	header := request.GetRequestHeader()
	descriptions := request.GetNodesToBrowse()
	if status := tooMany(len(descriptions)); status != statusGood {
		return c.fault(header, status)
	}
	limit := int(request.GetRequestedMaxReferencesPerNode())
	results := make([]ua.BrowseResult, len(descriptions))
	for i, d := range descriptions {
		references, status := c.s.references(d)
		results[i] = s.page(references, status, limit)
	}
	return ua.NewBrowseResponse(c.responseHeader(header, statusGood), results, nil)
}

// page is a browse result of at most limit references (0 for any number), keeping the rest
// for BrowseNext.
func (s *session) page(references []ua.ReferenceDescription, status uint32, limit int) ua.BrowseResult {
	if status != statusGood {
		return ua.NewBrowseResult(ua.NewStatusCode(status), byteString(nil), nil)
	}
	if limit == 0 || len(references) <= limit {
		return ua.NewBrowseResult(ua.NewStatusCode(statusGood), byteString(nil), references)
	}
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if len(s.browses) >= maxContinuationPoints {
		return ua.NewBrowseResult(ua.NewStatusCode(statusBadNoContinuationPoints), byteString(nil), nil)
	}
	point := randomBytes(16)
	s.browses[string(point)] = references[limit:]
	return ua.NewBrowseResult(ua.NewStatusCode(statusGood), byteString(point), references[:limit])
}

func (c *conn) browseNext(s *session, request ua.BrowseNextRequest) ua.ExtensionObjectDefinition {
	header := request.GetRequestHeader()
	points := request.GetContinuationPoints()
	if status := tooMany(len(points)); status != statusGood {
		return c.fault(header, status)
	}
	results := make([]ua.BrowseResult, len(points))
	for i, point := range points {
		key := string(bytesOf(point))
		s.mutex.Lock()
		rest, ok := s.browses[key]
		delete(s.browses, key)
		s.mutex.Unlock()
		switch {
		case !ok:
			results[i] = ua.NewBrowseResult(ua.NewStatusCode(statusBadContinuationPointInvalid), byteString(nil), nil)
		case request.GetReleaseContinuationPoints():
			results[i] = ua.NewBrowseResult(ua.NewStatusCode(statusGood), byteString(nil), nil)
		default:
			// The page size is the one of the first Browse: as many as it returned.
			results[i] = s.page(rest, statusGood, pageSize)
		}
	}
	return ua.NewBrowseResponse(c.responseHeader(header, statusGood), results, nil)
}

// pageSize is the page of a BrowseNext; Browse asked for its own.
const pageSize = 1000

// references are a node's references that a browse description asks for.
func (s *Server) references(d ua.BrowseDescription) ([]ua.ReferenceDescription, uint32) {
	n := s.space.nodes[keyOf(d.GetNodeId())]
	if n == nil {
		return nil, statusBadNodeIdUnknown
	}
	direction := d.GetBrowseDirection()
	if direction > ua.BrowseDirection_browseDirectionBoth {
		return nil, statusBadBrowseDirectionInvalid
	}
	referenceType, ok := referenceTypeOf(d.GetReferenceTypeId())
	if !ok {
		return nil, statusBadReferenceTypeIdInvalid
	}
	var out []ua.ReferenceDescription
	for _, r := range n.references {
		if direction == ua.BrowseDirection_browseDirectionForward && !r.forward ||
			direction == ua.BrowseDirection_browseDirectionInverse && r.forward {
			continue
		}
		if referenceType != 0 && r.typeId != referenceType && !(d.GetIncludeSubtypes() && isSubtype(r.typeId, referenceType)) {
			continue
		}
		target := s.space.nodes[r.target]
		if mask := d.GetNodeClassMask(); mask != 0 && mask&uint32(target.class) == 0 {
			continue
		}
		out = append(out, referenceDescription(r, target))
	}
	return out, statusGood
}

// referenceTypeOf is the reference type a browse asks for: 0 for any.
func referenceTypeOf(id ua.NodeId) (uint32, bool) {
	key := keyOf(id)
	if key == "" || key == "ns=0;i=0" {
		return 0, true
	}
	var t uint32
	if _, err := fmt.Sscanf(key, "ns=0;i=%d", &t); err != nil {
		return 0, false
	}
	_, known := superTypes[t]
	return t, known || t == idReferences
}

func referenceDescription(r reference, target *node) ua.ReferenceDescription {
	typeDefinition := ua.NewExpandedNodeId(false, false, ua.NewNodeIdTwoByte(0), nil, nil)
	if target.typeDef != 0 {
		typeDefinition = ua.NewExpandedNodeId(false, false, numericNodeId(0, target.typeDef).GetNodeId(), nil, nil)
	}
	return ua.NewReferenceDescription(numericNodeId(0, r.typeId), r.forward,
		ua.NewExpandedNodeId(false, false, target.id.GetNodeId(), nil, nil),
		ua.NewQualifiedName(target.namespace, str(target.name)), text(target.name), target.class, typeDefinition)
}

func (c *conn) translate(request ua.TranslateBrowsePathsToNodeIdsRequest) ua.ExtensionObjectDefinition {
	header := request.GetRequestHeader()
	paths := request.GetBrowsePaths()
	if status := tooMany(len(paths)); status != statusGood {
		return c.fault(header, status)
	}
	results := make([]ua.BrowsePathResult, len(paths))
	for i, path := range paths {
		results[i] = c.s.followPath(path)
	}
	return ua.NewTranslateBrowsePathsToNodeIdsResponse(c.responseHeader(header, statusGood), results, nil)
}

func (s *Server) followPath(path ua.BrowsePath) ua.BrowsePathResult {
	result := func(status uint32, targets []ua.BrowsePathTarget) ua.BrowsePathResult {
		return ua.NewBrowsePathResult(ua.NewStatusCode(status), targets)
	}
	start := keyOf(path.GetStartingNode())
	if s.space.nodes[start] == nil {
		return result(statusBadNodeIdUnknown, nil)
	}
	var elements []ua.RelativePathElement
	if path.GetRelativePath() != nil {
		elements = path.GetRelativePath().GetElements()
	}
	if len(elements) == 0 {
		return result(statusBadNothingToDo, nil)
	}
	current := []string{start}
	for _, e := range elements {
		referenceType, ok := referenceTypeOf(e.GetReferenceTypeId())
		if !ok {
			return result(statusBadNoMatch, nil)
		}
		name := e.GetTargetName()
		var next []string
		for _, key := range current {
			for _, r := range s.space.nodes[key].references {
				if r.forward == e.GetIsInverse() {
					continue
				}
				if referenceType != 0 && r.typeId != referenceType && !(e.GetIncludeSubtypes() && isSubtype(r.typeId, referenceType)) {
					continue
				}
				target := s.space.nodes[r.target]
				if name != nil && (target.namespace != name.GetNamespaceIndex() || target.name != stringOf(name.GetName())) {
					continue
				}
				next = append(next, r.target)
			}
		}
		if len(next) == 0 {
			return result(statusBadNoMatch, nil)
		}
		current = next
	}
	targets := make([]ua.BrowsePathTarget, len(current))
	for i, key := range current {
		targets[i] = ua.NewBrowsePathTarget(ua.NewExpandedNodeId(false, false, s.space.nodes[key].id.GetNodeId(), nil, nil), 0xFFFFFFFF)
	}
	return result(statusGood, targets)
}

// Read and Write.

func (c *conn) read(request ua.ReadRequest) ua.ExtensionObjectDefinition {
	header := request.GetRequestHeader()
	nodes := request.GetNodesToRead()
	if status := tooMany(len(nodes)); status != statusGood {
		return c.fault(header, status)
	}
	timestamps := request.GetTimestampsToReturn()
	if timestamps > ua.TimestampsToReturn_timestampsToReturnNeither {
		return c.fault(header, statusBadTimestampsToReturnInvalid)
	}
	results := make([]ua.DataValue, len(nodes))
	for i, r := range nodes {
		results[i] = c.s.readValueId(r, timestamps)
	}
	return ua.NewReadResponse(c.responseHeader(header, statusGood), results, nil)
}

func (s *Server) readValueId(r ua.ReadValueId, timestamps ua.TimestampsToReturn) ua.DataValue {
	n := s.space.nodes[keyOf(r.GetNodeId())]
	if n == nil {
		return statusValue(statusBadNodeIdUnknown)
	}
	if stringOf(r.GetIndexRange()) != "" {
		return statusValue(statusBadIndexRangeInvalid)
	}
	if encoding := r.GetDataEncoding(); encoding != nil && stringOf(encoding.GetName()) != "" {
		return statusValue(statusBadDataEncodingUnsupported)
	}
	return s.readAttribute(n, r.GetAttributeId(), timestamps)
}

func statusValue(status uint32) ua.DataValue {
	return ua.NewDataValue(false, false, false, false, true, false, nil, ua.NewStatusCode(status), nil, nil, nil, nil)
}

// dataValue makes a DataValue with the timestamps asked for.
func dataValue(variant ua.Variant, status uint32, source time.Time, timestamps ua.TimestampsToReturn) ua.DataValue {
	var sourceTime, serverTime *int64
	if (timestamps == ua.TimestampsToReturn_timestampsToReturnSource || timestamps == ua.TimestampsToReturn_timestampsToReturnBoth) && !source.IsZero() {
		t := dateTime(source)
		sourceTime = &t
	}
	if timestamps == ua.TimestampsToReturn_timestampsToReturnServer || timestamps == ua.TimestampsToReturn_timestampsToReturnBoth {
		t := dateTime(time.Now())
		serverTime = &t
	}
	// An optional field goes out when it is set: the status only when it is not Good.
	var statusCode ua.StatusCode
	if status != statusGood {
		statusCode = ua.NewStatusCode(status)
	}
	return ua.NewDataValue(false, false, serverTime != nil, sourceTime != nil, status != statusGood, variant != nil,
		variant, statusCode, sourceTime, nil, serverTime, nil)
}

func scalar(variant ua.Variant, err error) ua.DataValue {
	if err != nil {
		return statusValue(statusBadInternalError)
	}
	return dataValue(variant, statusGood, time.Time{}, ua.TimestampsToReturn_timestampsToReturnNeither)
}

func (s *Server) readAttribute(n *node, attribute uint32, timestamps ua.TimestampsToReturn) ua.DataValue {
	variable := n.class == ua.NodeClass_nodeClassVariable
	switch attribute {
	case attrNodeId:
		return dataValue(ua.NewVariantNodeId(false, false, nil, nil, nil, []ua.NodeId{n.id}), statusGood, time.Time{}, ua.TimestampsToReturn_timestampsToReturnNeither)
	case attrNodeClass:
		return scalar(variantOf(int32(n.class), TypeInt32))
	case attrBrowseName:
		return dataValue(ua.NewVariantQualifiedName(false, false, nil, nil, nil, []ua.QualifiedName{ua.NewQualifiedName(n.namespace, str(n.name))}), statusGood, time.Time{}, ua.TimestampsToReturn_timestampsToReturnNeither)
	case attrDisplayName:
		return dataValue(ua.NewVariantLocalizedText(false, false, nil, nil, nil, []ua.LocalizedText{text(n.name)}), statusGood, time.Time{}, ua.TimestampsToReturn_timestampsToReturnNeither)
	case attrDescription:
		return dataValue(ua.NewVariantLocalizedText(false, false, nil, nil, nil, []ua.LocalizedText{text(n.description)}), statusGood, time.Time{}, ua.TimestampsToReturn_timestampsToReturnNeither)
	case attrWriteMask, attrUserWriteMask:
		return scalar(variantOf(uint32(0), TypeUInt32))
	case attrEventNotifier:
		if n.class == ua.NodeClass_nodeClassObject {
			return scalar(variantOf(uint8(0), TypeByte))
		}
	case attrValue:
		if variable {
			return s.readValue(n, timestamps)
		}
	case attrDataType:
		if variable {
			return dataValue(ua.NewVariantNodeId(false, false, nil, nil, nil, []ua.NodeId{numericNodeId(0, n.dataType)}), statusGood, time.Time{}, ua.TimestampsToReturn_timestampsToReturnNeither)
		}
	case attrValueRank:
		if variable {
			return scalar(variantOf(n.valueRank, TypeInt32))
		}
	case attrArrayDimensions:
		if variable {
			if n.dims == nil {
				return dataValue(nil, statusGood, time.Time{}, ua.TimestampsToReturn_timestampsToReturnNeither)
			}
			return scalar(variantOf(n.dims, TypeUInt32))
		}
	case attrAccessLevel:
		if variable {
			return scalar(variantOf(n.accessLevel(true), TypeByte))
		}
	case attrUserAccessLevel:
		if variable {
			return scalar(variantOf(n.accessLevel(true), TypeByte))
		}
	case attrMinimumSamplingInterval:
		if variable {
			return scalar(variantOf(float64(0), TypeDouble))
		}
	case attrHistorizing:
		if variable {
			return scalar(variantOf(false, TypeBoolean))
		}
	}
	return statusValue(statusBadAttributeIdInvalid)
}

// sample is a variable's current value and status.
type sample struct {
	variant ua.Variant
	status  uint32
	time    time.Time
}

func (s *Server) sample(n *node) sample {
	if n.value != nil {
		v, t := n.value()
		if t == 0 {
			return sample{status: statusBadNotReadable}
		}
		variant, err := variantOf(v, t)
		if err != nil {
			return sample{status: statusBadInternalError}
		}
		return sample{variant: variant, time: time.Now()}
	}
	if n.point == nil {
		return sample{status: statusBadNotReadable}
	}
	value, ok := s.cfg.Source.Read(*n.point)
	if !ok || value.Value == nil {
		return sample{status: statusBadWaitingForInitialData}
	}
	variant, err := variantOf(value.Value, n.point.DataType)
	if err != nil {
		s.onError(fmt.Errorf("opcua: point %s: %w", n.point.Name, err))
		return sample{status: statusBadTypeMismatch}
	}
	status := uint32(statusGood)
	if !value.Good {
		status = statusBad
	}
	return sample{variant: variant, status: status, time: value.Time}
}

func (s *Server) readValue(n *node, timestamps ua.TimestampsToReturn) ua.DataValue {
	got := s.sample(n)
	if got.variant == nil {
		return statusValue(got.status)
	}
	return dataValue(got.variant, got.status, got.time, timestamps)
}

func (c *conn) writeValues(s *session, request ua.WriteRequest) ua.ExtensionObjectDefinition {
	header := request.GetRequestHeader()
	values := request.GetNodesToWrite()
	if status := tooMany(len(values)); status != statusGood {
		return c.fault(header, status)
	}
	s.mutex.Lock()
	user := s.user
	s.mutex.Unlock()
	results := make([]uint32, len(values))
	for i, w := range values {
		results[i] = c.writeValue(w, user)
	}
	return ua.NewWriteResponse(c.responseHeader(header, statusGood), statusCodes(results), nil)
}

func (c *conn) writeValue(w ua.WriteValue, user string) uint32 {
	n := c.s.space.nodes[keyOf(w.GetNodeId())]
	if n == nil {
		return statusBadNodeIdUnknown
	}
	if w.GetAttributeId() < attrNodeId || w.GetAttributeId() > attrHistorizing {
		return statusBadAttributeIdInvalid
	}
	if w.GetAttributeId() != attrValue || n.point == nil || !n.point.Writable {
		return statusBadNotWritable
	}
	if !c.mayWrite {
		return statusBadUserAccessDenied
	}
	if stringOf(w.GetIndexRange()) != "" {
		return statusBadIndexRangeInvalid
	}
	if w.GetValue() == nil || !w.GetValue().GetValueSpecified() || w.GetValue().GetValue() == nil {
		return statusBadTypeMismatch
	}
	value, status := convert(*n.point, w.GetValue().GetValue())
	if status != statusGood {
		return status
	}
	if err := c.s.cfg.Source.Write(*n.point, value, user); err != nil {
		c.s.onError(fmt.Errorf("opcua: writing %s: %w", n.point.Name, err))
		return statusBadNotWritable
	}
	return statusGood
}

// convert decodes a written Variant into a value of the point's type: a number of another
// numeric type converts, anything else must have the point's type, and an array the point's
// length.
func convert(p Point, variant ua.Variant) (any, uint32) {
	value, dataType, err := valueOf(variant)
	if err != nil {
		return nil, statusBadTypeMismatch
	}
	numeric := func(t uint32) bool { return t >= TypeSByte && t <= TypeDouble }
	if dataType != p.DataType {
		if !numeric(dataType) || !numeric(p.DataType) {
			return nil, statusBadTypeMismatch
		}
		converted, err := variantOf(value, p.DataType)
		if err != nil {
			return nil, statusBadTypeMismatch
		}
		if value, _, err = valueOf(converted); err != nil {
			return nil, statusBadTypeMismatch
		}
	}
	rv := reflect.ValueOf(value)
	array := rv.Kind() == reflect.Slice
	if array != (p.ArrayLength > 0) || array && rv.Len() != p.ArrayLength {
		return nil, statusBadTypeMismatch
	}
	return value, statusGood
}
