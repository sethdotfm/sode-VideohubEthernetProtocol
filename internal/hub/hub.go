// Package hub is the Videohub Ethernet Protocol client. It is the only
// writer of state.Store. See docs/videohub-protocol.md for what is verified.
package hub

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sethdotfm/sode-vep/internal/config"
	"github.com/sethdotfm/sode-vep/internal/state"
)

const (
	hdrDevice = "VIDEOHUB DEVICE"
	hdrInLbl  = "INPUT LABELS"
	hdrOutLbl = "OUTPUT LABELS"
	hdrRoute  = "VIDEO OUTPUT ROUTING"
	hdrLock   = "VIDEO OUTPUT LOCKS"
	hdrEnd    = "END PRELUDE"
)

var resyncOrder = []string{hdrInLbl, hdrOutLbl, hdrRoute, hdrLock}

var (
	errRefused = errors.New("hub refused")
	errTimeout = errors.New("hub did not answer")
	errDown    = errors.New("hub down")
)

// Client talks to one Videohub. Create with New, run with Run.
type Client struct {
	store *state.Store
	cfg   func() config.Config
	logf  func(string, ...any)
	debug bool // log Videohub wire traffic

	// Timings; tests shorten them before Run.
	dialTimeout time.Duration
	backoffMin  time.Duration
	backoffMax  time.Duration
	quiet       time.Duration // initial dump counts as complete after this much silence
	pingAfter   time.Duration // idle time before PING
	deadAfter   time.Duration // idle time before the link is dropped
	cmdTimeout  time.Duration // wait for ACK/NAK
	resyncEvery time.Duration // overrides cfg().ResyncSeconds when > 0

	mu      sync.Mutex
	cur     *session
	cancel  context.CancelFunc // cancels the current attempt
	reconf  chan struct{}
	lastSyn atomic.Int64
}

func New(store *state.Store, cfg func() config.Config, logf func(string, ...any), debug bool) *Client {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Client{
		store: store, cfg: cfg, logf: logf, debug: debug,
		dialTimeout: 5 * time.Second,
		backoffMin:  time.Second,
		backoffMax:  10 * time.Second,
		quiet:       750 * time.Millisecond,
		pingAfter:   10 * time.Second,
		deadAfter:   15 * time.Second,
		cmdTimeout:  3 * time.Second,
		reconf:      make(chan struct{}, 1),
	}
}

// LastResync is when the last full re-read finished (or the initial dump
// completed); zero if never.
func (c *Client) LastResync() time.Time {
	n := c.lastSyn.Load()
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// Reconfigure drops the connection so a changed hub address takes effect.
func (c *Client) Reconfigure() {
	c.mu.Lock()
	if c.cancel != nil {
		c.cancel()
	}
	c.mu.Unlock()
	select {
	case c.reconf <- struct{}{}:
	default:
	}
}

func (c *Client) debugf(format string, a ...any) {
	if c.debug {
		c.logf(format, a...)
	}
}

// Run connects, serves and reconnects until ctx is done.
func (c *Client) Run(ctx context.Context) {
	backoff := c.backoffMin
	for ctx.Err() == nil {
		c.store.SetDown()
		// cancel is installed before the address is read, so a Reconfigure
		// racing with this loop either cancels this session or leaves a
		// token that ends the next wait.
		actx, cancel := context.WithCancel(ctx)
		c.mu.Lock()
		c.cancel = cancel
		c.mu.Unlock()
		addr := c.cfg().HubAddr()
		if addr == "" {
			select {
			case <-ctx.Done():
			case <-c.reconf:
			}
			c.mu.Lock()
			c.cancel = nil
			c.mu.Unlock()
			cancel()
			continue
		}
		wasReady, err := c.session(actx, addr)
		c.mu.Lock()
		c.cancel = nil
		c.mu.Unlock()
		cancel()
		c.store.SetDown()
		if ctx.Err() != nil {
			return
		}
		c.logf("hub %s: link lost: %v", addr, err)
		if wasReady {
			backoff = c.backoffMin
		}
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
		case <-c.reconf:
			backoff = c.backoffMin
		case <-t.C:
			backoff = min(backoff*2, c.backoffMax)
		}
		t.Stop()
	}
}

type session struct {
	c    *Client
	conn net.Conn
	done chan struct{}

	lastRx  atomic.Int64
	cmdMu   sync.Mutex // one command in flight
	replies chan string

	mu      sync.Mutex // guards the fields below
	d       *dump      // the initial dump; nil once built, which is what ready means
	nin     int        // input count fixed at ready
	quietTm *time.Timer
}

type rxReader struct {
	r  net.Conn
	rx *atomic.Int64
}

func (r rxReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 {
		r.rx.Store(time.Now().UnixNano())
	}
	return n, err
}

// session runs one connection; it returns when the link is gone.
func (c *Client) session(ctx context.Context, addr string) (wasReady bool, err error) {
	d := net.Dialer{Timeout: c.dialTimeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return false, err
	}
	c.logf("hub %s: connected", addr)
	s := &session{
		c: c, conn: conn, done: make(chan struct{}), replies: make(chan string, 8),
		d: newDump(),
	}
	s.lastRx.Store(time.Now().UnixNano())
	s.quietTm = time.AfterFunc(time.Hour, s.quietFired)
	s.quietTm.Stop()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	c.mu.Lock()
	c.cur = s
	c.mu.Unlock()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); s.keepalive() }()
	go func() { defer wg.Done(); s.resyncLoop() }()

	err = s.readLoop()

	c.mu.Lock()
	c.cur = nil
	c.mu.Unlock()
	stop()
	close(s.done)
	_ = conn.Close()
	s.quietTm.Stop()
	wg.Wait()
	s.mu.Lock()
	wasReady = s.d == nil
	s.mu.Unlock()
	c.store.SetDown()
	return wasReady, err
}

func (s *session) readLoop() error {
	sc := bufio.NewScanner(rxReader{s.conn, &s.lastRx})
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	var p parser
	for sc.Scan() {
		line := sc.Text()
		s.c.debugf("hub rx: %q", line)
		b, reply := p.feed(line)
		if reply != "" {
			select {
			case s.replies <- reply:
			default:
			}
		}
		if b != nil {
			s.handle(b)
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return errors.New("connection closed")
}

func (s *session) handle(b *block) {
	s.mu.Lock()
	if s.d != nil {
		s.collect(b)
		s.mu.Unlock()
		return
	}
	nin := s.nin
	s.mu.Unlock()
	st := s.c.store
	switch b.hdr {
	case hdrInLbl:
		st.ApplyLabels(labelMap(b.lines), nil)
	case hdrOutLbl:
		st.ApplyLabels(nil, labelMap(b.lines))
	case hdrRoute:
		rt := routeMap(b.lines)
		for o, in := range rt {
			if in >= nin {
				rt[o] = -1
			}
		}
		st.ApplyRoutes(rt)
	case hdrLock:
		st.ApplyLocks(lockMap(b.lines))
	case hdrDevice:
		if present := kv(b.lines)["Device present"]; present != "" && !strings.EqualFold(present, "true") {
			s.c.logf("hub: device present = %q, dropping link", present)
			s.c.store.SetDown()
			_ = s.conn.Close()
		}
	}
}

// collect feeds the initial dump and arms the quiet fallback. s.mu is held.
func (s *session) collect(b *block) {
	wasBad := s.d.devBad
	if s.d.add(b) {
		s.finish()
		return
	}
	if s.d.devBad && !wasBad {
		s.c.logf("hub: device present = %q, waiting", s.d.dev["Device present"])
	}
	if w := s.d.quietWait(s.c.quiet); w > 0 {
		s.quietTm.Reset(w)
	}
}

func (s *session) quietFired() {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.done:
		return
	default:
	}
	if s.d != nil && s.d.quietWait(s.c.quiet) > 0 {
		s.finish()
	}
}

// finish publishes the initial dump. s.mu is held.
func (s *session) finish() {
	s.quietTm.Stop()
	u := s.d.build()
	s.d = nil
	s.nin = len(u.inputs)
	s.c.store.SetUp(u.model, u.inputs, u.outputs, u.route, u.lock)
	s.c.lastSyn.Store(time.Now().UnixNano())
	s.c.logf("hub: ready, %q %dx%d", u.model, len(u.inputs), len(u.outputs))
}

// command sends text and waits for ACK/NAK; one at a time. A timeout drops
// the link so a late reply can never be mistaken for the next command's.
func (s *session) command(text string) error {
	s.cmdMu.Lock()
	defer s.cmdMu.Unlock()
	for len(s.replies) > 0 {
		<-s.replies
	}
	s.c.debugf("hub tx: %q", text)
	_ = s.conn.SetWriteDeadline(time.Now().Add(s.c.cmdTimeout))
	if _, err := s.conn.Write([]byte(text)); err != nil {
		_ = s.conn.Close()
		return err
	}
	t := time.NewTimer(s.c.cmdTimeout)
	defer t.Stop()
	select {
	case r := <-s.replies:
		if r == "ACK" {
			return nil
		}
		return errRefused
	case <-t.C:
		_ = s.conn.Close()
		return errTimeout
	case <-s.done:
		return errDown
	}
}

func (s *session) isReady() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.d == nil
}

func (s *session) keepalive() {
	tick := max(min(s.c.pingAfter, s.c.deadAfter)/4, time.Millisecond)
	t := time.NewTicker(tick)
	defer t.Stop()
	var pinging atomic.Bool
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
		}
		idle := time.Since(time.Unix(0, s.lastRx.Load()))
		if idle >= s.c.deadAfter {
			s.c.logf("hub: no data for %s, dropping link", idle.Round(time.Millisecond))
			_ = s.conn.Close()
			return
		}
		if idle >= s.c.pingAfter && pinging.CompareAndSwap(false, true) {
			go func() {
				defer pinging.Store(false)
				_ = s.command("PING:\n\n")
			}()
		}
	}
}

func (s *session) resyncLoop() {
	every := func() time.Duration {
		if s.c.resyncEvery > 0 {
			return s.c.resyncEvery
		}
		return time.Duration(max(s.c.cfg().ResyncSeconds, 5)) * time.Second
	}
	t := time.NewTimer(every())
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
		}
		if s.isReady() {
			s.doResync()
		}
		if !t.Stop() {
			select {
			case <-t.C:
			default:
			}
		}
		t.Reset(every())
	}
}

func (s *session) doResync() {
	for _, h := range resyncOrder {
		if err := s.command(h + ":\n\n"); err != nil {
			s.c.logf("hub: resync %s: %v", h, err)
			if !errors.Is(err, errRefused) {
				return
			}
		}
	}
	s.c.lastSyn.Store(time.Now().UnixNano())
}

// Take asks the hub to route in to out. nil means the hub ACKed; the route
// change arrives through the status block and the store.
func (c *Client) Take(out, in int) error {
	snap := c.store.Snapshot()
	if !snap.Up {
		return errDown
	}
	if out < 0 || out >= len(snap.Route) || in < 0 || in >= len(snap.Inputs) {
		return fmt.Errorf("take %d->%d out of range", in, out)
	}
	c.mu.Lock()
	s := c.cur
	c.mu.Unlock()
	if s == nil || !s.isReady() {
		return errDown
	}
	return s.command(fmt.Sprintf("%s:\n%d %d\n\n", hdrRoute, out, in))
}
