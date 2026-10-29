package hub

import (
	"slices"
	"testing"
	"time"

	"github.com/sethdotfm/sode-vep/internal/state"
)

func blk(hdr string, lines ...string) *block { return &block{hdr: hdr, lines: lines} }

func dev(lines ...string) *block { return blk(hdrDevice, lines...) }

func TestDumpMergesUpdateBlocks(t *testing.T) {
	d := newDump()
	d.add(dev("Model name: M", "Video inputs: 4", "Video outputs: 3"))
	d.add(blk(hdrOutLbl, "0 A", "1 B"))
	d.add(blk(hdrRoute, "0 1", "1 2"))
	// an update mid-dump touches one entry; the rest must survive
	d.add(blk(hdrOutLbl, "1 B2"))
	d.add(blk(hdrRoute, "1 3"))
	d.add(blk(hdrLock, "2 L"))
	d.add(blk(hdrLock, "0 O"))
	u := d.build()
	if !slices.Equal(u.outputs, []string{"A", "B2", ""}) {
		t.Errorf("outputs %q", u.outputs)
	}
	if !slices.Equal(u.route, []int{1, 3, -1}) {
		t.Errorf("route %v", u.route)
	}
	if u.lock[0] != 'O' || u.lock[1] != state.Unlocked || u.lock[2] != 'L' {
		t.Errorf("lock %q", u.lock)
	}
	if u.model != "M" {
		t.Errorf("model %q", u.model)
	}
}

func TestDumpEndPreludeCompletes(t *testing.T) {
	d := newDump()
	for _, b := range []*block{dev("Video inputs: 2"), blk(hdrInLbl, "0 x"), blk(hdrRoute, "0 0"), blk(hdrLock, "0 U")} {
		if d.add(b) {
			t.Fatalf("%s completed early", b.hdr)
		}
	}
	if !d.add(blk(hdrEnd)) {
		t.Fatal("END PRELUDE did not complete")
	}
}

func TestDumpQuietWait(t *testing.T) {
	const q = 100 * time.Millisecond
	d := newDump()
	if w := d.quietWait(q); w != 0 {
		t.Errorf("empty: %v", w)
	}
	d.add(dev("Video inputs: 2"))
	d.add(blk(hdrInLbl, "0 x"))
	if w := d.quietWait(q); w != 0 {
		t.Errorf("no routing: %v", w)
	}
	d.add(blk(hdrRoute, "0 0"))
	if w := d.quietWait(q); w != 4*q {
		t.Errorf("routing alone: %v", w)
	}
	d.add(blk(hdrLock, "0 U"))
	if w := d.quietWait(q); w != q {
		t.Errorf("routing+locks: %v", w)
	}
	d.add(dev("Device present: false"))
	if w := d.quietWait(q); w != 0 {
		t.Errorf("device not present: %v", w)
	}
}

func TestDumpDeviceNotPresentBlocksThenRecovers(t *testing.T) {
	d := newDump()
	d.add(dev("Device present: false", "Video inputs: 2", "Video outputs: 2"))
	d.add(blk(hdrRoute, "0 1"))
	if !d.devBad {
		t.Fatal("devBad not set")
	}
	if d.add(blk(hdrEnd)) {
		t.Fatal("completed while device not present")
	}
	d.add(dev("Device present: TRUE", "Video inputs: 2", "Video outputs: 2"))
	if d.devBad {
		t.Fatal("devBad not cleared")
	}
	if !d.add(blk(hdrEnd)) {
		t.Fatal("did not complete after recovery")
	}
	// a block without the key also counts as present
	d2 := newDump()
	d2.add(dev("Model name: M"))
	if d2.devBad {
		t.Fatal("missing key treated as absent")
	}
}

func TestDumpSizing(t *testing.T) {
	// from the device block, even when indices run beyond it
	d := newDump()
	d.add(dev("Video inputs: 2", "Video outputs: 3"))
	d.add(blk(hdrInLbl, "0 a", "5 far"))
	d.add(blk(hdrOutLbl, "9 far"))
	u := d.build()
	if len(u.inputs) != 2 || len(u.outputs) != 3 || len(u.route) != 3 || len(u.lock) != 3 {
		t.Fatalf("sizes %d %d %d %d", len(u.inputs), len(u.outputs), len(u.route), len(u.lock))
	}
	if u.inputs[0] != "a" {
		t.Errorf("inputs %q", u.inputs)
	}

	// no device counts: max index seen
	d = newDump()
	d.add(blk(hdrInLbl, "0 a", "3 d"))
	d.add(blk(hdrOutLbl, "1 o"))
	d.add(blk(hdrRoute, "4 0"))
	d.add(blk(hdrLock, "6 L"))
	u = d.build()
	if len(u.inputs) != 4 || len(u.outputs) != 7 {
		t.Errorf("fallback sizes %d %d", len(u.inputs), len(u.outputs))
	}
	if u.lock[6] != 'L' || u.route[4] != 0 {
		t.Errorf("route %v lock %q", u.route, u.lock)
	}

	// nothing at all
	u = newDump().build()
	if len(u.inputs) != 0 || len(u.outputs) != 0 {
		t.Errorf("empty sizes %d %d", len(u.inputs), len(u.outputs))
	}
}

func TestDumpCapsAtMaxIndex(t *testing.T) {
	d := newDump()
	d.add(dev("Video inputs: 99999", "Video outputs: 99999"))
	d.add(blk(hdrInLbl, "4095 last"))
	d.add(blk(hdrOutLbl, "2 o"))
	u := d.build()
	if len(u.inputs) != maxIndex || u.inputs[maxIndex-1] != "last" {
		t.Errorf("inputs len %d", len(u.inputs))
	}
	if len(u.outputs) != 3 {
		t.Errorf("outputs len %d", len(u.outputs))
	}
}

func TestDumpRouteRangeClamp(t *testing.T) {
	d := newDump()
	d.add(dev("Video inputs: 2", "Video outputs: 4"))
	d.add(blk(hdrRoute, "0 1", "1 2", "2 -1", "3 7"))
	u := d.build()
	// routes >= nin are left unset (-1), as is an explicit -1
	if !slices.Equal(u.route, []int{1, -1, -1, -1}) {
		t.Errorf("route %v", u.route)
	}
}
