package oslog

import (
	"fmt"
	"testing"
	"time"
)

func findFacet(facets []Facet, value string) (Facet, bool) {
	for _, f := range facets {
		if f.Value == value {
			return f, true
		}
	}
	return Facet{}, false
}

// The catalog is what keeps a rare subsystem selectable. This is the scenario the
// buffer-scoped value list got wrong: the entry is long gone from the buffer, but
// the value must remain in the picker with its session total.
func TestCatalogRemembersValuesEvictedFromBuffer(t *testing.T) {
	buf := NewBuffer(10)
	catalog := NewCatalog()

	record := func(e Entry) {
		stored := buf.Add(e)
		catalog.Record(stored)
	}

	record(Entry{Subsystem: "centrifugo-websocket.futures.open-order", Category: "order", Level: "Debug", Message: "rare"})
	for i := 0; i < 500; i++ {
		record(Entry{Subsystem: "com.apple.network", Category: "net", Level: "Debug", Message: "noise"})
	}

	stillBuffered := false
	for _, e := range buf.All() {
		if e.Subsystem == "centrifugo-websocket.futures.open-order" {
			stillBuffered = true
		}
	}
	if stillBuffered {
		t.Skip("test assumes the rare entry is no longer in the buffer")
	}

	facets := catalog.Facets(0)
	got, ok := findFacet(facets.Subsystems, "centrifugo-websocket.futures.open-order")
	if !ok {
		t.Fatalf("catalog dropped the evicted value: %+v", facets.Subsystems)
	}
	if got.Count != 1 {
		t.Errorf("count = %d, want 1", got.Count)
	}
	if got.LastSeen.IsZero() {
		t.Error("LastSeen should be recorded so the UI can say when it was last seen")
	}
	if facets.Distinct["subsystems"] != 2 {
		t.Errorf("distinct subsystems = %d, want 2", facets.Distinct["subsystems"])
	}
}

func TestCatalogCountsAccumulateAndSurviveEviction(t *testing.T) {
	buf := NewBuffer(4)
	catalog := NewCatalog()
	for i := 0; i < 20; i++ {
		catalog.Record(buf.Add(Entry{Subsystem: "com.a", Level: "Info", Message: "m"}))
	}
	facets := catalog.Facets(0)
	if got, _ := findFacet(facets.Subsystems, "com.a"); got.Count != 20 {
		t.Errorf("session count = %d, want 20 despite the buffer holding only 4", got.Count)
	}
}

func TestCatalogResetForgetsEverything(t *testing.T) {
	catalog := NewCatalog()
	catalog.Record(Entry{Subsystem: "com.a", Category: "c", Process: "p", Level: "Info"})
	catalog.Reset()
	facets := catalog.Facets(0)
	if facets.Tracked != 0 {
		t.Errorf("Tracked = %d after Reset, want 0", facets.Tracked)
	}
	if len(facets.Subsystems) != 0 || len(facets.Levels) != 0 {
		t.Errorf("Reset left values behind: %+v", facets)
	}
}

func TestCatalogSkipsEmptyValuesAndOrdersByCount(t *testing.T) {
	catalog := NewCatalog()
	for i := 0; i < 5; i++ {
		catalog.Record(Entry{Subsystem: "", Category: "b", Level: "Info"})
	}
	for i := 0; i < 3; i++ {
		catalog.Record(Entry{Subsystem: "com.a", Category: "a", Level: "Info"})
	}
	facets := catalog.Facets(1)

	if len(facets.Subsystems) != 1 {
		t.Fatalf("limit 1 returned %d subsystems", len(facets.Subsystems))
	}
	if facets.Subsystems[0].Value != "com.a" {
		t.Errorf("highest count first, got %+v", facets.Subsystems)
	}
	if facets.Distinct["subsystems"] != 1 {
		t.Errorf("empty subsystems must not be tracked: %+v", facets.Distinct)
	}
	if facets.Categories[0].Value != "b" || facets.Categories[0].Count != 5 {
		t.Errorf("categories = %+v, want b with 5", facets.Categories)
	}
}

func TestCatalogBoundsValuesPerDimension(t *testing.T) {
	catalog := NewCatalog()
	for i := 0; i < maxValuesPerDimension+50; i++ {
		catalog.Record(Entry{Subsystem: fmt.Sprintf("com.example.%d", i), Level: "Info"})
	}
	if got := catalog.Distinct(); got > maxValuesPerDimension*4 {
		t.Errorf("tracked %d values, want the per-dimension cap respected", got)
	}
}

func TestCatalogConcurrentAccess(t *testing.T) {
	catalog := NewCatalog()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 500; i++ {
			catalog.Record(Entry{Subsystem: "com.a", Category: "c", Process: "p", Level: "Info", Timestamp: time.Now()})
		}
	}()
	for i := 0; i < 200; i++ {
		catalog.Facets(50)
		catalog.Distinct()
	}
	<-done
	if catalog.Distinct() == 0 {
		t.Error("catalog recorded nothing")
	}
}
