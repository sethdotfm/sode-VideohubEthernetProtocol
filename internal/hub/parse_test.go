package hub

import (
	"reflect"
	"strings"
	"testing"
)

func feedAll(s string) (blocks []*block, replies []string) {
	var p parser
	for _, l := range strings.Split(s, "\n") {
		b, r := p.feed(l)
		if b != nil {
			blocks = append(blocks, b)
		}
		if r != "" {
			replies = append(replies, r)
		}
	}
	return
}

func TestParserBlocksAndReplies(t *testing.T) {
	in := "ACK\r\n\r\nINPUT LABELS:\r\n0 Cam One\r\n3 A  B\r\n\r\nWEIRD BLOCK:\r\nfoo\r\n\r\nNAK\r\n\r\nstray line\r\n"
	bs, rs := feedAll(in)
	if !reflect.DeepEqual(rs, []string{"ACK", "NAK"}) {
		t.Fatalf("replies %v", rs)
	}
	if len(bs) != 2 || bs[0].hdr != "INPUT LABELS" || bs[1].hdr != "WEIRD BLOCK" {
		t.Fatalf("blocks %+v", bs)
	}
	m := labelMap(bs[0].lines)
	if m[0] != "Cam One" || m[3] != "A  B" || len(m) != 2 {
		t.Fatalf("labels %v", m)
	}
}

func TestACKInsideBlockIsData(t *testing.T) {
	bs, rs := feedAll("X:\nACK\n\n")
	if len(rs) != 0 || len(bs) != 1 {
		t.Fatalf("%v %v", bs, rs)
	}
}

func TestDeviceBlock(t *testing.T) {
	bs, _ := feedAll("VIDEOHUB DEVICE:\nDevice present: true\nModel name: Smart Videohub 20x20\nVideo inputs: 20\nVideo outputs: 20\n\n")
	d := kv(bs[0].lines)
	if d["Model name"] != "Smart Videohub 20x20" || d["Video inputs"] != "20" || d["Device present"] != "true" {
		t.Fatalf("%v", d)
	}
}

func TestRoutesLocksIgnoreGarbage(t *testing.T) {
	r := routeMap([]string{"0 5", "x 1", "2 y", "-1 3", "4 -1", "7"})
	if !reflect.DeepEqual(r, map[int]int{0: 5, 4: -1}) {
		t.Fatalf("%v", r)
	}
	l := lockMap([]string{"0 U", "1 O", "2 L", "3 Q", "4"})
	if !reflect.DeepEqual(l, map[int]byte{0: 'U', 1: 'O', 2: 'L'}) {
		t.Fatalf("%v", l)
	}
}

func TestSanitize(t *testing.T) {
	for in, want := range map[string]string{
		"Caf\u00e9 \u65e5\u672c": "Caf\u00e9 \u65e5\u672c",
		"a\x00b\x1b[31mc\td":     "ab[31mcd",
		"bad\xffutf":             "badutf",
		"  pad \r":               "pad",
	} {
		if got := sanitize(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}
