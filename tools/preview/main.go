// Command preview serves the FlexMeters UI over plain HTTP so it can be
// tried in any browser without Wails (for example on a Mac whose toolchain
// cannot link the native build). A small shim stands in for the Wails
// bindings: window.go.app.App.<Method>(...) becomes POST /api/<Method>, and
// window.runtime.EventsOn listens on a server-sent event stream.
//
//	go run ./tools/preview          # then open http://127.0.0.1:34200
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync"

	"github.com/va3mw/FlexMeters/internal/app"
	"github.com/va3mw/FlexMeters/internal/config"
)

const shim = `<script>
(() => {
  const call = (name) => async (...args) => {
    const r = await fetch("/api/" + name, { method: "POST", body: JSON.stringify(args) });
    const t = await r.text();
    return t ? JSON.parse(t) : null;
  };
  window.go = { app: { App: new Proxy({}, { get: (_, name) => call(name) }) } };
  const es = new EventSource("/events"), subs = {};
  es.onmessage = e => { const m = JSON.parse(e.data); (subs[m.ev] || []).forEach(f => f(m.data)); };
  window.runtime = { EventsOn: (ev, f) => (subs[ev] ||= []).push(f) };
})();
</script>`

func main() {
	listen := flag.String("listen", "127.0.0.1:34200", "HTTP address")
	flag.Parse()
	l := log.New(os.Stderr, "", log.LstdFlags)
	a := app.New("preview", l, config.Load())

	var mu sync.Mutex
	clients := map[chan []byte]bool{}
	app.Start(a, func(ev string, data any) {
		b, _ := json.Marshal(map[string]any{"ev": ev, "data": data})
		mu.Lock()
		for c := range clients {
			select {
			case c <- b:
			default: // slow client: drop
			}
		}
		mu.Unlock()
	})

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		b, err := os.ReadFile("frontend/dist/index.html")
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(strings.Replace(string(b), "<script>", shim+"\n<script>", 1)))
	})
	http.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		c := make(chan []byte, 64)
		mu.Lock()
		clients[c] = true
		mu.Unlock()
		defer func() { mu.Lock(); delete(clients, c); mu.Unlock() }()
		fl := w.(http.Flusher)
		fl.Flush()
		for {
			select {
			case <-r.Context().Done():
				return
			case b := <-c:
				fmt.Fprintf(w, "data: %s\n\n", b)
				fl.Flush()
			}
		}
	})
	http.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		m := reflect.ValueOf(a).MethodByName(strings.TrimPrefix(r.URL.Path, "/api/"))
		if !m.IsValid() {
			http.NotFound(w, r)
			return
		}
		var raw []json.RawMessage
		json.NewDecoder(r.Body).Decode(&raw)
		t := m.Type()
		args := make([]reflect.Value, t.NumIn())
		for i := range args {
			p := reflect.New(t.In(i))
			if i < len(raw) {
				json.Unmarshal(raw[i], p.Interface())
			}
			args[i] = p.Elem()
		}
		out := m.Call(args)
		if len(out) > 0 && t.Out(0).Name() != "error" {
			json.NewEncoder(w).Encode(out[0].Interface())
		}
	})
	l.Printf("FlexMeters preview on http://%s", *listen)
	l.Fatal(http.ListenAndServe(*listen, nil))
}
