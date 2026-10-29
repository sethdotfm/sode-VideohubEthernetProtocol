// Package vhsim is a fake Blackmagic Videohub speaking the Videohub Ethernet
// Protocol (docs/videohub-protocol.md). It sends the full preamble on connect,
// answers block requests and PING, ACKs/NAKs changes and broadcasts changed
// lines to every client, the requester included. It is the single fake hub:
// cmd/vhsim runs it, and the hub tests drive it through the methods below.
package vhsim

import (
	"bufio"
	"context"
	"fmt"
	"math/rand/v2"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Options configures a Server. Inputs and Outputs must be >= 1.
type Options struct {
	Model   string
	Inputs  int
	Outputs int
	Locks   []int // outputs locked at startup by another client
	// Quirks for exercising the client; all off by default.
	NoEndPrelude bool // omit END PRELUDE (the v2.3 document has none)
	DeviceAbsent bool // "Device present: false" and no blocks
	CRLFPreamble bool // CRLF line ends in the protocol preamble
}

type client struct {
	conn   net.Conn
	wmu    sync.Mutex
	silent *atomic.Bool
}

func (c *client) write(s string) {
	if c.silent != nil && c.silent.Load() {
		return
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, _ = c.conn.Write([]byte(s))
}

// Server is a fake Videohub.
type Server struct {
	opt     Options
	silent  atomic.Bool
	mu      sync.Mutex
	ln      net.Listener
	addr    string
	reqs    []int // output of every routing request received, in order
	model   string
	inputs  []string
	outputs []string
	route   []int
	owner   []*client // lock owner per output; nil = unlocked
	clients map[*client]bool
	down    bool
	ext     *client // the "other client" that holds startup locks
}

// New builds a Server; call Start or Serve to listen.
func New(o Options) *Server {
	nin, nout := o.Inputs, o.Outputs
	s := &Server{opt: o, model: o.Model, clients: map[*client]bool{}, ext: &client{}}
	for i := 0; i < nin; i++ {
		s.inputs = append(s.inputs, fmt.Sprintf("Input %d", i+1))
	}
	for i := 0; i < nout; i++ {
		s.outputs = append(s.outputs, fmt.Sprintf("Output %d", i+1))
		s.route = append(s.route, i%nin)
	}
	s.owner = make([]*client, nout)
	for _, n := range o.Locks {
		if n >= 0 && n < nout {
			s.owner[n] = s.ext
		}
	}
	return s
}

func lockChar(owner, c *client) string {
	switch owner {
	case nil:
		return "U"
	case c:
		return "O"
	}
	return "L"
}

func block(h string, lines []string) string {
	return h + ":\n" + strings.Join(lines, "") + "\n"
}

func intLines(idx []int, val func(int) string) []string {
	var l []string
	for _, i := range idx {
		l = append(l, fmt.Sprintf("%d %s\n", i, val(i)))
	}
	return l
}

func seq(n int) []int {
	r := make([]int, n)
	for i := range r {
		r[i] = i
	}
	return r
}

// blockFor renders header h for client c limited to idx; "" if h is unknown.
// Caller holds s.mu.
func (s *Server) blockFor(h string, c *client, idx []int) string {
	all := func(n int) []int {
		if idx == nil {
			return seq(n)
		}
		return idx
	}
	switch h {
	case "INPUT LABELS":
		return block(h, intLines(all(len(s.inputs)), func(i int) string { return s.inputs[i] }))
	case "OUTPUT LABELS":
		return block(h, intLines(all(len(s.outputs)), func(i int) string { return s.outputs[i] }))
	case "VIDEO OUTPUT ROUTING":
		return block(h, intLines(all(len(s.route)), func(i int) string { return strconv.Itoa(s.route[i]) }))
	case "VIDEO OUTPUT LOCKS":
		return block(h, intLines(all(len(s.owner)), func(i int) string { return lockChar(s.owner[i], c) }))
	}
	return ""
}

func (s *Server) preamble(c *client) string {
	var b strings.Builder
	nl := "\n"
	if s.opt.CRLFPreamble {
		nl = "\r\n"
	}
	b.WriteString("PROTOCOL PREAMBLE:" + nl + "Version: 2.3" + nl + nl)
	present := "true"
	if s.opt.DeviceAbsent {
		present = "false"
	}
	fmt.Fprintf(&b, "VIDEOHUB DEVICE:\nDevice present: %s\nModel name: %s\nVideo inputs: %d\nVideo processing units: 0\nVideo outputs: %d\nVideo monitoring outputs: 0\nSerial ports: 0\n\n",
		present, s.model, len(s.inputs), len(s.outputs))
	if s.opt.DeviceAbsent {
		return b.String()
	}
	for _, h := range []string{"INPUT LABELS", "OUTPUT LABELS", "VIDEO OUTPUT ROUTING", "VIDEO OUTPUT LOCKS"} {
		b.WriteString(s.blockFor(h, c, nil))
	}
	if !s.opt.NoEndPrelude {
		b.WriteString("END PRELUDE:\n\n")
	}
	return b.String()
}

// broadcast sends the changed lines of header h to every client (requester
// included). Caller holds s.mu.
func (s *Server) broadcast(h string, idx []int) {
	if len(idx) == 0 {
		return
	}
	for c := range s.clients {
		c.write(s.blockFor(h, c, idx))
	}
}

func (s *Server) setRoute(out, in int) bool {
	if s.route[out] == in {
		return false
	}
	s.route[out] = in
	return true
}

// PanelOnce changes one random route as a hardware panel would.
func (s *Server) PanelOnce() {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := rand.IntN(len(s.outputs))
	in := rand.IntN(len(s.inputs))
	if s.setRoute(out, in) {
		s.broadcast("VIDEO OUTPUT ROUTING", []int{out})
	}
}

// RunPanel calls PanelOnce every d until ctx is done.
func (s *Server) RunPanel(ctx context.Context, d time.Duration) {
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.PanelOnce()
		}
	}
}

// SetDown drops every client and refuses new ones (while true); false lets
// clients in again. The listener stays open.
func (s *Server) SetDown(d bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.down = d
	if d {
		for c := range s.clients {
			_ = c.conn.Close()
		}
	}
}

// RunDownCycle waits after, goes down for dur, then comes back up.
func (s *Server) RunDownCycle(ctx context.Context, after, dur time.Duration, logf func(string, ...any)) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(after):
	}
	logf("going down for %s", dur)
	s.SetDown(true)
	select {
	case <-ctx.Done():
	case <-time.After(dur):
	}
	s.SetDown(false)
	logf("back up")
}

// Start listens on addr and serves in the background.
func (s *Server) Start(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.setListener(ln)
	go s.accept(ln)
	return nil
}

// Serve serves on ln until it is closed (by Stop or the caller).
func (s *Server) Serve(ln net.Listener) error {
	s.setListener(ln)
	s.accept(ln)
	return nil
}

func (s *Server) setListener(ln net.Listener) {
	s.mu.Lock()
	s.ln, s.addr = ln, ln.Addr().String()
	s.mu.Unlock()
}

func (s *Server) accept(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

// Addr is the address being listened on (kept after Stop for Restart).
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

// Stop closes the listener and every connection, as a powered-off hub does.
func (s *Server) Stop() {
	s.mu.Lock()
	ln := s.ln
	s.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
	s.DropClients()
}

// Restart listens again on the same address, retrying while it is released.
func (s *Server) Restart() error {
	addr := s.Addr()
	var ln net.Listener
	var err error
	for i := 0; i < 50; i++ {
		if ln, err = net.Listen("tcp", addr); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		return err
	}
	s.setListener(ln)
	go s.accept(ln)
	return nil
}

// DropClients closes every connection but keeps listening.
func (s *Server) DropClients() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.clients {
		_ = c.conn.Close()
	}
}

// SetSilent makes the server stop answering and sending (a hung hub).
func (s *Server) SetSilent(v bool) { s.silent.Store(v) }

// Route is the current input for an output.
func (s *Server) Route(out int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.route[out]
}

// RouteRequests is a copy of the outputs routing was requested for, in order,
// including requests that were NAKed.
func (s *Server) RouteRequests() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int(nil), s.reqs...)
}

// SetRoute changes a route as another client or a panel would, telling everyone.
func (s *Server) SetRoute(out, in int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.route[out] = in
	s.broadcast("VIDEO OUTPUT ROUTING", []int{out})
}

// SetInputLabel and SetOutputLabel change a label without telling anyone.
func (s *Server) SetInputLabel(i int, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inputs[i] = name
}

func (s *Server) SetOutputLabel(i int, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.outputs[i] = name
}

// LockExternal locks an output as another client, telling everyone.
func (s *Server) LockExternal(out int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.owner[out] = s.ext
	s.broadcast("VIDEO OUTPUT LOCKS", []int{out})
}

type entry struct {
	idx int
	val string
}

func parseLines(lines []string, max int) ([]entry, bool) {
	var es []entry
	for _, l := range lines {
		a, b, _ := strings.Cut(l, " ")
		n, err := strconv.Atoi(a)
		if err != nil || n < 0 || n >= max {
			return nil, false
		}
		es = append(es, entry{n, b})
	}
	return es, true
}

func (s *Server) handle(conn net.Conn) {
	c := &client{conn: conn, silent: &s.silent}
	s.mu.Lock()
	if s.down {
		s.mu.Unlock()
		_ = conn.Close()
		return
	}
	s.clients[c] = true
	c.write(s.preamble(c))
	s.mu.Unlock()
	defer func() {
		_ = conn.Close()
		s.mu.Lock()
		delete(s.clients, c)
		var freed []int
		for i, o := range s.owner {
			if o == c {
				s.owner[i] = nil
				freed = append(freed, i)
			}
		}
		s.broadcast("VIDEO OUTPUT LOCKS", freed)
		s.mu.Unlock()
	}()

	sc := bufio.NewScanner(conn)
	var hdr string
	var lines []string
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if hdr == "" {
			if line != "" {
				hdr = strings.TrimSuffix(line, ":")
				lines = nil
			}
			continue
		}
		if line != "" {
			lines = append(lines, line)
			continue
		}
		s.command(c, hdr, lines)
		hdr = ""
	}
}

func (s *Server) command(c *client, h string, lines []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.silent.Load() {
		return
	}
	nak := func() { c.write("NAK\n\n") }
	if h == "PING" {
		c.write("ACK\n\n")
		return
	}
	if len(lines) == 0 { // block request
		if r := s.blockFor(h, c, nil); r != "" {
			c.write("ACK\n\n" + r)
			return
		}
		nak()
		return
	}
	if h == "VIDEO OUTPUT ROUTING" {
		for _, l := range lines {
			a, _, _ := strings.Cut(l, " ")
			if out, err := strconv.Atoi(a); err == nil {
				s.reqs = append(s.reqs, out)
			}
		}
	}
	var max int
	switch h {
	case "INPUT LABELS":
		max = len(s.inputs)
	case "OUTPUT LABELS", "VIDEO OUTPUT LOCKS", "VIDEO OUTPUT ROUTING":
		max = len(s.outputs)
	default:
		nak()
		return
	}
	es, ok := parseLines(lines, max)
	if !ok {
		nak()
		return
	}
	var changed []int
	switch h {
	case "INPUT LABELS", "OUTPUT LABELS":
		dst := s.inputs
		if h == "OUTPUT LABELS" {
			dst = s.outputs
		}
		for _, e := range es {
			if e.val == "" {
				nak()
				return
			}
		}
		for _, e := range es {
			if dst[e.idx] != e.val {
				dst[e.idx] = e.val
				changed = append(changed, e.idx)
			}
		}
	case "VIDEO OUTPUT ROUTING":
		for _, e := range es {
			in, err := strconv.Atoi(e.val)
			if err != nil || in < 0 || in >= len(s.inputs) || (s.owner[e.idx] != nil && s.owner[e.idx] != c) {
				nak()
				return
			}
		}
		for _, e := range es {
			in, _ := strconv.Atoi(e.val)
			if s.setRoute(e.idx, in) {
				changed = append(changed, e.idx)
			}
		}
	case "VIDEO OUTPUT LOCKS":
		for _, e := range es {
			if e.val != "O" && e.val != "U" && e.val != "F" {
				nak()
				return
			}
			if e.val == "U" && s.owner[e.idx] != nil && s.owner[e.idx] != c {
				nak() // only the owner may unlock; others use F
				return
			}
			if e.val == "O" && s.owner[e.idx] != nil && s.owner[e.idx] != c {
				nak()
				return
			}
		}
		for _, e := range es {
			var n *client
			if e.val == "O" {
				n = c
			}
			if s.owner[e.idx] != n {
				s.owner[e.idx] = n
				changed = append(changed, e.idx)
			}
		}
	}
	c.write("ACK\n\n")
	s.broadcast(h, changed)
}
