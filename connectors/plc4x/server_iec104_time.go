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

// Time in the IEC 60870-5-104 outstation (beehive R-141): the source time
// of a value, kept end to end, as a CP56Time2a time tag on spontaneous
// changes (time-tags=true), the value's quality, and the answer to a clock
// synchronisation command.
//
// Times are sent in UTC. The outstation never sets the machine's clock from
// a controlling station: the unit's clock is kept by its own time service
// (PTP or NTP, doc/time.md in beehive), and a C_CS_NA_1 is confirmed with
// the outstation's own time, so the station can see the difference.

import (
	"time"

	iec "github.com/apache/plc4x/plc4go/protocols/iec608705104/readwrite/model"
	"github.com/apiarytech/honeycomb"
)

// cp56 encodes t as a CP56Time2a, in UTC. invalid marks a time the
// outstation does not trust (a tag never stamped).
func cp56(t time.Time, invalid bool) iec.SevenOctetBinaryTime {
	t = t.UTC()
	ms := uint16(t.Second()*1000 + t.Nanosecond()/int(time.Millisecond))
	dow := uint8(t.Weekday())
	if dow == 0 {
		dow = 7 // Sunday is 7; Monday 1
	}
	year := t.Year() - 2000
	if year < 0 || year > 99 {
		year, invalid = 0, true
	}
	return iec.NewSevenOctetBinaryTime(ms, invalid, false, uint8(t.Minute()), false, uint8(t.Hour()), dow, uint8(t.Day()), uint8(t.Month()), uint8(year))
}

// FromCP56 reads a CP56Time2a as a UTC time.
func FromCP56(c iec.SevenOctetBinaryTime) time.Time {
	ms := int(c.GetMilliseconds())
	return time.Date(2000+int(c.GetYear()), time.Month(c.GetMonth()), int(c.GetDay()), int(c.GetHour()), int(c.GetMinutes()),
		ms/1000, (ms%1000)*int(time.Millisecond), time.UTC)
}

// iecQuality is a tag's quality as IEC 60870-5-104 flags: invalid when it
// is bad, not topical when it is uncertain.
func (c *Connector) iecQuality(tag string) (invalid, notTopical bool) {
	t, ok := c.db.GetTag(tag)
	if !ok {
		return false, false
	}
	switch t.Quality {
	case honeycomb.QualityBad:
		return true, false
	case honeycomb.QualityUncertain:
		return false, true
	}
	return false, false
}

// iecTimedValue is a point's monitored object with its tag's time
// (CP56Time2a); ok is false when the tag has no value.
func (c *Connector) iecTimedValue(p *iecPoint) (iec.InformationObject, bool) {
	t, ok := c.db.GetTag(p.tag)
	if !ok {
		return nil, false
	}
	raw, err := c.db.GetTagValue(p.tag)
	if err != nil || raw == nil {
		return nil, false
	}
	iv, nt := c.iecQuality(p.tag)
	at := cp56(t.Timestamp, t.Timestamp.IsZero())
	qds := iec.NewQualityDescriptor(iv, nt, false, false, false)
	switch p.kind.name {
	case "SP":
		return iec.NewInformationObjectWithSevenByteTime_SINGLE_POINT_INFORMATION(p.ioa, iec.NewSinglePointInformation(iv, nt, false, false, asBool(raw)), at), true
	case "DP":
		code := uint8(1)
		if asBool(raw) {
			code = 2
		}
		return iec.NewInformationObjectWithSevenByteTime_DOUBLE_POINT_INFORMATION(p.ioa, iec.NewDoublePointInformation(iv, nt, false, false, code), at), true
	case "FLOAT":
		return iec.NewInformationObjectWithSevenByteTime_MEASURED_VALUE_SHORT_FLOATING_POINT_NUMBER(p.ioa, float32(asFloat(raw)), qds, at), true
	case "SCALED":
		return iec.NewInformationObjectWithSevenByteTime_MEASURED_VALUE_SCALED_VALUE(p.ioa, iec.NewScaledValue(int16(asFloat(raw))), qds, at), true
	default:
		return iec.NewInformationObjectWithSevenByteTime_MEASURED_VALUE_NORMALISED_VALUE(p.ioa, iec.NewNormalizedValue(uint16(int16(asFloat(raw)))), qds, at), true
	}
}

// clockSync answers a C_CS_NA_1 with an activation confirmation carrying
// the outstation's own time. The machine's clock is not set.
func (s *iecSession) clockSync(asdu iec.ASDU) {
	obj := iec.NewInformationObjectWithoutTime_CLOCK_SYNCHRONISATION_COMMAND(0, cp56(time.Now(), false))
	s.sendASDU(iec.NewASDU(iec.TypeIdentification_CLOCK_SYNCHRONISATION_COMMAND, false, false, false, cotActivationCon, 0, asdu.GetAsduAddressField(), []iec.InformationObject{obj}))
}
