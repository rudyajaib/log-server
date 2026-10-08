package oslog

import (
	"testing"
	"time"
)

func TestBufferIDsAreMonotonicAcrossEviction(t *testing.T) {
	b := NewBuffer(3)
	for i := 0; i < 5; i++ {
		b.Add(Entry{Message: string(rune('a' + i))})
	}
	if got := b.Len(); got != 3 {
		t.Fatalf("Len = %d, want 3", got)
	}
	if got := b.Total(); got != 5 {
		t.Errorf("Total = %d, want 5", got)
	}
	if got := b.Evicted(); got != 2 {
		t.Errorf("Evicted = %d, want 2", got)
	}

	all := b.All()
	if len(all) != 3 {
		t.Fatalf("All returned %d entries, want 3", len(all))
	}
	for i, e := range all {
		if e.ID != uint64(i+3) {
			t.Errorf("entry %d has ID %d, want %d", i, e.ID, i+3)
		}
	}
	if got := b.LastID(); got != 5 {
		t.Errorf("LastID = %d, want 5", got)
	}
}

func TestBufferSinceResumesWithoutDuplicates(t *testing.T) {
	b := NewBuffer(10)
	for i := 0; i < 5; i++ {
		b.Add(Entry{Message: "m"})
	}
	since := b.Since(3)
	if len(since) != 2 {
		t.Fatalf("Since(3) returned %d entries, want 2", len(since))
	}
	if since[0].ID != 4 || since[1].ID != 5 {
		t.Errorf("Since(3) IDs = %d,%d, want 4,5", since[0].ID, since[1].ID)
	}
	if got := b.Since(5); len(got) != 0 {
		t.Errorf("Since(lastID) returned %d entries, want 0", len(got))
	}
}

func TestBufferClearKeepsIDSequence(t *testing.T) {
	b := NewBuffer(5)
	b.Add(Entry{Message: "first"})
	b.Add(Entry{Message: "second"})
	b.Clear()
	if b.Len() != 0 {
		t.Fatalf("Len after Clear = %d, want 0", b.Len())
	}
	added := b.Add(Entry{Message: "after clear"})
	if added.ID != 3 {
		t.Errorf("ID after Clear = %d, want 3 so live consumers stay consistent", added.ID)
	}
	if b.Total() != 3 {
		t.Errorf("Total after Clear = %d, want 3", b.Total())
	}
}

func TestBufferQueryLimitsAndCountsMatches(t *testing.T) {
	b := NewBuffer(50)
	for i := 0; i < 10; i++ {
		level := "Info"
		if i%2 == 0 {
			level = "Error"
		}
		b.Add(Entry{Level: level, Subsystem: "com.a", Message: "m"})
	}
	filter, err := NewFilter(map[string][]string{"level": {"Error"}})
	if err != nil {
		t.Fatal(err)
	}

	entries, matched := b.Query(Query{Filter: filter, Limit: 3})
	if matched != 5 {
		t.Errorf("matched = %d, want 5", matched)
	}
	if len(entries) != 3 {
		t.Fatalf("returned %d entries, want the 3 most recent matches", len(entries))
	}
	// The most recent matches must be the tail of the matching set.
	if entries[0].ID != 5 || entries[2].ID != 9 {
		t.Errorf("returned IDs %d..%d, want 5..9", entries[0].ID, entries[2].ID)
	}

	after, matchedAfter := b.Query(Query{Filter: filter, After: 5})
	if len(after) != 2 || matchedAfter != 2 {
		t.Errorf("Query(after=5) returned %d entries (%d matched), want 2 and 2", len(after), matchedAfter)
	}

	// Backward paging: the two matches immediately older than ID 7.
	older, olderMatched := b.Query(Query{Filter: filter, Limit: 2, Before: 7})
	if olderMatched != 3 {
		t.Errorf("backward match count = %d, want 3 (IDs 1,3,5)", olderMatched)
	}
	if len(older) != 2 || older[1].ID != 5 {
		t.Errorf("backward page = %v, want the newest two matches under ID 7 ending at 5", ids(older))
	}
}

func ids(entries []Entry) []uint64 {
	out := make([]uint64, len(entries))
	for i, e := range entries {
		out[i] = e.ID
	}
	return out
}

func TestBufferConcurrentAccess(t *testing.T) {
	b := NewBuffer(64)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 500; i++ {
			b.Add(Entry{Subsystem: "com.a", Category: "c", Message: "m", Timestamp: time.Now()})
		}
	}()
	for i := 0; i < 200; i++ {
		b.All()
		b.Since(uint64(i))
		b.Count(Filter{})
		b.Len()
	}
	<-done
	if b.Total() != 500 {
		t.Errorf("Total = %d, want 500", b.Total())
	}
}
