// Package hub is the Videohub Ethernet Protocol client. It is the only
// writer of state.Store. See docs/videohub-protocol.md for what is verified.
package hub

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
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
	resync  chan struct{}
	reconf  chan struct{}
	lastSyn atomic.Int64
}

func New(store *state.Store, cfg func() config.Config, logf func(string, ...any)) *Client {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Client{
		store: store, cfg: cfg, logf: logf,
		dialTimeout: 5 * time.Second,
		backoffMin:  time.Second,
		backoffMax:  10 * time.Second,
		quiet:       750 * time.Millisecond,
		pingAfter:   10 * time.Second,
		deadAfter:   15 * time.Second,
		cmdTimeout:  3 * time.Second,
		resync:      make(chan struct{}, 1),
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

// Resync forces a re-poll of labels, routes and locks.
func (c *Client) Resync() {
	select {
	case c.resync <- struct{}{}:
	default:
	}
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
	if c.cfg().Debug {
		c.logf(format, a...)
	}
}

// Run connects, serves and reconnects until ctx is done.
func (c *Client) Run(ctx context.Context) {
	backoff := c.backoffMin
	for ctx.Err() == nil {
		c.store.SetDown()
		addr := c.cfg().HubAddr()
		if addr == "" {
			select {
			case <-ctx.Done():
			case <-c.reconf:
			}
			continue
		}
		actx, cancel := context.WithCancel(ctx)
		c.mu.Lock()
		c.cancel = cancel
		c.mu.Unlock()
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
	ready   bool
	devBad  bool
	dev     map[string]string
	inL     map[int]string
	outL    map[int]string
	route   map[int]int
	lock    map[int]byte
	gotRT   bool
	gotLK   bool
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
		inL: map[int]string{}, outL: map[int]string{}, route: map[int]int{}, lock: map[int]byte{},
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
	wasReady = s.ready
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
	if !s.ready {
		s.collect(b)
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	st := s.c.store
	switch b.hdr {
	case hdrInLbl:
		st.ApplyLabels(labelMap(b.lines), nil)
	case hdrOutLbl:
		st.ApplyLabels(nil, labelMap(b.lines))
	case hdrRoute:
		st.ApplyRoutes(routeMap(b.lines))
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

// collect gathers the initial dump. s.mu is held.
func (s *session) collect(b *block) {
	switch b.hdr {
	case hdrDevice:
		s.dev = kv(b.lines)
		if p := s.dev["Device present"]; p != "" && !strings.EqualFold(p, "true") {
			if !s.devBad {
				s.c.logf("hub: device present = %q, waiting", p)
			}
			s.devBad = true
		} else {
			s.devBad = false
		}
	case hdrInLbl:
		s.inL = labelMap(b.lines)
	case hdrOutLbl:
		s.outL = labelMap(b.lines)
	case hdrRoute:
		s.route, s.gotRT = routeMap(b.lines), true
	case hdrLock:
		s.lock, s.gotLK = lockMap(b.lines), true
	case hdrEnd:
		s.finish()
		return
	}
	if s.gotRT && s.gotLK && !s.devBad {
		s.quietTm.Reset(s.c.quiet)
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
	if s.gotRT && s.gotLK {
		s.finish()
	}
}

// finish publishes the initial dump. s.mu is held.
func (s *session) finish() {
	if s.ready || s.devBad {
		return
	}
	s.quietTm.Stop()
	nin, _ := strconv.Atoi(s.dev["Video inputs"])
	nout, _ := strconv.Atoi(s.dev["Video outputs"])
	if nin <= 0 || nin > maxIndex {
		nin = maxKey(s.inL) + 1
		if nin > maxIndex {
			nin = maxIndex
		}
	}
	if nout <= 0 || nout > maxIndex {
		nout = max(maxKey(s.outL), maxKey(s.route), maxKey(s.lock)) + 1
	}
	inputs, outputs := make([]string, nin), make([]string, nout)
	route, lock := make([]int, nout), make([]byte, nout)
	for i := range route {
		route[i], lock[i] = -1, state.Unlocked
	}
	for i, v := range s.inL {
		if i < nin {
			inputs[i] = v
		}
	}
	for i, v := range s.outL {
		if i < nout {
			outputs[i] = v
		}
	}
	for i, v := range s.route {
		if i < nout {
			route[i] = v
		}
	}
	for i, v := range s.lock {
		if i < nout {
			lock[i] = v
		}
	}
	s.ready = true
	s.c.store.SetUp(s.dev["Model name"], inputs, outputs, route, lock)
	s.c.lastSyn.Store(time.Now().UnixNano())
	s.c.logf("hub: ready, %q %dx%d", s.dev["Model name"], nin, nout)
}

func maxKey[V any](m map[int]V) int {
	r := -1
	for k := range m {
		r = max(r, k)
	}
	return r
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
	return s.ready
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
		case <-s.c.resync:
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
