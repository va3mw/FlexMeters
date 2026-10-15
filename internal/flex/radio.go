package flex

import (
	"bufio"
	"fmt"
	"log"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Slice is the part of slice status we need to label slice meters.
type Slice struct {
	Index        int    `json:"index"`
	Letter       string `json:"letter"`
	ClientHandle uint32 `json:"client_handle"`
}

// Station is a GUI client (SmartSDR, Maestro, ...) connected to the radio.
type Station struct {
	Handle   uint32 `json:"handle"`
	ClientID string `json:"client_id"`
	Program  string `json:"program"`
	Name     string `json:"name"`
}

// Snapshot is a consistent copy of the radio state the UI shows.
type Snapshot struct {
	Connected    bool      `json:"connected"`
	IP           string    `json:"ip"`
	Model        string    `json:"model"`
	Nickname     string    `json:"nickname"`
	Version      string    `json:"version"`
	Stations     []Station `json:"stations"`
	BoundStation string    `json:"bound_station"`
	BoundHandle  uint32    `json:"bound_handle"`
	Slices       []Slice   `json:"slices"`
	Meters       []Meter   `json:"meters"`
	UDPPort      int       `json:"udp_port"`
	Packets      uint64    `json:"packets"`
}

// ReplyFunc receives a command's reply code (0 = ok) and message.
type ReplyFunc func(code uint32, msg string)

// APIPort is the radio's TCP API port; VitaPort its UDP stream port.
// Tests and the fake radio override them.
var (
	APIPort  = "4992"
	VitaPort = 4991
)

// staleAfter is how long a meter keeps its last value with no new packet.
const staleAfter = 3 * time.Second

// Radio is a receive-only, non-GUI client of the SmartSDR TCP API. It sends
// only subscriptions, "client udpport", "client bind", "info", "meter list"
// and "ping"; nothing that changes how the radio operates.
type Radio struct {
	log      *log.Logger
	onUpdate func()
	notifyCh chan struct{}

	mu          sync.Mutex
	conn        net.Conn
	connected   bool
	ip, model   string
	nickname    string
	version     string
	seq         uint32
	pending     map[uint32]ReplyFunc
	myHandle    uint32
	meters      map[int]*Meter
	slices      map[int]*Slice
	stations    map[uint32]*Station
	wantStation string
	boundHandle uint32
	boundName   string
	lastRx      time.Time
	stop        chan struct{}
	udpPort     int

	// Latest raw value and arrival time per meter id, written by the UDP
	// loop and read by the UI ticker without taking mu.
	raw     [65536]atomic.Int32
	updated [65536]atomic.Int64
	packets atomic.Uint64
}

func NewRadio(l *log.Logger, onUpdate func()) *Radio {
	r := &Radio{log: l, onUpdate: onUpdate, notifyCh: make(chan struct{}, 1)}
	go func() {
		for range r.notifyCh {
			if r.onUpdate != nil {
				r.onUpdate()
			}
			// Meter announcements arrive in bursts; coalesce them.
			time.Sleep(100 * time.Millisecond)
		}
	}()
	return r
}

func (r *Radio) notify() {
	select {
	case r.notifyCh <- struct{}{}:
	default:
	}
}

// Connect starts a connection loop that reconnects until Disconnect.
func (r *Radio) Connect(ip, model, station string) {
	r.Disconnect()
	r.mu.Lock()
	r.ip, r.model, r.wantStation = ip, model, station
	r.nickname, r.version = "", "" // filled in again by the new radio
	stop := make(chan struct{})
	r.stop = stop
	r.mu.Unlock()
	go r.loop(ip, stop)
}

func (r *Radio) Disconnect() {
	r.mu.Lock()
	if r.stop != nil {
		close(r.stop)
		r.stop = nil
	}
	if r.conn != nil {
		r.conn.Close()
	}
	r.ip = ""
	r.mu.Unlock()
	r.notify()
}

// SetStation selects the GUI station to bind to ("" = auto if only one).
func (r *Radio) SetStation(name string) {
	r.mu.Lock()
	r.wantStation = name
	r.boundHandle, r.boundName = 0, ""
	r.mu.Unlock()
	r.tryBind()
	r.notify()
}

func (r *Radio) loop(ip string, stop chan struct{}) {
	for {
		err := r.session(ip, stop)
		select {
		case <-stop:
			r.log.Printf("disconnected from radio")
			return
		default:
		}
		r.log.Printf("radio connection lost: %v; retrying in 3 s", err)
		select {
		case <-stop:
			return
		case <-time.After(3 * time.Second):
		}
	}
}

func (r *Radio) session(ip string, stop chan struct{}) error {
	addr := net.JoinHostPort(ip, APIPort)
	r.log.Printf("connecting to radio at %s", addr)
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return err
	}
	var radioIP net.IP
	if a, err := net.ResolveIPAddr("ip4", ip); err == nil {
		radioIP = a.IP
	}
	udp, udpPort, err := openUDP()
	if err != nil {
		r.log.Printf("%v (meter values will not arrive)", err)
	} else {
		go r.udpLoop(udp, radioIP)
	}
	r.mu.Lock()
	r.conn = conn
	r.udpPort = udpPort
	r.pending = map[uint32]ReplyFunc{}
	r.meters = map[int]*Meter{}
	r.slices = map[int]*Slice{}
	r.stations = map[uint32]*Station{}
	r.boundHandle, r.boundName, r.myHandle = 0, "", 0
	r.lastRx = time.Now()
	r.mu.Unlock()
	r.packets.Store(0)

	done := make(chan struct{})
	defer func() {
		close(done)
		conn.Close()
		if udp != nil {
			udp.Close()
		}
		r.mu.Lock()
		r.connected = false
		r.conn = nil
		r.udpPort = 0
		r.meters = map[int]*Meter{}
		r.boundHandle, r.boundName = 0, ""
		r.mu.Unlock()
		r.notify()
	}()

	// Watchdog: ping every 2 s, drop the link if nothing heard for 10 s.
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-stop:
				conn.Close()
				return
			case <-t.C:
				r.mu.Lock()
				silent := time.Since(r.lastRx)
				r.mu.Unlock()
				if silent > 10*time.Second {
					r.log.Printf("radio silent for %v, closing connection", silent.Round(time.Second))
					conn.Close()
					return
				}
				r.Command("ping", nil)
			}
		}
	}()

	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		r.mu.Lock()
		r.lastRx = time.Now()
		r.mu.Unlock()
		r.handleLine(sc.Text())
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return fmt.Errorf("connection closed by radio")
}

// openUDP binds the local VITA socket. Like FlexLib it starts at 4991 and
// walks up if the port is taken (SmartSDR on the same PC uses 4991).
func openUDP() (*net.UDPConn, int, error) {
	var lastErr error
	for port := 4991; port <= 4991+40; port++ {
		c, err := net.ListenUDP("udp4", &net.UDPAddr{Port: port})
		if err == nil {
			c.SetReadBuffer(1 << 20)
			return c, port, nil
		}
		lastErr = err
	}
	return nil, 0, fmt.Errorf("no free UDP port for meter data: %w", lastErr)
}

// udpLoop stores meter values. Only packets from the radio are accepted.
func (r *Radio) udpLoop(c *net.UDPConn, radio net.IP) {
	buf := make([]byte, 16384)
	for {
		n, src, err := c.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if radio != nil && !src.IP.Equal(radio) {
			continue
		}
		pairs, err := ParseMeterPacket(buf[:n])
		if err != nil {
			continue // FFT, audio, etc. are not requested, but ignore them anyway
		}
		r.packets.Add(1)
		now := time.Now().UnixNano()
		for _, p := range pairs {
			r.raw[p.ID].Store(int32(p.Raw))
			r.updated[p.ID].Store(now)
		}
	}
}

func (r *Radio) handleLine(line string) {
	if line == "" {
		return
	}
	switch line[0] {
	case 'V':
		r.mu.Lock()
		r.version = line[1:]
		r.mu.Unlock()
		r.log.Printf("radio API version %s", line[1:])
	case 'H':
		h, _ := strconv.ParseUint(line[1:], 16, 32)
		r.mu.Lock()
		r.myHandle = uint32(h)
		r.connected = true
		udpPort := r.udpPort
		r.mu.Unlock()
		r.log.Printf("connected, our client handle 0x%08X", h)
		for _, s := range []string{"sub client all", "sub slice all", "sub radio all", "sub meter all"} {
			r.Command(s, nil)
		}
		r.Command("meter list", func(code uint32, msg string) {
			if code == 0 && r.applyMeterStatus(msg) {
				r.notify()
			}
		})
		r.Command("info", r.onInfo)
		if udpPort != 0 {
			// LAN clients tell the radio where to send VITA streams
			// (Radio.cs: "client udpport"; WAN clients use udp_register).
			r.Command(fmt.Sprintf("client udpport %d", udpPort), func(code uint32, _ string) {
				if code == 0 {
					r.log.Printf("meter data will arrive on UDP %d", udpPort)
				}
			})
		}
		r.notify()
	case 'R':
		r.handleReply(line[1:])
	case 'S':
		if _, body, ok := strings.Cut(line, "|"); ok && r.handleStatus(body) {
			r.notify()
		}
	case 'M':
		r.log.Printf("radio message: %s", line[1:])
	}
}

func (r *Radio) handleReply(s string) {
	parts := strings.SplitN(s, "|", 3)
	if len(parts) < 2 {
		return
	}
	seq, _ := strconv.ParseUint(parts[0], 10, 32)
	code, _ := strconv.ParseUint(parts[1], 16, 32)
	msg := ""
	if len(parts) == 3 {
		msg = parts[2]
	}
	r.mu.Lock()
	fn := r.pending[uint32(seq)]
	delete(r.pending, uint32(seq))
	r.mu.Unlock()
	if fn != nil {
		fn(uint32(code), msg)
	}
}

func (r *Radio) onInfo(code uint32, msg string) {
	if code != 0 {
		return
	}
	kv := map[string]string{}
	for _, f := range strings.Split(msg, ",") {
		if k, v, ok := strings.Cut(strings.TrimSpace(f), "="); ok {
			kv[k] = strings.Trim(v, `"`)
		}
	}
	r.mu.Lock()
	if kv["model"] != "" {
		r.model = kv["model"]
	}
	if kv["nickname"] != "" && r.nickname == "" {
		r.nickname = kv["nickname"]
	}
	r.mu.Unlock()
	r.notify()
}

// Command sends an API command; cb (optional) receives the reply.
func (r *Radio) Command(cmd string, cb ReplyFunc) {
	r.mu.Lock()
	conn := r.conn
	if conn == nil {
		r.mu.Unlock()
		return
	}
	r.seq++
	seq := r.seq
	r.pending[seq] = func(code uint32, msg string) {
		if code != 0 {
			r.log.Printf("command failed (0x%08X %s): %s", code, msg, cmd)
		}
		if cb != nil {
			cb(code, msg)
		}
	}
	r.mu.Unlock()
	conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if _, err := fmt.Fprintf(conn, "C%d|%s\n", seq, cmd); err != nil {
		r.log.Printf("write to radio failed: %v", err)
		conn.Close()
	}
}

// handleStatus applies a status line and reports whether UI state changed.
func (r *Radio) handleStatus(body string) bool {
	obj, rest, _ := strings.Cut(body, " ")
	switch obj {
	case "meter":
		return r.applyMeterStatus(rest)
	case "slice":
		f := strings.Fields(rest)
		if len(f) < 1 {
			return false
		}
		idx, err := strconv.Atoi(f[0])
		if err != nil {
			return false
		}
		r.applySlice(idx, parseKV(f[1:]))
		return true
	case "client":
		f := strings.Fields(rest)
		if len(f) < 2 {
			return false
		}
		h, _ := strconv.ParseUint(strings.TrimPrefix(f[0], "0x"), 16, 32)
		r.applyClient(uint32(h), f[1], parseKV(f[2:]))
		return true
	case "radio":
		kv := parseKV(strings.Fields(rest))
		if v, ok := kv["nickname"]; ok {
			r.mu.Lock()
			r.nickname = unescape(v)
			r.mu.Unlock()
			return true
		}
	}
	return false
}

func (r *Radio) applyMeterStatus(body string) bool {
	ms := ParseMeterStatus(body)
	if len(ms.Removed) == 0 && len(ms.Fields) == 0 {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.meters == nil {
		return false
	}
	for _, i := range ms.Removed {
		delete(r.meters, i)
	}
	for i, kv := range ms.Fields {
		m := r.meters[i]
		if m == nil {
			m = &Meter{Index: i}
			r.meters[i] = m
		}
		m.apply(kv)
	}
	return true
}

func (r *Radio) applySlice(idx int, kv map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.slices == nil {
		return
	}
	s := r.slices[idx]
	if s == nil {
		s = &Slice{Index: idx}
		r.slices[idx] = s
	}
	inUse := true
	for k, v := range kv {
		switch k {
		case "in_use":
			inUse = v == "1"
		case "index_letter":
			s.Letter = v
		case "client_handle":
			h, _ := strconv.ParseUint(strings.TrimPrefix(v, "0x"), 16, 32)
			s.ClientHandle = uint32(h)
		}
	}
	if !inUse {
		delete(r.slices, idx)
	}
}

func (r *Radio) applyClient(h uint32, state string, kv map[string]string) {
	r.mu.Lock()
	if r.stations == nil {
		r.mu.Unlock()
		return
	}
	if state == "disconnected" {
		delete(r.stations, h)
		if h == r.boundHandle {
			r.log.Printf("bound station went away; waiting for it to return")
			r.boundHandle, r.boundName = 0, ""
		}
		r.mu.Unlock()
		r.tryBind()
		return
	}
	if h == r.myHandle {
		r.mu.Unlock()
		return
	}
	st := r.stations[h]
	if st == nil {
		st = &Station{Handle: h}
		r.stations[h] = st
	}
	if v, ok := kv["client_id"]; ok {
		st.ClientID = v
	}
	if v, ok := kv["program"]; ok {
		st.Program = unescape(v)
	}
	if v, ok := kv["station"]; ok {
		st.Name = unescape(v)
	}
	r.mu.Unlock()
	r.tryBind()
}

// tryBind binds to the wanted station if present. With none chosen, a
// single GUI station is picked automatically (same rule as FlexTCI).
func (r *Radio) tryBind() {
	r.mu.Lock()
	if !r.connected || r.boundHandle != 0 {
		r.mu.Unlock()
		return
	}
	var gui []*Station
	for _, st := range r.stations {
		if st.ClientID != "" && st.Name != "" {
			gui = append(gui, st)
		}
	}
	var pick *Station
	for _, st := range gui {
		if st.Name == r.wantStation {
			pick = st
		}
	}
	if pick == nil && r.wantStation == "" && len(gui) == 1 {
		pick = gui[0]
	}
	if pick == nil {
		r.mu.Unlock()
		return
	}
	r.boundHandle, r.boundName = pick.Handle, pick.Name
	id, name := pick.ClientID, pick.Name
	r.mu.Unlock()
	r.log.Printf("binding to station %q", name)
	r.Command("client bind client_id="+id, nil)
}

// Snapshot returns the radio state, meters sorted by index.
func (r *Radio) Snapshot() Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := Snapshot{
		Connected:    r.connected,
		IP:           r.ip,
		Model:        r.model,
		Nickname:     r.nickname,
		Version:      r.version,
		BoundStation: r.boundName,
		BoundHandle:  r.boundHandle,
		UDPPort:      r.udpPort,
		Packets:      r.packets.Load(),
		Stations:     []Station{},
		Slices:       []Slice{},
		Meters:       []Meter{},
	}
	for _, st := range r.stations {
		if st.ClientID != "" && st.Name != "" {
			s.Stations = append(s.Stations, *st)
		}
	}
	sort.Slice(s.Stations, func(i, j int) bool { return s.Stations[i].Name < s.Stations[j].Name })
	for _, sl := range r.slices {
		s.Slices = append(s.Slices, *sl)
	}
	sort.Slice(s.Slices, func(i, j int) bool { return s.Slices[i].Index < s.Slices[j].Index })
	for _, m := range r.meters {
		s.Meters = append(s.Meters, *m)
	}
	sort.Slice(s.Meters, func(i, j int) bool { return s.Meters[i].Index < s.Meters[j].Index })
	return s
}

// Values returns the scaled value of each requested meter that has had a
// packet within staleAfter. Missing entries mean "no data".
func (r *Radio) Values(ids []int) map[int]float64 {
	r.mu.Lock()
	units := make([]string, len(ids))
	for k, id := range ids {
		if m := r.meters[id]; m != nil {
			units[k] = m.Unit
		} else {
			units[k] = "\x00"
		}
	}
	r.mu.Unlock()
	cut := time.Now().Add(-staleAfter).UnixNano()
	out := make(map[int]float64, len(ids))
	for k, id := range ids {
		if id < 0 || id > 65535 || units[k] == "\x00" {
			continue
		}
		if r.updated[id].Load() < cut {
			continue
		}
		out[id] = Scale(units[k], int16(r.raw[id].Load()))
	}
	return out
}

func parseKV(fields []string) map[string]string {
	m := map[string]string{}
	for _, f := range fields {
		k, v, _ := strings.Cut(f, "=")
		m[k] = v
	}
	return m
}
