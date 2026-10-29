package hub

import (
	"maps"
	"strconv"
	"strings"
	"time"

	"github.com/sethdotfm/sode-vep/internal/state"
)

// dump accumulates the hub's initial dump. It is a plain value: the caller
// provides locking and any timer.
type dump struct {
	dev    map[string]string
	devBad bool // "Device present" is not true: do not complete
	inL    map[int]string
	outL   map[int]string
	route  map[int]int
	lock   map[int]byte
	gotRT  bool
	gotLK  bool
}

// setup is what state.Store.SetUp takes.
type setup struct {
	model   string
	inputs  []string
	outputs []string
	route   []int
	lock    []byte
}

func newDump() *dump {
	return &dump{
		inL: map[int]string{}, outL: map[int]string{}, route: map[int]int{}, lock: map[int]byte{},
	}
}

// add merges b into the dump (later blocks add to earlier ones, never
// replace them) and reports whether END PRELUDE completed it. A dump whose
// device is not present never completes.
func (d *dump) add(b *block) (done bool) {
	switch b.hdr {
	case hdrDevice:
		d.dev = kv(b.lines)
		p := d.dev["Device present"]
		d.devBad = p != "" && !strings.EqualFold(p, "true")
	case hdrInLbl:
		maps.Copy(d.inL, labelMap(b.lines))
	case hdrOutLbl:
		maps.Copy(d.outL, labelMap(b.lines))
	case hdrRoute:
		maps.Copy(d.route, routeMap(b.lines))
		d.gotRT = true
	case hdrLock:
		maps.Copy(d.lock, lockMap(b.lines))
		d.gotLK = true
	case hdrEnd:
		return !d.devBad
	}
	return false
}

// quietWait is how long the hub must stay silent before the dump may be
// taken as complete without END PRELUDE (not in the v2.3 document): 0 means
// no fallback yet. Routing alone waits longer, in case the locks block is
// still coming.
func (d *dump) quietWait(quiet time.Duration) time.Duration {
	switch {
	case !d.gotRT || d.devBad:
		return 0
	case d.gotLK:
		return quiet
	default:
		return 4 * quiet
	}
}

// build sizes the hub from the device block (falling back to the highest
// index seen) and lays the maps out as slices. Unset routes are -1, unset
// locks Unlocked; routes to inputs that do not exist become -1.
func (d *dump) build() setup {
	nin, _ := strconv.Atoi(d.dev["Video inputs"])
	nout, _ := strconv.Atoi(d.dev["Video outputs"])
	if nin <= 0 || nin > maxIndex {
		nin = min(maxKey(d.inL)+1, maxIndex)
	}
	if nout <= 0 || nout > maxIndex {
		nout = max(maxKey(d.outL), maxKey(d.route), maxKey(d.lock)) + 1
	}
	s := setup{
		model:  d.dev["Model name"],
		inputs: make([]string, nin), outputs: make([]string, nout),
		route: make([]int, nout), lock: make([]byte, nout),
	}
	for i := range s.route {
		s.route[i], s.lock[i] = -1, state.Unlocked
	}
	for i, v := range d.inL {
		if i < nin {
			s.inputs[i] = v
		}
	}
	for i, v := range d.outL {
		if i < nout {
			s.outputs[i] = v
		}
	}
	for i, v := range d.route {
		if i < nout && v < nin {
			s.route[i] = v
		}
	}
	for i, v := range d.lock {
		if i < nout {
			s.lock[i] = v
		}
	}
	return s
}

func maxKey[V any](m map[int]V) int {
	r := -1
	for k := range m {
		r = max(r, k)
	}
	return r
}
