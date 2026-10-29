package swp08test

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/sethdotfm/sode-vep/internal/swp08"
)

// fakeRouter accepts connections and ACKs only the messages answer picks.
// The connection stays open either way, like a hung router's would.
func fakeRouter(t *testing.T, answer func(msg []byte) bool) string {
	addr, _ := fakeRouterAccepted(t, answer)
	return addr
}

// fakeRouterAccepted is fakeRouter, plus a channel that gets a value each
// time it accepts a connection.
func fakeRouterAccepted(t *testing.T, answer func(msg []byte) bool) (string, chan struct{}) {
	t.Helper()
	return fakeRouterReplies(t, func(m []byte) []byte {
		if answer(m) {
			return swp08.AckFrame
		}
		return nil
	})
}

// fakeRouterReplies writes whatever reply returns for each message (nil:
// nothing).
func fakeRouterReplies(t *testing.T, reply func(msg []byte) []byte) (string, chan struct{}) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	accepted := make(chan struct{}, 16)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			select {
			case accepted <- struct{}{}:
			default:
			}
			go func() {
				defer conn.Close()
				var d swp08.Decoder
				buf := make([]byte, 1024)
				for {
					n, err := conn.Read(buf)
					if err != nil {
						return
					}
					d.Feed(buf[:n], func(f swp08.Frame) {
						if f.Kind != swp08.KindMsg {
							return
						}
						if b := reply(f.Msg); b != nil {
							conn.Write(b)
						}
					})
				}
			}()
		}
	}()
	return ln.Addr().String(), accepted
}

type stateLog struct {
	mu  sync.Mutex
	got []State
}

func (l *stateLog) add(s State, _ error) {
	l.mu.Lock()
	l.got = append(l.got, s)
	l.mu.Unlock()
}

func (l *stateLog) has(s State) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, g := range l.got {
		if g == s {
			return true
		}
	}
	return false
}

// fastClient is a client with short timings, run until the test ends.
func fastClient(t *testing.T, addr string) (*Client, *stateLog, func()) {
	t.Helper()
	c := NewClient(addr, swp08.ExtInterrogate(0, 0, 0))
	c.ackTimeout, c.probeIdle = 50*time.Millisecond, 300*time.Millisecond
	var log stateLog
	c.OnState = log.add
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	start := func() {
		go func() { c.Run(ctx); close(done) }()
	}
	// Stop and wait for Run, so nothing outlives the test.
	t.Cleanup(func() { cancel(); <-done })
	return c, &log, start
}

// eventually polls cond for up to two seconds.
func eventually(cond func() bool) bool {
	for end := time.Now().Add(2 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return true
		}
	}
	return cond()
}

func runClient(t *testing.T, addr string, onUp func(*Client)) *stateLog {
	t.Helper()
	c, log, start := fastClient(t, addr)
	if onUp != nil {
		c.OnUp = func() { onUp(c) }
	}
	start()
	return log
}

// A router that takes the TCP connection but never answers is never shown
// as up, and the session is dropped.
func TestSilentRouterNeverUp(t *testing.T) {
	addr := fakeRouter(t, func([]byte) bool { return false })
	log := runClient(t, addr, nil)
	if !eventually(func() bool { return log.has(StateDown) }) {
		t.Fatal("silent router never dropped")
	}
	if log.has(StateUp) {
		t.Fatal("silent router was reported up")
	}
}

// A router that ignores one command but answers the probe stays up.
func TestIgnoredCommandKeepsLink(t *testing.T) {
	addr := fakeRouter(t, func(m []byte) bool { return m[0] == swp08.CmdExtInterrogate })
	log := runClient(t, addr, func(c *Client) { c.Send([]byte{0x7F, 0x00}, Why{}) })
	if !eventually(func() bool { return log.has(StateUp) }) {
		t.Fatal("router never came up")
	}
	// The ignored send's tries, its probe, and a couple of idle probes.
	time.Sleep(3*50*time.Millisecond + 3*300*time.Millisecond)
	if log.has(StateDown) {
		t.Fatal("link dropped over one ignored command")
	}
}

// Until the router first answers, Send refuses rather than queueing behind
// a probe that may never be answered.
func TestSendRefusedUntilHeard(t *testing.T) {
	addr, accepted := fakeRouterAccepted(t, func([]byte) bool { return false })
	c, _, start := fastClient(t, addr)
	start()
	select { // connected, so now waiting on the probe's reply
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("never connected")
	}
	if err := c.Send([]byte{0x7F}, Why{}); err != ErrDown {
		t.Fatalf("Send before the first reply = %v, want ErrDown", err)
	}
}

// New settings reconnect without a down state, so the browser shows no
// router-lost alarm for a deliberate change.
func TestRetargetIsNotDown(t *testing.T) {
	addr := fakeRouter(t, func([]byte) bool { return true })
	c, log, start := fastClient(t, addr)
	start()
	if !eventually(func() bool { return log.has(StateUp) }) {
		t.Fatal("router never came up")
	}
	c.SetTarget(addr, swp08.ExtInterrogate(0, 0, 0))
	time.Sleep(200 * time.Millisecond)
	if log.has(StateDown) {
		t.Fatal("retarget reported the router down")
	}
}

// New settings saved while a dial is still running (a wrong address that
// hasn't timed out yet) are a reconnect too, not a failure.
func TestRetargetMidDialIsNotDown(t *testing.T) {
	good := fakeRouter(t, func([]byte) bool { return true })
	c, log, start := fastClient(t, "hangs:8910")
	dialing := make(chan struct{}, 1)
	real := c.dial
	c.dial = func(ctx context.Context, addr string) (net.Conn, error) {
		if addr != good {
			dialing <- struct{}{}
			<-ctx.Done() // a dial that never completes on its own
			return nil, ctx.Err()
		}
		return real(ctx, addr)
	}
	start()
	select {
	case <-dialing:
	case <-time.After(2 * time.Second):
		t.Fatal("never started dialing")
	}
	c.SetTarget(good, swp08.ExtInterrogate(0, 0, 0))
	if !eventually(func() bool { return log.has(StateUp) }) {
		t.Fatal("never came up on the new address")
	}
	if log.has(StateDown) {
		t.Fatal("retarget mid-dial reported the router down")
	}
}

// A settings save while Run waits out a failed dial must not mark the
// next session: a later real drop still reports down.
func TestDropAfterRetargetInBackoffIsDown(t *testing.T) {
	var mu sync.Mutex
	answer := true
	good := fakeRouter(t, func([]byte) bool { mu.Lock(); defer mu.Unlock(); return answer })
	c, log, start := fastClient(t, "127.0.0.1:1") // refused at once: Run backs off
	start()
	if !eventually(func() bool { return log.has(StateDown) }) {
		t.Fatal("bad address never failed")
	}
	log.mu.Lock()
	log.got = nil
	log.mu.Unlock()
	c.SetTarget(good, swp08.ExtInterrogate(0, 0, 0))
	if !eventually(func() bool { return log.has(StateUp) }) {
		t.Fatal("never came up on the new address")
	}
	mu.Lock()
	answer = false // the router goes silent
	mu.Unlock()
	if !eventually(func() bool { return log.has(StateDown) }) {
		t.Fatal("a real drop after a retarget wasn't reported down")
	}
}

type txLog struct {
	mu  sync.Mutex
	got []TxEvent
}

func (l *txLog) add(e TxEvent) {
	l.mu.Lock()
	l.got = append(l.got, e)
	l.mu.Unlock()
}

// find returns the first event for cmd.
func (l *txLog) find(cmd byte) (TxEvent, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.got {
		if e.Msg[0] == cmd {
			return e, true
		}
	}
	return TxEvent{}, false
}

// OnTx reports each message once, with how many writes it took and how it
// ended, and the probe as a keepalive.
func TestOnTx(t *testing.T) {
	var mu sync.Mutex
	naked := false
	addr, _ := fakeRouterReplies(t, func(m []byte) []byte {
		switch m[0] {
		case swp08.CmdExtInterrogate:
			return swp08.AckFrame
		case 0x7F: // NAKed once, then ACKed
			mu.Lock()
			defer mu.Unlock()
			if !naked {
				naked = true
				return swp08.NakFrame
			}
			return swp08.AckFrame
		}
		return nil // 0x7E: never answered
	})
	c, _, start := fastClient(t, addr)
	var tx txLog
	c.OnTx = tx.add
	c.OnUp = func() {
		c.Send([]byte{0x7F}, Why{Reason: "take", By: "10.0.0.9"})
		c.Send([]byte{0x7E}, Why{Reason: "resync"})
	}
	start()
	if !eventually(func() bool { _, ok := tx.find(0x7E); return ok }) {
		t.Fatal("no event for the unanswered message")
	}
	if e, _ := tx.find(swp08.CmdExtInterrogate); e.Why.Reason != "keepalive" || e.Result != TxAck || e.Tries != 1 {
		t.Errorf("probe = %+v, want an ACKed keepalive", e)
	}
	if e, _ := tx.find(0x7F); e.Result != TxAck || e.Tries != 2 || e.Why != (Why{Reason: "take", By: "10.0.0.9"}) {
		t.Errorf("NAKed then ACKed = %+v, want ack after 2 tries", e)
	}
	if e, _ := tx.find(0x7E); e.Result != TxNoAck || e.Tries != sendTries {
		t.Errorf("unanswered = %+v, want noack after %d tries", e, sendTries)
	}
}

// Messages still queued when the connection ends are reported dropped,
// not lost silently.
func TestOnTxDropped(t *testing.T) {
	addr := fakeRouter(t, func(m []byte) bool { return m[0] == swp08.CmdExtInterrogate })
	c, log, start := fastClient(t, addr)
	c.ackTimeout = time.Second // the unanswered message holds the writer
	var tx txLog
	c.OnTx = tx.add
	start()
	if !eventually(func() bool { return log.has(StateUp) }) {
		t.Fatal("router never came up")
	}
	c.Send([]byte{0x7E}, Why{})
	c.Send([]byte{0x7D}, Why{Reason: "take"})
	time.Sleep(100 * time.Millisecond) // 0x7E written, waiting for its ACK
	c.SetTarget(addr, swp08.ExtInterrogate(0, 0, 0))
	if !eventually(func() bool { _, ok := tx.find(0x7D); return ok }) {
		t.Fatal("no event for the queued message")
	}
	if e, _ := tx.find(0x7D); e.Result != TxDropped || e.Tries != 0 || e.Why.Reason != "take" {
		t.Errorf("queued = %+v, want dropped", e)
	}
}
