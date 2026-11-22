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
	"maps"
	"math"
	"reflect"
	"slices"
	"sync"
	"time"

	ua "github.com/apache/plc4x/plc4go/protocols/opcua/readwrite/model"
)

// Bounds of subscriptions (OPC UA Part 4, 5.13).
const (
	publishingIntervalMin = 50 * time.Millisecond
	publishingIntervalMax = time.Hour
	publishingDefault     = time.Second
	keepAliveDefault      = 10
	retransmitQueue       = 64
	itemQueueMax          = 100
)

// subscription samples its monitored items every publishing interval and sends what changed in
// the answer to a waiting Publish request, or a keep-alive when nothing did for a while.
// Everything in it is guarded by its session's mutex.
type subscription struct {
	s  *session
	id uint32

	interval         time.Duration
	lifetime         uint32
	keepAlive        uint32
	maxNotifications uint32
	enabled          bool

	items    map[uint32]*monitoredItem
	nextItem uint32

	sequence       uint32 // of the next notification message
	retransmit     []ua.NotificationMessage
	keepAliveCount uint32
	lifetimeCount  uint32

	ticker   *time.Ticker
	kick     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

// monitoredItem watches the Value of a variable.
type monitoredItem struct {
	id            uint32
	node          *node
	clientHandle  uint32
	mode          ua.MonitoringMode
	queueSize     int
	discardOldest bool
	timestamps    ua.TimestampsToReturn
	trigger       ua.DataChangeTrigger
	deadband      float64 // absolute; 0 for none

	sampled bool
	last    sample
	lastRaw []byte
	queue   []ua.DataValue
}

func (sub *subscription) stop() {
	sub.stopOnce.Do(func() {
		close(sub.done)
		sub.ticker.Stop()
	})
}

// revise sets a subscription's timing from what the client asked for.
func (sub *subscription) revise(interval float64, lifetime, keepAlive, maxNotifications uint32) {
	sub.interval = time.Duration(interval * float64(time.Millisecond))
	if interval <= 0 || math.IsNaN(interval) {
		sub.interval = publishingDefault
	}
	sub.interval = min(max(sub.interval, publishingIntervalMin), publishingIntervalMax)
	if keepAlive == 0 {
		keepAlive = keepAliveDefault
	}
	sub.keepAlive = min(keepAlive, uint32(publishingIntervalMax/sub.interval)+1)
	sub.lifetime = max(lifetime, 3*sub.keepAlive)
	sub.maxNotifications = maxNotifications
}

func (c *conn) subscriptionOf(s *session, id uint32) *subscription {
	if s.subscriptions == nil {
		return nil
	}
	return s.subscriptions[id]
}

func (c *conn) createSubscription(s *session, request ua.CreateSubscriptionRequest) ua.ExtensionObjectDefinition {
	header := request.GetRequestHeader()
	s.mutex.Lock()
	if len(s.subscriptions) >= maxSubscriptions || s.closed {
		s.mutex.Unlock()
		return c.fault(header, statusBadTooManySubscriptions)
	}
	sub := &subscription{s: s, id: c.s.subscriptionIds.Add(1), enabled: request.GetPublishingEnabled(),
		items: map[uint32]*monitoredItem{}, sequence: 1, kick: make(chan struct{}, 1), done: make(chan struct{})}
	sub.revise(request.GetRequestedPublishingInterval(), request.GetRequestedLifetimeCount(), request.GetRequestedMaxKeepAliveCount(), request.GetMaxNotificationsPerPublish())
	sub.ticker = time.NewTicker(sub.interval)
	s.subscriptions[sub.id] = sub
	response := ua.NewCreateSubscriptionResponse(c.responseHeader(header, statusGood), sub.id,
		float64(sub.interval)/float64(time.Millisecond), sub.lifetime, sub.keepAlive)
	s.mutex.Unlock()
	go c.runSubscription(sub)
	return response
}

func (c *conn) modifySubscription(s *session, request ua.ModifySubscriptionRequest) ua.ExtensionObjectDefinition {
	header := request.GetRequestHeader()
	s.mutex.Lock()
	defer s.mutex.Unlock()
	sub := c.subscriptionOf(s, request.GetSubscriptionId())
	if sub == nil {
		return c.fault(header, statusBadSubscriptionIdInvalid)
	}
	sub.revise(request.GetRequestedPublishingInterval(), request.GetRequestedLifetimeCount(), request.GetRequestedMaxKeepAliveCount(), request.GetMaxNotificationsPerPublish())
	sub.ticker.Reset(sub.interval)
	sub.lifetimeCount = 0
	return ua.NewModifySubscriptionResponse(c.responseHeader(header, statusGood), float64(sub.interval)/float64(time.Millisecond), sub.lifetime, sub.keepAlive)
}

func (c *conn) setPublishingMode(s *session, request ua.SetPublishingModeRequest) ua.ExtensionObjectDefinition {
	header := request.GetRequestHeader()
	ids := request.GetSubscriptionIds()
	if status := tooMany(len(ids)); status != statusGood {
		return c.fault(header, status)
	}
	s.mutex.Lock()
	defer s.mutex.Unlock()
	results := make([]uint32, len(ids))
	for i, id := range ids {
		if sub := c.subscriptionOf(s, id); sub != nil {
			sub.enabled = request.GetPublishingEnabled()
			sub.lifetimeCount = 0
		} else {
			results[i] = statusBadSubscriptionIdInvalid
		}
	}
	return ua.NewSetPublishingModeResponse(c.responseHeader(header, statusGood), statusCodes(results), nil)
}

func (c *conn) deleteSubscriptions(s *session, request ua.DeleteSubscriptionsRequest) ua.ExtensionObjectDefinition {
	header := request.GetRequestHeader()
	ids := request.GetSubscriptionIds()
	if status := tooMany(len(ids)); status != statusGood {
		return c.fault(header, status)
	}
	s.mutex.Lock()
	results := make([]uint32, len(ids))
	for i, id := range ids {
		if sub := c.subscriptionOf(s, id); sub != nil {
			sub.stop()
			delete(s.subscriptions, id)
		} else {
			results[i] = statusBadSubscriptionIdInvalid
		}
	}
	orphans := c.orphanedPublishes(s)
	s.mutex.Unlock()
	c.answerOrphans(orphans)
	return ua.NewDeleteSubscriptionsResponse(c.responseHeader(header, statusGood), statusCodes(results), nil)
}

// orphanedPublishes takes the waiting Publish requests of a session left without
// subscriptions, to answer with BadNoSubscription.
func (c *conn) orphanedPublishes(s *session) []publishRequest {
	if len(s.subscriptions) > 0 {
		return nil
	}
	orphans := s.publishes
	s.publishes = nil
	return orphans
}

func (c *conn) answerOrphans(orphans []publishRequest) {
	for _, p := range orphans {
		_ = c.respond(p.requestId, c.fault(handleHeader(p.requestHandle), statusBadNoSubscription))
	}
}

// handleHeader is a request header carrying only a request handle, for answering a request
// whose header is gone.
func handleHeader(handle uint32) ua.RequestHeader {
	return ua.NewRequestHeader(ua.NewNodeId(ua.NewNodeIdTwoByte(0)), 0, handle, 0, nullString(), 0, nullExtensionObject())
}

// filterOf reads a monitored item's filter: none, or a DataChangeFilter without a deadband or
// with an absolute one.
func filterOf(object ua.ExtensionObject) (ua.DataChangeTrigger, float64, uint32) {
	trigger := ua.DataChangeTrigger_dataChangeTriggerStatusValue
	if object == nil || object.GetBody() == nil {
		return trigger, 0, statusGood
	}
	filter, ok := object.GetBody().(ua.DataChangeFilter)
	if !ok {
		return trigger, 0, statusBadMonitoredItemFilterUnsupported
	}
	switch filter.GetDeadbandType() {
	case 0:
		return filter.GetTrigger(), 0, statusGood
	case 1:
		if filter.GetDeadbandValue() < 0 {
			return trigger, 0, statusBadMonitoredItemFilterUnsupported
		}
		return filter.GetTrigger(), filter.GetDeadbandValue(), statusGood
	}
	return trigger, 0, statusBadMonitoredItemFilterUnsupported
}

func queueSizeOf(requested uint32) int {
	return int(min(max(requested, 1), itemQueueMax))
}

func (c *conn) createMonitoredItems(s *session, request ua.CreateMonitoredItemsRequest) ua.ExtensionObjectDefinition {
	header := request.GetRequestHeader()
	items := request.GetItemsToCreate()
	if status := tooMany(len(items)); status != statusGood {
		return c.fault(header, status)
	}
	timestamps := request.GetTimestampsToReturn()
	if timestamps > ua.TimestampsToReturn_timestampsToReturnNeither {
		return c.fault(header, statusBadTimestampsToReturnInvalid)
	}
	s.mutex.Lock()
	defer s.mutex.Unlock()
	sub := c.subscriptionOf(s, request.GetSubscriptionId())
	if sub == nil {
		return c.fault(header, statusBadSubscriptionIdInvalid)
	}
	results := make([]ua.MonitoredItemCreateResult, len(items))
	interval := float64(sub.interval) / float64(time.Millisecond)
	for i, item := range items {
		failed := func(status uint32) ua.MonitoredItemCreateResult {
			return ua.NewMonitoredItemCreateResult(ua.NewStatusCode(status), 0, 0, 0, nullExtensionObject())
		}
		target := item.GetItemToMonitor()
		n := c.s.space.nodes[keyOf(target.GetNodeId())]
		parameters := item.GetRequestedParameters()
		trigger, deadband, filterStatus := filterOf(parameters.GetFilter())
		switch {
		case n == nil:
			results[i] = failed(statusBadNodeIdUnknown)
		case target.GetAttributeId() != attrValue || n.class != ua.NodeClass_nodeClassVariable:
			results[i] = failed(statusBadAttributeIdInvalid)
		case stringOf(target.GetIndexRange()) != "":
			results[i] = failed(statusBadIndexRangeInvalid)
		case item.GetMonitoringMode() > ua.MonitoringMode_monitoringModeReporting:
			results[i] = failed(statusBadMonitoringModeInvalid)
		case filterStatus != statusGood:
			results[i] = failed(filterStatus)
		case len(sub.items) >= maxMonitoredItems:
			results[i] = failed(statusBadTooManyMonitoredItems)
		default:
			sub.nextItem++
			m := &monitoredItem{id: sub.nextItem, node: n, clientHandle: parameters.GetClientHandle(), mode: item.GetMonitoringMode(),
				queueSize: queueSizeOf(parameters.GetQueueSize()), discardOldest: parameters.GetDiscardOldest(), timestamps: timestamps,
				trigger: trigger, deadband: deadband}
			sub.items[m.id] = m
			results[i] = ua.NewMonitoredItemCreateResult(ua.NewStatusCode(statusGood), m.id, interval, uint32(m.queueSize), nullExtensionObject())
		}
	}
	return ua.NewCreateMonitoredItemsResponse(c.responseHeader(header, statusGood), results, nil)
}

func (c *conn) modifyMonitoredItems(s *session, request ua.ModifyMonitoredItemsRequest) ua.ExtensionObjectDefinition {
	header := request.GetRequestHeader()
	items := request.GetItemsToModify()
	if status := tooMany(len(items)); status != statusGood {
		return c.fault(header, status)
	}
	if request.GetTimestampsToReturn() > ua.TimestampsToReturn_timestampsToReturnNeither {
		return c.fault(header, statusBadTimestampsToReturnInvalid)
	}
	s.mutex.Lock()
	defer s.mutex.Unlock()
	sub := c.subscriptionOf(s, request.GetSubscriptionId())
	if sub == nil {
		return c.fault(header, statusBadSubscriptionIdInvalid)
	}
	interval := float64(sub.interval) / float64(time.Millisecond)
	results := make([]ua.MonitoredItemModifyResult, len(items))
	for i, item := range items {
		m := sub.items[item.GetMonitoredItemId()]
		parameters := item.GetRequestedParameters()
		trigger, deadband, filterStatus := filterOf(parameters.GetFilter())
		switch {
		case m == nil:
			results[i] = ua.NewMonitoredItemModifyResult(ua.NewStatusCode(statusBadMonitoredItemIdInvalid), 0, 0, nullExtensionObject())
		case filterStatus != statusGood:
			results[i] = ua.NewMonitoredItemModifyResult(ua.NewStatusCode(filterStatus), 0, 0, nullExtensionObject())
		default:
			m.clientHandle, m.discardOldest, m.timestamps = parameters.GetClientHandle(), parameters.GetDiscardOldest(), request.GetTimestampsToReturn()
			m.trigger, m.deadband = trigger, deadband
			m.queueSize = queueSizeOf(parameters.GetQueueSize())
			if len(m.queue) > m.queueSize {
				m.queue = m.queue[len(m.queue)-m.queueSize:]
			}
			results[i] = ua.NewMonitoredItemModifyResult(ua.NewStatusCode(statusGood), interval, uint32(m.queueSize), nullExtensionObject())
		}
	}
	return ua.NewModifyMonitoredItemsResponse(c.responseHeader(header, statusGood), results, nil)
}

func (c *conn) setMonitoringMode(s *session, request ua.SetMonitoringModeRequest) ua.ExtensionObjectDefinition {
	header := request.GetRequestHeader()
	ids := request.GetMonitoredItemIds()
	if status := tooMany(len(ids)); status != statusGood {
		return c.fault(header, status)
	}
	if request.GetMonitoringMode() > ua.MonitoringMode_monitoringModeReporting {
		return c.fault(header, statusBadMonitoringModeInvalid)
	}
	s.mutex.Lock()
	defer s.mutex.Unlock()
	sub := c.subscriptionOf(s, request.GetSubscriptionId())
	if sub == nil {
		return c.fault(header, statusBadSubscriptionIdInvalid)
	}
	results := make([]uint32, len(ids))
	for i, id := range ids {
		m := sub.items[id]
		if m == nil {
			results[i] = statusBadMonitoredItemIdInvalid
			continue
		}
		m.mode = request.GetMonitoringMode()
		if m.mode == ua.MonitoringMode_monitoringModeDisabled {
			m.sampled, m.lastRaw, m.queue = false, nil, nil
		}
	}
	return ua.NewSetMonitoringModeResponse(c.responseHeader(header, statusGood), statusCodes(results), nil)
}

func (c *conn) deleteMonitoredItems(s *session, request ua.DeleteMonitoredItemsRequest) ua.ExtensionObjectDefinition {
	header := request.GetRequestHeader()
	ids := request.GetMonitoredItemIds()
	if status := tooMany(len(ids)); status != statusGood {
		return c.fault(header, status)
	}
	s.mutex.Lock()
	defer s.mutex.Unlock()
	sub := c.subscriptionOf(s, request.GetSubscriptionId())
	if sub == nil {
		return c.fault(header, statusBadSubscriptionIdInvalid)
	}
	results := make([]uint32, len(ids))
	for i, id := range ids {
		if _, ok := sub.items[id]; ok {
			delete(sub.items, id)
		} else {
			results[i] = statusBadMonitoredItemIdInvalid
		}
	}
	return ua.NewDeleteMonitoredItemsResponse(c.responseHeader(header, statusGood), statusCodes(results), nil)
}

// publish takes a Publish request: it acknowledges what the client received and waits for a
// subscription to have something to send.
func (c *conn) publish(s *session, requestId uint32, request ua.PublishRequest) ua.ExtensionObjectDefinition {
	header := request.GetRequestHeader()
	s.mutex.Lock()
	if len(s.subscriptions) == 0 {
		s.mutex.Unlock()
		return c.fault(header, statusBadNoSubscription)
	}
	acks := request.GetSubscriptionAcknowledgements()
	results := make([]uint32, len(acks))
	for i, ack := range acks {
		sub := c.subscriptionOf(s, ack.GetSubscriptionId())
		if sub == nil {
			results[i] = statusBadSubscriptionIdInvalid
			continue
		}
		index := slices.IndexFunc(sub.retransmit, func(m ua.NotificationMessage) bool { return m.GetSequenceNumber() == ack.GetSequenceNumber() })
		if index < 0 {
			results[i] = statusBadSequenceNumberUnknown
			continue
		}
		sub.retransmit = slices.Delete(sub.retransmit, index, index+1)
	}
	p := publishRequest{requestId: requestId, requestHandle: header.GetRequestHandle(), received: time.Now(),
		timeout: time.Duration(header.GetTimeoutHint()) * time.Millisecond, results: results}
	var overflow []publishRequest
	s.publishes = append(s.publishes, p)
	if len(s.publishes) > maxPublishRequests {
		overflow = append(overflow, s.publishes[0])
		s.publishes = s.publishes[1:]
	}
	for _, sub := range s.subscriptions {
		select {
		case sub.kick <- struct{}{}:
		default:
		}
	}
	s.mutex.Unlock()
	for _, o := range overflow {
		_ = c.respond(o.requestId, c.fault(handleHeader(o.requestHandle), statusBadTooManyPublishRequests))
	}
	return nil
}

func (c *conn) republish(s *session, request ua.RepublishRequest) ua.ExtensionObjectDefinition {
	header := request.GetRequestHeader()
	s.mutex.Lock()
	defer s.mutex.Unlock()
	sub := c.subscriptionOf(s, request.GetSubscriptionId())
	if sub == nil {
		return c.fault(header, statusBadSubscriptionIdInvalid)
	}
	for _, m := range sub.retransmit {
		if m.GetSequenceNumber() == request.GetRetransmitSequenceNumber() {
			return ua.NewRepublishResponse(c.responseHeader(header, statusGood), m)
		}
	}
	return c.fault(header, statusBadMessageNotAvailable)
}

// runSubscription samples on every tick and publishes when the client has asked.
func (c *conn) runSubscription(sub *subscription) {
	for {
		sampling := false
		select {
		case <-sub.done:
			return
		case <-c.ctx.Done():
			return
		case <-sub.ticker.C:
			sampling = true
		case <-sub.kick:
		}
		requestId, response := c.cycle(sub, sampling)
		if response != nil {
			_ = c.respond(requestId, response)
		}
	}
}

// cycle samples the items (on a tick) and, when there is something to send and a Publish
// request waiting, makes its response.
func (c *conn) cycle(sub *subscription, sampling bool) (uint32, ua.ExtensionObjectDefinition) {
	s := sub.s
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if s.closed || s.subscriptions[sub.id] != sub {
		return 0, nil
	}
	if sampling {
		for _, m := range sub.items {
			if m.mode != ua.MonitoringMode_monitoringModeDisabled {
				c.s.sampleItem(m)
			}
		}
	}
	var notifications []ua.MonitoredItemNotification
	more := false
	if sub.enabled {
		for _, id := range slices.Sorted(maps.Keys(sub.items)) {
			m := sub.items[id]
			if m.mode != ua.MonitoringMode_monitoringModeReporting {
				continue
			}
			for len(m.queue) > 0 {
				if sub.maxNotifications != 0 && uint32(len(notifications)) >= sub.maxNotifications {
					more = true
					break
				}
				notifications = append(notifications, ua.NewMonitoredItemNotification(m.clientHandle, m.queue[0]))
				m.queue = m.queue[1:]
			}
		}
	}
	keepAliveDue := false
	if len(notifications) == 0 {
		if sampling {
			sub.keepAliveCount++
		}
		keepAliveDue = sub.keepAliveCount >= sub.keepAlive
		if !keepAliveDue {
			return 0, nil
		}
	}
	if len(s.publishes) == 0 {
		if len(notifications) > 0 {
			sub.requeue(notifications)
		}
		if sampling {
			sub.lifetimeCount++
			if sub.lifetimeCount >= sub.lifetime {
				sub.stop()
				delete(s.subscriptions, sub.id)
			}
		}
		return 0, nil
	}
	p := s.publishes[0]
	s.publishes = s.publishes[1:]
	sub.keepAliveCount, sub.lifetimeCount = 0, 0
	now := dateTime(time.Now())
	var message ua.NotificationMessage
	if len(notifications) == 0 {
		message = ua.NewNotificationMessage(sub.sequence, now, nil) // a keep-alive does not use its number
	} else {
		data := ua.NewDataChangeNotification(notifications, nil)
		message = ua.NewNotificationMessage(sub.sequence, now, []ua.ExtensionObject{
			ua.NewBinaryExtensionObjectWithMask(expandedId(data.GetExtensionId()), ua.NewExtensionObjectEncodingMask(false, false, true), data),
		})
		sub.sequence++
		if sub.sequence == 0 {
			sub.sequence = 1
		}
		sub.retransmit = append(sub.retransmit, message)
		if len(sub.retransmit) > retransmitQueue {
			sub.retransmit = sub.retransmit[1:]
		}
	}
	available := make([]uint32, len(sub.retransmit))
	for i, m := range sub.retransmit {
		available[i] = m.GetSequenceNumber()
	}
	return p.requestId, ua.NewPublishResponse(c.responseHeader(handleHeader(p.requestHandle), statusGood), sub.id, available, more,
		message, statusCodes(p.results), nil)
}

// requeue puts notifications that found no Publish request back at the front of their items'
// queues.
func (sub *subscription) requeue(notifications []ua.MonitoredItemNotification) {
	byHandle := map[uint32][]ua.DataValue{}
	for _, n := range notifications {
		byHandle[n.GetClientHandle()] = append(byHandle[n.GetClientHandle()], n.GetValue())
	}
	for _, m := range sub.items {
		if values, ok := byHandle[m.clientHandle]; ok {
			m.queue = append(values, m.queue...)
			delete(byHandle, m.clientHandle)
		}
	}
}

// sampleItem reads an item's variable and queues the value when it changed as the item's
// trigger and deadband say.
func (s *Server) sampleItem(m *monitoredItem) {
	got := s.sample(m.node)
	var raw []byte
	if got.variant != nil {
		raw, _ = got.variant.Serialize()
	}
	if m.sampled && !m.changed(got, raw) {
		return
	}
	m.sampled, m.last, m.lastRaw = true, got, raw
	if m.mode == ua.MonitoringMode_monitoringModeDisabled {
		return
	}
	value := statusValue(got.status)
	if got.variant != nil {
		value = dataValue(got.variant, got.status, got.time, m.timestamps)
	}
	if len(m.queue) >= m.queueSize {
		if m.discardOldest {
			m.queue = m.queue[1:]
		} else {
			m.queue = m.queue[:len(m.queue)-1]
		}
	}
	m.queue = append(m.queue, value)
}

func (m *monitoredItem) changed(got sample, raw []byte) bool {
	if got.status != m.last.status {
		return true
	}
	if m.trigger == ua.DataChangeTrigger_dataChangeTriggerStatus {
		return false
	}
	if m.trigger == ua.DataChangeTrigger_dataChangeTriggerStatusValueTimestamp && !got.time.Equal(m.last.time) {
		return true
	}
	if bytes.Equal(raw, m.lastRaw) {
		return false
	}
	if m.deadband > 0 {
		if a, b, ok := numbers(got.variant, m.last.variant); ok {
			for i := range a {
				if math.Abs(a[i]-b[i]) > m.deadband {
					return true
				}
			}
			return false
		}
	}
	return true
}

// numbers are two Variants' elements as numbers, when both are numeric and of one length.
func numbers(x, y ua.Variant) ([]float64, []float64, bool) {
	list := func(v ua.Variant) ([]float64, bool) {
		value, t, err := valueOf(v)
		if err != nil || t < TypeSByte || t > TypeDouble {
			return nil, false
		}
		elems, _ := elements(value)
		out := make([]float64, len(elems))
		for i, e := range elems {
			switch e.Kind() {
			case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
				out[i] = float64(e.Int())
			case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
				out[i] = float64(e.Uint())
			default:
				out[i] = e.Float()
			}
		}
		return out, true
	}
	a, okA := list(x)
	b, okB := list(y)
	return a, b, okA && okB && len(a) == len(b)
}
