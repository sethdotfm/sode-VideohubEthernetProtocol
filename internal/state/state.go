// Package state is the cache shared by the Videohub client (the only writer)
// and the SW-P-08 server and web page (readers and subscribers).
package state

import (
	"maps"
	"slices"
	"sync"
)

// Lock states, as the Videohub reports them: U unlocked, O locked by this
// client, L locked by another client.
const (
	Unlocked    byte = 'U'
	LockedOwn   byte = 'O'
	LockedOther byte = 'L'
)

// Locked reports whether takes to an output in this state are refused.
func Locked(l byte) bool { return l == LockedOwn || l == LockedOther }

// Router is how the SW-P-08 server sends a take on to the hub. A nil error
// means the hub accepted it (ACK); the route change itself arrives later as
// an EvRoute event, and only if it really changed.
type Router interface {
	Take(out, in int) error
}

type EventKind int

const (
	EvUp     EventKind = iota // prelude complete; Snapshot is valid
	EvDown                    // hub gone; cache is cleared
	EvRoute                   // Out now routed from In
	EvLabels                  // one label changed; Input says which side
	EvLock                    // Out's lock changed
	EvResync                  // subscriber was too slow and lost events: re-read Snapshot
)

// Event fields used per kind:
//
//	EvRoute:  Out (output) and In (the input now routed to it)
//	EvLock:   Out (output)
//	EvLabels: Out (index of the changed label, not necessarily an output) and
//	          Input (true if it is an input's label, false for an output's)
//	others:   none
type Event struct {
	Kind  EventKind
	Out   int
	In    int
	Input bool
}

// Snapshot is a copy; callers may keep and modify it.
type Snapshot struct {
	Up      bool
	Model   string
	Inputs  []string
	Outputs []string
	Route   []int  // output → input, -1 unknown
	Lock    []byte // output → U/O/L
}

type sub struct {
	ch   chan Event
	lost bool
}

type Store struct {
	mu   sync.Mutex
	snap Snapshot
	subs map[*sub]struct{}
}

func New() *Store { return &Store{subs: map[*sub]struct{}{}} }

func (s *Store) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return clone(s.snap)
}

func clone(n Snapshot) Snapshot {
	n.Inputs = append([]string(nil), n.Inputs...)
	n.Outputs = append([]string(nil), n.Outputs...)
	n.Route = append([]int(nil), n.Route...)
	n.Lock = append([]byte(nil), n.Lock...)
	return n
}

// Subscribe returns a buffered event channel. A subscriber that falls behind
// gets an EvResync and should re-read Snapshot.
func (s *Store) Subscribe() (<-chan Event, func()) {
	u := &sub{ch: make(chan Event, 1024)}
	s.mu.Lock()
	s.subs[u] = struct{}{}
	s.mu.Unlock()
	return u.ch, func() {
		s.mu.Lock()
		delete(s.subs, u)
		s.mu.Unlock()
	}
}

// publish must be called with s.mu held.
func (s *Store) publish(evs ...Event) {
	for u := range s.subs {
		if u.lost {
			select {
			case u.ch <- Event{Kind: EvResync}:
				u.lost = false
			default:
			}
		}
		for _, e := range evs {
			select {
			case u.ch <- e:
			default:
				u.lost = true
			}
		}
	}
}

// SetUp installs a complete prelude. Missing route/lock entries are filled as
// unknown/unlocked. It always publishes EvUp.
func (s *Store) SetUp(model string, inputs, outputs []string, route []int, lock []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := make([]int, len(outputs))
	l := make([]byte, len(outputs))
	for i := range outputs {
		r[i], l[i] = -1, Unlocked
		if i < len(route) {
			r[i] = route[i]
		}
		if i < len(lock) && lock[i] != 0 {
			l[i] = lock[i]
		}
	}
	s.snap = Snapshot{Up: true, Model: model, Inputs: append([]string(nil), inputs...), Outputs: append([]string(nil), outputs...), Route: r, Lock: l}
	s.publish(Event{Kind: EvUp})
}

// SetDown clears the cache and publishes EvDown (once: repeats are no-ops).
func (s *Store) SetDown() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.snap.Up {
		return
	}
	s.snap = Snapshot{}
	s.publish(Event{Kind: EvDown})
}

// ApplyRoutes records output → input routes and publishes one EvRoute per
// real change, in ascending output order. Out-of-range entries are ignored.
func (s *Store) ApplyRoutes(m map[int]int) []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	var evs []Event
	for _, out := range slices.Sorted(maps.Keys(m)) {
		in := m[out]
		if out < 0 || out >= len(s.snap.Route) || s.snap.Route[out] == in {
			continue
		}
		s.snap.Route[out] = in
		evs = append(evs, Event{Kind: EvRoute, Out: out, In: in})
	}
	s.publish(evs...)
	return evs
}

// ApplyLocks records output → lock state and publishes one EvLock per change.
func (s *Store) ApplyLocks(m map[int]byte) []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	var evs []Event
	for _, out := range slices.Sorted(maps.Keys(m)) {
		l := m[out]
		if out < 0 || out >= len(s.snap.Lock) || s.snap.Lock[out] == l {
			continue
		}
		s.snap.Lock[out] = l
		evs = append(evs, Event{Kind: EvLock, Out: out})
	}
	s.publish(evs...)
	return evs
}

// ApplyLabels records changed labels and publishes one EvLabels per change.
func (s *Store) ApplyLabels(inputs, outputs map[int]string) []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	var evs []Event
	set := func(dst []string, m map[int]string, input bool) {
		for _, i := range slices.Sorted(maps.Keys(m)) {
			if i < 0 || i >= len(dst) || dst[i] == m[i] {
				continue
			}
			dst[i] = m[i]
			evs = append(evs, Event{Kind: EvLabels, Out: i, Input: input})
		}
	}
	set(s.snap.Inputs, inputs, true)
	set(s.snap.Outputs, outputs, false)
	s.publish(evs...)
	return evs
}
