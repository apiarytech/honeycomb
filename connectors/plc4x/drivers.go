package plc4x

import (
	"fmt"
	"slices"
	"strings"
	"sync"

	plc4go "github.com/apache/plc4x/plc4go/pkg/api"
	"github.com/apache/plc4x/plc4go/pkg/api/config"
	"github.com/apache/plc4x/plc4go/pkg/api/drivers"
)

// registerFunc registers one PLC4X driver, and the transports it needs, with
// a driver manager.
type registerFunc func(plc4go.PlcDriverManager, ...config.WithOption) plc4go.PlcDriver

var allDrivers = []registerFunc{
	drivers.RegisterAbEthDriver, drivers.RegisterAdsDriver, drivers.RegisterBacnetDriver,
	drivers.RegisterCBusDriver, drivers.RegisterEipDriver, drivers.RegisterLogixDriver,
	drivers.RegisterFirmataDriver, drivers.RegisterIec608705104Driver, drivers.RegisterKnxDriver,
	drivers.RegisterModbusTcpDriver, drivers.RegisterModbusRtuDriver, drivers.RegisterModbusAsciiDriver,
	drivers.RegisterOpcuaDriver, drivers.RegisterS7Driver, drivers.RegisterSlmpDriver, drivers.RegisterUmasDriver,
}

var (
	byProtocolOnce sync.Once
	byProtocol     map[string]registerFunc
)

// protocols maps each PLC4X protocol code (the URL scheme, e.g. "modbus-tcp")
// to the function that registers its driver. The codes come from the drivers
// themselves, registered once with a throwaway manager.
func protocols() map[string]registerFunc {
	byProtocolOnce.Do(func() {
		byProtocol = map[string]registerFunc{}
		for _, register := range allDrivers {
			byProtocol[register(plc4go.NewPlcDriverManager()).GetProtocolCode()] = register
		}
	})
	return byProtocol
}

// KnownProtocols returns the protocol codes PLC4X provides, sorted.
func KnownProtocols() []string {
	var out []string
	for code := range protocols() {
		out = append(out, code)
	}
	slices.Sort(out)
	return out
}

// ProtocolOf returns the protocol code of a PLC4X connection string: the part
// before the first ':' ("modbus-tcp" in "modbus-tcp://10.0.0.5:502", "opcua"
// in "opcua:tcp://host:4840").
func ProtocolOf(url string) string {
	code, _, _ := strings.Cut(url, ":")
	return strings.ToLower(code)
}

// Protocols returns the protocol codes the connections use, sorted and
// without repeats.
func Protocols(connections []Connection) []string {
	var out []string
	for _, c := range connections {
		out = append(out, ProtocolOf(c.URL))
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// NewDriverManagerFor returns a PLC4X driver manager with only the drivers
// for the given protocol codes (and their transports) registered. A device
// can then reach no other protocol's code. Unknown codes are an error.
func NewDriverManagerFor(codes ...string) (plc4go.PlcDriverManager, error) {
	known := protocols()
	m := plc4go.NewPlcDriverManager()
	for _, code := range codes {
		register, ok := known[strings.ToLower(code)]
		if !ok {
			return nil, fmt.Errorf("plc4x: unknown protocol %q (known: %s)", code, strings.Join(KnownProtocols(), ", "))
		}
		register(m)
	}
	return m, nil
}

// NewDriverManager returns a PLC4X driver manager with every PLC4X driver
// registered. Prefer NewDriverManagerFor: New registers only the drivers its
// connections use.
func NewDriverManager() plc4go.PlcDriverManager {
	m := plc4go.NewPlcDriverManager()
	for _, register := range allDrivers {
		register(m)
	}
	return m
}
