// Command vhsim is a fake Blackmagic Videohub for development, speaking the
// Videohub Ethernet Protocol (docs/videohub-protocol.md). It sends the full
// preamble on connect, answers block requests and PING, ACKs/NAKs changes and
// broadcasts changed lines to every client, the requester included.
//
// Env: VHSIM_PORT (9990), VHSIM_INPUTS (40), VHSIM_OUTPUTS (40), VHSIM_MODEL,
// VHSIM_PANEL_SECONDS (hardware-panel route every N s, 0=off), VHSIM_LOCKS
// ("3,5": outputs locked by another client), VHSIM_DOWN_AFTER / VHSIM_DOWN_FOR
// (seconds: drop all clients and refuse connections, 0=off).
package main

import (
	"bufio"
	"fmt"
	"log"
	"math/rand/v2"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type client struct {
	conn net.Conn
	wmu  sync.Mutex
}

func (c *client) write(s string) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, _ = c.conn.Write([]byte(s))
}

type sim struct {
	mu      sync.Mutex
	model   string
	inputs  []string
	outputs []string
	route   []int
	owner   []*client // lock owner per output; nil = unlocked
	clients map[*client]bool
	down    bool
	ext     *client // the "other client" that holds startup locks
}

func newSim(model string, nin, nout int) *sim {
	s := &sim{model: model, clients: map[*client]bool{}, ext: &client{}}
	for i := 0; i < nin; i++ {
		s.inputs = append(s.inputs, fmt.Sprintf("Input %d", i+1))
	}
	for i := 0; i < nout; i++ {
		s.outputs = append(s.outputs, fmt.Sprintf("Output %d", i+1))
		s.route = append(s.route, i%nin)
	}
	s.owner = make([]*client, nout)
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
func (s *sim) blockFor(h string, c *client, idx []int) string {
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

func (s *sim) preamble(c *client) string {
	var b strings.Builder
	b.WriteString("PROTOCOL PREAMBLE:\nVersion: 2.3\n\n")
	fmt.Fprintf(&b, "VIDEOHUB DEVICE:\nDevice present: true\nModel name: %s\nVideo inputs: %d\nVideo processing units: 0\nVideo outputs: %d\nVideo monitoring outputs: 0\nSerial ports: 0\n\n",
		s.model, len(s.inputs), len(s.outputs))
	for _, h := range []string{"INPUT LABELS", "OUTPUT LABELS", "VIDEO OUTPUT ROUTING", "VIDEO OUTPUT LOCKS"} {
		b.WriteString(s.blockFor(h, c, nil))
	}
	b.WriteString("END PRELUDE:\n\n")
	return b.String()
}

// broadcast sends the changed lines of header h to every client (requester
// included). Caller holds s.mu.
func (s *sim) broadcast(h string, idx []int) {
	if len(idx) == 0 {
		return
	}
	for c := range s.clients {
		c.write(s.blockFor(h, c, idx))
	}
}

func (s *sim) setRoute(out, in int) bool {
	if s.route[out] == in {
		return false
	}
	s.route[out] = in
	return true
}

// panelOnce changes one random route as a hardware panel would.
func (s *sim) panelOnce() {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := rand.IntN(len(s.outputs))
	in := rand.IntN(len(s.inputs))
	if s.setRoute(out, in) {
		s.broadcast("VIDEO OUTPUT ROUTING", []int{out})
	}
}

func (s *sim) setDown(d bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.down = d
	if d {
		for c := range s.clients {
			_ = c.conn.Close()
		}
	}
}

func (s *sim) serve(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
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

func (s *sim) handle(conn net.Conn) {
	c := &client{conn: conn}
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

func (s *sim) command(c *client, h string, lines []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
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

func envInt(k string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return n
	}
	return def
}

func envStr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	nin, nout := envInt("VHSIM_INPUTS", 40), envInt("VHSIM_OUTPUTS", 40)
	if nin < 1 || nout < 1 {
		log.Fatal("VHSIM_INPUTS and VHSIM_OUTPUTS must be >= 1")
	}
	s := newSim(envStr("VHSIM_MODEL", "Blackmagic Smart Videohub 40x40"), nin, nout)
	for _, f := range strings.Split(os.Getenv("VHSIM_LOCKS"), ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(f)); err == nil && n >= 0 && n < nout {
			s.owner[n] = s.ext
		}
	}
	if n := envInt("VHSIM_PANEL_SECONDS", 0); n > 0 {
		go func() {
			for range time.Tick(time.Duration(n) * time.Second) {
				s.panelOnce()
			}
		}()
	}
	if after := envInt("VHSIM_DOWN_AFTER", 0); after > 0 {
		dur := time.Duration(envInt("VHSIM_DOWN_FOR", 10)) * time.Second
		go func() {
			time.Sleep(time.Duration(after) * time.Second)
			log.Printf("going down for %s", dur)
			s.setDown(true)
			time.Sleep(dur)
			s.setDown(false)
			log.Print("back up")
		}()
	}
	addr := ":" + strconv.Itoa(envInt("VHSIM_PORT", 9990))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("vhsim %dx%d on %s", nin, nout, addr)
	s.serve(ln)
}
