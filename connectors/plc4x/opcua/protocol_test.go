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
	"crypto/x509"
	"encoding/binary"
	"io"
	"net"
	"slices"
	"strconv"
	"testing"
	"time"

	ua "github.com/apache/plc4x/plc4go/protocols/opcua/readwrite/model"
)

// rawClient speaks UA-TCP without security, message by message, to test what a client
// library would hide: faults, chunks, keep-alives and retransmission.
type rawClient struct {
	t         *testing.T
	nc        net.Conn
	chunkSize int
	sequence  uint32
	requestId uint32
	handle    uint32
	channel   uint32
	token     uint32
	auth      ua.NodeId
	pending   map[uint32]ua.ExtensionObjectDefinition
}

func dialRaw(t *testing.T, address string) *rawClient {
	t.Helper()
	nc, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { nc.Close() })
	c := &rawClient{t: t, nc: nc, chunkSize: 65535, auth: ua.NewNodeId(ua.NewNodeIdTwoByte(0)), pending: map[uint32]ua.ExtensionObjectDefinition{}}
	_ = nc.SetDeadline(time.Now().Add(30 * time.Second))
	return c
}

func (c *rawClient) readChunk() []byte {
	c.t.Helper()
	header := make([]byte, 8)
	if _, err := io.ReadFull(c.nc, header); err != nil {
		c.t.Fatalf("read: %v", err)
	}
	chunk := make([]byte, binary.LittleEndian.Uint32(header[4:]))
	copy(chunk, header)
	if _, err := io.ReadFull(c.nc, chunk[8:]); err != nil {
		c.t.Fatalf("read: %v", err)
	}
	return chunk
}

func (c *rawClient) hello() {
	c.t.Helper()
	out := []byte("HELF\x00\x00\x00\x00")
	for _, v := range []uint32{0, 65535, 65535, 0, 0} {
		out = binary.LittleEndian.AppendUint32(out, v)
	}
	out = appendBytes(out, []byte("opc.tcp://test"))
	setChunkSize(out)
	if _, err := c.nc.Write(out); err != nil {
		c.t.Fatal(err)
	}
	if ack := c.readChunk(); string(ack[:4]) != "ACKF" {
		c.t.Fatalf("answer to Hello: %q", ack[:4])
	}
}

func (c *rawClient) header() ua.RequestHeader {
	c.handle++
	return ua.NewRequestHeader(c.auth, dateTime(time.Now()), c.handle, 0, nullString(), 10000, nullExtensionObject())
}

func (c *rawClient) sendOpen() {
	c.t.Helper()
	body, err := serializeBody(ua.NewOpenSecureChannelRequest(c.header(), 0, ua.SecurityTokenRequestType_securityTokenRequestTypeIssue,
		ua.MessageSecurityMode_messageSecurityModeNone, byteString(nil), 3600000))
	if err != nil {
		c.t.Fatal(err)
	}
	c.sequence++
	c.requestId++
	out := []byte("OPNF\x00\x00\x00\x00\x00\x00\x00\x00")
	out = appendBytes(out, []byte(SECURITY_POLICY_NONE_URI))
	out = appendBytes(out, nil)
	out = appendBytes(out, nil)
	out = binary.LittleEndian.AppendUint32(out, c.sequence)
	out = binary.LittleEndian.AppendUint32(out, c.requestId)
	out = append(out, body...)
	setChunkSize(out)
	if _, err := c.nc.Write(out); err != nil {
		c.t.Fatal(err)
	}
}

func (c *rawClient) open() {
	c.t.Helper()
	c.sendOpen()
	chunk := c.readChunk()
	header, err := parseAsymmetricHeader(chunk)
	if err != nil {
		c.t.Fatal(err)
	}
	response, err := parseBody(chunk[header.end+sequenceHeaderSize:])
	if err != nil {
		c.t.Fatal(err)
	}
	opened := response.(ua.OpenSecureChannelResponse)
	c.channel = opened.GetSecurityToken().GetChannelId()
	c.token = opened.GetSecurityToken().GetTokenId()
}

// send sends a request in chunks of c.chunkSize and returns its request id.
func (c *rawClient) send(request ua.ExtensionObjectDefinition) uint32 {
	c.t.Helper()
	body, err := serializeBody(request)
	if err != nil {
		c.t.Fatal(err)
	}
	c.requestId++
	room := c.chunkSize - symmetricSecurityHeaderEnd - sequenceHeaderSize
	for start := 0; start == 0 || start < len(body); start += room {
		end := min(start+room, len(body))
		chunkType := byte('C')
		if end == len(body) {
			chunkType = 'F'
		}
		c.sequence++
		out := []byte{'M', 'S', 'G', chunkType, 0, 0, 0, 0}
		out = binary.LittleEndian.AppendUint32(out, c.channel)
		out = binary.LittleEndian.AppendUint32(out, c.token)
		out = binary.LittleEndian.AppendUint32(out, c.sequence)
		out = binary.LittleEndian.AppendUint32(out, c.requestId)
		out = append(out, body[start:end]...)
		setChunkSize(out)
		if _, err := c.nc.Write(out); err != nil {
			c.t.Fatal(err)
		}
	}
	return c.requestId
}

// receive waits for the response to request id, keeping others for later.
func (c *rawClient) receive(id uint32) ua.ExtensionObjectDefinition {
	c.t.Helper()
	parts := map[uint32][]byte{}
	for {
		if response, ok := c.pending[id]; ok {
			delete(c.pending, id)
			return response
		}
		chunk := c.readChunk()
		if string(chunk[:3]) != "MSG" {
			c.t.Fatalf("message %q: %q", chunk[:4], chunk[8:])
		}
		requestId := binary.LittleEndian.Uint32(chunk[symmetricSecurityHeaderEnd+4:])
		parts[requestId] = append(parts[requestId], chunk[symmetricSecurityHeaderEnd+sequenceHeaderSize:]...)
		if chunk[3] != 'F' {
			continue
		}
		response, err := parseBody(parts[requestId])
		if err != nil {
			c.t.Fatalf("response to %d: %v", requestId, err)
		}
		delete(parts, requestId)
		c.pending[requestId] = response
	}
}

func (c *rawClient) call(request ua.ExtensionObjectDefinition) ua.ExtensionObjectDefinition {
	c.t.Helper()
	return c.receive(c.send(request))
}

// fault is the service result of a ServiceFault, 0 for another response.
func fault(response ua.ExtensionObjectDefinition) uint32 {
	if f, ok := response.(ua.ServiceFault); ok {
		return f.GetResponseHeader().GetServiceResult().GetStatusCode()
	}
	return 0
}

func (c *rawClient) session(activate bool) {
	c.t.Helper()
	app := ua.NewApplicationDescription(str("urn:test"), str("urn:test"), text("test"), ua.ApplicationType_applicationTypeClient, nullString(), nullString(), nil)
	created, ok := c.call(ua.NewCreateSessionRequest(c.header(), app, nullString(), str("opc.tcp://test"), str("raw"),
		byteString(randomBytes(32)), byteString(nil), 60000, 0)).(ua.CreateSessionResponse)
	if !ok {
		c.t.Fatal("CreateSession failed")
	}
	c.auth = created.GetAuthenticationToken()
	if !activate {
		return
	}
	token := ua.NewAnonymousIdentityToken(str(userTokenAnonymous))
	response := c.call(ua.NewActivateSessionRequest(c.header(), ua.NewSignatureData(nullString(), byteString(nil)), nil, nil,
		ua.NewBinaryExtensionObjectWithMask(expandedId(token.GetExtensionId()), ua.NewExtensionObjectEncodingMask(false, false, true), token),
		ua.NewSignatureData(nullString(), byteString(nil))))
	if _, ok := response.(ua.ActivateSessionResponse); !ok {
		c.t.Fatalf("ActivateSession: 0x%08X", fault(response))
	}
}

func nodeOf(namespace uint16, name string) ua.NodeId {
	if namespace == 0 {
		id, _ := strconv.Atoi(name)
		return numericNodeId(0, uint32(id))
	}
	return stringNodeId(namespace, name)
}

func (c *rawClient) read(id ua.NodeId) ua.DataValue {
	c.t.Helper()
	response, ok := c.call(ua.NewReadRequest(c.header(), 0, ua.TimestampsToReturn_timestampsToReturnNeither,
		[]ua.ReadValueId{ua.NewReadValueId(id, attrValue, nullString(), ua.NewQualifiedName(0, nullString()))})).(ua.ReadResponse)
	if !ok {
		c.t.Fatal("Read failed")
	}
	return response.GetResults()[0]
}

func TestProtocol(t *testing.T) {
	points := append(testPoints(), Point{Name: "Big", DataType: TypeDouble, ArrayLength: 10000, Writable: true})
	source := newMemory()
	big := make([]float64, 10000)
	for i := range big {
		big[i] = float64(i)
	}
	source.set("Big", big)
	address := startServer(t, Config{Points: points, Source: source})
	c := dialRaw(t, address)
	c.hello()
	c.open()

	// Services need a session, and an activated one.
	if status := fault(c.call(ua.NewReadRequest(c.header(), 0, 0, nil))); status != statusBadSessionIdInvalid {
		t.Errorf("Read without a session: 0x%08X", status)
	}
	c.session(false)
	if status := fault(c.call(ua.NewReadRequest(c.header(), 0, 0, []ua.ReadValueId{ua.NewReadValueId(nodeOf(1, "Tank"), attrValue, nullString(), ua.NewQualifiedName(0, nullString()))}))); status != statusBadSessionNotActivated {
		t.Errorf("Read before ActivateSession: 0x%08X", status)
	}
	c.session(true)
	if status := fault(c.call(ua.NewAddNodesRequest(c.header(), nil))); status != statusBadServiceUnsupported {
		t.Errorf("AddNodes: 0x%08X", status)
	}
	if status := fault(c.call(ua.NewReadRequest(c.header(), 0, 0, nil))); status != statusBadNothingToDo {
		t.Errorf("Read of nothing: 0x%08X", status)
	}

	// A point without a value yet.
	if v := c.read(nodeOf(1, "Tank")); v.GetStatusCode() == nil || v.GetStatusCode().GetStatusCode() != statusBadWaitingForInitialData {
		t.Errorf("Tank: %v", v)
	}

	// A response and a request too large for one chunk.
	if v, _, _ := valueOf(c.read(nodeOf(1, "Big")).GetValue()); !slices.Equal(v.([]float64), big) {
		t.Error("Big read back wrong")
	}
	for i := range big {
		big[i] = -float64(i)
	}
	variant, _ := variantOf(big, TypeDouble)
	c.chunkSize = 8192
	written, ok := c.call(ua.NewWriteRequest(c.header(), []ua.WriteValue{ua.NewWriteValue(nodeOf(1, "Big"), attrValue, nullString(),
		dataValue(variant, statusGood, time.Time{}, ua.TimestampsToReturn_timestampsToReturnNeither))})).(ua.WriteResponse)
	if !ok || written.GetResults()[0].GetStatusCode() != statusGood {
		t.Fatalf("Write of Big: %v", written)
	}
	c.chunkSize = 65535
	if got := source.get("Big").([]float64); got[9999] != -9999 {
		t.Errorf("Big written as ...%v", got[9999])
	}

	// Writes the server refuses.
	scalar, _ := variantOf(1.0, TypeDouble)
	text, _ := variantOf("x", TypeString)
	for _, w := range []struct {
		node    ua.NodeId
		variant ua.Variant
		want    uint32
	}{
		{nodeOf(1, "Line1.Speed"), scalar, statusBadNotWritable},
		{nodeOf(1, "Line1.Count"), text, statusBadTypeMismatch},
		{nodeOf(1, "Line1.Temps"), scalar, statusBadTypeMismatch},
		{nodeOf(1, "Line1.Refused"), scalar, statusBadNotWritable},
		{nodeOf(1, "Nope"), scalar, statusBadNodeIdUnknown},
	} {
		response := c.call(ua.NewWriteRequest(c.header(), []ua.WriteValue{ua.NewWriteValue(w.node, attrValue, nullString(),
			dataValue(w.variant, statusGood, time.Time{}, ua.TimestampsToReturn_timestampsToReturnNeither))})).(ua.WriteResponse)
		if got := response.GetResults()[0].GetStatusCode(); got != w.want {
			t.Errorf("write %s: 0x%08X, want 0x%08X", keyOf(w.node), got, w.want)
		}
	}

	// Browse a page at a time.
	browse := c.call(ua.NewBrowseRequest(c.header(), ua.NewViewDescription(ua.NewNodeId(ua.NewNodeIdTwoByte(0)), 0, 0), 1,
		[]ua.BrowseDescription{ua.NewBrowseDescription(nodeOf(0, "85"), ua.BrowseDirection_browseDirectionForward, nodeOf(0, "33"), true, 0, 63)})).(ua.BrowseResponse)
	result := browse.GetResults()[0]
	names := []string{}
	for _, r := range result.GetReferences() {
		names = append(names, stringOf(r.GetBrowseName().GetName()))
	}
	point := bytesOf(result.GetContinuationPoint())
	if len(names) != 1 || point == nil {
		t.Fatalf("first page %v, continuation %x", names, point)
	}
	next := c.call(ua.NewBrowseNextRequest(c.header(), false, []ua.PascalByteString{byteString(point)})).(ua.BrowseResponse)
	for _, r := range next.GetResults()[0].GetReferences() {
		names = append(names, stringOf(r.GetBrowseName().GetName()))
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"Big", "Line1", "Server", "Tank"}) {
		t.Errorf("Objects has %v", names)
	}
	again := c.call(ua.NewBrowseNextRequest(c.header(), false, []ua.PascalByteString{byteString(point)})).(ua.BrowseResponse)
	if got := again.GetResults()[0].GetStatusCode().GetStatusCode(); got != statusBadContinuationPointInvalid {
		t.Errorf("a used continuation point: 0x%08X", got)
	}

	// Browse paths.
	element := func(name string) ua.RelativePathElement {
		return ua.NewRelativePathElement(nodeOf(0, "33"), true, false, ua.NewQualifiedName(1, str(name)))
	}
	paths := c.call(ua.NewTranslateBrowsePathsToNodeIdsRequest(c.header(), []ua.BrowsePath{
		ua.NewBrowsePath(nodeOf(0, "85"), ua.NewRelativePath([]ua.RelativePathElement{element("Line1"), element("Speed")})),
		ua.NewBrowsePath(nodeOf(0, "85"), ua.NewRelativePath([]ua.RelativePathElement{element("Line2")})),
	})).(ua.TranslateBrowsePathsToNodeIdsResponse)
	if r := paths.GetResults()[0]; r.GetStatusCode().GetStatusCode() != statusGood || len(r.GetTargets()) != 1 ||
		keyOfDefinition(r.GetTargets()[0].GetTargetId().GetNodeId()) != "ns=1;s=Line1.Speed" {
		t.Errorf("Line1/Speed: %v", r)
	}
	if got := paths.GetResults()[1].GetStatusCode().GetStatusCode(); got != statusBadNoMatch {
		t.Errorf("Line2: 0x%08X", got)
	}

	// A subscription: the first value, keep-alives, acknowledgement and republishing.
	created := c.call(ua.NewCreateSubscriptionRequest(c.header(), 50, 30, 3, 0, true, 0)).(ua.CreateSubscriptionResponse)
	sub := created.GetSubscriptionId()
	if created.GetRevisedPublishingInterval() != 50 || created.GetRevisedMaxKeepAliveCount() != 3 {
		t.Errorf("revised %v ms, keep-alive %d", created.GetRevisedPublishingInterval(), created.GetRevisedMaxKeepAliveCount())
	}
	items := c.call(ua.NewCreateMonitoredItemsRequest(c.header(), sub, ua.TimestampsToReturn_timestampsToReturnBoth, []ua.MonitoredItemCreateRequest{
		ua.NewMonitoredItemCreateRequest(ua.NewReadValueId(nodeOf(1, "Line1.Speed"), attrValue, nullString(), ua.NewQualifiedName(0, nullString())),
			ua.MonitoringMode_monitoringModeReporting, ua.NewMonitoringParameters(7, 0, nullExtensionObject(), 10, true)),
		ua.NewMonitoredItemCreateRequest(ua.NewReadValueId(nodeOf(1, "Nope"), attrValue, nullString(), ua.NewQualifiedName(0, nullString())),
			ua.MonitoringMode_monitoringModeReporting, ua.NewMonitoringParameters(8, 0, nullExtensionObject(), 10, true)),
	})).(ua.CreateMonitoredItemsResponse)
	if got := items.GetResults()[1].GetStatusCode().GetStatusCode(); got != statusBadNodeIdUnknown {
		t.Errorf("monitoring Nope: 0x%08X", got)
	}
	publish := func(acks ...ua.SubscriptionAcknowledgement) ua.PublishResponse {
		t.Helper()
		response, ok := c.call(ua.NewPublishRequest(c.header(), acks)).(ua.PublishResponse)
		if !ok {
			t.Fatal("Publish failed")
		}
		return response
	}
	values := func(r ua.PublishResponse) []float64 {
		var out []float64
		for _, data := range r.GetNotificationMessage().GetNotificationData() {
			for _, item := range data.GetBody().(ua.DataChangeNotification).GetMonitoredItems() {
				if item.GetClientHandle() != 7 {
					t.Errorf("client handle %d", item.GetClientHandle())
				}
				v, _, _ := valueOf(item.GetValue().GetValue())
				out = append(out, v.(float64))
			}
		}
		return out
	}
	first := publish()
	if got := values(first); !slices.Equal(got, []float64{12.5}) || first.GetNotificationMessage().GetSequenceNumber() != 1 {
		t.Fatalf("first notification %v, sequence %d", got, first.GetNotificationMessage().GetSequenceNumber())
	}
	start := time.Now()
	keepAlive := publish()
	if len(keepAlive.GetNotificationMessage().GetNotificationData()) != 0 || !slices.Equal(keepAlive.GetAvailableSequenceNumbers(), []uint32{1}) {
		t.Errorf("keep-alive %v, available %v", keepAlive.GetNotificationMessage(), keepAlive.GetAvailableSequenceNumbers())
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Errorf("keep-alive after %v, before 3 intervals", elapsed)
	}
	republished, ok := c.call(ua.NewRepublishRequest(c.header(), sub, 1)).(ua.RepublishResponse)
	if !ok || republished.GetNotificationMessage().GetSequenceNumber() != 1 {
		t.Errorf("Republish 1: %v", republished)
	}
	source.set("Line1.Speed", 50.0)
	second := publish(ua.NewSubscriptionAcknowledgement(sub, 1), ua.NewSubscriptionAcknowledgement(sub, 9))
	if got := values(second); !slices.Equal(got, []float64{50}) || second.GetNotificationMessage().GetSequenceNumber() != 2 {
		t.Errorf("second notification %v", got)
	}
	if results := second.GetResults(); len(results) != 2 || results[0].GetStatusCode() != statusGood || results[1].GetStatusCode() != statusBadSequenceNumberUnknown {
		t.Errorf("acknowledgement results %v", results)
	}
	if status := fault(c.call(ua.NewRepublishRequest(c.header(), sub, 1))); status != statusBadMessageNotAvailable {
		t.Errorf("Republish of an acknowledged message: 0x%08X", status)
	}

	// A Publish waiting when the subscription goes is answered.
	waiting := c.send(ua.NewPublishRequest(c.header(), nil))
	deleted := c.call(ua.NewDeleteSubscriptionsRequest(c.header(), []uint32{sub, 999})).(ua.DeleteSubscriptionsResponse)
	if got := deleted.GetResults()[1].GetStatusCode(); got != statusBadSubscriptionIdInvalid {
		t.Errorf("deleting 999: 0x%08X", got)
	}
	if status := fault(c.receive(waiting)); status != statusBadNoSubscription {
		t.Errorf("waiting Publish: 0x%08X", status)
	}
	if status := fault(c.call(ua.NewPublishRequest(c.header(), nil))); status != statusBadNoSubscription {
		t.Errorf("Publish without subscriptions: 0x%08X", status)
	}

	// After CloseSession the session is gone.
	if _, ok := c.call(ua.NewCloseSessionRequest(c.header(), true)).(ua.CloseSessionResponse); !ok {
		t.Error("CloseSession failed")
	}
	if status := fault(c.call(ua.NewReadRequest(c.header(), 0, 0, nil))); status != statusBadSessionIdInvalid {
		t.Errorf("Read after CloseSession: 0x%08X", status)
	}
}

func TestProtocolErrors(t *testing.T) {
	address := startServer(t, Config{Points: testPoints(), Source: newMemory()})

	// The first message must be Hello.
	c := dialRaw(t, address)
	c.sendOpen()
	errMessage := c.readChunk()
	if string(errMessage[:4]) != "ERRF" || binary.LittleEndian.Uint32(errMessage[8:]) != statusBadTcpMessageTypeInvalid {
		t.Errorf("answer to OPN before Hello: %q", errMessage)
	}

	// A message on another channel.
	c = dialRaw(t, address)
	c.hello()
	c.open()
	c.channel++
	c.send(ua.NewReadRequest(c.header(), 0, 0, nil))
	errMessage = c.readChunk()
	if string(errMessage[:4]) != "ERRF" || binary.LittleEndian.Uint32(errMessage[8:]) != statusBadSecureChannelIdInvalid {
		t.Errorf("answer to a message on another channel: %q", errMessage)
	}

	// A server without a None endpoint still lets a None channel find its endpoints.
	certificate, key := identity(t, "urn:apiarytech:honeycomb:test")
	secured := startServer(t, Config{ApplicationURI: "urn:apiarytech:honeycomb:test", Points: testPoints(), Source: newMemory(),
		Endpoints: []Endpoint{{Policy: "Basic256Sha256", Mode: "SignAndEncrypt"}}, Certificate: certificate, Key: key,
		TrustClient: func([]*x509.Certificate) error { return nil }})
	c = dialRaw(t, secured)
	c.hello()
	c.open()
	endpoints, ok := c.call(ua.NewGetEndpointsRequest(c.header(), str("opc.tcp://test"), nil, nil)).(ua.GetEndpointsResponse)
	if !ok || len(endpoints.GetEndpoints()) != 1 || endpoints.GetEndpoints()[0].GetSecurityMode() != ua.MessageSecurityMode_messageSecurityModeSignAndEncrypt {
		t.Errorf("endpoints %v", endpoints)
	}
	app := ua.NewApplicationDescription(str("urn:test"), str("urn:test"), text("test"), ua.ApplicationType_applicationTypeClient, nullString(), nullString(), nil)
	if status := fault(c.call(ua.NewCreateSessionRequest(c.header(), app, nullString(), str("opc.tcp://test"), str("raw"),
		byteString(randomBytes(32)), byteString(nil), 60000, 0))); status != statusBadSecurityPolicyRejected {
		t.Errorf("CreateSession on a discovery channel: 0x%08X", status)
	}
}
