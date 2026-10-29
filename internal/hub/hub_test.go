package hub

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sethdotfm/sode-vep/internal/config"
	"github.com/sethdotfm/sode-vep/internal/state"
	"github.com/sethdotfm/sode-vep/internal/vhsim"
)

type rig struct {
	t      *testing.T
	h      *vhsim.Server
	st     *state.Store
	c      *Client
	events <-chan state.Event
	cancel context.CancelFunc
	done   chan struct{}
}

// newHub starts a fake hub named "Fake Hub"; mut may set quirks first.
func newHub(t *testing.T, nin, nout int, mut ...func(*vhsim.Options)) *vhsim.Server {
	t.Helper()
	o := vhsim.Options{Model: "Fake Hub", Inputs: nin, Outputs: nout}
	for _, m := range mut {
		m(&o)
	}
	h := vhsim.New(o)
	if err := h.Start("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Stop)
	return h
}

func start(t *testing.T, h *vhsim.Server, tune func(*Client)) *rig {
	t.Helper()
	st := state.New()
	cfg := config.Defaults()
	cfg.Hub = h.Addr()
	c := New(st, func() config.Config { return cfg }, t.Logf, false)
	c.backoffMin, c.backoffMax = 20*time.Millisecond, 50*time.Millisecond
	c.quiet = 80 * time.Millisecond
	c.cmdTimeout = time.Second
	if tune != nil {
		tune(c)
	}
	ev, _ := st.Subscribe()
	ctx, cancel := context.WithCancel(context.Background())
	r := &rig{t: t, h: h, st: st, c: c, events: ev, cancel: cancel, done: make(chan struct{})}
	go func() { c.Run(ctx); close(r.done) }()
	t.Cleanup(func() { cancel(); <-r.done })
	return r
}

// expect waits for an event matching f, skipping others.
func (r *rig) expect(what string, f func(state.Event) bool) state.Event {
	r.t.Helper()
	to := time.After(3 * time.Second)
	for {
		select {
		case e := <-r.events:
			if f(e) {
				return e
			}
		case <-to:
			r.t.Fatalf("timeout waiting for %s", what)
		}
	}
}

func kind(k state.EventKind) func(state.Event) bool {
	return func(e state.Event) bool { return e.Kind == k }
}

func TestReadyAfterEndPrelude(t *testing.T) {
	// CRLF line ends in the preamble: a real hub's lines may end either way.
	h := newHub(t, 4, 6, func(o *vhsim.Options) { o.CRLFPreamble = true })
	r := start(t, h, func(c *Client) { c.quiet = time.Hour })
	r.expect("up", kind(state.EvUp))
	s := r.st.Snapshot()
	if !s.Up || s.Model != "Fake Hub" || len(s.Inputs) != 4 || len(s.Outputs) != 6 || s.Route[5] != 1 || s.Lock[0] != 'U' || s.Outputs[2] != "Output 3" {
		t.Fatalf("%+v", s)
	}
	if r.c.LastResync().IsZero() {
		t.Error("LastResync not set")
	}
}

func TestReadyViaQuietTimer(t *testing.T) {
	h := newHub(t, 3, 3, func(o *vhsim.Options) { o.NoEndPrelude = true })
	r := start(t, h, nil)
	r.expect("up", kind(state.EvUp))
	if s := r.st.Snapshot(); len(s.Route) != 3 || s.Route[2] != 2 {
		t.Fatalf("%+v", s)
	}
}

func TestDevicePresentFalseStaysDown(t *testing.T) {
	h := newHub(t, 2, 2, func(o *vhsim.Options) { o.DeviceAbsent = true })
	r := start(t, h, nil)
	select {
	case e := <-r.events:
		t.Fatalf("unexpected event %+v", e)
	case <-time.After(300 * time.Millisecond):
	}
	if r.st.Snapshot().Up {
		t.Fatal("up")
	}
}

func TestTakeAckAndBroadcast(t *testing.T) {
	h := newHub(t, 4, 4)
	r := start(t, h, nil)
	r.expect("up", kind(state.EvUp))
	if err := r.c.Take(1, 3); err != nil {
		t.Fatal(err)
	}
	e := r.expect("route", kind(state.EvRoute))
	if e.Out != 1 || e.In != 3 || r.st.Snapshot().Route[1] != 3 {
		t.Fatalf("%+v", e)
	}
	if err := r.c.Take(9, 0); err == nil {
		t.Error("out of range accepted")
	}
}

func TestTakeLockedNak(t *testing.T) {
	h := newHub(t, 4, 4, func(o *vhsim.Options) { o.Locks = []int{2} })
	r := start(t, h, nil)
	r.expect("up", kind(state.EvUp))
	if r.st.Snapshot().Lock[2] != 'L' {
		t.Fatal("lock not seen")
	}
	err := r.c.Take(2, 0)
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("err %v", err)
	}
	if r.st.Snapshot().Route[2] != 2 {
		t.Error("route changed")
	}
}

func TestExternalChanges(t *testing.T) {
	h := newHub(t, 4, 4)
	r := start(t, h, nil)
	r.expect("up", kind(state.EvUp))
	h.SetRoute(0, 3)
	e := r.expect("route", kind(state.EvRoute))
	if e.Out != 0 || e.In != 3 {
		t.Fatalf("%+v", e)
	}
	h.LockExternal(1)
	r.expect("lock", kind(state.EvLock))
	if r.st.Snapshot().Lock[1] != 'L' {
		t.Fatal("lock not applied")
	}
}

func TestDropThenReconnect(t *testing.T) {
	h := newHub(t, 2, 2)
	r := start(t, h, nil)
	r.expect("up", kind(state.EvUp))
	h.DropClients()
	r.expect("down", kind(state.EvDown))
	if r.st.Snapshot().Up {
		t.Fatal("still up")
	}
	r.expect("up again", kind(state.EvUp))
}

func TestPingTimeoutDropsLink(t *testing.T) {
	h := newHub(t, 2, 2)
	r := start(t, h, func(c *Client) {
		c.pingAfter, c.deadAfter, c.cmdTimeout = 60*time.Millisecond, 200*time.Millisecond, 100*time.Millisecond
	})
	r.expect("up", kind(state.EvUp))
	h.SetSilent(true)
	r.expect("down", kind(state.EvDown))
	h.SetSilent(false)
	r.expect("up again", kind(state.EvUp))
}

func TestPingKeepsLinkAlive(t *testing.T) {
	h := newHub(t, 2, 2)
	r := start(t, h, func(c *Client) {
		c.pingAfter, c.deadAfter = 50*time.Millisecond, 150*time.Millisecond
	})
	r.expect("up", kind(state.EvUp))
	select {
	case e := <-r.events:
		t.Fatalf("link dropped: %+v", e)
	case <-time.After(600 * time.Millisecond):
	}
}

func TestResyncAppliesHiddenLabelChange(t *testing.T) {
	h := newHub(t, 2, 3)
	r := start(t, h, func(c *Client) { c.resyncEvery = 80 * time.Millisecond })
	r.expect("up", kind(state.EvUp))
	h.SetOutputLabel(1, "Studio B")
	before := r.c.LastResync()
	e := r.expect("labels", kind(state.EvLabels))
	if e.Out != 1 || e.Input || r.st.Snapshot().Outputs[1] != "Studio B" {
		t.Fatalf("%+v %v", e, r.st.Snapshot().Outputs)
	}
	deadline := time.Now().Add(time.Second)
	for !r.c.LastResync().After(before) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !r.c.LastResync().After(before) {
		t.Error("LastResync not advanced")
	}
}

func TestPeriodicResync(t *testing.T) {
	h := newHub(t, 2, 3)
	r := start(t, h, func(c *Client) { c.resyncEvery = 80 * time.Millisecond })
	r.expect("up", kind(state.EvUp))
	h.SetOutputLabel(0, "Periodic")
	r.expect("labels", kind(state.EvLabels))
}

func TestReconfigureEmptyHubIdlesDown(t *testing.T) {
	h := newHub(t, 2, 2)
	st := state.New()
	cfg := config.Defaults()
	cfg.Hub = h.Addr()
	var mu = make(chan struct{}, 1)
	mu <- struct{}{}
	get := func() config.Config { <-mu; defer func() { mu <- struct{}{} }(); return cfg }
	c := New(st, get, t.Logf, false)
	c.backoffMin, c.backoffMax, c.quiet = 20*time.Millisecond, 50*time.Millisecond, 50*time.Millisecond
	ev, _ := st.Subscribe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	wait := func(k state.EventKind) {
		t.Helper()
		to := time.After(3 * time.Second)
		for {
			select {
			case e := <-ev:
				if e.Kind == k {
					return
				}
			case <-to:
				t.Fatalf("timeout waiting for %v", k)
			}
		}
	}
	wait(state.EvUp)
	<-mu
	cfg.Hub = ""
	mu <- struct{}{}
	c.Reconfigure()
	wait(state.EvDown)
	time.Sleep(150 * time.Millisecond)
	if st.Snapshot().Up {
		t.Fatal("up with empty hub")
	}
	<-mu
	cfg.Hub = h.Addr()
	mu <- struct{}{}
	c.Reconfigure()
	wait(state.EvUp)
}
