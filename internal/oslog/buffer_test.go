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

	entries, matched := b.Query(filter, 3, 0)
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

	after, matchedAfter := b.Query(filter, 0, 5)
	if len(after) != 2 || matchedAfter != 2 {
		t.Errorf("Query(after=5) returned %d entries (%d matched), want 2 and 2", len(after), matchedAfter)
	}
}

func TestBufferScopedFacetsAreCascading(t *testing.T) {
	b := NewBuffer(50)
	seed := []Entry{
		{Subsystem: "com.a", Category: "net", Process: "App", Level: "Info"},
		{Subsystem: "com.a", Category: "db", Process: "App", Level: "Error"},
		{Subsystem: "com.b", Category: "net", Process: "Other", Level: "Info"},
	}
	for _, e := range seed {
		b.Add(e)
	}

	filter, err := NewFilter(map[string][]string{"subsystem": {"com.a"}})
	if err != nil {
		t.Fatal(err)
	}
	scoped := b.ScopedFacets(filter, 0)

	if scoped.Matched != 2 {
		t.Errorf("Matched = %d, want 2", scoped.Matched)
	}
	// Categories are narrowed by the subsystem filter.
	if len(scoped.Categories) != 2 {
		t.Fatalf("categories = %+v, want net and db", scoped.Categories)
	}
	// The subsystem list keeps other subsystems so the selection stays changeable.
	values := map[string]int{}
	for _, f := range scoped.Subsystems {
		values[f.Value] = f.Count
	}
	if values["com.b"] != 1 {
		t.Errorf("subsystem facets = %+v, want com.b to remain visible", scoped.Subsystems)
	}
	if values["com.a"] != 2 {
		t.Errorf("subsystem facets = %+v, want com.a to count 2", scoped.Subsystems)
	}
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
		b.ScopedFacets(Filter{}, 10)
		b.Len()
	}
	<-done
	if b.Total() != 500 {
		t.Errorf("Total = %d, want 500", b.Total())
	}
}

func TestTopFacetsOrdering(t *testing.T) {
	facets := topFacets(map[string]int{"b": 2, "a": 2, "c": 5}, 2)
	if len(facets) != 2 {
		t.Fatalf("got %d facets, want 2", len(facets))
	}
	if facets[0].Value != "c" || facets[0].Count != 5 {
		t.Errorf("first facet = %+v, want the highest count", facets[0])
	}
	if facets[1].Value != "a" {
		t.Errorf("second facet = %+v, want ties broken alphabetically", facets[1])
	}
}
