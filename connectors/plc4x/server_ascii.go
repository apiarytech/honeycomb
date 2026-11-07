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

// Modbus ASCII slave (beehive R-133): the requests and register map of the
// RTU slave, framed as Modbus over serial line 1.02, 2.5.2 has it. A frame
// is ':', the address, the PDU and the LRC each as two hex characters, and
// CR LF. The LRC is the two's complement of the sum of the bytes before it.

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/apache/plc4x/plc4go/spi/transports/serial/serialport"
)

// asciiCharTimeout is the longest silence inside a frame: one second, the
// specification's default; a frame cut by a longer one is dropped.
const asciiCharTimeout = time.Second

// asciiMaxFrame is the longest frame between ':' and CR LF: the address, a
// PDU of 253 bytes and the LRC, in hex.
const asciiMaxFrame = 2 * (1 + 253 + 1)

// lrc is the Modbus ASCII longitudinal redundancy check of b.
func lrc(b []byte) byte {
	var sum byte
	for _, x := range b {
		sum += x
	}
	return -sum
}

// answerASCII returns the response frame (':' to CR LF) to one request
// frame given without its ':' and CR LF, or nil when it is not for this
// unit, is a broadcast, is not hex or fails its LRC.
func (s *server) answerASCII(c *Connector, name string, text []byte) []byte {
	frame := make([]byte, hex.DecodedLen(len(text)))
	if _, err := hex.Decode(frame, text); err != nil || len(frame) < 3 {
		return nil
	}
	if frame[0] != s.unit || lrc(frame[:len(frame)-1]) != frame[len(frame)-1] {
		return nil
	}
	out := append([]byte{s.unit}, s.handle(c, name, frame[1:len(frame)-1], true, "serial")...)
	out = append(out, lrc(out))
	text = make([]byte, 0, 2*len(out)+3)
	text = append(text, ':')
	for _, b := range out {
		text = append(text, "0123456789ABCDEF"[b>>4], "0123456789ABCDEF"[b&15])
	}
	return append(text, '\r', '\n')
}

// asciiLine is a serial port as the ASCII slave uses it.
type asciiLine interface {
	io.ReadWriter
	SetReadDeadline(time.Time) error
}

// serveASCII opens the serial port and answers requests until ctx ends or
// the port fails.
func (c *Connector) serveASCII(ctx context.Context, cs *connState) error {
	s := cs.server
	port, err := serialport.Open(s.port, s.config)
	if err != nil {
		return fmt.Errorf("open %s: %w", s.port, err)
	}
	stop := context.AfterFunc(ctx, func() { port.Close() })
	defer func() {
		if stop() {
			port.Close()
		}
	}()
	c.record(cs.Name, func(st *Status) { st.Connected = true }, nil)
	if err := c.serveASCIILine(ctx, cs, port); err != nil {
		return fmt.Errorf("%s: %w", s.port, err)
	}
	return nil
}

// serveASCIILine answers the frames on line until ctx ends or the line
// fails. A ':' starts a frame (dropping one begun before it), LF ends it.
func (c *Connector) serveASCIILine(ctx context.Context, cs *connState, line asciiLine) error {
	s := cs.server
	var frame []byte
	in := false // inside a frame
	chunk := make([]byte, 256)
	for {
		deadline := time.Time{}
		if in {
			deadline = time.Now().Add(asciiCharTimeout)
		}
		if err := line.SetReadDeadline(deadline); err != nil {
			return err
		}
		n, err := line.Read(chunk)
		for _, b := range chunk[:n] {
			switch {
			case b == ':':
				frame, in = frame[:0], true
			case !in:
			case b == '\n':
				in = false
				if len(frame) == 0 || frame[len(frame)-1] != '\r' {
					continue
				}
				if reply := s.answerASCII(c, cs.Name, frame[:len(frame)-1]); reply != nil {
					if _, err := line.Write(reply); err != nil {
						return fmt.Errorf("write: %w", err)
					}
					c.record(cs.Name, func(st *Status) { st.Reads++; st.LastRead = time.Now() }, nil)
				}
			case len(frame) >= asciiMaxFrame+1: // +1 for the CR
				in = false // too long: noise
			default:
				frame = append(frame, b)
			}
		}
		switch {
		case err == nil:
		case errors.Is(err, os.ErrDeadlineExceeded):
			in = false // a gap inside a frame: drop it
		default:
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("read: %w", err)
		}
	}
}
