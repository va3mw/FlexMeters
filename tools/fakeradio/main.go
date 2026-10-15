// Command fakeradio imitates the parts of a FlexRadio that FlexMeters uses:
// discovery broadcasts, the TCP API (meter, slice and client status) and
// VITA-49 meter packets. The meter set is illustrative, not a copy of any
// real radio's list. It never needs a real radio.
//
//	go run ./tools/fakeradio            # discovery to 127.0.0.1, API on :4992
//	go run ./tools/fakeradio -churn     # slice B closes and reopens every 20 s
package main

import (
	"bufio"
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"math"
	"math/rand"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/va3mw/FlexMeters/internal/flex"
)

type meter struct {
	idx            int
	src, num, name string
	desc, unit     string
	low, high      float64
	gen            func(t float64) float64
}

const station = "0x11111111"

var (
	addr     = flag.String("listen", ":4992", "TCP API address")
	bcast    = flag.String("discovery", "127.0.0.1:4992", "where to send discovery packets")
	churn    = flag.Bool("churn", false, "close and reopen slice B every 20 s")
	alwaysTX = flag.Bool("tx", false, "transmit meters always active (for screenshots)")
)

func noise(a float64) float64 { return (rand.Float64() - 0.5) * a }

// keyed is true for 4 s out of every 10, to animate the TX meters.
func keyed(t float64) bool { return *alwaysTX || math.Mod(t, 10) < 4 }

// dbm converts watts to dBm, the unit the radio uses for TX power.
func dbm(w float64) float64 {
	if w <= 0.001 {
		return 0
	}
	return 10*math.Log10(w) + 30
}

// meters mirrors the list a FLEX-8600 (v4.3.16) announced with no GUI
// client attached, plus PASTAT/PAEFF from an AU-520 (see DECISIONS.md) and
// illustrative slice and amplifier meters.
func meters(sliceB bool) []meter {
	ms := []meter{
		{1, "COD-", "1", "MICPEAK", "Signal strength of MIC output in CODEC", "dBFS", -150, 20, func(t float64) float64 { return -20 + 15*math.Abs(math.Sin(t*2.1)) }},
		{2, "COD-", "2", "MIC", "Average Signal strength of MIC ouput in CODEC", "dBFS", -150, 20, func(t float64) float64 { return -30 + 18*math.Abs(math.Sin(t*2.1)) + noise(3) }},
		{3, "TX-", "5", "HWALC", "Voltage present at the Hardware ALC RCA Plug", "dBFS", -150, 20, func(t float64) float64 { return 0 }},
		{4, "RAD", "334", "+13.8A", "Main radio input voltage at PA", "Volts", 10.5, 15, func(t float64) float64 {
			if keyed(t) {
				return 13.6 + noise(0.05)
			}
			return 14.0 + noise(0.03)
		}},
		{5, "RAD", "300", "PACURRENT", "PA current", "Amps", 0, 10, func(t float64) float64 {
			if keyed(t) {
				return 7.5 + noise(0.4)
			}
			return 0.01
		}},
		{6, "RAD", "0", "+13.8B", "Main radio input voltage at CPU (continuous)", "Volts", 10.5, 15, func(t float64) float64 { return 14.6 + noise(0.02) }},
		{7, "RAD", "3", "MAINFAN", "Main radio fan RPM", "RPM", 0, 8192, func(t float64) float64 { return 984 + 100*math.Sin(t*0.1) }},
		{8, "TX-", "1", "FWDPWR", "RF Power Forward", "dBm", 0, 53, func(t float64) float64 {
			if keyed(t) {
				return dbm(95 + noise(6))
			}
			return 0
		}},
		{9, "TX-", "2", "REFPWR", "RF Power Reflected", "dBm", 0, 53, func(t float64) float64 {
			if keyed(t) {
				return dbm(4 + noise(1))
			}
			return 0
		}},
		{10, "TX-", "3", "SWR", "RF SWR", "SWR", 1, 999, func(t float64) float64 {
			if keyed(t) {
				return 1.4 + 0.15*math.Sin(t*3)
			}
			return 1
		}},
		{11, "TX-", "4", "PATEMP", "PA Temperature", "degC", 0, 120, func(t float64) float64 { return 32 + 6*math.Sin(t*0.05) }},
		{12, "EXT_WVF", "1176883659", "FreeDV_SNR", "External Meter", "dB", -99, 99, func(t float64) float64 { return -99 }},
		{15, "RAD", "7", "PASTAT", "PA Fault Status", "bits", 0, 256, func(t float64) float64 { return 0 }},
		{16, "TX-", "6", "PAEFF", "PA Efficiency", "Percent", 0, 100, func(t float64) float64 {
			if keyed(t) {
				return 58
			}
			return 0
		}},
		{13, "SLC", "0", "LEVEL", "Signal strength", "dBm", -150, 20, func(t float64) float64 { return -95 + 25*math.Sin(t*0.7) + noise(4) }},
		{14, "AMP", "0x40000001", "FWD", "Amplifier forward power", "dBm", 0, 63, func(t float64) float64 {
			if keyed(t) {
				return dbm(1100 + noise(50))
			}
			return 0
		}},
	}
	if sliceB {
		ms = append(ms,
			meter{20, "SLC", "1", "LEVEL", "Signal strength", "dBm", -150, 20, func(t float64) float64 { return -110 + 8*math.Sin(t*1.3) + noise(3) }})
	}
	return ms
}

func status(m meter) string {
	i := m.idx
	return fmt.Sprintf("%d.src=%s#%d.num=%s#%d.nam=%s#%d.low=%.1f#%d.hi=%.1f#%d.desc=%s#%d.unit=%s#%d.fps=20#",
		i, m.src, i, m.num, i, m.name, i, m.low, i, m.high, i, m.desc, i, m.unit, i)
}

func raw(unit string, v float64) int16 {
	switch unit {
	case "dB", "dBm", "dBFS", "SWR":
		v *= 128
	case "Volts", "Amps":
		v *= 256
	case "degC", "degF":
		v *= 64
	}
	return int16(math.Max(-32768, math.Min(32767, math.Round(v))))
}

func main() {
	flag.Parse()
	go discovery()
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("fake radio API on %s, discovery to %s", *addr, *bcast)
	for {
		c, err := ln.Accept()
		if err != nil {
			log.Fatal(err)
		}
		go serve(c)
	}
}

func discovery() {
	c, err := net.Dial("udp4", *bcast)
	if err != nil {
		log.Fatal(err)
	}
	payload := "discovery_protocol_version=3.0.0.2 model=FLEX-8600 serial=1234-5678-8600-0001 version=4.1.3.39644 nickname=Bench\x7fRadio callsign=VA3MW ip=127.0.0.1 port=4992 status=Available gui_client_stations=VA3MW-Shack"
	for len(payload)%4 != 0 {
		payload += " "
	}
	words := 7 + len(payload)/4
	b := make([]byte, words*4)
	binary.BigEndian.PutUint32(b[0:], uint32(3)<<28|1<<27|uint32(words))
	binary.BigEndian.PutUint32(b[4:], 0x800)
	binary.BigEndian.PutUint32(b[8:], 0x001C2D)
	binary.BigEndian.PutUint32(b[12:], 0x534CFFFF)
	copy(b[28:], payload)
	for {
		c.Write(b)
		time.Sleep(time.Second)
	}
}

func serve(c net.Conn) {
	defer c.Close()
	log.Printf("client %s connected", c.RemoteAddr())
	var mu sync.Mutex
	send := func(s string) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(c, "%s\n", s)
	}
	send("V1.4.0.0")
	send("H2A2A2A2A")

	var (
		stateMu sync.Mutex
		sliceB  = true
		udpPort int
		done    = make(chan struct{})
	)
	defer close(done)

	if *churn {
		go func() {
			for {
				select {
				case <-done:
					return
				case <-time.After(20 * time.Second):
				}
				stateMu.Lock()
				sliceB = !sliceB
				on := sliceB
				stateMu.Unlock()
				if on {
					send("S0|slice 1 in_use=1 index_letter=B client_handle=" + station)
					for _, m := range meters(true)[16:] {
						send("S0|meter " + status(m))
					}
				} else {
					send("S0|meter 20 removed")
					send("S0|slice 1 in_use=0")
				}
				log.Printf("slice B open: %v", on)
			}
		}()
	}

	stream := func(port int) {
		host, _, _ := net.SplitHostPort(c.RemoteAddr().String())
		u, err := net.Dial("udp4", net.JoinHostPort(host, fmt.Sprint(port)))
		if err != nil {
			log.Printf("udp: %v", err)
			return
		}
		defer u.Close()
		start := time.Now()
		t := time.NewTicker(50 * time.Millisecond)
		defer t.Stop()
		var count uint8
		for {
			select {
			case <-done:
				return
			case <-t.C:
			}
			stateMu.Lock()
			ms := meters(sliceB)
			stateMu.Unlock()
			now := time.Since(start).Seconds()
			pairs := make([]flex.MeterPair, len(ms))
			for i, m := range ms {
				pairs[i] = flex.MeterPair{ID: uint16(m.idx), Raw: raw(m.unit, m.gen(now))}
			}
			u.Write(flex.BuildMeterPacket(count, pairs))
			count++
		}
	}

	sc := bufio.NewScanner(c)
	for sc.Scan() {
		line := sc.Text()
		seqS, cmd, ok := strings.Cut(strings.TrimPrefix(line, "C"), "|")
		if !ok {
			continue
		}
		log.Printf("<- %s", cmd)
		reply := ""
		switch {
		case cmd == "sub client all":
			send("S0|client " + station + " connected local_ptt=1 client_id=8E1A4F2C-0000-4000-8000-000000000001 program=SmartSDR-Win station=VA3MW-Shack")
		case cmd == "sub slice all":
			send("S0|slice 0 in_use=1 index_letter=A client_handle=" + station + " RF_frequency=14.074000 mode=DIGU")
			stateMu.Lock()
			if sliceB {
				send("S0|slice 1 in_use=1 index_letter=B client_handle=" + station + " RF_frequency=7.074000 mode=DIGU")
			}
			stateMu.Unlock()
		case cmd == "sub meter all":
			stateMu.Lock()
			ms := meters(sliceB)
			stateMu.Unlock()
			for _, m := range ms {
				send("S0|meter " + status(m))
			}
		case cmd == "meter list":
			stateMu.Lock()
			ms := meters(sliceB)
			stateMu.Unlock()
			var sb strings.Builder
			for _, m := range ms {
				sb.WriteString(status(m))
			}
			reply = sb.String()
		case cmd == "info":
			reply = `model="FLEX-8600",chassis_serial="1234-5678-8600-0001",nickname="Bench Radio",callsign="VA3MW"`
		case strings.HasPrefix(cmd, "client udpport "):
			fmt.Sscan(strings.TrimPrefix(cmd, "client udpport "), &udpPort)
			go stream(udpPort)
		}
		send(fmt.Sprintf("R%s|0|%s", seqS, reply))
	}
	log.Printf("client %s gone", c.RemoteAddr())
}
