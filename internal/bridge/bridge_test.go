package bridge

import (
	"context"
	"log"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/sethdotfm/sode-vep/internal/state"
	"github.com/sethdotfm/sode-vep/internal/swp08"
)

type fakeRouter struct {
	mu    sync.Mutex
	takes [][2]int
	err   error
	ch    chan [2]int
}

func (f *fakeRouter) Take(out, in int) error {
	f.mu.Lock()
	f.takes = append(f.takes, [2]int{out, in})
	f.mu.Unlock()
	f.ch <- [2]int{out, in}
	return f.err
}

type rig struct {
	t     *testing.T
	store *state.Store
	rt    *fakeRouter
	srv   *Server
	addr  string
}

func newRig(t *testing.T, up bool) *rig {
	t.Helper()
	r := &rig{t: t, store: state.New(), rt: &fakeRouter{ch: make(chan [2]int, 16)}}
	if up {
		r.up()
	}
	r.srv = New(r.store, r.rt, func() int { return 9 }, log.Printf)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.srv.Serve(ctx, ln); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	r.addr = ln.Addr().String()
	// Serve subscribes asynchronously; wait until it is accepting state.
	time.Sleep(50 * time.Millisecond)
	return r
}

func (r *rig) up() {
	r.store.SetUp("Hub", []string{"Cam 1", "Café ☃ long name here", "C"},
		[]string{"PGM", "PVW", "Rec B"}, []int{0, 1, 2}, []byte{'U', 'U', 'U'})
}

// tc is a raw SW-P-08 test client.
type tc struct {
	t    *testing.T
	conn net.Conn
	msgs chan []byte
	acks chan bool
}

func (r *rig) dial() *tc {
	r.t.Helper()
	conn, err := net.Dial("tcp", r.addr)
	if err != nil {
		r.t.Fatal(err)
	}
	c := &tc{t: r.t, conn: conn, msgs: make(chan []byte, 256), acks: make(chan bool, 256)}
	r.t.Cleanup(func() { conn.Close() })
	go func() {
		var d swp08.Decoder
		buf := make([]byte, 4096)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				close(c.msgs)
				return
			}
			d.Feed(buf[:n], func(f swp08.Frame) {
				switch f.Kind {
				case swp08.KindMsg:
					c.msgs <- f.Msg
				case swp08.KindAck:
					c.acks <- true
				case swp08.KindNak:
					c.acks <- false
				}
			})
		}
	}()
	return c
}

// send writes a message and expects its ACK.
func (c *tc) send(m []byte) {
	c.t.Helper()
	c.conn.Write(swp08.Encode(m))
	select {
	case ok := <-c.acks:
		if !ok {
			c.t.Fatal("NAK")
		}
	case <-time.After(2 * time.Second):
		c.t.Fatal("no ACK")
	}
}

func (c *tc) next() []byte {
	c.t.Helper()
	select {
	case m, ok := <-c.msgs:
		if !ok {
			c.t.Fatal("connection closed")
		}
		return m
	case <-time.After(2 * time.Second):
		c.t.Fatal("no message")
	}
	return nil
}

func (c *tc) quiet() {
	c.t.Helper()
	select {
	case m := <-c.msgs:
		c.t.Fatalf("unexpected message % X", m)
	case <-time.After(150 * time.Millisecond):
	}
}

func (c *tc) closed() bool {
	select {
	case _, ok := <-c.msgs:
		return !ok
	case <-time.After(2 * time.Second):
		return false
	}
}

func TestACKAndNAK(t *testing.T) {
	r := newRig(t, true)
	c := r.dial()
	c.conn.Write([]byte{swp08.DLE, swp08.STX, 1, 2, 3, swp08.DLE, swp08.ETX}) // bad checksum
	if ok := <-c.acks; ok {
		t.Fatal("want NAK")
	}
	c.send([]byte{0x7F, 1}) // unknown but valid: ACKed, nothing more
	c.quiet()
}

func TestInterrogateAndLevels(t *testing.T) {
	r := newRig(t, true)
	c := r.dial()
	c.send(swp08.ExtInterrogate(3, 0, 2))
	ev, _ := swp08.Parse(c.next())
	if x := ev.Crosspoints[0]; x.Matrix != 3 || x.Dst != 2 || x.Src != 2 || x.Changed() {
		t.Errorf("%+v", x)
	}
	c.send(swp08.ExtInterrogate(0, 1, 2)) // level 1: nothing routed
	ev, _ = swp08.Parse(c.next())
	if x := ev.Crosspoints[0]; x.Level != 1 || x.Src != 0xFFFF {
		t.Errorf("%+v", x)
	}
	c.send(swp08.ExtInterrogate(0, 0, 99)) // out of range: nothing
	c.quiet()
}

func TestTallyDump(t *testing.T) {
	r := newRig(t, true)
	names := make([]string, 130)
	routes := make([]int, 130)
	locks := make([]byte, 130)
	for i := range names {
		names[i], routes[i], locks[i] = "o", i%3, 'U'
	}
	routes[5] = -1
	r.store.SetUp("Hub", []string{"a", "b", "c"}, names, routes, locks)
	time.Sleep(50 * time.Millisecond)
	c := r.dial()
	c.send(swp08.ExtTallyDumpReq(0, 0))
	var got []swp08.Crosspoint
	for range 3 {
		ev, err := swp08.Parse(c.next())
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, ev.Crosspoints...)
	}
	if len(got) != 130 || got[5].Src != 0xFFFF || got[129].Src != 129%3 || got[64].Dst != 64 {
		t.Errorf("dump wrong: n=%d %+v", len(got), got[5])
	}
	c.send(swp08.ExtTallyDumpReq(0, 2))
	for range 3 {
		ev, _ := swp08.Parse(c.next())
		for _, x := range ev.Crosspoints {
			if x.Src != 0xFFFF {
				t.Fatalf("level 2: %+v", x)
			}
		}
	}
}

func TestNames(t *testing.T) {
	r := newRig(t, true)
	c := r.dial()
	for _, ext := range []bool{false, true} {
		c.send(swp08.NamesReq(false, ext, 1, 0, 1)) // 8 chars
		ev, err := swp08.Parse(c.next())
		if err != nil || ev.Names == nil || ev.Names.Matrix != 1 {
			t.Fatalf("ext=%t: %v %+v", ext, err, ev.Names)
		}
		n := ev.Names.Names
		if len(n) != 3 || n[0] != "Cam 1" || n[1] != "Cafe ?  "[:7] && n[1] != "Cafe ? l" || n[2] != "C" {
			t.Errorf("ext=%t names %q", ext, n)
		}
		if n[1] != "Cafe ? l" {
			t.Errorf("fold/cut: %q", n[1])
		}
	}
	c.send(swp08.NamesReq(true, true, 0, 0, 0)) // dest, 4 chars
	ev, _ := swp08.Parse(c.next())
	if !ev.Names.Dest || ev.Names.Names[0] != "PGM" || ev.Names.Names[2] != "Rec" {
		t.Errorf("%+v", ev.Names)
	}
}

func TestConnectCallsTake(t *testing.T) {
	r := newRig(t, true)
	c := r.dial()
	c.send(swp08.ExtConnect(0, 0, 1, 2))
	select {
	case tk := <-r.rt.ch:
		if tk != [2]int{1, 2} {
			t.Errorf("take %v", tk)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no take")
	}
	c.quiet()                             // no direct reply
	c.send(swp08.ExtConnect(0, 1, 1, 2))  // level 1 ignored
	c.send(swp08.ExtConnect(0, 0, 1, 77)) // bad source ignored
	c.quiet()
	if len(r.rt.ch) != 0 {
		t.Error("unexpected take")
	}
}

func TestTakeErrorDoesNotBlock(t *testing.T) {
	r := newRig(t, true)
	r.rt.err = context.DeadlineExceeded
	c := r.dial()
	c.send(swp08.ExtConnect(0, 0, 1, 2))
	<-r.rt.ch
	c.send(swp08.ExtInterrogate(0, 0, 0))
	c.next()
}

func TestRouteChangePushedToAll(t *testing.T) {
	r := newRig(t, true)
	a, b := r.dial(), r.dial()
	a.send(swp08.ExtInterrogate(2, 0, 0)) // a uses matrix 2
	a.next()
	b.send(swp08.ExtInterrogate(0, 0, 0))
	b.next()
	if r.srv.Clients() != 2 {
		t.Errorf("clients %d", r.srv.Clients())
	}
	r.store.ApplyRoutes(map[int]int{1: 2})
	for i, c := range []*tc{a, b} {
		ev, _ := swp08.Parse(c.next())
		x := ev.Crosspoints[0]
		if !x.Changed() || x.Dst != 1 || x.Src != 2 || x.Matrix != []int{2, 0}[i] {
			t.Errorf("client %d: %+v", i, x)
		}
	}
	r.store.ApplyRoutes(map[int]int{1: 2}) // no-op
	a.quiet()
	b.quiet()
}

func TestLockedConnectDropped(t *testing.T) {
	r := newRig(t, true)
	r.store.ApplyLocks(map[int]byte{1: 'L'})
	c := r.dial()
	c.send(swp08.ExtConnect(0, 0, 1, 0))
	c.quiet()
	if len(r.rt.ch) != 0 {
		t.Fatal("take forwarded to locked output")
	}
}

func TestProtect(t *testing.T) {
	r := newRig(t, true)
	r.store.ApplyLocks(map[int]byte{2: 'O'})
	c := r.dial()
	c.send(swp08.ExtProtectDumpReq(0, 0, 0))
	ev, err := swp08.Parse(c.next())
	if err != nil || len(ev.Protects) != 3 || ev.Protects[0].Protected || !ev.Protects[2].Protected || ev.Protects[2].Device != 9 {
		t.Fatalf("%v %+v", err, ev.Protects)
	}
	r.store.ApplyLocks(map[int]byte{0: 'L', 2: 'U'})
	got := map[int]swp08.Protect{}
	for range 2 {
		ev, err := swp08.Parse(c.next())
		if err != nil || len(ev.Protects) != 1 {
			t.Fatal(err, ev)
		}
		got[ev.Protects[0].Dst] = ev.Protects[0]
	}
	if p := got[0]; !p.Protected || p.Device != 9 {
		t.Errorf("lock: %+v", p)
	}
	if p := got[2]; p.Protected {
		t.Errorf("unlock: %+v", p)
	}
}

func TestHubDownAndRecovery(t *testing.T) {
	r := newRig(t, true)
	c := r.dial()
	c.send(swp08.ExtInterrogate(0, 0, 0))
	c.next()
	r.store.SetDown()
	if !c.closed() {
		t.Fatal("session not closed")
	}
	c2 := r.dial() // accepted then closed
	if !c2.closed() {
		t.Fatal("new client not refused")
	}
	r.up()
	time.Sleep(100 * time.Millisecond)
	c3 := r.dial()
	c3.send(swp08.ExtInterrogate(0, 0, 0))
	c3.next()
	r.store.SetDown()
	c3.closed()
	for i := 0; i < 50 && r.srv.Clients() != 0; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if r.srv.Clients() != 0 {
		t.Errorf("clients %d", r.srv.Clients())
	}
}

func TestStartsDown(t *testing.T) {
	r := newRig(t, false)
	if !r.dial().closed() {
		t.Fatal("accepted while hub down")
	}
}

func TestWithRealClient(t *testing.T) {
	r := newRig(t, true)
	got := make(chan []byte, 16)
	cl := swp08.NewClient(r.addr, swp08.ExtInterrogate(0, 0, 0))
	cl.OnMsg = func(m []byte) { got <- m }
	up := make(chan struct{}, 1)
	cl.OnUp = func() { up <- struct{}{} }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go cl.Run(ctx)
	select {
	case <-up:
	case <-time.After(3 * time.Second):
		t.Fatal("client never came up")
	}
	cl.Send(swp08.ExtConnect(0, 0, 0, 2), swp08.Why{})
	select {
	case tk := <-r.rt.ch:
		if tk != [2]int{0, 2} {
			t.Errorf("take %v", tk)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no take")
	}
}

func TestFold(t *testing.T) {
	if len(latin) != 64 {
		t.Fatal(len(latin))
	}
	for in, want := range map[string]string{"Ünï": "Uni", "日本": "??", "abc\x01": "abc?", "toolongname": "toolo"} {
		if got := fold(in, 5); got != want {
			t.Errorf("fold(%q) = %q, want %q", in, got, want)
		}
	}
}
