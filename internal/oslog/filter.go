package oslog

import (
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"
)

// Filter describes which entries a consumer wants. Zero values mean "no
// restriction": an empty Filter matches everything.
//
// Values in Subsystems, Categories, Processes and Levels support the `*`
// wildcard so a caller can express "com.apple.*" style patterns.
type Filter struct {
	Subsystems       []string `json:"subsystems,omitempty"`
	Categories       []string `json:"categories,omitempty"`
	Processes        []string `json:"processes,omitempty"`
	Levels           []string `json:"levels,omitempty"`
	Query            string   `json:"query,omitempty"`
	Regex            bool     `json:"regex,omitempty"`
	CaseSensitive    bool     `json:"caseSensitive,omitempty"`
	RequireSubsystem bool     `json:"requireSubsystem,omitempty"`

	subsystemSet map[string]struct{}
	categorySet  map[string]struct{}
	processSet   map[string]struct{}
	levelSet     map[string]struct{}
	re           *regexp.Regexp
	needle       string
}

// NewFilter builds a Filter from request query parameters. The parameters are
// repeatable: subsystem=a&subsystem=b, category=..., process=..., level=...,
// plus q (free text), regex=true, case=true and hasSubsystem=true.
func NewFilter(values url.Values) (Filter, error) {
	f := Filter{
		Subsystems:       cleanValues(values["subsystem"]),
		Categories:       cleanValues(values["category"]),
		Processes:        cleanValues(values["process"]),
		Levels:           cleanValues(values["level"]),
		Query:            strings.TrimSpace(firstValue(values, "q")),
		Regex:            boolValue(values, "regex"),
		CaseSensitive:    boolValue(values, "case"),
		RequireSubsystem: boolValue(values, "hasSubsystem"),
	}
	f.subsystemSet = toSet(f.Subsystems)
	f.categorySet = toSet(f.Categories)
	f.processSet = toSet(f.Processes)
	f.levelSet = toSet(f.Levels)

	if f.Query != "" {
		if f.Regex {
			expr := f.Query
			if !f.CaseSensitive {
				expr = "(?i)" + expr
			}
			re, err := regexp.Compile(expr)
			if err != nil {
				return Filter{}, fmt.Errorf("invalid regular expression: %w", err)
			}
			f.re = re
		} else if !f.CaseSensitive {
			f.needle = strings.ToLower(f.Query)
		} else {
			f.needle = f.Query
		}
	}
	return f, nil
}

// Match reports whether the entry satisfies every configured restriction. The
// checks short-circuit, because this runs once per entry per viewer on the live
// fan-out path and once per entry for every whole-buffer scan.
func (f Filter) Match(e Entry) bool {
	if f.RequireSubsystem && e.Subsystem == "" {
		return false
	}
	if !matchDimension(f.subsystemSet, f.Subsystems, e.Subsystem) {
		return false
	}
	if !matchDimension(f.categorySet, f.Categories, e.Category) {
		return false
	}
	if !matchDimension(f.processSet, f.Processes, e.Process) {
		return false
	}
	if !matchDimension(f.levelSet, f.Levels, e.Level) {
		return false
	}
	return f.MatchText(e)
}

// MatchText applies only the free text query. Each field is tested on its own so
// that scanning a whole buffer does not allocate a combined string per entry.
func (f Filter) MatchText(e Entry) bool {
	if f.Query == "" {
		return true
	}
	if f.re != nil {
		return f.re.MatchString(e.Message) || f.re.MatchString(e.Subsystem) ||
			f.re.MatchString(e.Category) || f.re.MatchString(e.Process) || f.re.MatchString(e.Level)
	}
	if f.CaseSensitive {
		for _, s := range [...]string{e.Message, e.Subsystem, e.Category, e.Process, e.Level} {
			if s != "" && strings.Contains(s, f.needle) {
				return true
			}
		}
		return false
	}
	// Case-insensitive matching runs against the haystack prepared once at ingest;
	// the fallback covers entries constructed directly (tests, history replay).
	search := e.search
	if search == "" {
		search = strings.ToLower(e.searchable())
	}
	return strings.Contains(search, f.needle)
}

// MatchesText exposes free-text matching against an arbitrary string so the UI can
// reuse the same rules.
func (f Filter) MatchesText(haystack string) bool {
	if f.Query == "" {
		return true
	}
	if f.re != nil {
		return f.re.MatchString(haystack)
	}
	if f.CaseSensitive {
		return strings.Contains(haystack, f.needle)
	}
	return strings.Contains(strings.ToLower(haystack), f.needle)
}

// MatchAll reports whether no restriction at all is configured.
func (f Filter) MatchAll() bool {
	return len(f.Subsystems) == 0 && len(f.Categories) == 0 && len(f.Processes) == 0 &&
		len(f.Levels) == 0 && f.Query == "" && !f.RequireSubsystem
}

// Describe renders a short human readable summary used in export file headers.
func (f Filter) Describe() string {
	var parts []string
	if f.RequireSubsystem {
		parts = append(parts, "hasSubsystem=true")
	}
	for _, p := range []struct {
		name   string
		values []string
	}{
		{"subsystem", f.Subsystems},
		{"category", f.Categories},
		{"process", f.Processes},
		{"level", f.Levels},
	} {
		if len(p.values) > 0 {
			parts = append(parts, p.name+"="+strings.Join(p.values, "|"))
		}
	}
	if f.Query != "" {
		mode := "text"
		if f.Regex {
			mode = "regex"
		}
		parts = append(parts, mode+"="+f.Query)
	}
	if len(parts) == 0 {
		return "all events"
	}
	return strings.Join(parts, " ")
}

// haystack is the concatenation searched by the case-insensitive text query.
func (e Entry) haystack() string {
	return e.Message + " " + e.Subsystem + " " + e.Category + " " + e.Process + " " + e.Level
}

func matchDimension(set map[string]struct{}, patterns []string, value string) bool {
	if len(patterns) == 0 {
		return true
	}
	if _, ok := set[value]; ok {
		return true
	}
	for _, p := range patterns {
		if !strings.Contains(p, "*") {
			continue
		}
		if ok, err := path.Match(strings.ToLower(p), strings.ToLower(value)); err == nil && ok {
			return true
		}
	}
	return false
}

func toSet(values []string) map[string]struct{} {
	if len(values) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(values))
	for _, v := range values {
		set[v] = struct{}{}
	}
	return set
}

// cleanValues drops empty values and duplicates while preserving order.
func cleanValues(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if _, ok := seen[part]; ok {
				continue
			}
			seen[part] = struct{}{}
			out = append(out, part)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func firstValue(values url.Values, key string) string {
	if vs := values[key]; len(vs) > 0 {
		return vs[0]
	}
	return ""
}

func boolValue(values url.Values, key string) bool {
	switch strings.ToLower(firstValue(values, key)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
