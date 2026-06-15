package bridge

import (
	"sort"
	"testing"
	"time"
)

func TestControlledSet_dropsLightsAfterWindow(t *testing.T) {
	// Given: a 1-minute window with a controllable clock
	now := time.Unix(0, 0)
	s := NewControlledSet(time.Minute)
	s.now = func() time.Time { return now }

	// When: two lights are driven, then time advances past the window for one
	s.Seen("uuid-a")
	now = now.Add(30 * time.Second)
	s.Seen("uuid-b") // refreshed later than a

	// Then: within the window both are present
	if got := sortedCurrent(s); len(got) != 2 {
		t.Fatalf("within window = %v, want both", got)
	}

	// When: 40s pass — uuid-a is now 70s old (out), uuid-b 40s old (in)
	now = now.Add(40 * time.Second)
	got := sortedCurrent(s)
	if len(got) != 1 || got[0] != "uuid-b" {
		t.Fatalf("after window = %v, want [uuid-b]", got)
	}

	// And: re-driving uuid-a brings it back (sliding, relearned)
	s.Seen("uuid-a")
	if got := sortedCurrent(s); len(got) != 2 {
		t.Fatalf("after re-drive = %v, want both", got)
	}
}

func TestControlledSet_emptyWhenNothingDriven(t *testing.T) {
	s := NewControlledSet(time.Minute)
	if got := s.Current(); len(got) != 0 {
		t.Fatalf("expected empty set, got %v", got)
	}
}

func TestControlledSet_ignoresEmptyUUID(t *testing.T) {
	s := NewControlledSet(time.Minute)
	s.Seen("")
	if got := s.Current(); len(got) != 0 {
		t.Fatalf("empty uuid should be ignored, got %v", got)
	}
}

func sortedCurrent(s *ControlledSet) []string {
	got := s.Current()
	sort.Strings(got)
	return got
}
