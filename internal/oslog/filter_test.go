package oslog

import (
	"net/url"
	"testing"
)

func mustFilter(t *testing.T, values map[string][]string) Filter {
	t.Helper()
	f, err := NewFilter(url.Values(values))
	if err != nil {
		t.Fatalf("NewFilter(%v) failed: %v", values, err)
	}
	return f
}

func TestFilterDimensions(t *testing.T) {
	entry := Entry{
		Subsystem: "com.example.app",
		Category:  "network",
		Process:   "ExampleApp",
		Level:     "Error",
		Message:   "Connection failed after 3 retries",
	}

	cases := []struct {
		name   string
		values map[string][]string
		want   bool
	}{
		{"empty matches everything", nil, true},
		{"matching subsystem", map[string][]string{"subsystem": {"com.example.app"}}, true},
		{"other subsystem", map[string][]string{"subsystem": {"com.other"}}, false},
		{"matching category", map[string][]string{"category": {"network"}}, true},
		{"other category", map[string][]string{"category": {"ui"}}, false},
		{"matching process", map[string][]string{"process": {"ExampleApp"}}, true},
		{"matching level", map[string][]string{"level": {"Error"}}, true},
		{"other level", map[string][]string{"level": {"Debug"}}, false},
		{"multiple subsystems act as OR", map[string][]string{"subsystem": {"com.other", "com.example.app"}}, true},
		{"dimensions act as AND", map[string][]string{"subsystem": {"com.example.app"}, "level": {"Debug"}}, false},
		{"substring query", map[string][]string{"q": {"failed"}}, true},
		{"query matches subsystem", map[string][]string{"q": {"example"}}, true},
		{"query matches category", map[string][]string{"q": {"network"}}, true},
		{"query matches process", map[string][]string{"q": {"exampleapp"}}, true},
		{"query is case insensitive by default", map[string][]string{"q": {"CONNECTION"}}, true},
		{"case sensitive query misses", map[string][]string{"q": {"CONNECTION"}, "case": {"true"}}, false},
		{"case sensitive query hits", map[string][]string{"q": {"Connection"}, "case": {"true"}}, true},
		{"regex query", map[string][]string{"q": {`failed after \d+`}, "regex": {"true"}}, true},
		{"regex query without match", map[string][]string{"q": {`^\d+$`}, "regex": {"true"}}, false},
		{"wildcard subsystem", map[string][]string{"subsystem": {"com.example.*"}}, true},
		{"wildcard subsystem miss", map[string][]string{"subsystem": {"com.other.*"}}, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := mustFilter(t, c.values).Match(entry); got != c.want {
				t.Errorf("Match = %v, want %v", got, c.want)
			}
		})
	}
}

func TestFilterRequireSubsystem(t *testing.T) {
	withSubsystem := Entry{Subsystem: "com.example.app"}
	withoutSubsystem := Entry{Subsystem: "", Message: "noisy framework log"}

	filter := mustFilter(t, map[string][]string{"hasSubsystem": {"true"}})
	if !filter.Match(withSubsystem) {
		t.Error("entry with a subsystem should pass")
	}
	if filter.Match(withoutSubsystem) {
		t.Error("entry without a subsystem should be filtered out")
	}
}

func TestFilterCommaSeparatedValues(t *testing.T) {
	filter := mustFilter(t, map[string][]string{"subsystem": {"com.a,com.b"}})
	if len(filter.Subsystems) != 2 {
		t.Fatalf("Subsystems = %v, want two values", filter.Subsystems)
	}
	if !filter.Match(Entry{Subsystem: "com.b"}) {
		t.Error("comma separated values should be usable individually")
	}
}

func TestFilterInvalidRegex(t *testing.T) {
	if _, err := NewFilter(url.Values{"q": {"["}, "regex": {"true"}}); err == nil {
		t.Fatal("expected an error for an invalid regular expression")
	}
}

func TestFilterWithoutDimension(t *testing.T) {
	filter := mustFilter(t, map[string][]string{
		"subsystem":    {"com.a"},
		"category":     {"net"},
		"q":            {"boom"},
		"hasSubsystem": {"true"},
	})
	entry := Entry{Subsystem: "com.b", Category: "net", Message: "boom"}

	if filter.Match(entry) {
		t.Fatal("precondition: the subsystem restriction should reject this entry")
	}
	if !filter.WithoutDimension("subsystem").Match(entry) {
		t.Error("dropping the subsystem dimension should let the entry through")
	}
	if filter.WithoutDimension("category").Match(entry) {
		t.Error("dropping an unrelated dimension must not relax the subsystem filter")
	}
	// Query and the other dimensions must survive.
	if !filter.WithoutDimension("subsystem").MatchesText(entry.haystack()) {
		t.Error("the text query should be preserved")
	}
}

func TestFilterMatchAll(t *testing.T) {
	if !mustFilter(t, nil).MatchAll() {
		t.Error("an empty filter should match everything")
	}
	if mustFilter(t, map[string][]string{"q": {"x"}}).MatchAll() {
		t.Error("a query filter should not be reported as matching everything")
	}
	if mustFilter(t, map[string][]string{"hasSubsystem": {"true"}}).MatchAll() {
		t.Error("hasSubsystem should count as a restriction")
	}
}

func TestFilterDescribe(t *testing.T) {
	desc := mustFilter(t, map[string][]string{
		"subsystem": {"com.a"},
		"level":     {"Error"},
		"q":         {"boom"},
	}).Describe()
	for _, want := range []string{"subsystem=com.a", "level=Error", "text=boom"} {
		if !contains(desc, want) {
			t.Errorf("Describe() = %q, want it to contain %q", desc, want)
		}
	}
	if got := mustFilter(t, nil).Describe(); got != "all events" {
		t.Errorf("Describe() of an empty filter = %q, want \"all events\"", got)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle || indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
