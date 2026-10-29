package hub

import (
	"bufio"
	"context"
	"net"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sethdotfm/sode-vep/internal/bridge"
	"github.com/sethdotfm/sode-vep/internal/config"
	"github.com/sethdotfm/sode-vep/internal/swp08"
	"github.com/sethdotfm/sode-vep/internal/swp08/swp08test"
	"github.com/sethdotfm/sode-vep/internal/vhsim"
)

// End-to-end: fake Videohub -> hub.Client -> state.Store -> bridge.Server ->
// the real swp08test.Client, over loopback TCP.

const e2eProtectDevice = 7

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	end := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(end) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// swpProbe is a real swp08test.Client that records every message it receives.
type swpProbe struct {
	t    *testing.T
	c    *swp08test.Client
	mu   sync.Mutex
	msgs [][]byte
	up   atomic.Bool
	down atomic.Bool // has been seen down since start
}

func newSwpProbe(t *testing.T, addr string) *swpProbe {
	p := &swpProbe{t: t}
	p.c = swp08test.NewClient(addr, swp08.ExtInterrogate(0, 0, 0))
	p.c.OnMsg = func(m []byte) {
		p.mu.Lock()
		p.msgs = append(p.msgs, append([]byte(nil), m...))
		p.mu.Unlock()
	}
	p.c.OnState = func(s swp08test.State, _ error) {
		p.up.Store(s == swp08test.StateUp)
		if s == swp08test.StateDown {
			p.down.Store(true)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.c.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	eventually(t, "swp08 client up", p.up.Load)
	return p
}

func (p *swpProbe) mark() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.msgs)
}

func (p *swpProbe) since(from int) [][]byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.msgs[from:])
}

// wait returns the first message at or after from that satisfies pred.
func (p *swpProbe) wait(from int, what string, pred func([]byte) bool) []byte {
	p.t.Helper()
	var got []byte
	eventually(p.t, what, func() bool {
		for _, m := range p.since(from) {
			if pred(m) {
				got = m
				return true
			}
		}
		return false
	})
	return got
}

func (p *swpProbe) send(m []byte) {
	p.t.Helper()
	if err := p.c.Send(m, swp08test.Why{}); err != nil {
		p.t.Fatalf("send: %v", err)
	}
}

func isCmd(cmd byte) func([]byte) bool { return func(m []byte) bool { return m[0] == cmd } }

func (p *swpProbe) xp(from int, cmd byte, dst int) swp08.Crosspoint {
	p.t.Helper()
	m := p.wait(from, "xpoint", func(m []byte) bool {
		if m[0] != cmd {
			return false
		}
		ev, err := swp08.Parse(m)
		return err == nil && len(ev.Crosspoints) == 1 && ev.Crosspoints[0].Dst == dst
	})
	ev, _ := swp08.Parse(m)
	return ev.Crosspoints[0]
}

func (p *swpProbe) protect(from int, cmd byte, dst int) swp08.Protect {
	p.t.Helper()
	m := p.wait(from, "protect message", func(m []byte) bool {
		if m[0] != cmd {
			return false
		}
		ev, err := swp08.Parse(m)
		return err == nil && len(ev.Protects) == 1 && ev.Protects[0].Dst == dst
	})
	ev, _ := swp08.Parse(m)
	return ev.Protects[0]
}

// rawHub is a plain TCP client of the fake hub, like a panel.
type rawHub struct {
	t     *testing.T
	conn  net.Conn
	lines chan string
}

func newRawHub(t *testing.T, h *vhsim.Server) *rawHub {
	conn, err := net.Dial("tcp", h.Addr())
	if err != nil {
		t.Fatal(err)
	}
	r := &rawHub{t: t, conn: conn, lines: make(chan string, 4096)}
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		sc := bufio.NewScanner(conn)
		for sc.Scan() {
			select {
			case r.lines <- sc.Text():
			default:
			}
		}
	}()
	r.waitLine("END PRELUDE:")
	return r
}

func (r *rawHub) waitLine(want string) {
	r.t.Helper()
	to := time.After(5 * time.Second)
	for {
		select {
		case l := <-r.lines:
			if l == want {
				return
			}
		case <-to:
			r.t.Fatalf("raw hub client: no %q line", want)
		}
	}
}

// do sends a command and waits for the hub's ACK.
func (r *rawHub) do(cmd string) {
	r.t.Helper()
	if _, err := r.conn.Write([]byte(cmd)); err != nil {
		r.t.Fatal(err)
	}
	r.waitLine("ACK")
}

type e2e struct {
	t    *testing.T
	h    *vhsim.Server
	rig  *rig
	addr string // bridge
}

// newE2E wires hub -> store -> bridge; prep may configure the fake hub first.
func newE2E(t *testing.T, nin, nout int, prep func(*vhsim.Server)) *e2e {
	t.Helper()
	h := newHub(t, nin, nout)
	if prep != nil {
		prep(h)
	}
	r := start(t, h, nil)
	cfg := config.Defaults()
	cfg.ProtectDevice = e2eProtectDevice
	srv := bridge.New(r.st, r.c, func() int { return cfg.ProtectDevice }, t.Logf)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = srv.Serve(ctx, ln); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	eventually(t, "store up", func() bool { return r.st.Snapshot().Up })
	return &e2e{t: t, h: h, rig: r, addr: ln.Addr().String()}
}

func TestE2E(t *testing.T) {
	t.Run("take", func(t *testing.T) {
		e := newE2E(t, 4, 6, nil)
		p := newSwpProbe(t, e.addr)
		from := p.mark()
		p.send(swp08.ExtConnect(0, 0, 2, 3))
		x := p.xp(from, swp08.CmdExtConnected, 2)
		if x.Src != 3 || x.Level != 0 || x.Matrix != 0 {
			t.Fatalf("0x84 = %+v", x)
		}
		if got := e.rig.st.Snapshot().Route[2]; got != 3 {
			t.Errorf("store route = %d", got)
		}
		if got := e.h.Route(2); got != 3 {
			t.Errorf("hub route = %d", got)
		}
	})

	t.Run("noop take", func(t *testing.T) {
		e := newE2E(t, 4, 6, nil)
		p := newSwpProbe(t, e.addr)
		cur := e.h.Route(2) // 2
		from := p.mark()
		p.send(swp08.ExtConnect(0, 0, 2, cur))
		// The hub did get the request, so the silence below means something.
		eventually(t, "hub saw the take", func() bool { return slices.Contains(e.h.RouteRequests(), 2) })
		time.Sleep(400 * time.Millisecond)
		if m := p.since(from); len(m) != 0 {
			t.Fatalf("no-op take produced %x", m)
		}
		from = p.mark()
		p.send(swp08.ExtInterrogate(0, 0, 2))
		if x := p.xp(from, swp08.CmdExtTally, 2); x.Src != cur {
			t.Fatalf("0x83 = %+v, want src %d", x, cur)
		}
	})

	t.Run("panel route", func(t *testing.T) {
		e := newE2E(t, 4, 6, nil)
		p := newSwpProbe(t, e.addr)
		raw := newRawHub(t, e.h)
		from := p.mark()
		raw.do("VIDEO OUTPUT ROUTING:\n4 2\n\n")
		if x := p.xp(from, swp08.CmdExtConnected, 4); x.Src != 2 {
			t.Fatalf("0x84 = %+v", x)
		}
		if got := e.rig.st.Snapshot().Route[4]; got != 2 {
			t.Errorf("store route = %d", got)
		}
	})

	t.Run("locked at startup", func(t *testing.T) {
		e := newE2E(t, 4, 6, func(h *vhsim.Server) { h.LockExternal(1) })
		if l := e.rig.st.Snapshot().Lock[1]; l != 'L' {
			t.Fatalf("store lock = %c, want L", l)
		}
		p := newSwpProbe(t, e.addr)
		from := p.mark()
		p.send(swp08.ExtProtectDumpReq(0, 0, 0))
		m := p.wait(from, "0x94", isCmd(swp08.CmdExtProtectDump))
		ev, err := swp08.Parse(m)
		if err != nil || len(ev.Protects) != 6 {
			t.Fatalf("parse: %v %+v", err, ev)
		}
		for i, pr := range ev.Protects {
			if want := i == 1; pr.Protected != want {
				t.Errorf("output %d protected = %v", i, pr.Protected)
			}
		}
		if ev.Protects[1].Device != e2eProtectDevice {
			t.Errorf("device = %d", ev.Protects[1].Device)
		}

		// A connect to the locked output goes nowhere. Takes are queued in
		// order, so once a control take to output 0 has reached the hub the
		// earlier one has been dealt with.
		before := e.h.Route(1)
		from = p.mark()
		p.send(swp08.ExtConnect(0, 0, 1, 3))
		p.send(swp08.ExtConnect(0, 0, 0, 2))
		if x := p.xp(from, swp08.CmdExtConnected, 0); x.Src != 2 {
			t.Fatalf("control 0x84 = %+v", x)
		}
		if reqs := e.h.RouteRequests(); !reflect.DeepEqual(reqs, []int{0}) {
			t.Errorf("hub routing requests = %v, want only the control [0]", reqs)
		}
		if got := e.h.Route(1); got != before {
			t.Errorf("locked output rerouted %d -> %d", before, got)
		}
	})

	t.Run("lock change live", func(t *testing.T) {
		e := newE2E(t, 4, 6, nil)
		p := newSwpProbe(t, e.addr)
		raw := newRawHub(t, e.h)
		from := p.mark()
		raw.do("VIDEO OUTPUT LOCKS:\n3 O\n\n")
		pr := p.protect(from, swp08.CmdExtProtectConnected, 3)
		if !pr.Protected || pr.Device != e2eProtectDevice {
			t.Fatalf("0x8D = %+v", pr)
		}
		if l := e.rig.st.Snapshot().Lock[3]; l != 'L' {
			t.Errorf("store lock = %c, want L", l)
		}
		from = p.mark()
		raw.do("VIDEO OUTPUT LOCKS:\n3 U\n\n")
		if pr := p.protect(from, swp08.CmdExtProtectDisconnected, 3); pr.Protected {
			t.Fatalf("0x8F = %+v", pr)
		}
	})

	t.Run("fake hub per-connection lock view", func(t *testing.T) {
		h := newHub(t, 2, 2)
		a, b := newRawHub(t, h), newRawHub(t, h)
		a.do("VIDEO OUTPUT LOCKS:\n0 O\n\n")
		b.waitLine("0 L")
		a.waitLine("0 O")
		if _, err := b.conn.Write([]byte("VIDEO OUTPUT ROUTING:\n0 1\n\n")); err != nil {
			t.Fatal(err)
		}
		b.waitLine("NAK")
		if reqs := h.RouteRequests(); !reflect.DeepEqual(reqs, []int{0}) || h.Route(0) != 0 {
			t.Errorf("reqs %v route %d", reqs, h.Route(0))
		}
	})

	t.Run("hub drop", func(t *testing.T) {
		e := newE2E(t, 4, 6, nil)
		p := newSwpProbe(t, e.addr)
		e.h.Stop()
		eventually(t, "swp08 session closed", p.down.Load)
		eventually(t, "store down", func() bool { return !e.rig.st.Snapshot().Up })
		// New connections are accepted and closed at once.
		eventually(t, "bridge refuses", func() bool {
			conn, err := net.Dial("tcp", e.addr)
			if err != nil {
				return true
			}
			defer conn.Close()
			_ = conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
			_, err = conn.Read(make([]byte, 1))
			ne, timeout := err.(net.Error)
			return err != nil && !(timeout && ne.Timeout())
		})
		if err := e.h.Restart(); err != nil {
			t.Fatal(err)
		}
		eventually(t, "store up again", func() bool { return e.rig.st.Snapshot().Up })
		p2 := newSwpProbe(t, e.addr)
		from := p2.mark()
		p2.send(swp08.ExtTallyDumpReq(0, 0))
		m := p2.wait(from, "0x97", isCmd(swp08.CmdExtTallyDump))
		ev, err := swp08.Parse(m)
		if err != nil || len(ev.Crosspoints) != 6 {
			t.Fatalf("parse: %v %+v", err, ev)
		}
		for i, x := range ev.Crosspoints {
			if x.Dst != i || x.Src != i%4 {
				t.Errorf("tally %+v", x)
			}
		}
		from = p2.mark()
		p2.send(swp08.NamesReq(true, true, 0, 0, 3))
		p2.wait(from, "names", isCmd(swp08.CmdExtDstNames))
	})

	t.Run("names", func(t *testing.T) {
		e := newE2E(t, 3, 3, func(h *vhsim.Server) {
			h.SetInputLabel(0, "Café Ünïcode long")
			h.SetInputLabel(1, "日本X")
			h.SetOutputLabel(2, "Schön Monitor 12345")
		})
		p := newSwpProbe(t, e.addr)
		names := func(req []byte, cmd byte) []string {
			from := p.mark()
			p.send(req)
			ev, err := swp08.Parse(p.wait(from, "names", isCmd(cmd)))
			if err != nil || ev.Names == nil {
				t.Fatalf("parse: %v", err)
			}
			return ev.Names.Names
		}
		// Code 1 = 8 characters, extended and standard.
		wantIn := []string{"Cafe Uni", "??X", "Input 3"}
		if got := names(swp08.NamesReq(false, true, 0, 0, 1), swp08.CmdExtSrcNames); !reflect.DeepEqual(got, wantIn) {
			t.Errorf("ext src 8: %q", got)
		}
		if got := names(swp08.NamesReq(false, false, 0, 0, 1), swp08.CmdSrcNames); !reflect.DeepEqual(got, wantIn) {
			t.Errorf("std src 8: %q", got)
		}
		// Code 0 = 4 characters.
		if got := names(swp08.NamesReq(true, false, 0, 0, 0), swp08.CmdDstNames); !reflect.DeepEqual(got, []string{"Outp", "Outp", "Scho"}) {
			t.Errorf("std dst 4: %q", got)
		}
		// Code 3 = 16 characters.
		if got := names(swp08.NamesReq(true, true, 0, 0, 3), swp08.CmdExtDstNames); got[2] != "Schon Monitor 12" {
			t.Errorf("ext dst 16: %q", got)
		}
	})
}
