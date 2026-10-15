package flex

import (
	"math"
	"testing"
)

func TestParseMeterStatus(t *testing.T) {
	ms := ParseMeterStatus("7.src=SLC#7.num=10#7.nam=LEVEL#7.low=-150.0#7.hi=20.0#7.desc=Signal strength#7.unit=dBm#7.fps=10#")
	kv := ms.Fields[7]
	if kv["nam"] != "LEVEL" || kv["desc"] != "Signal strength" || kv["unit"] != "dBm" {
		t.Fatalf("fields: %v", kv)
	}
	var m Meter
	m.apply(kv)
	if m.Num != 10 || m.Low != -150 || m.High != 20 || m.FPS != 10 {
		t.Fatalf("meter: %+v", m)
	}
	// num for an amplifier is a client handle in hex
	if parseNum("0x1234ABCD") != 0x1234ABCD {
		t.Fatal("hex num")
	}
	rm := ParseMeterStatus("12 removed")
	if len(rm.Removed) != 1 || rm.Removed[0] != 12 {
		t.Fatalf("removed: %+v", rm)
	}
}

func TestScale(t *testing.T) {
	cases := []struct {
		unit string
		raw  int16
		want float64
	}{
		{"dBm", -73 * 128, -73},
		{"SWR", 192, 1.5},
		{"Volts", 13 * 256, 13},
		{"degC", 45 * 64, 45},
		{"Watts", 100, 100},
	}
	for _, c := range cases {
		if got := Scale(c.unit, c.raw); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%s %d: got %v want %v", c.unit, c.raw, got, c.want)
		}
	}
}

func TestMeterPacketRoundTrip(t *testing.T) {
	in := []MeterPair{{1, -9344}, {65535, 32767}, {42, 0}}
	out, err := ParseMeterPacket(BuildMeterPacket(5, in))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != len(in) {
		t.Fatalf("got %d pairs", len(out))
	}
	for i := range in {
		if out[i] != in[i] {
			t.Errorf("pair %d: %+v != %+v", i, out[i], in[i])
		}
	}
	// a non-meter class is rejected
	b := BuildMeterPacket(0, in)
	b[15] = 0x01
	if _, err := ParseMeterPacket(b); err == nil {
		t.Fatal("accepted non-meter class")
	}
}
