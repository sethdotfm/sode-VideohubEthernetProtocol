package hub

import (
	"strconv"
	"strings"
	"unicode"
)

// block is one parsed Videohub block: an ALL-CAPS header (without the
// colon) and its raw lines.
type block struct {
	hdr   string
	lines []string
}

// parser splits the line stream into blocks and bare ACK/NAK replies. It
// never fails: anything it does not understand is skipped.
type parser struct {
	hdr   string
	lines []string
	in    bool
}

// feed consumes one line (newline already removed). It returns a completed
// block, or a reply ("ACK"/"NAK") seen outside any block.
func (p *parser) feed(line string) (b *block, reply string) {
	line = strings.TrimRight(line, "\r")
	if !p.in {
		t := strings.TrimSpace(line)
		switch {
		case t == "":
		case t == "ACK" || t == "NAK":
			return nil, t
		case strings.HasSuffix(t, ":"):
			p.in, p.hdr, p.lines = true, strings.TrimSpace(strings.TrimSuffix(t, ":")), nil
		}
		return nil, ""
	}
	if strings.TrimSpace(line) == "" {
		p.in = false
		return &block{hdr: p.hdr, lines: p.lines}, ""
	}
	p.lines = append(p.lines, line)
	return nil, ""
}

// maxIndex bounds port indices so a hostile or buggy hub cannot make us
// allocate unbounded arrays.
const maxIndex = 4096

type entry struct {
	idx int
	val string
}

// indexed parses `index value` lines; value is everything after the first
// space. Lines that do not start with a valid index are ignored.
func indexed(lines []string) []entry {
	var es []entry
	for _, l := range lines {
		a, v, _ := strings.Cut(strings.TrimSpace(l), " ")
		n, err := strconv.Atoi(a)
		if err != nil || n < 0 || n >= maxIndex {
			continue
		}
		es = append(es, entry{n, v})
	}
	return es
}

func labelMap(lines []string) map[int]string {
	m := map[int]string{}
	for _, e := range indexed(lines) {
		m[e.idx] = sanitize(e.val)
	}
	return m
}

func routeMap(lines []string) map[int]int {
	m := map[int]int{}
	for _, e := range indexed(lines) {
		if in, err := strconv.Atoi(strings.TrimSpace(e.val)); err == nil && in >= -1 && in < maxIndex {
			m[e.idx] = in
		}
	}
	return m
}

func lockMap(lines []string) map[int]byte {
	m := map[int]byte{}
	for _, e := range indexed(lines) {
		switch v := strings.TrimSpace(e.val); v {
		case "U", "O", "L":
			m[e.idx] = v[0]
		}
	}
	return m
}

// kv parses the `Key: value` lines of the device block.
func kv(lines []string) map[string]string {
	m := map[string]string{}
	for _, l := range lines {
		k, v, ok := strings.Cut(l, ":")
		if ok {
			m[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return m
}

// sanitize makes an untrusted label safe: valid UTF-8, no control chars.
func sanitize(s string) string {
	s = strings.ToValidUTF8(s, "")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	return strings.TrimSpace(s)
}
