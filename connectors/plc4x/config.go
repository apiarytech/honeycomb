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
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/apiarytech/honeycomb"
)

// Config is a set of connections loaded from a file. Its JSON form is:
//
//	{
//	  "diagnostics": "PLC4X.",
//	  "connections": [{
//	    "name": "press1",
//	    "url": "modbus-tcp://10.0.0.5:502",
//	    "interval": "100ms",
//	    "mode": "poll",
//	    "bindings": [
//	      {"tag": "Press1.Pressure", "address": "holding-register:1:REAL"},
//	      {"tag": "Press1.Valve", "address": "coil:1:BOOL", "direction": "output"}
//	    ]
//	  }]
//	}
//
// "interval" is a Go duration ("100ms", "2s"). "mode" is "poll" (default),
// "change-of-state" or "cyclic". "direction" is "input" (default), "output" or
// "inout". "diagnostics" is the WithDiagnostics prefix; leave it out for none.
type Config struct {
	Diagnostics string
	Connections []Connection
}

// LoadConfig reads a Config from a JSON file.
func LoadConfig(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("plc4x: load config: %w", err)
	}
	defer f.Close()
	cfg, err := ParseConfig(f)
	if err != nil {
		return Config{}, fmt.Errorf("plc4x: load config %s: %w", path, err)
	}
	return cfg, nil
}

// ParseConfig reads a Config in the JSON form described on Config. Unknown keys
// are rejected, so a misspelled key fails at startup instead of being ignored.
func ParseConfig(r io.Reader) (Config, error) {
	var file struct {
		Diagnostics string `json:"diagnostics"`
		Connections []struct {
			Name     string `json:"name"`
			URL      string `json:"url"`
			Interval string `json:"interval"`
			Mode     Mode   `json:"mode"`
			Bindings []struct {
				Tag       string    `json:"tag"`
				Address   string    `json:"address"`
				Direction Direction `json:"direction"`
			} `json:"bindings"`
		} `json:"connections"`
	}
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&file); err != nil {
		return Config{}, err
	}

	cfg := Config{Diagnostics: file.Diagnostics}
	for _, fc := range file.Connections {
		conn := Connection{Name: fc.Name, URL: fc.URL, Mode: fc.Mode}
		if fc.Interval != "" {
			interval, err := time.ParseDuration(fc.Interval)
			if err != nil {
				return Config{}, fmt.Errorf("connection %q: interval: %w", fc.Name, err)
			}
			conn.Interval = interval
		}
		for _, fb := range fc.Bindings {
			conn.Bindings = append(conn.Bindings, Binding{Tag: fb.Tag, Address: fb.Address, Direction: fb.Direction})
		}
		cfg.Connections = append(cfg.Connections, conn)
	}
	return cfg, nil
}

// NewFromConfig creates a Connector from a Config. Options given here are
// applied after the Config's, so they take precedence.
func NewFromConfig(db *honeycomb.TagDatabase, cfg Config, opts ...Option) (*Connector, error) {
	if cfg.Diagnostics != "" {
		opts = append([]Option{WithDiagnostics(cfg.Diagnostics)}, opts...)
	}
	return New(db, cfg.Connections, opts...)
}

var directionNames = []string{Input: "input", Output: "output", InOut: "inout"}

func (d Direction) String() string {
	if int(d) < len(directionNames) {
		return directionNames[d]
	}
	return fmt.Sprintf("Direction(%d)", uint8(d))
}

// MarshalText implements encoding.TextMarshaler.
func (d Direction) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// UnmarshalText accepts "input", "output" or "inout".
func (d *Direction) UnmarshalText(text []byte) error {
	for i, name := range directionNames {
		if string(text) == name {
			*d = Direction(i)
			return nil
		}
	}
	return fmt.Errorf("invalid direction %q (want input, output or inout)", text)
}

var modeNames = []string{Poll: "poll", ChangeOfState: "change-of-state", Cyclic: "cyclic"}

func (m Mode) String() string {
	if int(m) < len(modeNames) {
		return modeNames[m]
	}
	return fmt.Sprintf("Mode(%d)", uint8(m))
}

// MarshalText implements encoding.TextMarshaler.
func (m Mode) MarshalText() ([]byte, error) { return []byte(m.String()), nil }

// UnmarshalText accepts "poll", "change-of-state" or "cyclic".
func (m *Mode) UnmarshalText(text []byte) error {
	for i, name := range modeNames {
		if string(text) == name {
			*m = Mode(i)
			return nil
		}
	}
	return fmt.Errorf("invalid mode %q (want poll, change-of-state or cyclic)", text)
}
