package hub

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sethdotfm/sode-vep/internal/config"
	"github.com/sethdotfm/sode-vep/internal/state"
)

type rig struct {
	t      *testing.T
	h      *fakeHub
	st     *state.Store
	c      *Client
	events <-chan state.Event
	cancel context.CancelFunc
	done   chan struct{}
}

func start(t *testing.T, h *fakeHub, tune func(*Client)) *rig {
	t.Helper()
	h.serve()
	st := state.New()
	cfg := config.Defaults()
	cfg.Hub = h.addr()
	c := New(st, func() config.Config { return cfg }, t.Logf)
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
	h := newFakeHub(t, 4, 6)
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
	h := newFakeHub(t, 3, 3)
	h.endPrelude = false
	r := start(t, h, nil)
	r.expect("up", kind(state.EvUp))
	if s := r.st.Snapshot(); len(s.Route) != 3 || s.Route[2] != 2 {
		t.Fatalf("%+v", s)
	}
}

func TestDevicePresentFalseStaysDown(t *testing.T) {
	h := newFakeHub(t, 2, 2)
	h.present = "false"
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
	h := newFakeHub(t, 4, 4)
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
	h := newFakeHub(t, 4, 4)
	h.owner[2] = h.ext
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
	h := newFakeHub(t, 4, 4)
	r := start(t, h, nil)
	r.expect("up", kind(state.EvUp))
	h.externalRoute(0, 3)
	e := r.expect("route", kind(state.EvRoute))
	if e.Out != 0 || e.In != 3 {
		t.Fatalf("%+v", e)
	}
	h.lockExternally(1)
	r.expect("lock", kind(state.EvLock))
	if r.st.Snapshot().Lock[1] != 'L' {
		t.Fatal("lock not applied")
	}
}

func TestDropThenReconnect(t *testing.T) {
	h := newFakeHub(t, 2, 2)
	r := start(t, h, nil)
	r.expect("up", kind(state.EvUp))
	h.dropAll()
	r.expect("down", kind(state.EvDown))
	if r.st.Snapshot().Up {
		t.Fatal("still up")
	}
	r.expect("up again", kind(state.EvUp))
}

func TestPingTimeoutDropsLink(t *testing.T) {
	h := newFakeHub(t, 2, 2)
	r := start(t, h, func(c *Client) {
		c.pingAfter, c.deadAfter, c.cmdTimeout = 60*time.Millisecond, 200*time.Millisecond, 100*time.Millisecond
	})
	r.expect("up", kind(state.EvUp))
	h.silent.Store(true)
	r.expect("down", kind(state.EvDown))
	h.silent.Store(false)
	r.expect("up again", kind(state.EvUp))
}

func TestPingKeepsLinkAlive(t *testing.T) {
	h := newFakeHub(t, 2, 2)
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
	h := newFakeHub(t, 2, 3)
	r := start(t, h, nil)
	r.expect("up", kind(state.EvUp))
	h.renameQuietly(1, "Studio B")
	before := r.c.LastResync()
	time.Sleep(2 * time.Millisecond)
	r.c.Resync()
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
	h := newFakeHub(t, 2, 3)
	r := start(t, h, func(c *Client) { c.resyncEvery = 80 * time.Millisecond })
	r.expect("up", kind(state.EvUp))
	h.renameQuietly(0, "Periodic")
	r.expect("labels", kind(state.EvLabels))
}

func TestReconfigureEmptyHubIdlesDown(t *testing.T) {
	h := newFakeHub(t, 2, 2)
	h.serve()
	st := state.New()
	cfg := config.Defaults()
	cfg.Hub = h.addr()
	var mu = make(chan struct{}, 1)
	mu <- struct{}{}
	get := func() config.Config { <-mu; defer func() { mu <- struct{}{} }(); return cfg }
	c := New(st, get, t.Logf)
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
	cfg.Hub = h.addr()
	mu <- struct{}{}
	c.Reconfigure()
	wait(state.EvUp)
}
