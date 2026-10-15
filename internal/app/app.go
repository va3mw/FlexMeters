package app

import (
	"log"
	"sync"
	"time"

	"github.com/va3mw/FlexMeters/internal/config"
	"github.com/va3mw/FlexMeters/internal/flex"
)

// valueRate is how often meter values are pushed to the UI.
const valueRate = 15 // Hz

// State is everything the UI needs except live values.
type State struct {
	Version string           `json:"version"`
	Radio   flex.Snapshot    `json:"radio"`
	Radios  []flex.RadioInfo `json:"radios"`
	Config  config.Config    `json:"config"`
}

// Emitter sends an event to the UI (runtime.EventsEmit under Wails).
type Emitter func(event string, data any)

type App struct {
	emit    Emitter
	version string
	log     *log.Logger
	cfg     *config.Store
	disc    *flex.Discovery
	radio   *flex.Radio

	mu     sync.Mutex
	wanted []int
	quit   chan struct{}
}

func New(version string, l *log.Logger, cfg *config.Store) *App {
	a := &App{version: version, log: l, cfg: cfg, quit: make(chan struct{})}
	a.radio = flex.NewRadio(l, a.pushState)
	a.disc = flex.NewDiscovery(l, a.onDiscovery)
	return a
}

// Start begins discovery and the value pump. It is a function, not a
// method, so Wails does not expose it to the UI.
func Start(a *App, emit Emitter) {
	a.emit = emit
	a.disc.Start()
	if c := a.cfg.Get(); c.RadioIP != "" {
		a.radio.Connect(c.RadioIP, "", c.Station)
	}
	go a.valueLoop()
}

// Stop disconnects and ends background work.
func Stop(a *App) {
	close(a.quit)
	a.radio.Disconnect()
	a.disc.Stop()
}

func (a *App) pushState() {
	if a.emit != nil {
		a.emit("state", a.GetState())
	}
}

// onDiscovery follows the saved radio if DHCP gives it a new address.
func (a *App) onDiscovery() {
	c := a.cfg.Get()
	if c.RadioSerial != "" {
		if info, ok := a.disc.Find(c.RadioSerial); ok && info.IP != c.RadioIP {
			a.log.Printf("radio %s moved %s -> %s, reconnecting", info.Serial, c.RadioIP, info.IP)
			a.cfg.Update(func(c *config.Config) { c.RadioIP = info.IP })
			a.radio.Connect(info.IP, info.Model, c.Station)
		}
	}
	a.pushState()
}

func (a *App) valueLoop() {
	t := time.NewTicker(time.Second / valueRate)
	defer t.Stop()
	for {
		select {
		case <-a.quit:
			return
		case <-t.C:
		}
		a.mu.Lock()
		ids := a.wanted
		a.mu.Unlock()
		if len(ids) == 0 || a.emit == nil {
			continue
		}
		a.emit("values", a.radio.Values(ids))
	}
}

// ---- bound to the UI ----

func (a *App) GetState() State {
	return State{Version: a.version, Radio: a.radio.Snapshot(), Radios: a.disc.List(), Config: a.cfg.Get()}
}

// Connect connects to a discovered radio by serial, or to ip if serial is "".
func (a *App) Connect(serial, ip string) {
	model := ""
	if serial != "" {
		if info, ok := a.disc.Find(serial); ok {
			ip, model = info.IP, info.Model
		}
	}
	if ip == "" {
		return
	}
	a.cfg.Update(func(c *config.Config) { c.RadioSerial, c.RadioIP = serial, ip })
	a.radio.Connect(ip, model, a.cfg.Get().Station)
}

func (a *App) Disconnect() {
	a.cfg.Update(func(c *config.Config) { c.RadioIP, c.RadioSerial = "", "" })
	a.radio.Disconnect()
}

func (a *App) SelectStation(name string) {
	a.cfg.Update(func(c *config.Config) { c.Station = name })
	a.radio.SetStation(name)
}

// SetWanted sets the meter indices whose values are pushed to the UI.
func (a *App) SetWanted(ids []int) {
	a.mu.Lock()
	a.wanted = ids
	a.mu.Unlock()
}

// SaveLayout stores the selected meter keys (in display order).
func (a *App) SaveLayout(selected []string, pickerOpen bool) error {
	return a.cfg.Update(func(c *config.Config) { c.Selected, c.PickerOpen = selected, pickerOpen })
}

// SetRange overrides a meter's display range; low == high clears it.
func (a *App) SetRange(key string, low, high float64) error {
	return a.cfg.Update(func(c *config.Config) {
		if low == high {
			delete(c.Ranges, key)
		} else {
			c.Ranges[key] = config.Range{Low: low, High: high}
		}
	})
}
