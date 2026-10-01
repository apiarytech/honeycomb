/*
 * Copyright (C) 2026 Franklin D. Amador
 *
 * This software is dual-licensed under the terms of the GPL v3.0 and
 * a commercial license. You may choose to use this software under either
 * license.
 *
 * See the LICENSE files in the project root for full license text.
 */

// This file, quality.go, defines tag quality and the bridges that translate it
// to and from OPC DA, OPC UA and Apache PLC4X. The bridges take and return plain
// integers and names, so honeycomb does not depend on any protocol library.
package honeycomb

import "fmt"

// Quality reports how trustworthy a tag's value is.
type Quality uint8

const (
	// QualityUnknown means no value has been written since the tag was created.
	QualityUnknown Quality = 0
	// QualityGood means the value is valid and current.
	QualityGood Quality = 1
	// QualityUncertain means the value may be stale or inaccurate, e.g. a value
	// restored from storage at power-up, or the last value read before a
	// transient communication failure.
	QualityUncertain Quality = 2
	// QualityBad means the value must not be used, e.g. after a device,
	// configuration or communication failure.
	QualityBad Quality = 3
)

// String returns "Unknown", "Good", "Uncertain" or "Bad".
func (q Quality) String() string {
	switch q {
	case QualityUnknown:
		return "Unknown"
	case QualityGood:
		return "Good"
	case QualityUncertain:
		return "Uncertain"
	case QualityBad:
		return "Bad"
	}
	return fmt.Sprintf("Quality(%d)", uint8(q))
}

// IsValid reports whether q is one of the defined qualities.
func (q Quality) IsValid() bool {
	return q <= QualityBad
}

// restored returns the quality a value has after being reloaded from storage:
// a Good value becomes Uncertain because the process may have changed while the
// runtime was down; Uncertain, Bad and Unknown values keep their quality.
func (q Quality) restored() Quality {
	if q == QualityGood {
		return QualityUncertain
	}
	return q
}

// OPC DA quality is a 16-bit word whose low byte is laid out QQSSSSLL: two
// quality bits, four substatus bits and two limit bits. The high byte is vendor-specific.
const (
	OPCDAQualityMask       uint16 = 0xC0
	OPCDABad               uint16 = 0x00
	OPCDAUncertain         uint16 = 0x40
	OPCDAGood              uint16 = 0xC0
	OPCDAWaitingForInitial uint16 = 0x20 // Bad, substatus "waiting for initial data" (OPC DA 3.0).
)

// OPCDA converts q to an OPC DA quality word. Unknown maps to
// "Bad - waiting for initial data", which OPC DA defines for exactly that case.
func (q Quality) OPCDA() uint16 {
	switch q {
	case QualityGood:
		return OPCDAGood
	case QualityUncertain:
		return OPCDAUncertain
	case QualityUnknown:
		return OPCDAWaitingForInitial
	}
	return OPCDABad
}

// QualityFromOPCDA converts an OPC DA quality word. Substatus and limit bits are
// dropped, except "waiting for initial data", which maps back to Unknown. The
// reserved quality bits 10 map to Bad.
func QualityFromOPCDA(code uint16) Quality {
	switch {
	case code&0xFC == OPCDAWaitingForInitial:
		return QualityUnknown
	case code&OPCDAQualityMask == OPCDAGood:
		return QualityGood
	case code&OPCDAQualityMask == OPCDAUncertain:
		return QualityUncertain
	}
	return QualityBad
}

// OPC UA StatusCode is a 32-bit value whose top two bits give the severity.
// The low 16 bits carry info flags and do not affect quality.
const (
	OPCUASeverityMask             uint32 = 0xC0000000
	OPCUAGood                     uint32 = 0x00000000
	OPCUAUncertain                uint32 = 0x40000000
	OPCUABad                      uint32 = 0x80000000
	OPCUABadWaitingForInitialData uint32 = 0x80320000
)

// OPCUA converts q to an OPC UA StatusCode. Unknown maps to BadWaitingForInitialData.
func (q Quality) OPCUA() uint32 {
	switch q {
	case QualityGood:
		return OPCUAGood
	case QualityUncertain:
		return OPCUAUncertain
	case QualityUnknown:
		return OPCUABadWaitingForInitialData
	}
	return OPCUABad
}

// QualityFromOPCUA converts an OPC UA StatusCode by its severity bits.
// BadWaitingForInitialData maps back to Unknown; the reserved severity 11 maps to Bad.
func QualityFromOPCUA(code uint32) Quality {
	switch {
	case code&0xFFFF0000 == OPCUABadWaitingForInitialData:
		return QualityUnknown
	case code&OPCUASeverityMask == OPCUAGood:
		return QualityGood
	case code&OPCUASeverityMask == OPCUAUncertain:
		return QualityUncertain
	}
	return QualityBad
}

// QualityFromPLC4X converts an Apache PLC4X response code, given by its name as
// returned by PlcResponseCode.GetName() (e.g. "OK", "REQUEST_TIMEOUT"). Names are
// used instead of numeric values so that honeycomb does not depend on plc4go.
//
// PLC4X values carry no quality of their own; the response code says whether the
// read succeeded. Transient failures (busy, pending, timeout) give Uncertain, since
// the tag still holds the last value read. Every other failure, and any unknown
// name, gives Bad. A bridge that keeps failing may escalate Uncertain to Bad itself.
func QualityFromPLC4X(responseCode string) Quality {
	switch responseCode {
	case "OK":
		return QualityGood
	case "REMOTE_BUSY", "RESPONSE_PENDING", "REQUEST_TIMEOUT":
		return QualityUncertain
	}
	return QualityBad
}
