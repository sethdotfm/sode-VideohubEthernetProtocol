package state

import "testing"

func up(s *Store) {
	s.SetUp("Test Hub", []string{"A", "B", "C"}, []string{"X", "Y"}, []int{0, 1}, []byte{'U', 'L'})
}

func TestApplyRoutesOnlyPublishesChanges(t *testing.T) {
	s := New()
	up(s)
	ch, cancel := s.Subscribe()
	defer cancel()
	evs := s.ApplyRoutes(map[int]int{0: 0, 1: 2, 9: 1})
	if len(evs) != 1 || evs[0] != (Event{Kind: EvRoute, Out: 1, In: 2}) {
		t.Fatalf("events = %+v", evs)
	}
	if e := <-ch; e.Kind != EvRoute || e.Out != 1 {
		t.Fatalf("published %+v", e)
	}
	if got := s.Snapshot().Route; got[0] != 0 || got[1] != 2 {
		t.Fatalf("route = %v", got)
	}
}

func TestLocksAndLabels(t *testing.T) {
	s := New()
	up(s)
	if evs := s.ApplyLocks(map[int]byte{0: 'O', 1: 'L'}); len(evs) != 1 || evs[0].Out != 0 {
		t.Fatalf("lock events = %+v", evs)
	}
	evs := s.ApplyLabels(map[int]string{2: "Cam"}, map[int]string{0: "Mon", 1: "Y"})
	if len(evs) != 2 || !evs[0].Input || evs[1].Input || evs[1].Out != 0 {
		t.Fatalf("label events = %+v", evs)
	}
}

func TestDownClearsOnceAndSnapshotIsACopy(t *testing.T) {
	s := New()
	up(s)
	snap := s.Snapshot()
	snap.Route[0] = 99
	if s.Snapshot().Route[0] != 0 {
		t.Fatal("snapshot aliases the cache")
	}
	ch, cancel := s.Subscribe()
	defer cancel()
	s.SetDown()
	s.SetDown()
	if e := <-ch; e.Kind != EvDown {
		t.Fatalf("got %+v", e)
	}
	select {
	case e := <-ch:
		t.Fatalf("second SetDown published %+v", e)
	default:
	}
	if s.Snapshot().Up {
		t.Fatal("still up")
	}
}

func TestSlowSubscriberGetsResync(t *testing.T) {
	s := New()
	up(s)
	ch, cancel := s.Subscribe()
	defer cancel()
	for i := 0; i < cap(ch)+5; i++ {
		s.ApplyRoutes(map[int]int{0: i % 2 * 2})
		s.ApplyRoutes(map[int]int{0: 1})
	}
	for len(ch) > 0 {
		<-ch
	}
	s.ApplyRoutes(map[int]int{0: 2})
	if e := <-ch; e.Kind != EvResync {
		t.Fatalf("first event after overflow = %+v", e)
	}
}
