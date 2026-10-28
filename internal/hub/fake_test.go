package hub

import (
	"bufio"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// A minimal fake Videohub derived from cmd/vhsim (package main, so it cannot
// be imported).

type fclient struct {
	conn net.Conn
	wmu  sync.Mutex
	h    *fakeHub
}

func (c *fclient) write(s string) {
	if c.h.silent.Load() {
		return
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, _ = c.conn.Write([]byte(s))
}

type fakeHub struct {
	mu         sync.Mutex
	ln         net.Listener
	inputs     []string
	outputs    []string
	route      []int
	owner      []*fclient
	clients    map[*fclient]bool
	ext        *fclient
	endPrelude bool
	present    string
	silent     atomic.Bool
	once       sync.Once
	routeReqs  []int // output of every routing request received, in order
}

func newFakeHub(t interface{ Cleanup(func()) }, nin, nout int) *fakeHub {
	h := &fakeHub{clients: map[*fclient]bool{}, endPrelude: true, present: "true"}
	h.ext = &fclient{h: h}
	for i := 0; i < nin; i++ {
		h.inputs = append(h.inputs, fmt.Sprintf("Input %d", i+1))
	}
	for i := 0; i < nout; i++ {
		h.outputs = append(h.outputs, fmt.Sprintf("Output %d", i+1))
		h.route = append(h.route, i%nin)
	}
	h.owner = make([]*fclient, nout)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	h.ln = ln
	t.Cleanup(func() { h.mu.Lock(); l := h.ln; h.mu.Unlock(); _ = l.Close(); h.dropAll() })
	return h
}

// serve starts accepting; tests configure the fake before calling it.
func (h *fakeHub) serve() {
	h.once.Do(func() {
		ln := h.ln
		go func() {
			for {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				go h.handle(conn)
			}
		}()
	})
}

func (h *fakeHub) addr() string { return h.ln.Addr().String() }

func (h *fakeHub) dropAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		_ = c.conn.Close()
	}
}

func lockChar(owner, c *fclient) string {
	switch owner {
	case nil:
		return "U"
	case c:
		return "O"
	}
	return "L"
}

func (h *fakeHub) blockFor(hd string, c *fclient, idx []int) string {
	all := func(n int) []int {
		if idx != nil {
			return idx
		}
		r := make([]int, n)
		for i := range r {
			r[i] = i
		}
		return r
	}
	var b strings.Builder
	line := func(i int, v string) { fmt.Fprintf(&b, "%d %s\n", i, v) }
	b.WriteString(hd + ":\n")
	switch hd {
	case "INPUT LABELS":
		for _, i := range all(len(h.inputs)) {
			line(i, h.inputs[i])
		}
	case "OUTPUT LABELS":
		for _, i := range all(len(h.outputs)) {
			line(i, h.outputs[i])
		}
	case "VIDEO OUTPUT ROUTING":
		for _, i := range all(len(h.route)) {
			line(i, strconv.Itoa(h.route[i]))
		}
	case "VIDEO OUTPUT LOCKS":
		for _, i := range all(len(h.owner)) {
			line(i, lockChar(h.owner[i], c))
		}
	default:
		return ""
	}
	b.WriteString("\n")
	return b.String()
}

func (h *fakeHub) broadcast(hd string, idx []int) {
	for c := range h.clients {
		c.write(h.blockFor(hd, c, idx))
	}
}

// externalRoute changes a route as another client or panel would.
func (h *fakeHub) externalRoute(out, in int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.route[out] = in
	h.broadcast("VIDEO OUTPUT ROUTING", []int{out})
}

// renameQuietly changes an output label without telling anyone.
func (h *fakeHub) renameQuietly(out int, name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.outputs[out] = name
}

func (h *fakeHub) lockExternally(out int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.owner[out] = h.ext
	h.broadcast("VIDEO OUTPUT LOCKS", []int{out})
}

func (h *fakeHub) prelude(c *fclient) string {
	var b strings.Builder
	b.WriteString("PROTOCOL PREAMBLE:\r\nVersion: 2.3\r\n\r\n")
	fmt.Fprintf(&b, "VIDEOHUB DEVICE:\nDevice present: %s\nModel name: Fake Hub\nVideo inputs: %d\nVideo outputs: %d\n\n", h.present, len(h.inputs), len(h.outputs))
	if h.present != "true" {
		return b.String()
	}
	for _, hd := range []string{"INPUT LABELS", "OUTPUT LABELS", "VIDEO OUTPUT ROUTING", "VIDEO OUTPUT LOCKS"} {
		b.WriteString(h.blockFor(hd, c, nil))
	}
	if h.endPrelude {
		b.WriteString("END PRELUDE:\n\n")
	}
	return b.String()
}

func (h *fakeHub) handle(conn net.Conn) {
	c := &fclient{conn: conn, h: h}
	h.mu.Lock()
	h.clients[c] = true
	c.write(h.prelude(c))
	h.mu.Unlock()
	defer func() {
		_ = conn.Close()
		h.mu.Lock()
		delete(h.clients, c)
		h.mu.Unlock()
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
		h.command(c, hdr, lines)
		hdr = ""
	}
}

func (h *fakeHub) command(c *fclient, hd string, lines []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.silent.Load() {
		return
	}
	if hd == "PING" {
		c.write("ACK\n\n")
		return
	}
	if len(lines) == 0 {
		if r := h.blockFor(hd, c, nil); r != "" {
			c.write("ACK\n\n" + r)
			return
		}
		c.write("NAK\n\n")
		return
	}
	if hd == "VIDEO OUTPUT LOCKS" {
		h.setLocks(c, lines)
		return
	}
	if hd != "VIDEO OUTPUT ROUTING" {
		c.write("NAK\n\n")
		return
	}
	var changed []int
	for _, l := range lines {
		a, b, _ := strings.Cut(l, " ")
		out, e1 := strconv.Atoi(a)
		in, e2 := strconv.Atoi(b)
		if e1 == nil {
			h.routeReqs = append(h.routeReqs, out)
		}
		if e1 != nil || e2 != nil || out < 0 || out >= len(h.route) || in < 0 || in >= len(h.inputs) ||
			(h.owner[out] != nil && h.owner[out] != c) {
			c.write("NAK\n\n")
			return
		}
		if h.route[out] != in {
			h.route[out] = in
			changed = append(changed, out)
		}
	}
	c.write("ACK\n\n")
	h.broadcast(hd, changed)
}

// setLocks handles "<out> O" (take the lock) and "<out> U" (release it, only
// by its owner). h.mu is held.
func (h *fakeHub) setLocks(c *fclient, lines []string) {
	var changed []int
	for _, l := range lines {
		a, b, _ := strings.Cut(l, " ")
		out, err := strconv.Atoi(a)
		if err != nil || out < 0 || out >= len(h.owner) || (b != "O" && b != "U") ||
			(h.owner[out] != nil && h.owner[out] != c) {
			c.write("NAK\n\n")
			return
		}
		want := (*fclient)(nil)
		if b == "O" {
			want = c
		}
		if h.owner[out] != want {
			h.owner[out] = want
			changed = append(changed, out)
		}
	}
	c.write("ACK\n\n")
	h.broadcast("VIDEO OUTPUT LOCKS", changed)
}

// routeRequests is a copy of the outputs routing was requested for.
func (h *fakeHub) routeRequests() []int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]int(nil), h.routeReqs...)
}

// routeOf is the hub's current input for an output.
func (h *fakeHub) routeOf(out int) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.route[out]
}

// stop closes the listener and every connection, as a powered-off hub does.
func (h *fakeHub) stop() {
	h.mu.Lock()
	ln := h.ln
	h.mu.Unlock()
	_ = ln.Close()
	h.dropAll()
}

// restart listens again on the same address.
func (h *fakeHub) restart() error {
	h.mu.Lock()
	addr := h.ln.Addr().String()
	h.mu.Unlock()
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
	h.mu.Lock()
	h.ln = ln
	h.once = sync.Once{}
	h.mu.Unlock()
	h.serve()
	return nil
}
