// Package swp08test is a copy of Sode's controller-side SW-P-08 client, kept
// only to test the bridge against (no product code uses it). Fixes should be
// mirrored from Sode.
package swp08test

import (
	"context"
	"errors"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sethdotfm/sode-vep/internal/swp08"
)

var (
	ErrDown = errors.New("router not connected")
	ErrBusy = errors.New("router send queue full")
)

const (
	ackTimeout  = time.Second
	probeIdle   = 10 * time.Second // send the probe after this long without rx
	sendTries   = 3
	maxBackoff  = 10 * time.Second
	stableAfter = 30 * time.Second // a session this long resets the backoff
)

type State string

const (
	StateConnecting State = "connecting"
	StateUp         State = "up"
	StateDown       State = "down"
)

// Reason is why a message was sent, as the sent log records it (it is
// logged as this string).
type Reason string

const (
	ReasonTake      Reason = "take"
	ReasonUndo      Reason = "undo"
	ReasonRetry     Reason = "retry"
	ReasonCheck     Reason = "check"
	ReasonConfirm   Reason = "confirm"
	ReasonResync    Reason = "resync"
	ReasonNames     Reason = "names"
	ReasonKeepalive Reason = "keepalive"
)

// Why says what a message was sent for, for the sent log.
type Why struct {
	Reason Reason
	By     string // the browser's address, for take traffic
	// Done, if set, is closed once the message's fate is known: ACKed,
	// given up on, or dropped. Callers use it to count their timeouts from
	// when the router could have seen the message, not from when it queued.
	Done chan struct{}
}

// Tx results, as reported to OnTx.
const (
	TxAck     = "ack"     // the router ACKed it
	TxNak     = "nak"     // NAKed on the last try; gave up
	TxNoAck   = "noack"   // written, nothing back; gave up
	TxWrite   = "write"   // the write itself failed
	TxDropped = "dropped" // queued, but the connection ended before it was written
)

// TxEvent is one message's fate on the wire: one per message, however
// many times it was written.
type TxEvent struct {
	Msg    []byte
	Why    Why
	At     time.Time // first write (when queued, for dropped)
	Tries  int
	Result string
	Err    string // for TxWrite
}

type out struct {
	msg []byte
	why Why
	at  time.Time
}

// Client keeps one TCP connection to a router alive and serializes sends,
// waiting for each ACK before the next message as SW-P-08 requires.
type Client struct {
	Debug bool
	// OnUp runs once per connection, after Send is usable.
	OnUp func()
	// OnMsg runs on the reader goroutine for each valid message; keep it quick.
	OnMsg func(msg []byte)
	// OnState reports connection changes; err explains a drop.
	OnState func(s State, err error)
	// OnTx reports each message written to the router (or dropped from the
	// queue) once its outcome is known. It runs on the writer goroutine,
	// which waits for ACKs: it must not block.
	OnTx func(TxEvent)

	// Timings, from the constants; tests shorten them.
	ackTimeout, probeIdle time.Duration
	// dial opens the TCP connection; tests replace it.
	dial func(ctx context.Context, addr string) (net.Conn, error)

	mu     sync.Mutex
	addr   string
	probe  []byte             // sent on connect and when the link is quiet; must provoke a reply
	q      chan out           // nil until the router has answered
	cancel context.CancelFunc // ends the current session
	kick   chan struct{}

	wmu sync.Mutex
}

var (
	errNoRouter = errors.New("no router configured")
	errRetarget = errors.New("reconnecting with new settings")
)

func NewClient(addr string, probe []byte) *Client {
	d := net.Dialer{Timeout: 5 * time.Second, KeepAlive: 10 * time.Second}
	return &Client{addr: addr, probe: probe, kick: make(chan struct{}, 1),
		ackTimeout: ackTimeout, probeIdle: probeIdle,
		dial: func(ctx context.Context, addr string) (net.Conn, error) { return d.DialContext(ctx, "tcp", addr) }}
}

// SetTarget switches router and reconnects at once. An empty addr idles.
func (c *Client) SetTarget(addr string, probe []byte) {
	c.mu.Lock()
	c.addr, c.probe = addr, probe
	cancel := c.cancel
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

func (c *Client) target() (string, []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.addr, c.probe
}

// Send queues a message. Messages queued on a connection that drops are
// discarded, never replayed on the next one: a stale take must not fire later.
//
// The push happens under mu, so once a session has cleared c.q nothing more
// lands in its queue and every message there is reported (sent or dropped).
func (c *Client) Send(msg []byte, why Why) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.q == nil {
		return ErrDown
	}
	select {
	case c.q <- out{msg, why, time.Now()}:
		return nil
	default:
		return ErrBusy
	}
}

func (c *Client) Run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		select { // a kick from before this attempt is already honored
		case <-c.kick:
		default:
		}
		addr, _ := c.target()
		if addr == "" {
			c.state(StateDown, errNoRouter)
			select {
			case <-ctx.Done():
				return
			case <-c.kick:
			}
			continue
		}
		c.state(StateConnecting, nil)
		start := time.Now()
		err := c.session(ctx, addr)
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, errRetarget) {
			// New settings, not a failure: straight back to connecting.
			log.Printf("swp08: %s: %v", addr, err)
			backoff = time.Second
			continue
		}
		c.state(StateDown, err)
		if time.Since(start) > stableAfter {
			backoff = time.Second
		}
		log.Printf("swp08: %s: %v (retry in %s)", addr, err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-c.kick: // new target: retry now
			backoff = time.Second
			continue
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

func (c *Client) state(s State, err error) {
	if c.OnState != nil {
		c.OnState(s, err)
	}
}

// session runs one connection. Ended by SetTarget (new settings) at any
// point, mid-dial included, it returns errRetarget whatever error that
// surfaced as: a reconnect, not a failure.
func (c *Client) session(ctx context.Context, addr string) (err error) {
	sctx, cancel := context.WithCancel(ctx)
	var q chan out
	var wdone chan struct{} // closed when the writer returns
	defer func() {
		// A retarget is told by sctx ending first, so before cancel.
		if ctx.Err() == nil && sctx.Err() != nil {
			err = errRetarget
		}
		c.mu.Lock()
		c.q = nil // no more Sends into q
		c.cancel = nil
		c.mu.Unlock()
		cancel()
		if wdone != nil {
			<-wdone // nothing still taking from q
		}
		// Whatever the writer didn't get to is never sent.
		for q != nil {
			select {
			case m := <-q:
				c.tx(TxEvent{Msg: m.msg, Why: m.why, At: m.at, Result: TxDropped})
			default:
				q = nil
			}
		}
	}()
	c.mu.Lock()
	c.cancel = cancel
	stale := c.addr != addr // SetTarget ran before this session could be cancelled
	c.mu.Unlock()
	if stale {
		return errRetarget
	}

	conn, err := c.dial(sctx, addr)
	if err != nil {
		return err
	}
	log.Printf("swp08: connected to %s", addr)
	go func() {
		<-sctx.Done()
		conn.Close()
	}()

	q = make(chan out, 4096)
	acks := make(chan bool, 1)
	rx := newLastRx()

	errc := make(chan error, 2)
	go func() { errc <- c.reader(conn, acks, rx) }()
	wdone = make(chan struct{})
	go func() { errc <- c.writer(sctx, conn, q, acks, rx); close(wdone) }()

	// wait blocks until ch is closed (nil: forever) or the session ends;
	// nil means ch closed.
	wait := func(ch <-chan struct{}) error {
		select {
		case <-ch:
			return nil
		case err := <-errc:
			return err
		case <-sctx.Done():
			if ctx.Err() == nil {
				return errRetarget
			}
			return ctx.Err()
		}
	}

	// Up only once the router answers: a hung one still accepts TCP. The
	// writer's first probe provokes that reply. Until then Send says
	// ErrDown, so nothing queues behind a probe that may never be answered.
	if _, probe := c.target(); probe != nil {
		if err := wait(rx.heard); err != nil {
			return err
		}
	}
	c.mu.Lock()
	c.q = q
	c.mu.Unlock()
	c.state(StateUp, nil)
	if c.OnUp != nil {
		c.OnUp()
	}
	return wait(nil)
}

func (c *Client) tx(e TxEvent) {
	if c.OnTx != nil {
		c.OnTx(e)
	}
	if e.Why.Done != nil {
		close(e.Why.Done)
	}
}

func (c *Client) write(conn net.Conn, b []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.Debug {
		log.Printf("swp08: tx % X", b)
	}
	conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, err := conn.Write(b)
	return err
}

// lastRx tracks when the router last sent anything, as a monotonic offset
// from the session's start, so a wall-clock step can't fake or hide a reply.
type lastRx struct {
	start time.Time
	at    atomic.Int64  // time.Duration since start of the latest rx
	heard chan struct{} // closed on the first rx
	once  sync.Once
}

func newLastRx() *lastRx {
	return &lastRx{start: time.Now(), heard: make(chan struct{})}
}

func (l *lastRx) now() time.Duration { return time.Since(l.start) }

func (l *lastRx) mark() {
	l.at.Store(int64(l.now()))
	l.once.Do(func() { close(l.heard) })
}

// since reports whether anything arrived after t (from now()).
func (l *lastRx) since(t time.Duration) bool { return time.Duration(l.at.Load()) > t }

func (l *lastRx) idle() time.Duration { return l.now() - time.Duration(l.at.Load()) }

func (c *Client) reader(conn net.Conn, acks chan bool, rx *lastRx) error {
	var dec swp08.Decoder
	buf := make([]byte, 4096)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return err
		}
		rx.mark()
		if c.Debug {
			log.Printf("swp08: rx % X", buf[:n])
		}
		var werr error
		dec.Feed(buf[:n], func(f swp08.Frame) {
			switch f.Kind {
			case swp08.KindAck, swp08.KindNak:
				select {
				case acks <- f.Kind == swp08.KindAck:
				default:
				}
			case swp08.KindBad:
				log.Printf("swp08: %v, sending NAK", f.Err)
				werr = c.write(conn, swp08.NakFrame)
			case swp08.KindMsg:
				if f.Err != nil && c.Debug {
					log.Printf("swp08: %v (accepted)", f.Err)
				}
				werr = c.write(conn, swp08.AckFrame)
				if c.OnMsg != nil {
					c.OnMsg(f.Msg)
				}
			}
		})
		if werr != nil {
			return werr
		}
	}
}

// writer sends queued messages one at a time and keeps the link checked.
// The router going quiet with the TCP session still open (powered off,
// cable pulled, hung) shows up as a probe that gets nothing back at all,
// and drops the connection so it shows as down and reconnects. A message
// that alone goes unanswered (a command this router ignores) only costs a
// probe: if that's answered, the router is fine.
func (c *Client) writer(ctx context.Context, conn net.Conn, q chan out, acks chan bool, rx *lastRx) error {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	check := func() error {
		_, probe := c.target()
		if probe == nil {
			return nil
		}
		heard, err := c.transmit(ctx, conn, out{msg: probe, why: Why{Reason: ReasonKeepalive}}, acks, rx)
		if err == nil && !heard {
			err = errors.New("no reply from router")
		}
		return err
	}
	// The first probe, sent at once: the session waits for its reply.
	if err := check(); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case m := <-q:
			if ctx.Err() != nil { // picked over Done: it won't be written
				c.tx(TxEvent{Msg: m.msg, Why: m.why, At: m.at, Result: TxDropped})
				return ctx.Err()
			}
			heard, err := c.transmit(ctx, conn, m, acks, rx)
			if err != nil {
				return err
			}
			if !heard {
				if err := check(); err != nil {
					return err
				}
			}
		case <-tick.C:
			if rx.idle() > c.probeIdle {
				if err := check(); err != nil {
					return err
				}
			}
		}
	}
}

// transmit sends o and waits for its ACK, retrying on NAK or silence.
// heard reports whether anything at all came back from the router while
// it tried.
func (c *Client) transmit(ctx context.Context, conn net.Conn, o out, acks chan bool, rx *lastRx) (heard bool, err error) {
	m := o.msg
	frame := swp08.Encode(m)
	sent := rx.now()
	e := TxEvent{Msg: m, Why: o.why, At: time.Now(), Result: TxNoAck}
	defer func() { c.tx(e) }()
	for try := 1; try <= sendTries; try++ {
		select { // discard any stray ACK left from before
		case <-acks:
		default:
		}
		e.Tries = try
		if err := c.write(conn, frame); err != nil {
			e.Result, e.Err = TxWrite, err.Error()
			return false, err
		}
		select {
		case ok := <-acks:
			if ok {
				e.Result = TxAck
				return true, nil
			}
			e.Result = TxNak
			log.Printf("swp08: NAK for cmd 0x%02X (try %d/%d)", m[0], try, sendTries)
		case <-time.After(c.ackTimeout):
			e.Result = TxNoAck
			log.Printf("swp08: no ACK for cmd 0x%02X (try %d/%d)", m[0], try, sendTries)
		case <-ctx.Done():
			e.Result = TxNoAck
			return false, ctx.Err()
		}
	}
	log.Printf("swp08: gave up on cmd 0x%02X after %d tries", m[0], sendTries)
	return rx.since(sent), nil
}
