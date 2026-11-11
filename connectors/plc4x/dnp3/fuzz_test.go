/*
 * Copyright (C) 2026 Franklin D. Amador
 *
 * This software is dual-licensed under the terms of the GPL v3.0 and
 * a commercial license. You may choose to use this software under either
 * license.
 *
 * See the LICENSE files in the project root for full license text.
 */

package dnp3

import (
	"bytes"
	"net"
	"testing"
	"time"
)

// nopConn takes whatever the outstation writes.
type nopConn struct{ bytes.Buffer }

func (*nopConn) Read([]byte) (int, error)         { return 0, net.ErrClosed }
func (*nopConn) Close() error                     { return nil }
func (*nopConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (*nopConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (*nopConn) SetDeadline(time.Time) error      { return nil }
func (*nopConn) SetReadDeadline(time.Time) error  { return nil }
func (*nopConn) SetWriteDeadline(time.Time) error { return nil }

// Whatever a master sends, the outstation answers or ignores it: it never
// panics, and its answers fit a fragment.
func FuzzRequest(f *testing.F) {
	for _, seed := range [][]byte{
		{0xC0, FuncRead, 60, 1, qAll},
		{0xC1, FuncRead, 1, 2, qStartStop16, 0, 0, 5, 0},
		{0xC2, FuncWrite, 80, 1, qStartStop8, 7, 7, 0},
		{0xC3, FuncDirectOperate, 12, 1, qIndex16, 1, 0, 0, 0, 3, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		{0xC4, FuncSelect, 41, 3, qIndex8, 1, 0, 0, 0, 0x80, 0x3F, 0},
		{0xC5, FuncRead, 2, 0, qCount8, 3},
	} {
		f.Add(seed)
	}
	src := &source{vals: map[Point]Value{{Kind: BinaryInput, Index: 0}: {Online: true}, {Kind: BinaryOutput, Index: 0}: {Online: true},
		{Kind: AnalogOutput, Index: 0}: {Online: true}}}
	o, err := New(Config{Address: 1, Source: src, Points: []Point{{Kind: BinaryInput, Index: 0, Class: 1},
		{Kind: BinaryOutput, Index: 0, Writable: true}, {Kind: AnalogOutput, Index: 0, Writable: true}}})
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, frag []byte) {
		ParseHeaders(frag, true)
		ParseHeaders(frag, false)
		c := &nopConn{}
		s := &session{o: o, conn: c, commands: true, confirmSeq: -1}
		s.request(frag, false)
	})
}
