package web

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sethdotfm/sode-vep/internal/config"
	"github.com/sethdotfm/sode-vep/internal/state"
)

type fixture struct {
	store *state.Store
	cfg   config.Config
	set   error
	srv   *httptest.Server
	cl    atomic.Int32
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{store: state.New(), cfg: config.Defaults()}
	f.store.SetUp("Videohub 40x40", []string{"Cam 1", "Cam 2"}, []string{"Mon A", "Mon B", "Rec"}, []int{1, 0, -1}, []byte{'U', 'O', 'L'})
	start := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	f.srv = httptest.NewServer(New(Deps{
		Store:  f.store,
		Config: func() config.Config { return f.cfg },
		SetConfig: func(c config.Config) error {
			if f.set != nil {
				return f.set
			}
			f.cfg = c
			return nil
		},
		Clients: func() int { return int(f.cl.Load()) },
		Info:    func() Info { return Info{Listen: ":8910", Started: start, Version: "v1", HubAddr: "10.0.0.5:9990"} },
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func TestHealthzAndStatic(t *testing.T) {
	f := newFixture(t)
	for path, want := range map[string]string{"/healthz": "ok", "/": "Sode · Videohub", "/app.js": "EventSource", "/app.css": "--vep-scale"} {
		r, err := http.Get(f.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		var sb strings.Builder
		bufio.NewReader(r.Body).WriteTo(&sb)
		r.Body.Close()
		if r.StatusCode != 200 || !strings.Contains(sb.String(), want) {
			t.Errorf("%s: %d, missing %q", path, r.StatusCode, want)
		}
	}
}

func TestAPIState(t *testing.T) {
	f := newFixture(t)
	f.cl.Store(2)
	r, err := http.Get(f.srv.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var s stateJSON
	if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
		t.Fatal(err)
	}
	if !s.Up || s.Model != "Videohub 40x40" || s.Hub != "10.0.0.5:9990" || s.Clients != 2 || s.Listen != ":8910" || s.Version != "v1" {
		t.Errorf("%+v", s)
	}
	if len(s.Outputs) != 3 || s.Route[0] != 1 || s.Route[2] != -1 || strings.Join(s.Lock, "") != "UOL" || len(s.Inputs) != 2 {
		t.Errorf("%+v", s)
	}
}

func TestAPIStateDownIsEmptyArrays(t *testing.T) {
	f := newFixture(t)
	f.store.SetDown()
	r, err := http.Get(f.srv.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var raw map[string]json.RawMessage
	json.NewDecoder(r.Body).Decode(&raw)
	if string(raw["up"]) != "false" || string(raw["outputs"]) != "[]" || string(raw["route"]) != "[]" {
		t.Errorf("%s", raw)
	}
}

func put(t *testing.T, url, body string) (*http.Response, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPut, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var m map[string]any
	json.NewDecoder(r.Body).Decode(&m)
	return r, m
}

func TestAPIConfig(t *testing.T) {
	f := newFixture(t)
	r, _ := http.Get(f.srv.URL + "/api/config")
	var c config.Config
	json.NewDecoder(r.Body).Decode(&c)
	r.Body.Close()
	if c != config.Defaults() {
		t.Errorf("%+v", c)
	}

	resp, m := put(t, f.srv.URL+"/api/config", `{"hub":" 10.1.1.1 ","protectDevice":7,"resyncSeconds":30}`)
	if resp.StatusCode != 200 || f.cfg.Hub != "10.1.1.1" || f.cfg.ProtectDevice != 7 || m["resyncSeconds"] != float64(30) {
		t.Fatalf("%d %+v %v", resp.StatusCode, f.cfg, m)
	}

	before := f.cfg
	for _, body := range []string{`{"resyncSeconds":1}`, `{"hub":"h:99999"}`, `{"protectDevice":5000}`, `not json`, `{"bogus":1}`, `{"protectDevice":1} {"protectDevice":2}`, `{"debug":true}`} {
		resp, m := put(t, f.srv.URL+"/api/config", body)
		if resp.StatusCode != 400 || m["error"] == "" || f.cfg != before {
			t.Errorf("%s: %d %v", body, resp.StatusCode, m)
		}
	}

	req, _ := http.NewRequest(http.MethodPut, f.srv.URL+"/api/config", strings.NewReader(`{"protectDevice":2}`))
	req.Header.Set("Content-Type", "text/plain")
	if r, err := http.DefaultClient.Do(req); err != nil || r.StatusCode != 415 || f.cfg != before {
		t.Errorf("text/plain PUT: %v %v", r, err)
	}

	f.set = errors.New("disk full")
	resp, m = put(t, f.srv.URL+"/api/config", `{"protectDevice":3}`)
	if resp.StatusCode != 500 || m["error"] != "disk full" {
		t.Errorf("%d %v", resp.StatusCode, m)
	}

	req, _ = http.NewRequest(http.MethodDelete, f.srv.URL+"/api/config", nil)
	r, _ = http.DefaultClient.Do(req)
	r.Body.Close()
	if r.StatusCode != 405 {
		t.Errorf("DELETE: %d", r.StatusCode)
	}
}

// readEvent returns the next "event: state" payload from an SSE stream.
func readEvent(t *testing.T, sc *bufio.Scanner) stateJSON {
	t.Helper()
	got := make(chan stateJSON, 1)
	go func() {
		for sc.Scan() {
			if d, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				var s stateJSON
				json.Unmarshal([]byte(d), &s)
				got <- s
				return
			}
		}
	}()
	select {
	case s := <-got:
		return s
	case <-time.After(3 * time.Second):
		t.Fatal("no event")
		return stateJSON{}
	}
}

func TestEvents(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, f.srv.URL+"/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	sc := bufio.NewScanner(resp.Body)
	first := readEvent(t, sc)
	if !first.Up || first.Route[0] != 1 {
		t.Fatalf("%+v", first)
	}

	// A burst of store events yields a fresh state.
	f.store.ApplyRoutes(map[int]int{0: 0, 2: 1})
	f.store.ApplyLabels(nil, map[int]string{1: "Renamed"})
	next := readEvent(t, sc)
	if next.Route[0] != 0 || next.Route[2] != 1 || next.Outputs[1] != "Renamed" {
		t.Fatalf("%+v", next)
	}

	f.store.SetDown()
	if s := readEvent(t, sc); s.Up {
		t.Fatalf("still up: %+v", s)
	}
}

func TestEventsHeartbeatAndClientCount(t *testing.T) {
	old1, old2 := heartbeat, refreshAll
	heartbeat, refreshAll = 30*time.Millisecond, 30*time.Millisecond
	defer func() { heartbeat, refreshAll = old1, old2 }()
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, f.srv.URL+"/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	readEvent(t, sc)
	f.cl.Store(3)
	got := make(chan bool, 1)
	go func() {
		hb := false
		for sc.Scan() {
			if sc.Text() == ": hb" {
				hb = true
			}
			if d, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				var s stateJSON
				json.Unmarshal([]byte(d), &s)
				if s.Clients == 3 {
					got <- hb
					return
				}
			}
		}
	}()
	select {
	case <-got:
	case <-time.After(3 * time.Second):
		t.Fatal("client count never arrived")
	}
}

func TestEventsCleanup(t *testing.T) {
	f := newFixture(t)
	base := runtime.NumGoroutine()
	for i := 0; i < 10; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, f.srv.URL+"/events", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		readEvent(t, bufio.NewScanner(resp.Body))
		cancel()
		resp.Body.Close()
	}
	http.DefaultClient.CloseIdleConnections()
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > base+2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > base+2 {
		t.Errorf("goroutines: %d before, %d after", base, n)
	}
	// The store has no subscribers left: a change must not block or panic.
	f.store.ApplyRoutes(map[int]int{0: 1})
}
