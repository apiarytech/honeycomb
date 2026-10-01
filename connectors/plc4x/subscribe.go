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
	"errors"
	"fmt"
	"sync/atomic"

	plc4go "github.com/apache/plc4x/plc4go/pkg/api"
	apiModel "github.com/apache/plc4x/plc4go/pkg/api/model"
)

// subscribe registers the connection's inputs with the device. Events are
// stored while active is set; the session clears it when it ends.
func (c *Connector) subscribe(ctx context.Context, plcConn plc4go.PlcConnection, cs *connState, active *atomic.Bool) error {
	consumer := func(event apiModel.PlcSubscriptionEvent) {
		if active.Load() {
			c.handleEvent(cs, event)
		}
	}
	builder := plcConn.SubscriptionRequestBuilder()
	for _, b := range cs.Bindings {
		if !b.Direction.reads() {
			continue
		}
		if cs.Mode == Cyclic {
			builder.AddCyclicTagAddress(b.Tag, b.Address, cs.Interval)
		} else {
			builder.AddChangeOfStateTagAddress(b.Tag, b.Address)
		}
		builder.AddPreRegisteredConsumer(b.Tag, consumer)
	}
	request, err := builder.Build()
	if err != nil {
		return fmt.Errorf("build subscription request: %w", err)
	}

	var result apiModel.PlcSubscriptionRequestResult
	select {
	case result = <-request.Execute(ctx):
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := result.GetErr(); err != nil {
		return err
	}

	// A tag the device refused keeps the subscription for the others.
	response := result.GetResponse()
	var errs []error
	for _, name := range response.GetTagNames() {
		if code := response.GetResponseCode(name); code != apiModel.PlcResponseCode_OK {
			_ = c.receive(cs, name, code, nil) // sets the tag's quality from the code
			errs = append(errs, fmt.Errorf("tag '%s': device refused subscription: %s", name, code))
		}
	}
	if err := errors.Join(errs...); err != nil {
		c.record(cs.Name, nil, err)
	}
	return nil
}

// handleEvent stores the values reported by a subscription event.
func (c *Connector) handleEvent(cs *connState, event apiModel.PlcSubscriptionEvent) {
	var errs []error
	for _, name := range event.GetTagNames() {
		if b, bound := cs.byTag[name]; !bound || !b.Direction.reads() {
			continue
		}
		if err := c.receive(cs, name, event.GetResponseCode(name), event.GetValue(name)); err != nil {
			errs = append(errs, err)
		}
	}
	c.recordRead(cs.Name, errors.Join(errs...))
}
