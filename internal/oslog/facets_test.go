package oslog

import (
	"fmt"
	"testing"
)

// seededBuffer builds a buffer whose values vary across every dimension so the
// facet lists have something to disagree about.
func seededBuffer(n int) *Buffer {
	b := NewBuffer(n)
	subs := []string{"com.a", "com.b", "com.a", "com.c", ""}
	cats := []string{"net", "db", "net", "ui", "db"}
	procs := []string{"App", "App", "Other", "", "Other"}
	levels := []string{"Info", "Error", "Debug", "Info", "Error"}
	for i := 0; i < n; i++ {
		b.Add(Entry{
			Subsystem: subs[i%len(subs)],
			Category:  cats[i%len(cats)],
			Process:   procs[i%len(procs)],
			Level:     levels[i%len(levels)],
			Message:   fmt.Sprintf("message %d keyword", i),
		})
	}
	return b
}

// The fast path computes all four lists in one pass by reasoning about which
// restrictions each entry satisfies. This checks it against the definition it
// replaced: for each dimension, the list of entries matching the filter with that
// one dimension removed.
func TestScopedFacetsMatchDimensionRemovedDefinition(t *testing.T) {
	buf := seededBuffer(200)

	filters := map[string]Filter{
		"empty":        mustFilter(t, nil),
		"subsystem":    mustFilter(t, map[string][]string{"subsystem": {"com.a"}}),
		"category":     mustFilter(t, map[string][]string{"category": {"net"}}),
		"process":      mustFilter(t, map[string][]string{"process": {"App"}}),
		"level":        mustFilter(t, map[string][]string{"level": {"Error"}}),
		"text":         mustFilter(t, map[string][]string{"q": {"keyword"}}),
		"textNoMatch":  mustFilter(t, map[string][]string{"q": {"absent"}}),
		"multi":        mustFilter(t, map[string][]string{"subsystem": {"com.a", "com.b"}, "level": {"Info", "Debug"}}),
		"requirement":  mustFilter(t, map[string][]string{"hasSubsystem": {"true"}}),
		"wildcard":     mustFilter(t, map[string][]string{"subsystem": {"com.*"}}),
		"textAndLevel": mustFilter(t, map[string][]string{"q": {"message"}, "level": {"Debug"}}),
	}

	dimensions := map[string]string{
		"subsystem": "subsystem",
		"category":  "category",
		"process":   "process",
		"level":     "level",
	}
	value := func(e Entry, dim string) string {
		switch dim {
		case "subsystem":
			return e.Subsystem
		case "category":
			return e.Category
		case "process":
			return e.Process
		default:
			return e.Level
		}
	}

	for name, filter := range filters {
		t.Run(name, func(t *testing.T) {
			got := buf.ScopedFacets(filter, 0)

			for dim := range dimensions {
				want := map[string]int{}
				for _, e := range buf.All() {
					if !filter.WithoutDimension(dim).Match(e) {
						continue
					}
					if v := value(e, dim); v != "" {
						want[v]++
					}
				}
				gotList := got.Subsystems
				switch dim {
				case "category":
					gotList = got.Categories
				case "process":
					gotList = got.Processes
				case "level":
					gotList = got.Levels
				}
				if len(gotList) != len(want) {
					t.Errorf("%s: %d values, want %d\n got %+v\nwant %+v", dim, len(gotList), len(want), gotList, want)
					continue
				}
				for _, facet := range gotList {
					if want[facet.Value] != facet.Count {
						t.Errorf("%s[%s] = %d, want %d", dim, facet.Value, facet.Count, want[facet.Value])
					}
				}
			}

			// The match count must agree with a straightforward recount.
			recount := 0
			for _, e := range buf.All() {
				if filter.Match(e) {
					recount++
				}
			}
			if got.Matched != recount {
				t.Errorf("Matched = %d, want %d", got.Matched, recount)
			}
			if c := buf.Count(filter); c != recount {
				t.Errorf("Count = %d, want %d", c, recount)
			}
		})
	}
}

// Caching the lowercase haystack at ingest must not change what matches.
func TestCachedSearchMatchesUncachedPath(t *testing.T) {
	entries := []Entry{
		{Subsystem: "Com.Example.App", Category: "Network", Process: "App", Level: "Info", Message: "Timeout Retry"},
		{Subsystem: "com.other", Category: "ui", Process: "Other", Level: "Debug", Message: "unrelated"},
		{Subsystem: "MIXED.Case", Category: "Cat", Process: "P", Level: "Error", Message: "MiXeD CaSe Text"},
	}
	// Stored entries carry the prepared haystack; a bare copy does not.
	stored := make([]Entry, len(entries))
	copy(stored, entries)
	for i := range stored {
		stored[i].BuildSearch()
	}

	for _, query := range []string{"timeout", "TIMEOUT", "com.example", "MiXeD", "network", "absent"} {
		for _, caseSensitive := range []bool{false, true} {
			f, err := NewFilter(map[string][]string{
				"q":    {query},
				"case": {fmt.Sprint(caseSensitive)},
			})
			if err != nil {
				t.Fatal(err)
			}
			for i := range entries {
				bare := f.Match(entries[i])
				prepared := f.Match(stored[i])
				if bare != prepared {
					t.Errorf("q=%q case=%v entry %d: bare=%v prepared=%v", query, caseSensitive, i, bare, prepared)
				}
			}
		}
	}
}

// A regex query must also be unaffected by the per-field rewrite.
func TestRegexMatchesEachField(t *testing.T) {
	e := Entry{Subsystem: "com.example.svc", Category: "net", Process: "App", Level: "Error", Message: "connect failed"}
	cases := map[string]bool{
		`connect`:      true,  // message
		`com\.example`: true,  // subsystem
		`^net$`:        true,  // category
		`^App$`:        true,  // process
		`Error`:        true,  // level
		`^failed`:      false, // anchored to the start of a single field
		`nomatch`:      false,
	}
	for pattern, want := range cases {
		f, err := NewFilter(map[string][]string{"q": {pattern}, "regex": {"true"}})
		if err != nil {
			t.Fatal(err)
		}
		if got := f.Match(e); got != want {
			t.Errorf("pattern %q = %v, want %v", pattern, got, want)
		}
	}
}
