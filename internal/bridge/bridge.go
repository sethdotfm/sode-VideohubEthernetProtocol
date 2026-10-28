// Package bridge is the SW-P-08 server Sode connects to. It answers from the
// state cache, passes takes to the hub, and pushes route and protect changes
// to every connected client.
package bridge

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sethdotfm/sode-vep/internal/state"
	"github.com/sethdotfm/sode-vep/internal/swp08"
)

const (
	writeTimeout = 2 * time.Second
	queueLen     = 2048 // frames queued per client before it counts as stuck
	none         = 0xFFFF
)

type take struct{ out, in int }

type Server struct {
	store         *state.Store
	router        state.Router
	protectDevice func() int
	logf          func(string, ...any)

	// takes is the one ordered queue of connects for the hub, so takes
	// reach it in the order Sode sent them and a slow hub never blocks a
	// client's read loop.
	takes chan take

	mu      sync.Mutex
	clients map[*client]struct{}
	down    bool
	n       atomic.Int32
}

func New(store *state.Store, router state.Router, protectDevice func() int, logf func(string, ...any)) *Server {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if protectDevice == nil {
		protectDevice = func() int { return 0 }
	}
	return &Server{store: store, router: router, protectDevice: protectDevice, logf: logf,
		takes: make(chan take, 256), clients: map[*client]struct{}{}, down: true}
}

// Clients is how many SW-P-08 clients are connected now.
func (s *Server) Clients() int { return int(s.n.Load()) }

type client struct {
	conn   net.Conn
	out    chan []byte
	done   chan struct{}
	once   sync.Once
	matrix atomic.Int32 // last matrix the client used; pushes go there
}

func (c *client) close() {
	c.once.Do(func() {
		close(c.done)
		c.conn.Close()
	})
}

// send queues a frame. A client whose queue is full is stuck: drop it.
func (c *client) send(b []byte) {
	select {
	case <-c.done:
	case c.out <- b:
	default:
		c.close()
	}
}

func (c *client) msg(m []byte) { c.send(swp08.Encode(m)) }

// writer is the only writer to the connection, so frames never interleave,
// and a stuck peer only ever blocks itself.
func (c *client) writer() {
	for {
		select {
		case <-c.done:
			return
		case b := <-c.out:
			c.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
			if _, err := c.conn.Write(b); err != nil {
				c.close()
				return
			}
		}
	}
}

// Serve accepts clients until ctx ends or the listener fails.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	evs, cancel := s.store.Subscribe()
	defer cancel()
	s.mu.Lock()
	s.down = !s.store.Snapshot().Up
	s.mu.Unlock()

	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	go s.events(ctx, evs)
	go s.takeLoop(ctx)

	for {
		conn, err := ln.Accept()
		if err != nil {
			s.closeAll(true)
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		s.mu.Lock()
		down := s.down
		var c *client
		if !down {
			c = &client{conn: conn, out: make(chan []byte, queueLen), done: make(chan struct{})}
			s.clients[c] = struct{}{}
			s.n.Add(1)
		}
		s.mu.Unlock()
		if down {
			conn.Close()
			continue
		}
		go c.writer()
		go s.serve(c)
	}
}

func (s *Server) closeAll(shutdown bool) {
	s.mu.Lock()
	if shutdown {
		s.down = true
	}
	var all []*client
	for c := range s.clients {
		all = append(all, c)
	}
	s.mu.Unlock()
	for _, c := range all {
		c.close()
	}
}

func (s *Server) setDown(down bool) {
	s.mu.Lock()
	changed := s.down != down
	s.down = down
	s.mu.Unlock()
	if changed {
		s.logf("hub %s: SW-P-08 %s", map[bool]string{true: "down", false: "up"}[down],
			map[bool]string{true: "sessions closed, refusing clients", false: "accepting clients"}[down])
	}
	if down {
		s.closeAll(false)
		s.drainTakes()
	}
}

// drainTakes forgets queued connects: Sode timed them out long ago.
func (s *Server) drainTakes() {
	for {
		select {
		case <-s.takes:
		default:
			return
		}
	}
}

func (s *Server) broadcast(f func(matrix int) [][]byte) {
	s.mu.Lock()
	var all []*client
	for c := range s.clients {
		all = append(all, c)
	}
	s.mu.Unlock()
	for _, c := range all {
		for _, m := range f(int(c.matrix.Load())) {
			c.msg(m)
		}
	}
}

func (s *Server) events(ctx context.Context, evs <-chan state.Event) {
	for {
		var ev state.Event
		select {
		case <-ctx.Done():
			return
		case ev = <-evs:
		}
		switch ev.Kind {
		case state.EvUp:
			s.setDown(false)
		case state.EvDown:
			s.setDown(true)
		case state.EvResync:
			s.setDown(!s.store.Snapshot().Up)
		case state.EvRoute:
			src := ev.In
			if src < 0 {
				src = none
			}
			s.broadcast(func(mx int) [][]byte { return [][]byte{swp08.ExtConnected(mx, 0, ev.Out, src)} })
		case state.EvLock:
			snap := s.store.Snapshot()
			if ev.Out < 0 || ev.Out >= len(snap.Lock) {
				continue
			}
			locked := state.Locked(snap.Lock[ev.Out])
			dev := s.protectDevice()
			s.broadcast(func(mx int) [][]byte {
				if locked {
					return [][]byte{swp08.ExtProtectConnected(mx, 0, ev.Out, dev)}
				}
				return [][]byte{swp08.ExtProtectDisconnected(mx, 0, ev.Out)}
			})
		}
	}
}

func (s *Server) serve(c *client) {
	s.logf("swp08 client %s connected", c.conn.RemoteAddr())
	defer func() {
		c.close()
		s.mu.Lock()
		delete(s.clients, c)
		s.mu.Unlock()
		s.n.Add(-1)
		s.logf("swp08 client %s gone", c.conn.RemoteAddr())
	}()
	var dec swp08.Decoder
	buf := make([]byte, 4096)
	for {
		n, err := c.conn.Read(buf)
		if err != nil {
			return
		}
		dec.Feed(buf[:n], func(f swp08.Frame) {
			switch f.Kind {
			case swp08.KindBad:
				c.send(swp08.NakFrame)
			case swp08.KindMsg:
				c.send(swp08.AckFrame)
				s.handle(c, f.Msg)
			}
		})
	}
}

// takeLoop hands queued connects to the router one at a time.
func (s *Server) takeLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case t := <-s.takes:
			if !s.store.Snapshot().Up {
				continue
			}
			if err := s.router.Take(t.out, t.in); err != nil {
				s.logf("take %d <- %d: %v", t.out, t.in, err)
			}
		}
	}
}
