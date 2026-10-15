package flex

import (
	"encoding/binary"
	"errors"
	"strconv"
	"strings"
)

// Meter is one meter the radio has announced with "meter" status lines.
// Field names follow FlexLib Meter.cs (v4.3.15); the status keys are
// src, num, nam, low, hi, desc, unit, fps (Radio.cs ParseMeterStatus).
type Meter struct {
	Index  int     `json:"index"`  // the id used in VITA meter packets
	Source string  `json:"source"` // SLC, TX-, RAD, COD-, AMP, HAAPI, ...
	Num    uint32  `json:"num"`    // slice index for SLC, client handle for AMP
	Name   string  `json:"name"`
	Desc   string  `json:"desc"`
	Unit   string  `json:"unit"`
	Low    float64 `json:"low"`
	High   float64 `json:"high"`
	FPS    float64 `json:"fps"`
}

// Scale converts a raw meter value to engineering units, as FlexLib
// Meter.UpdateValue does: dB/dBm/dBFS/SWR /128, Volts/Amps /256 (/1024 on
// firmware before 1.11, which we do not support), degF/degC /64, anything
// else (Watts, Percent, RPM) as-is.
func Scale(unit string, raw int16) float64 {
	switch unit {
	case "dB", "dBm", "dBFS", "SWR":
		return float64(raw) / 128
	case "Volts", "Amps":
		return float64(raw) / 256
	case "degF", "degC":
		return float64(raw) / 64
	}
	return float64(raw)
}

// MeterStatus is one parsed "meter" status line.
type MeterStatus struct {
	Removed []int                     // indices removed ("<i> removed")
	Fields  map[int]map[string]string // index -> key -> value
}

// ParseMeterStatus parses the body after "meter ". Two forms exist
// (Radio.cs ParseMeterStatus):
//
//	"5 removed"
//	"5.src=SLC#5.num=0#5.nam=LEVEL#5.low=-150.0#..."
//
// Fields are separated by '#', not spaces, because desc contains spaces.
// The same form is the reply to "meter list".
func ParseMeterStatus(body string) MeterStatus {
	var ms MeterStatus
	body = strings.TrimSpace(body)
	if body == "" {
		return ms
	}
	if strings.Contains(body, "removed") {
		w := strings.Fields(body)
		if len(w) >= 2 {
			if i, err := strconv.Atoi(w[0]); err == nil {
				ms.Removed = append(ms.Removed, i)
			}
		}
		return ms
	}
	ms.Fields = map[int]map[string]string{}
	for _, tok := range strings.Split(body, "#") {
		if tok == "" {
			continue
		}
		dot := strings.IndexByte(tok, '.')
		eq := strings.IndexByte(tok, '=')
		if dot < 0 || eq < dot {
			continue
		}
		idx, err := strconv.Atoi(tok[:dot])
		if err != nil {
			continue
		}
		m := ms.Fields[idx]
		if m == nil {
			m = map[string]string{}
			ms.Fields[idx] = m
		}
		m[tok[dot+1:eq]] = tok[eq+1:]
	}
	return ms
}

// apply copies parsed fields onto a meter.
func (m *Meter) apply(kv map[string]string) {
	for k, v := range kv {
		switch k {
		case "src":
			m.Source = v
		case "num":
			m.Num = parseNum(v)
		case "nam":
			m.Name = v
		case "desc":
			m.Desc = v
		case "unit":
			m.Unit = v
		case "low":
			m.Low, _ = strconv.ParseFloat(v, 64)
		case "hi":
			m.High, _ = strconv.ParseFloat(v, 64)
		case "fps":
			m.FPS, _ = strconv.ParseFloat(v, 64)
		}
	}
}

// parseNum reads num, which is 0x%08X for a client handle (amplifier,
// tuner) and plain decimal otherwise (Radio.cs, case "num").
func parseNum(v string) uint32 {
	if strings.HasPrefix(v, "0x") || strings.HasPrefix(v, "0X") {
		n, _ := strconv.ParseUint(v[2:], 16, 32)
		return uint32(n)
	}
	n, _ := strconv.ParseUint(v, 10, 32)
	return uint32(n)
}

// MeterClass is the VITA-49 packet class of meter packets (VitaFlex.cs
// SL_VITA_METER_CLASS). They are type 3, extension data with stream id.
const MeterClass = 0x8002

// MeterPair is one id/raw value from a meter packet.
type MeterPair struct {
	ID  uint16
	Raw int16
}

// ParseMeterPacket decodes a VITA-49 meter packet. The payload is a list of
// big-endian uint16 meter id, int16 raw value pairs (VitaMeterPacket.cs).
// It returns an error for any packet that is not a meter packet.
func ParseMeterPacket(b []byte) ([]MeterPair, error) {
	if len(b) < 4 {
		return nil, errors.New("short packet")
	}
	be := binary.BigEndian
	h := be.Uint32(b)
	typ := h >> 28
	size := int(h&0xFFFF) * 4
	if size > len(b) || size < 4 {
		size = len(b)
	}
	i := 4
	if typ == 1 || typ == 3 { // stream id
		i += 4
	}
	if h&(1<<27) == 0 {
		return nil, errors.New("no class id")
	}
	if i+8 > size {
		return nil, errors.New("truncated class id")
	}
	class := uint16(be.Uint32(b[i+4:]))
	i += 8
	if typ != 3 || class != MeterClass {
		return nil, errors.New("not a meter packet")
	}
	if (h>>22)&3 != 0 { // integer timestamp
		i += 4
	}
	if (h>>20)&3 != 0 { // fractional timestamp
		i += 8
	}
	end := size
	if h&(1<<26) != 0 { // trailer
		end -= 4
	}
	if i > end {
		return nil, errors.New("truncated packet")
	}
	n := (end - i) / 4
	out := make([]MeterPair, n)
	for k := 0; k < n; k++ {
		out[k] = MeterPair{ID: be.Uint16(b[i:]), Raw: int16(be.Uint16(b[i+2:]))}
		i += 4
	}
	return out, nil
}

// BuildMeterPacket encodes a meter packet (used by the fake radio and tests).
func BuildMeterPacket(count uint8, pairs []MeterPair) []byte {
	words := 7 + len(pairs)
	b := make([]byte, words*4)
	be := binary.BigEndian
	h := uint32(3)<<28 | 1<<27 | 3<<22 | 1<<20 | uint32(count&0x0F)<<16 | uint32(words)
	be.PutUint32(b[0:], h)
	be.PutUint32(b[4:], 0x00000700) // stream id; the radio's value is not used
	be.PutUint32(b[8:], 0x001C2D)
	be.PutUint32(b[12:], 0x534C<<16|MeterClass)
	for k, p := range pairs {
		be.PutUint16(b[28+k*4:], p.ID)
		be.PutUint16(b[30+k*4:], uint16(p.Raw))
	}
	return b
}
