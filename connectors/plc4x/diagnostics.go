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
	"fmt"
	"strings"

	"github.com/apiarytech/honeycomb"
	plc "github.com/apiarytech/royaljelly/iec"
)

// diagnosticField is one Status field published as a tag.
type diagnosticField struct {
	name     string
	dataType honeycomb.DataType
	value    func(Status) any
}

var diagnosticFields = []diagnosticField{
	{"Connected", honeycomb.TypeBOOL, func(s Status) any { return plc.BOOL(s.Connected) }},
	{"Subscribed", honeycomb.TypeBOOL, func(s Status) any { return plc.BOOL(s.Subscribed) }},
	{"Reads", honeycomb.TypeULINT, func(s Status) any { return plc.ULINT(s.Reads) }},
	{"Writes", honeycomb.TypeULINT, func(s Status) any { return plc.ULINT(s.Writes) }},
	{"Errors", honeycomb.TypeULINT, func(s Status) any { return plc.ULINT(s.Errors) }},
	{"LastError", honeycomb.TypeSTRING, func(s Status) any { return plc.STRING(errorText(s.LastError)) }},
	{"LastRead", honeycomb.TypeDT, func(s Status) any { return plc.DT(s.LastRead) }},
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	// Joined errors span several lines; a STRING tag is shown on one.
	return strings.ReplaceAll(err.Error(), "\n", "; ")
}

func (c *Connector) diagnosticTag(connection, field string) string {
	return c.diagPrefix + connection + "." + field
}

// createDiagnostics adds the diagnostic tags that do not exist yet and
// publishes the initial (disconnected) status.
func (c *Connector) createDiagnostics() error {
	if c.diagPrefix == "" {
		return nil
	}
	for _, cs := range c.connections {
		for _, f := range diagnosticFields {
			name := c.diagnosticTag(cs.Name, f.name)
			if _, exists := c.db.GetTag(name); exists {
				continue
			}
			tag := &honeycomb.Tag{
				Name:        name,
				TypeInfo:    &honeycomb.TypeInfo{DataType: f.dataType},
				Value:       f.value(Status{}),
				Description: fmt.Sprintf("PLC4X connection %q: %s", cs.Name, f.name),
			}
			if err := c.db.AddTag(tag); err != nil {
				return fmt.Errorf("plc4x: create diagnostic tag: %w", err)
			}
		}
		c.publishDiagnostics(cs.Name)
	}
	return nil
}

// publishDiagnostics writes the fields of a connection's Status that changed
// since they were last published.
func (c *Connector) publishDiagnostics(connection string) {
	if c.diagPrefix == "" {
		return
	}
	c.diagMu.Lock()
	defer c.diagMu.Unlock()

	// Read the status under diagMu so concurrent publishers cannot write an
	// older status after a newer one.
	s, _ := c.Status(connection)
	last, published := c.published[connection]
	for _, f := range diagnosticFields {
		value := f.value(s)
		if published && f.value(last) == value {
			continue
		}
		if err := c.db.SetTagValue(c.diagnosticTag(connection, f.name), value); err != nil {
			// Not recorded: recording would publish again.
			c.onError(connection, fmt.Errorf("diagnostic tag %s: %w", f.name, err))
		}
	}
	c.published[connection] = s
}
