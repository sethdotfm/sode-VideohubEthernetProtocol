package main

import (
	"bufio"
	"net"
	"strings"
	"testing"
	"time"
)

type tc struct {
	t *testing.T
	c net.Conn
	r *bufio.Reader
}

func start(t *testing.T) (*sim, string) {
	t.Helper()
	s := newSim("Test Hub", 4, 4)
	s.owner[3] = s.ext
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go s.serve(ln)
	return s, ln.Addr().String()
}

func dial(t *testing.T, addr string) *tc {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return &tc{t, c, bufio.NewReader(c)}
}

// block reads one block (header to blank line) as a string without the blank.
func (c *tc) block() string {
	c.t.Helper()
	_ = c.c.SetReadDeadline(time.Now().Add(2 * time.Second))
	var b strings.Builder
	for {
		l, err := c.r.ReadString('\n')
		if err != nil {
			c.t.Fatalf("read: %v (got %q)", err, b.String())
		}
		if l == "\n" {
			return b.String()
		}
		b.WriteString(l)
	}
}

func (c *tc) prelude() []string {
	var hs []string
	for {
		b := c.block()
		h, _, _ := strings.Cut(b, "\n")
		hs = append(hs, h)
		if h == "END PRELUDE:" {
			return hs
		}
	}
}

func (c *tc) send(s string) { _, _ = c.c.Write([]byte(s)) }

func TestPreamble(t *testing.T) {
	_, addr := start(t)
	c := dial(t, addr)
	got := strings.Join(c.prelude(), "|")
	want := "PROTOCOL PREAMBLE:|VIDEOHUB DEVICE:|INPUT LABELS:|OUTPUT LABELS:|VIDEO OUTPUT ROUTING:|VIDEO OUTPUT LOCKS:|END PRELUDE:"
	if got != want {
		t.Fatalf("got %s", got)
	}
	// lock on output 3 is L for this client; request locks block to see it
	c.send("VIDEO OUTPUT LOCKS:\n\n")
	if b := c.block(); b != "ACK\n" {
		t.Fatal(b)
	}
	if b := c.block(); !strings.Contains(b, "3 L\n") || !strings.Contains(b, "0 U\n") {
		t.Fatal(b)
	}
	c.send("PING:\n\n")
	if b := c.block(); b != "ACK\n" {
		t.Fatal(b)
	}
}

func TestRouteBroadcast(t *testing.T) {
	_, addr := start(t)
	a, b := dial(t, addr), dial(t, addr)
	a.prelude()
	b.prelude()
	a.send("VIDEO OUTPUT ROUTING:\n1 3\n\n")
	if got := a.block(); got != "ACK\n" {
		t.Fatal(got)
	}
	want := "VIDEO OUTPUT ROUTING:\n1 3\n"
	if got := a.block(); got != want {
		t.Fatalf("requester got %q", got)
	}
	if got := b.block(); got != want {
		t.Fatalf("other got %q", got)
	}
	// no-op: ACK, no broadcast
	a.send("VIDEO OUTPUT ROUTING:\n1 3\n\nPING:\n\n")
	if got := a.block(); got != "ACK\n" {
		t.Fatal(got)
	}
	if got := a.block(); got != "ACK\n" { // PING's ACK, nothing in between
		t.Fatal(got)
	}
}

func TestLockedNAK(t *testing.T) {
	_, addr := start(t)
	a := dial(t, addr)
	a.prelude()
	a.send("VIDEO OUTPUT ROUTING:\n3 0\n\n")
	if got := a.block(); got != "NAK\n" {
		t.Fatal(got)
	}
	// own lock: O for requester, L for other; routing allowed for owner
	b := dial(t, addr)
	b.prelude()
	a.send("VIDEO OUTPUT LOCKS:\n0 O\n\n")
	a.block()
	if got := a.block(); got != "VIDEO OUTPUT LOCKS:\n0 O\n" {
		t.Fatal(got)
	}
	if got := b.block(); got != "VIDEO OUTPUT LOCKS:\n0 L\n" {
		t.Fatal(got)
	}
	b.send("VIDEO OUTPUT ROUTING:\n0 2\n\n")
	if got := b.block(); got != "NAK\n" {
		t.Fatal(got)
	}
	b.send("VIDEO OUTPUT LOCKS:\n0 F\n\n")
	b.block()
	if got := b.block(); got != "VIDEO OUTPUT LOCKS:\n0 U\n" {
		t.Fatal(got)
	}
}

func TestPanelAndLabels(t *testing.T) {
	s, addr := start(t)
	a := dial(t, addr)
	a.prelude()
	s.mu.Lock()
	s.route = []int{0, 0, 0, 0}
	s.mu.Unlock()
	for i := 0; i < 50; i++ {
		s.panelOnce()
		if len(s.clients) == 1 && func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.route[0]+s.route[1]+s.route[2]+s.route[3] > 0 }() {
			break
		}
	}
	if got := a.block(); !strings.HasPrefix(got, "VIDEO OUTPUT ROUTING:\n") {
		t.Fatal(got)
	}
	a.send("OUTPUT LABELS:\n2 Café Cam\n\n")
	a.block()
	_ = a // drain panel updates until label arrives
	for {
		got := a.block()
		if strings.HasPrefix(got, "OUTPUT LABELS:") {
			if got != "OUTPUT LABELS:\n2 Café Cam\n" {
				t.Fatal(got)
			}
			break
		}
	}
}
