// Package web serves the one-page GUI and its JSON/SSE API.
package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"time"

	"github.com/sethdotfm/sode-vep/internal/config"
	"github.com/sethdotfm/sode-vep/internal/state"
	ui "github.com/sethdotfm/sode-vep/web"
)

// Info is the bridge facts the page shows besides the store.
type Info struct {
	Listen     string // SW-P-08 listen address, e.g. ":8910"
	Started    time.Time
	Version    string
	LastResync time.Time // zero until the first resync
	HubAddr    string    // host:port actually dialled
}

// Deps is what the page needs from the rest of the bridge.
type Deps struct {
	Store     *state.Store
	Config    func() config.Config
	SetConfig func(config.Config) error // validates and saves; the error text is shown in the UI
	Clients   func() int                // connected Sode clients
	Info      func() Info
}

var (
	debounce     = 100 * time.Millisecond
	heartbeat    = 15 * time.Second
	writeTimeout = 10 * time.Second
	refreshAll   = 5 * time.Second // re-send state if client count, resync time etc. changed
)

type stateJSON struct {
	Up         bool      `json:"up"`
	Model      string    `json:"model"`
	Hub        string    `json:"hub"`
	Inputs     []string  `json:"inputs"`
	Outputs    []string  `json:"outputs"`
	Route      []int     `json:"route"`
	Lock       []string  `json:"lock"`
	Clients    int       `json:"clients"`
	Listen     string    `json:"listen"`
	Started    time.Time `json:"started"`
	LastResync time.Time `json:"lastResync"`
	Version    string    `json:"version"`
}

type handler struct {
	Deps
	mux *http.ServeMux
}

// New returns the GUI handler.
func New(d Deps) http.Handler {
	h := &handler{Deps: d, mux: http.NewServeMux()}
	h.mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "ok")
	})
	h.mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, h.state())
	})
	h.mux.HandleFunc("/api/config", h.config)
	h.mux.HandleFunc("/events", h.events)
	static, _ := fs.Sub(ui.FS, ".")
	h.mux.Handle("/", http.FileServerFS(static))
	return h.mux
}

func (h *handler) state() stateJSON {
	s := h.Store.Snapshot()
	in := h.Info()
	lock := make([]string, len(s.Lock))
	for i, l := range s.Lock {
		lock[i] = string(rune(l))
	}
	return stateJSON{
		Up: s.Up, Model: s.Model, Hub: in.HubAddr,
		Inputs: nonNil(s.Inputs), Outputs: nonNil(s.Outputs), Route: nonNil(s.Route), Lock: lock,
		Clients: h.Clients(), Listen: in.Listen, Started: in.Started, LastResync: in.LastResync,
		Version: in.Version,
	}
}

func nonNil[T any](a []T) []T {
	if a == nil {
		return []T{}
	}
	return a
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func (h *handler) config(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, h.Config())
	case http.MethodPut:
		if ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); ct != "application/json" {
			writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "Content-Type must be application/json"})
			return
		}
		c := h.Config() // fields the page doesn't send keep their value
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad JSON: " + err.Error()})
			return
		}
		if dec.More() {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad JSON: trailing data"})
			return
		}
		c.Normalize()
		if err := c.Validate(); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if err := h.SetConfig(c); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, h.Config())
	default:
		w.Header().Set("Allow", "GET, PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// events streams the state: once on connect, then again (debounced) after
// every store event and whenever client count or resync time change.
func (h *handler) events(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	evs, cancel := h.Store.Subscribe()
	defer cancel()

	var last []byte
	send := func() bool {
		b, _ := json.Marshal(h.state())
		if bytes.Equal(b, last) {
			return true
		}
		last = b
		rc.SetWriteDeadline(time.Now().Add(writeTimeout))
		if _, err := fmt.Fprintf(w, "event: state\ndata: %s\n\n", b); err != nil {
			return false
		}
		fl.Flush()
		return true
	}
	if !send() {
		return
	}
	hb := time.NewTicker(heartbeat)
	defer hb.Stop()
	rf := time.NewTicker(refreshAll)
	defer rf.Stop()
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	pending := false
	for {
		select {
		case <-r.Context().Done():
			return
		case <-evs:
			if !pending {
				pending = true
				timer.Reset(debounce)
			}
		case <-timer.C:
			pending = false
			if !send() {
				return
			}
		case <-rf.C:
			if !send() {
				return
			}
		case <-hb.C:
			rc.SetWriteDeadline(time.Now().Add(writeTimeout))
			if _, err := fmt.Fprint(w, ": hb\n\n"); err != nil {
				return
			}
			fl.Flush()
		}
	}
}
