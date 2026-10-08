package oslog

import (
	"fmt"
	"strings"
	"testing"
)

func TestNewScopeCleansInput(t *testing.T) {
	scope, err := NewScope([]string{"  com.a  ", "", "com.b,com.c", "com.a", "  "})
	if err != nil {
		t.Fatalf("NewScope: %v", err)
	}
	want := []string{"com.a", "com.b", "com.c"}
	if len(scope.Subsystems) != len(want) {
		t.Fatalf("Subsystems = %v, want %v", scope.Subsystems, want)
	}
	for i, v := range want {
		if scope.Subsystems[i] != v {
			t.Errorf("Subsystems[%d] = %q, want %q", i, scope.Subsystems[i], v)
		}
	}
}

func TestNewScopeRejectsBadInput(t *testing.T) {
	cases := map[string][]string{
		"quote injection": {`com.a" OR subsystem == "com.b`},
		"parenthesis":     {`com.a) OR (1==1`},
		"newline":         {"com.a\ncom.b"},
		"too long":        {strings.Repeat("a", maxScopeValue+1)},
		"too many":        manyNames(maxScopeValues + 1),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := NewScope(input); err == nil {
				t.Errorf("NewScope(%v) accepted invalid input", input[:min(2, len(input))])
			}
		})
	}
}

// manyNames returns n distinct subsystem names.
func manyNames(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("com.example.subsystem%d", i)
	}
	return out
}

func TestScopeMatch(t *testing.T) {
	scope, err := NewScope([]string{"com.a", "com.b"})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]bool{
		"com.a":        true,
		"com.b":        true,
		"com.c":        false,
		"com.a.suffix": false, // exact match, like OSLog
		"":             false,
	}
	for subsystem, want := range cases {
		if got := scope.Match(Entry{Subsystem: subsystem}); got != want {
			t.Errorf("Match(subsystem=%q) = %v, want %v", subsystem, got, want)
		}
	}

	empty, err := NewScope(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !empty.Match(Entry{Subsystem: "anything"}) {
		t.Error("an empty scope must match everything")
	}
}

func TestScopePredicate(t *testing.T) {
	scope, err := NewScope([]string{"com.a", "com.b"})
	if err != nil {
		t.Fatal(err)
	}

	got := scope.Predicate("")
	want := `(subsystem == "com.a" OR subsystem == "com.b")`
	if got != want {
		t.Errorf("Predicate(\"\") = %q, want %q", got, want)
	}

	combined := scope.Predicate(`process == "App"`)
	wantCombined := `(process == "App") AND (subsystem == "com.a" OR subsystem == "com.b")`
	if combined != wantCombined {
		t.Errorf("Predicate with a base = %q, want %q", combined, wantCombined)
	}

	empty, err := NewScope(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := empty.Predicate(`process == "App"`); got != `process == "App"` {
		t.Errorf("an empty scope must leave the base predicate alone, got %q", got)
	}
}

func TestScopeDescribe(t *testing.T) {
	one, _ := NewScope([]string{"com.a"})
	if got := one.Describe(); got != "com.a" {
		t.Errorf("Describe = %q, want the single subsystem", got)
	}
	two, _ := NewScope([]string{"com.a", "com.b"})
	if got := two.Describe(); got != "2 subsystems" {
		t.Errorf("Describe = %q, want a count", got)
	}
	none, _ := NewScope(nil)
	if got := none.Describe(); got != "everything" {
		t.Errorf("Describe = %q, want everything", got)
	}
}

func TestScopeExclusionFlags(t *testing.T) {
	base := Entry{Subsystem: "com.example.app"}
	system := Entry{Subsystem: "com.apple.network"}
	noSubsystem := Entry{Subsystem: "", Message: "framework chatter"}

	apple, _ := NewScope(nil)
	apple.IgnoreApple = true
	if !apple.Match(base) {
		t.Error("a non-apple subsystem must survive IgnoreApple")
	}
	if apple.Match(system) {
		t.Error("IgnoreApple must drop com.apple subsystems")
	}
	if !apple.Match(noSubsystem) {
		t.Error("IgnoreApple alone must keep entries with no subsystem")
	}

	withSub, _ := NewScope(nil)
	withSub.IgnoreNoSubsystem = true
	if withSub.Match(noSubsystem) {
		t.Error("IgnoreNoSubsystem must drop entries carrying no subsystem")
	}
	if !withSub.Match(system) || !withSub.Match(base) {
		t.Error("IgnoreNoSubsystem must keep entries that do have a subsystem")
	}

	both, _ := NewScope(nil)
	both.IgnoreApple = true
	both.IgnoreNoSubsystem = true
	if both.Match(system) || both.Match(noSubsystem) {
		t.Error("combining the flags must drop both kinds of noise")
	}
	if !both.Match(base) {
		t.Error("combining the flags must keep real app logs")
	}
}

// An explicit allowlist is a deliberate instruction, so it wins over the
// exclusion flags.
func TestExplicitListOverridesIgnoreApple(t *testing.T) {
	scope, err := NewScope([]string{"com.apple.network", "com.example.app"})
	if err != nil {
		t.Fatal(err)
	}
	scope.IgnoreApple = true

	if !scope.Match(Entry{Subsystem: "com.apple.network"}) {
		t.Error("an explicitly listed com.apple subsystem must still be captured")
	}
	if !scope.Match(Entry{Subsystem: "com.example.app"}) {
		t.Error("listed subsystems must be captured")
	}
	if scope.Match(Entry{Subsystem: "com.apple.other"}) {
		t.Error("a non-listed subsystem must be dropped by the allowlist")
	}

	// The predicate must follow the same precedence, otherwise it would contradict
	// itself and capture nothing at the source.
	got := scope.Predicate("")
	want := `(subsystem == "com.apple.network" OR subsystem == "com.example.app")`
	if got != want {
		t.Errorf("Predicate = %q, want %q", got, want)
	}
	if strings.Contains(got, "NOT subsystem") {
		t.Errorf("Predicate %q must not also exclude com.apple when the list names one", got)
	}
}

// The description must not claim a restriction that is not applied.
func TestScopeDescribeOmitsIneffectiveIgnoreApple(t *testing.T) {
	listed, _ := NewScope([]string{"com.a", "com.b"})
	listed.IgnoreApple = true
	if got := listed.Describe(); got != "2 subsystems" {
		t.Errorf("Describe = %q, want the list only", got)
	}
}

func TestScopePredicateWithExclusions(t *testing.T) {
	scope, err := NewScope(nil)
	if err != nil {
		t.Fatal(err)
	}
	scope.IgnoreApple = true

	got := scope.Predicate("")
	want := `(NOT subsystem BEGINSWITH "com.apple")`
	if got != want {
		t.Errorf("Predicate = %q, want %q", got, want)
	}

	combined := scope.Predicate(`process == "App"`)
	wantCombined := `(process == "App") AND (NOT subsystem BEGINSWITH "com.apple")`
	if combined != wantCombined {
		t.Errorf("Predicate = %q, want %q", combined, wantCombined)
	}

	// A base predicate with no exclusions must be returned untouched.
	plain, _ := NewScope(nil)
	if got := plain.Predicate(`process == "App"`); got != `process == "App"` {
		t.Errorf("Predicate = %q, want the base predicate unchanged", got)
	}
}

func TestScopeIgnoreNoSubsystemIsNotInPredicate(t *testing.T) {
	scope, err := NewScope(nil)
	if err != nil {
		t.Fatal(err)
	}
	scope.IgnoreNoSubsystem = true
	// NSPredicate cannot express this (subsystem != "" does not exclude a missing
	// key and .length is not a valid field), so it must be absent rather than
	// present and ineffective.
	if got := scope.Predicate(""); got != "" {
		t.Errorf("Predicate = %q, want empty because the restriction is not expressible", got)
	}
}

func TestScopeDescribeWithFlags(t *testing.T) {
	scope, _ := NewScope(nil)
	scope.IgnoreApple = true
	scope.IgnoreNoSubsystem = true
	if got := scope.Describe(); got != "no com.apple, with subsystem" {
		t.Errorf("Describe = %q", got)
	}

}

func TestIsAppleSubsystem(t *testing.T) {
	cases := map[string]bool{
		"com.apple":         true,
		"com.apple.network": true,
		"com.application":   false, // must not match on a bare prefix
		"com.example.apple": false,
		"":                  false,
	}
	for value, want := range cases {
		if got := IsAppleSubsystem(value); got != want {
			t.Errorf("IsAppleSubsystem(%q) = %v, want %v", value, got, want)
		}
	}
}
