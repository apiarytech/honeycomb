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
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	ua "github.com/apache/plc4x/plc4go/protocols/opcua/readwrite/model"
	"github.com/apache/plc4x/plc4go/spi/utils"
)

// Limits of the transport (OPC UA Part 6, 7.1.2).
const (
	protocolVersion    = 0
	receiveBufferSize  = 65535
	minimumBufferSize  = 8192
	maxMessageSize     = 16 << 20
	maxChunkCount      = 0 // bounded by maxMessageSize
	helloTimeout       = 10 * time.Second
	tokenLifetimeMin   = 10 * time.Minute
	tokenLifetimeMax   = time.Hour
	asymmetricChunkMax = 64 << 10
)

// conn is one client connection: a secure channel and the sessions created on it.
type conn struct {
	s        *Server
	nc       net.Conn
	mayWrite bool
	ctx      context.Context

	sendSize int // the largest chunk the client takes
	channel  uint32
	security *channelSecurity
	endpoint endpoint
	// discoveryOnly marks a channel without security to a server that offers no endpoint
	// without it: such a channel only finds the endpoints (Part 4, 5.4).
	discoveryOnly bool
	tokenTTL      time.Duration
	expires       time.Time // of the current token

	sendMutex sync.Mutex
	sequence  uint32

	partial      map[uint32][]byte // bodies of messages still in chunks, by request id
	partialBytes int

	sessions []*session // created on this channel
}

// statusError is a failure the client is told about with its status code.
type statusError struct {
	status uint32
	reason string
}

func (e *statusError) Error() string { return fmt.Sprintf("0x%08X: %s", e.status, e.reason) }

func fail(status uint32, format string, args ...any) error {
	return &statusError{status: status, reason: fmt.Sprintf(format, args...)}
}

// Transport status codes.
const (
	statusBadTcpMessageTypeInvalid  = 0x807E0000
	statusBadTcpMessageTooLarge     = 0x80800000
	statusBadDecodingError          = 0x80070000
	statusBadTimeout                = 0x800A0000
	statusBadSecureChannelIdInvalid = 0x80220000
	statusBadRequestTooLarge        = 0x80B80000
)

// ServeConn answers one client until it closes the connection, breaks the protocol or ctx
// ends. mayWrite says whether the client may write values.
func (s *Server) ServeConn(ctx context.Context, nc net.Conn, mayWrite bool) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = nc.Close() })
	defer stop()
	c := &conn{s: s, nc: nc, mayWrite: mayWrite, ctx: ctx, partial: map[uint32][]byte{}}
	defer c.closeSessions()
	go c.watchSessions(ctx)
	if err := c.serve(); err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) && ctx.Err() == nil {
		var se *statusError
		if errors.As(err, &se) {
			c.sendError(se.status, se.reason)
		}
		s.onError(fmt.Errorf("opcua: client %s: %w", nc.RemoteAddr(), err))
	}
	_ = nc.Close()
}

func (c *conn) serve() error {
	_ = c.nc.SetReadDeadline(time.Now().Add(helloTimeout))
	chunk, err := c.readChunk(minimumBufferSize)
	if err != nil {
		return err
	}
	if string(chunk[:4]) != "HELF" {
		return fail(statusBadTcpMessageTypeInvalid, "the first message must be Hello")
	}
	if err := c.hello(chunk); err != nil {
		return err
	}
	for {
		deadline := time.Time{}
		if c.security == nil {
			deadline = time.Now().Add(helloTimeout)
		} else {
			deadline = c.expires.Add(c.tokenTTL / 4) // the client renews before, or is gone
		}
		_ = c.nc.SetReadDeadline(deadline)
		chunk, err := c.readChunk(receiveBufferSize)
		if err != nil {
			var timeout net.Error
			if errors.As(err, &timeout) && timeout.Timeout() {
				return fail(statusBadTimeout, "the secure channel expired")
			}
			return err
		}
		switch string(chunk[:3]) {
		case "OPN":
			if err := c.open(chunk); err != nil {
				return err
			}
		case "MSG":
			if err := c.message(chunk); err != nil {
				return err
			}
		case "CLO":
			if c.security != nil {
				_, _ = c.security.openSymmetric(chunk)
			}
			return io.EOF
		default:
			return fail(statusBadTcpMessageTypeInvalid, "message type %q", chunk[:3])
		}
	}
}

// readChunk reads one chunk of at most limit bytes.
func (c *conn) readChunk(limit int) ([]byte, error) {
	header := make([]byte, 8)
	if _, err := io.ReadFull(c.nc, header); err != nil {
		return nil, err
	}
	size := int(binary.LittleEndian.Uint32(header[4:]))
	if string(header[:3]) == "OPN" {
		limit = max(limit, asymmetricChunkMax) // a certificate chain may not fit a small buffer
	}
	if size < 8 || size > limit {
		return nil, fail(statusBadTcpMessageTooLarge, "chunk of %d bytes, the limit is %d", size, limit)
	}
	chunk := make([]byte, size)
	copy(chunk, header)
	if _, err := io.ReadFull(c.nc, chunk[8:]); err != nil {
		return nil, err
	}
	return chunk, nil
}

func (c *conn) write(chunk []byte) error {
	_ = c.nc.SetWriteDeadline(time.Now().Add(30 * time.Second))
	_, err := c.nc.Write(chunk)
	return err
}

// hello answers Hello with Acknowledge and the limits both sides keep.
func (c *conn) hello(chunk []byte) error {
	if len(chunk) < 32 {
		return fail(statusBadDecodingError, "Hello too short")
	}
	clientReceive := binary.LittleEndian.Uint32(chunk[12:])
	clientSend := binary.LittleEndian.Uint32(chunk[16:])
	if clientReceive < minimumBufferSize || clientSend < minimumBufferSize {
		return fail(statusBadDecodingError, "buffer sizes below %d bytes", minimumBufferSize)
	}
	c.sendSize = int(min(clientReceive, receiveBufferSize))
	ack := make([]byte, 28)
	copy(ack, "ACKF")
	binary.LittleEndian.PutUint32(ack[4:], 28)
	binary.LittleEndian.PutUint32(ack[8:], protocolVersion)
	binary.LittleEndian.PutUint32(ack[12:], uint32(min(clientSend, receiveBufferSize)))
	binary.LittleEndian.PutUint32(ack[16:], uint32(c.sendSize))
	binary.LittleEndian.PutUint32(ack[20:], maxMessageSize)
	binary.LittleEndian.PutUint32(ack[24:], maxChunkCount)
	return c.write(ack)
}

// sendError sends an Error message (the connection closes after it).
func (c *conn) sendError(status uint32, reason string) {
	if len(reason) > 4096 {
		reason = reason[:4096]
	}
	out := []byte("ERRF\x00\x00\x00\x00")
	out = binary.LittleEndian.AppendUint32(out, status)
	out = appendBytes(out, []byte(reason))
	setChunkSize(out)
	_ = c.write(out)
}

// open answers OpenSecureChannel: Issue opens the channel, Renew gives it a new token.
func (c *conn) open(chunk []byte) error {
	if string(chunk[3:4]) != "F" {
		return fail(statusBadDecodingError, "OpenSecureChannel in chunks")
	}
	header, err := parseAsymmetricHeader(chunk)
	if err != nil {
		return fail(statusBadDecodingError, "%v", err)
	}
	channelId := binary.LittleEndian.Uint32(chunk[8:])
	if c.security == nil {
		if channelId != 0 {
			return fail(statusBadSecureChannelIdInvalid, "no channel %d", channelId)
		}
		if err := c.prepare(header); err != nil {
			return err
		}
	} else if channelId != c.channel {
		return fail(statusBadSecureChannelIdInvalid, "this is channel %d, not %d", c.channel, channelId)
	}
	opened, err := c.security.openAsymmetric(chunk)
	if err != nil {
		return fail(statusBadSecurityChecksFailed, "%v", err)
	}
	if len(opened) < header.end+sequenceHeaderSize {
		return fail(statusBadDecodingError, "OpenSecureChannel too short")
	}
	requestId := binary.LittleEndian.Uint32(opened[header.end+4:])
	body, err := parseBody(opened[header.end+sequenceHeaderSize:])
	if err != nil {
		return fail(statusBadDecodingError, "%v", err)
	}
	request, ok := body.(ua.OpenSecureChannelRequest)
	if !ok {
		return fail(statusBadDecodingError, "%T on an OpenSecureChannel message", body)
	}
	renew := request.GetRequestType() == ua.SecurityTokenRequestType_securityTokenRequestTypeRenew
	if renew != (c.channel != 0) {
		return fail(statusBadSecurityChecksFailed, "%s on a channel that is %s", request.GetRequestType(), map[bool]string{true: "open", false: "not open"}[c.channel != 0])
	}
	if !renew {
		mode := uint32(request.GetSecurityMode())
		found := c.discoveryOnly && mode == 1
		for _, e := range c.s.endpoints {
			if e.policy == c.security.policy && e.mode == mode {
				c.endpoint, found = e, true
			}
		}
		if !found {
			return fail(statusBadSecurityPolicyRejected, "security policy %s with mode %s is not offered", c.security.policy.name, request.GetSecurityMode())
		}
		if !c.security.policy.isNone() {
			c.security.mode = request.GetSecurityMode()
		}
		c.channel = c.s.channels.Add(1)
	} else if request.GetSecurityMode() != c.security.mode && !c.security.policy.isNone() {
		return fail(statusBadSecurityChecksFailed, "Renew asks for another security mode")
	}
	lifetime := time.Duration(request.GetRequestedLifetime()) * time.Millisecond
	c.tokenTTL = min(max(lifetime, tokenLifetimeMin), tokenLifetimeMax)
	tokenId := c.s.tokens.Add(1)
	serverNonce, err := c.security.newLocalNonce()
	if err != nil {
		return err
	}
	now := time.Now()
	response := ua.NewOpenSecureChannelResponse(c.responseHeader(request.GetRequestHeader(), 0), protocolVersion,
		ua.NewChannelSecurityToken(c.channel, tokenId, dateTime(now), uint32(c.tokenTTL/time.Millisecond)), byteString(serverNonce))
	payload, err := serializeBody(response)
	if err != nil {
		return err
	}
	c.sendMutex.Lock()
	defer c.sendMutex.Unlock()
	out := []byte("OPNF\x00\x00\x00\x00")
	out = binary.LittleEndian.AppendUint32(out, c.channel)
	out = appendBytes(out, []byte(c.security.policy.uri))
	out = appendBytes(out, c.security.localCertificateIfSecured())
	out = appendBytes(out, c.security.remoteThumbprintIfSecured())
	out = binary.LittleEndian.AppendUint32(out, c.nextSequence())
	out = binary.LittleEndian.AppendUint32(out, requestId)
	out = append(out, payload...)
	setChunkSize(out)
	secured, err := c.security.secureAsymmetric(out)
	if err != nil {
		return err
	}
	if err := c.write(secured); err != nil {
		return err
	}
	var clientNonce []byte
	if nonce := request.GetClientNonce(); nonce != nil {
		clientNonce = nonce.GetStringValue()
	}
	if err := c.security.installToken(tokenId, clientNonce); err != nil {
		return fail(statusBadSecurityChecksFailed, "%v", err)
	}
	c.expires = now.Add(c.tokenTTL)
	return nil
}

// prepare builds the security of a new channel from its first OpenSecureChannel header: the
// policy it names, and the client certificate, which must be trusted.
func (c *conn) prepare(header asymmetricHeader) error {
	var policy *securityPolicy
	for _, e := range c.s.endpoints {
		if e.policy.uri == header.policyUri {
			policy = e.policy
		}
	}
	if policy == nil && header.policyUri == SECURITY_POLICY_NONE_URI {
		policy, _ = securityPolicyByName("None")
		c.discoveryOnly = true
	}
	if policy == nil {
		return fail(statusBadSecurityPolicyRejected, "security policy %s is not offered", header.policyUri)
	}
	if policy.isNone() {
		security, err := newChannelSecurity(policy, ua.MessageSecurityMode_messageSecurityModeNone, nil, nil, nil)
		c.security = security
		return err
	}
	chain, err := x509.ParseCertificates(header.senderCertificate)
	if err != nil || len(chain) == 0 {
		return fail(statusBadSecurityChecksFailed, "the client certificate does not parse: %v", err)
	}
	if err := c.s.cfg.TrustClient(chain); err != nil {
		return fail(statusBadCertificateUntrusted, "the client certificate is not trusted: %v", err)
	}
	// Sign until the request says which mode: the asymmetric algorithms are the same for both.
	security, err := newChannelSecurity(policy, ua.MessageSecurityMode_messageSecurityModeSign, c.s.cfg.Key, c.s.certChain[0].Raw, chain[0])
	if err != nil {
		return fail(statusBadSecurityChecksFailed, "%v", err)
	}
	c.security = security
	return nil
}

// message takes a MSG chunk: it collects the chunks of a request and, at the last, serves it.
func (c *conn) message(chunk []byte) error {
	if c.channel == 0 {
		return fail(statusBadSecureChannelIdInvalid, "a message before OpenSecureChannel")
	}
	if channelId := binary.LittleEndian.Uint32(chunk[8:]); channelId != c.channel {
		return fail(statusBadSecureChannelIdInvalid, "this is channel %d, not %d", c.channel, channelId)
	}
	opened, err := c.security.openSymmetric(chunk)
	if err != nil {
		return fail(statusBadSecurityChecksFailed, "%v", err)
	}
	requestId := binary.LittleEndian.Uint32(opened[symmetricSecurityHeaderEnd+4:])
	part := opened[symmetricSecurityHeaderEnd+sequenceHeaderSize:]
	switch opened[3] {
	case 'A':
		c.partialBytes -= len(c.partial[requestId])
		delete(c.partial, requestId)
		return nil
	case 'C':
		c.partial[requestId] = append(c.partial[requestId], part...)
		c.partialBytes += len(part)
		if c.partialBytes > maxMessageSize {
			return fail(statusBadRequestTooLarge, "requests of more than %d bytes", maxMessageSize)
		}
		return nil
	case 'F':
	default:
		return fail(statusBadDecodingError, "chunk type %q", opened[3])
	}
	body := part
	if previous, ok := c.partial[requestId]; ok {
		body = append(previous, part...)
		c.partialBytes -= len(previous)
		delete(c.partial, requestId)
	}
	request, err := parseBody(body)
	if err != nil {
		// The request handle is unknown: answer with a fault for handle 0.
		return c.respond(requestId, c.fault(nil, statusBadDecodingError))
	}
	response := c.serveRequest(requestId, request)
	if response == nil {
		return nil // answered later (Publish)
	}
	return c.respond(requestId, response)
}

// respond sends a response to request requestId, in as many chunks as it takes.
func (c *conn) respond(requestId uint32, response ua.ExtensionObjectDefinition) error {
	body, err := serializeBody(response)
	if err != nil {
		c.s.onError(fmt.Errorf("opcua: serializing %T: %w", response, err))
		if body, err = serializeBody(c.fault(nil, statusBadInternalError)); err != nil {
			return err
		}
	}
	c.sendMutex.Lock()
	defer c.sendMutex.Unlock()
	room := c.security.maxSymmetricBodySize(c.sendSize)
	for start := 0; start == 0 || start < len(body); start += room {
		end := min(start+room, len(body))
		chunkType := byte('C')
		if end == len(body) {
			chunkType = 'F'
		}
		out := make([]byte, 0, symmetricSecurityHeaderEnd+sequenceHeaderSize+end-start)
		out = append(out, 'M', 'S', 'G', chunkType, 0, 0, 0, 0)
		out = binary.LittleEndian.AppendUint32(out, c.channel)
		out = binary.LittleEndian.AppendUint32(out, 0) // the token, set by secureSymmetric
		out = binary.LittleEndian.AppendUint32(out, c.nextSequence())
		out = binary.LittleEndian.AppendUint32(out, requestId)
		out = append(out, body[start:end]...)
		setChunkSize(out)
		secured, err := c.security.secureSymmetric(out)
		if err != nil {
			return err
		}
		if err := c.write(secured); err != nil {
			return err
		}
	}
	return nil
}

func (c *conn) nextSequence() uint32 {
	c.sequence++
	if c.sequence > 4294966271 { // wraps below 1024 (Part 6, 6.7.2.4)
		c.sequence = 1
	}
	return c.sequence
}

func appendBytes(out, value []byte) []byte {
	if value == nil {
		return binary.LittleEndian.AppendUint32(out, 0xFFFFFFFF)
	}
	out = binary.LittleEndian.AppendUint32(out, uint32(len(value)))
	return append(out, value...)
}

// parseBody parses a message body: the encoding id of a service type, then the service.
func parseBody(body []byte) (ua.ExtensionObjectDefinition, error) {
	object, err := ua.ExtensionObjectParseWithBuffer[ua.ExtensionObject](context.Background(),
		utils.NewReadBufferByteBased(body, utils.WithByteOrderForReadBufferByteBased(binary.LittleEndian)), false)
	if err != nil {
		return nil, err
	}
	if object.GetBody() == nil {
		return nil, errors.New("a message of an unknown type")
	}
	return object.GetBody(), nil
}

func serializeBody(definition ua.ExtensionObjectDefinition) ([]byte, error) {
	buffer := utils.NewWriteBufferByteBased(utils.WithByteOrderForByteBasedBuffer(binary.LittleEndian))
	if err := ua.NewRootExtensionObject(expandedId(definition.GetExtensionId()), definition).SerializeWithWriteBuffer(context.Background(), buffer); err != nil {
		return nil, err
	}
	return bytes.Clone(buffer.GetBytes()), nil
}

func expandedId(id int32) ua.ExpandedNodeId {
	return ua.NewExpandedNodeId(false, false, ua.NewNodeIdFourByte(0, uint16(id)), nil, nil)
}
