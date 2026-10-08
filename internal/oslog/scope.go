package oslog

import (
	"fmt"
	"strings"
)

// Scope limits what a capture records. Entries outside the scope are discarded as
// they arrive, so they never reach the buffer, the value catalog or the history
// file. Scoping at the source is the only reliable way to keep a busy stream
// small: an unscoped capture of a chatty app produces hundreds of megabytes per
// hour, most of it framework noise nobody asked for.
type Scope struct {
	// Subsystems is an explicit allowlist. When it is non-empty, listed
	// subsystems are captured even if the exclusion flags below would otherwise
	// drop them: an explicit list is a deliberate instruction.
	Subsystems []string
	// IgnoreApple drops everything under the com.apple namespace, which is system
	// noise for an app-level investigation.
	IgnoreApple bool
	// IgnoreNoSubsystem drops entries that carry no subsystem at all. These are
	// usually framework chatter. It is enforced as entries arrive rather than in
	// the `log stream` predicate: NSPredicate has no way to test a missing
	// subsystem (`subsystem != ""` does not exclude it and `subsystem.length` is
	// not a valid field).
	IgnoreNoSubsystem bool
}

// AppleSubsystemPrefix is the namespace excluded by IgnoreApple.
const AppleSubsystemPrefix = "com.apple"

// Limits that keep a generated predicate reasonable.
const (
	maxScopeValues = 50
	maxScopeValue  = 200
)

// NewScope validates a user supplied subsystem allowlist. Values are trimmed and
// de-duplicated; an empty list means "no restriction".
//
// Subsystem names are restricted to a conservative character set rather than
// escaped. The values end up inside an NSPredicate, and rejecting anything
// unusual is both safer and easier to explain than quoting rules.
func NewScope(subsystems []string) (Scope, error) {
	var cleaned []string
	seen := map[string]struct{}{}
	for _, raw := range subsystems {
		for _, part := range strings.Split(raw, ",") {
			value := strings.TrimSpace(part)
			if value == "" {
				continue
			}
			if len(value) > maxScopeValue {
				return Scope{}, fmt.Errorf("subsystem %q is longer than %d characters", value, maxScopeValue)
			}
			if err := validateSubsystem(value); err != nil {
				return Scope{}, err
			}
			if _, dup := seen[value]; dup {
				continue
			}
			seen[value] = struct{}{}
			cleaned = append(cleaned, value)
		}
	}
	if len(cleaned) > maxScopeValues {
		return Scope{}, fmt.Errorf("at most %d subsystems can be scoped at once (got %d)", maxScopeValues, len(cleaned))
	}
	return Scope{Subsystems: cleaned}, nil
}

func validateSubsystem(value string) error {
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '-', r == '_', r == '+', r == ':', r == '/', r == '~', r == ' ':
		default:
			return fmt.Errorf("subsystem %q contains an unsupported character %q", value, string(r))
		}
	}
	return nil
}

// Empty reports whether the scope restricts nothing.
func (s Scope) Empty() bool {
	return len(s.Subsystems) == 0 && !s.IgnoreApple && !s.IgnoreNoSubsystem
}

// Match reports whether an entry is inside the scope. Subsystem values are
// compared exactly, matching how OSLog treats them.
func (s Scope) Match(e Entry) bool {
	if s.IgnoreNoSubsystem && e.Subsystem == "" {
		return false
	}
	if len(s.Subsystems) > 0 {
		// An explicit list is decisive, so a listed value is kept even when it
		// falls under IgnoreApple.
		for _, want := range s.Subsystems {
			if e.Subsystem == want {
				return true
			}
		}
		return false
	}
	if s.IgnoreApple && IsAppleSubsystem(e.Subsystem) {
		return false
	}
	return true
}

// IsAppleSubsystem reports whether a subsystem lives in the com.apple namespace.
func IsAppleSubsystem(subsystem string) bool {
	return subsystem == AppleSubsystemPrefix ||
		strings.HasPrefix(subsystem, AppleSubsystemPrefix+".")
}

// Predicate combines the scope with an optional user predicate so that as much
// filtering as possible happens inside `log stream` itself. Every restriction is
// re-checked as entries arrive, so the predicate is an optimisation rather than
// the source of truth.
//
// IgnoreNoSubsystem is deliberately absent: it cannot be expressed as an
// NSPredicate, so it is only enforced on arrival.
func (s Scope) Predicate(base string) string {
	base = strings.TrimSpace(base)

	var clauses []string
	switch {
	case len(s.Subsystems) > 0:
		// The explicit list is decisive, so the com.apple exclusion is not added:
		// a listed com.apple subsystem must still be captured, and emitting both
		// would produce a predicate that contradicts itself and captures nothing.
		clauses = append(clauses, s.subsystemClause())
	case s.IgnoreApple:
		clauses = append(clauses, `(NOT subsystem BEGINSWITH "`+AppleSubsystemPrefix+`")`)
	}
	if len(clauses) == 0 {
		// Nothing to add: leave the caller's predicate exactly as it was.
		return base
	}
	if base != "" {
		clauses = append([]string{"(" + base + ")"}, clauses...)
	}
	return strings.Join(clauses, " AND ")
}

func (s Scope) subsystemClause() string {
	parts := make([]string, len(s.Subsystems))
	for i, value := range s.Subsystems {
		parts[i] = `subsystem == "` + value + `"`
	}
	return "(" + strings.Join(parts, " OR ") + ")"
}

// Describe renders a short summary for the UI.
func (s Scope) Describe() string {
	if s.Empty() {
		return "everything"
	}
	var parts []string
	switch len(s.Subsystems) {
	case 0:
	case 1:
		parts = append(parts, s.Subsystems[0])
	default:
		parts = append(parts, fmt.Sprintf("%d subsystems", len(s.Subsystems)))
	}
	// With an explicit list the com.apple exclusion has no effect, so it is left
	// out of the description rather than implying a restriction that is not
	// applied.
	if s.IgnoreApple && len(s.Subsystems) == 0 {
		parts = append(parts, "no "+AppleSubsystemPrefix)
	}
	if s.IgnoreNoSubsystem {
		parts = append(parts, "with subsystem")
	}
	return strings.Join(parts, ", ")
}
