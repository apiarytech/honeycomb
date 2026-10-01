package plc4x

import (
	"context"
	"encoding/binary"
	"io"
	"math"
	"net"
	"reflect"
	"sync"
	"testing"
	"time"

	apiValues "github.com/apache/plc4x/plc4go/pkg/api/values"
	spiValues "github.com/apache/plc4x/plc4go/spi/values"
	"github.com/apiarytech/honeycomb"
	plc "github.com/apiarytech/royaljelly/iec"
)

// modbusServer is a minimal Modbus TCP server backed by an in-memory register
// table. It answers "read holding registers" (0x03), "write single register"
// (0x06) and "write multiple registers" (0x10), and counts the writes.
type modbusServer struct {
	listener net.Listener
	mu       sync.Mutex
	regs     [16]uint16
	writes   int
}

func startModbusServer(t *testing.T) *modbusServer {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &modbusServer{listener: l}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go s.serve(conn)
		}
	}()
	return s
}

func (s *modbusServer) addr() string { return s.listener.Addr().String() }

func (s *modbusServer) set(start int, words ...uint16) {
	s.mu.Lock()
	defer s.mu.Unlock()
	copy(s.regs[start:], words)
}

func (s *modbusServer) get(i int) uint16 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.regs[i]
}

func (s *modbusServer) writeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes
}

func (s *modbusServer) serve(conn net.Conn) {
	defer conn.Close()
	header := make([]byte, 7) // transaction id, protocol id, length, unit id
	for {
		if _, err := io.ReadFull(conn, header); err != nil {
			return
		}
		pdu := make([]byte, binary.BigEndian.Uint16(header[4:6])-1)
		if _, err := io.ReadFull(conn, pdu); err != nil {
			return
		}

		out := append([]byte{}, header[:4]...)
		reply := s.handle(pdu)
		out = binary.BigEndian.AppendUint16(out, uint16(len(reply)+1))
		out = append(out, header[6])
		if _, err := conn.Write(append(out, reply...)); err != nil {
			return
		}
	}
}

func (s *modbusServer) handle(pdu []byte) []byte {
	illegalAddress := []byte{pdu[0] | 0x80, 0x02}
	s.mu.Lock()
	defer s.mu.Unlock()
	start := int(binary.BigEndian.Uint16(pdu[1:3]))
	switch pdu[0] {
	case 0x03:
		count := int(binary.BigEndian.Uint16(pdu[3:5]))
		if start+count > len(s.regs) {
			return illegalAddress
		}
		reply := []byte{0x03, byte(2 * count)}
		for _, r := range s.regs[start : start+count] {
			reply = binary.BigEndian.AppendUint16(reply, r)
		}
		return reply
	case 0x06:
		if start >= len(s.regs) {
			return illegalAddress
		}
		s.regs[start] = binary.BigEndian.Uint16(pdu[3:5])
		s.writes++
		return pdu // the reply echoes the request
	case 0x10:
		count := int(binary.BigEndian.Uint16(pdu[3:5]))
		if start+count > len(s.regs) {
			return illegalAddress
		}
		for i := range count {
			s.regs[start+i] = binary.BigEndian.Uint16(pdu[6+2*i:])
		}
		s.writes++
		return pdu[:5]
	}
	return []byte{pdu[0] | 0x80, 0x01} // illegal function
}

func addTag(t *testing.T, db *honeycomb.TagDatabase, name string, dt honeycomb.DataType, value any) {
	t.Helper()
	if err := db.AddTag(&honeycomb.Tag{Name: name, TypeInfo: &honeycomb.TypeInfo{DataType: dt}, Value: value}); err != nil {
		t.Fatal(err)
	}
}

func TestConnectorReadsModbusIntoTags(t *testing.T) {
	server := startModbusServer(t)
	counter := uint32(123456)
	setpoint := math.Float32bits(12.5)
	server.set(0, uint16(counter>>16), uint16(counter), uint16(setpoint>>16), uint16(setpoint), 0xFFFF)

	db := honeycomb.NewTagDatabase()
	addTag(t, db, "Counter", honeycomb.TypeDINT, plc.DINT(0))
	addTag(t, db, "Setpoint", honeycomb.TypeREAL, plc.REAL(0))
	addTag(t, db, "Level", honeycomb.TypeINT, plc.INT(0))

	connector, err := New(db, []Connection{{
		Name:     "sim",
		URL:      "modbus-tcp://" + server.addr(),
		Interval: 50 * time.Millisecond,
		Bindings: []Binding{
			{Tag: "Counter", Address: "holding-register:1:DINT"},
			{Tag: "Setpoint", Address: "holding-register:3:REAL"},
			{Tag: "Level", Address: "holding-register:5:INT"},
		},
	}}, WithErrorHandler(func(name string, err error) { t.Logf("%s: %v", name, err) }))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { connector.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	want := map[string]any{"Counter": plc.DINT(123456), "Setpoint": plc.REAL(12.5), "Level": plc.INT(-1)}
	waitFor(t, func() bool {
		for name, v := range want {
			if got, _ := db.GetTagValue(name); got != v {
				return false
			}
		}
		return true
	})

	// Changes on the device reach the tag on a later poll.
	server.set(4, 42)
	waitFor(t, func() bool { v, _ := db.GetTagValue("Level"); return v == plc.INT(42) })

	if s, _ := connector.Status("sim"); !s.Connected || s.LastError != nil || s.LastRead.IsZero() {
		t.Errorf("unexpected status %+v", s)
	}
	for name := range want {
		if q, _ := db.GetTagQuality(name); q != honeycomb.QualityGood {
			t.Errorf("%s quality = %v, want Good", name, q)
		}
	}
}

func TestConnectorReportsUnreachableDevice(t *testing.T) {
	db := honeycomb.NewTagDatabase()
	addTag(t, db, "X", honeycomb.TypeINT, plc.INT(0))
	connector, err := New(db, []Connection{{
		Name: "down", URL: "modbus-tcp://127.0.0.1:1", Bindings: []Binding{{Tag: "X", Address: "holding-register:1:INT"}},
	}})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go connector.Run(ctx)
	waitFor(t, func() bool { s, ok := connector.Status("down"); return ok && s.LastError != nil && !s.Connected })
	if q, _ := db.GetTagQuality("X"); q != honeycomb.QualityBad {
		t.Errorf("quality of an unreachable input = %v, want Bad", q)
	}
}

func TestConvertArrays(t *testing.T) {
	got, err := convert(reflect.TypeFor[[]plc.INT](), listValue(t, 1, 2, 3))
	if err != nil {
		t.Fatal(err)
	}
	if s := got.([]plc.INT); len(s) != 3 || s[2] != 3 {
		t.Fatalf("got %v", got)
	}
}

func TestNewValidatesConnections(t *testing.T) {
	db := honeycomb.NewTagDatabase()
	for _, conns := range [][]Connection{
		{{Name: "", URL: "modbus-tcp://x"}},
		{{Name: "a", URL: "modbus-tcp://x"}, {Name: "a", URL: "modbus-tcp://y"}},
	} {
		if _, err := New(db, conns, WithDriverManager(nil)); err == nil {
			t.Errorf("New(%v) should fail", conns)
		}
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 10s")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func listValue(t *testing.T, items ...int16) apiValues.PlcValue {
	t.Helper()
	values := make([]apiValues.PlcValue, len(items))
	for i, v := range items {
		values[i] = spiValues.NewPlcINT(v)
	}
	return spiValues.NewPlcList(values)
}
