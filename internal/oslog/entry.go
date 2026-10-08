// Package oslog discovers, streams and stores Apple unified logging (OSLog)
// events produced by iOS simulators and connected iOS devices.
package oslog

import (
	"bytes"
	"encoding/json"
	"path"
	"strings"
	"time"
)

// logTimeLayout matches the timestamps emitted by `log stream --style ndjson`,
// e.g. "2026-10-08 14:17:12.175004+0700".
const logTimeLayout = "2006-01-02 15:04:05.999999999-0700"

// Event is the wire format of a single `log stream --style ndjson` line. Only
// the fields this tool consumes are decoded; everything else is ignored.
type Event struct {
	Timestamp        string `json:"timestamp"`
	MessageType      string `json:"messageType"`
	EventType        string `json:"eventType"`
	LogType          string `json:"logType"`
	Subsystem        string `json:"subsystem"`
	Category         string `json:"category"`
	EventMessage     string `json:"eventMessage"`
	ComposedMessage  string `json:"composedMessage"`
	FormatString     string `json:"formatString"`
	ProcessImagePath string `json:"processImagePath"`
	ProcessID        int    `json:"processID"`
	ThreadID         int    `json:"threadID"`
	ActivityID       uint64 `json:"activityIdentifier"`
	TraceID          uint64 `json:"traceID"`
	SenderImagePath  string `json:"senderImagePath"`
	SignpostName     string `json:"signpostName"`
	SignpostType     string `json:"signpostType"`
	SignpostScope    string `json:"signpostScope"`
}

// Signpost carries os_signpost metadata when present.
type Signpost struct {
	Name  string `json:"name,omitempty"`
	Type  string `json:"type,omitempty"`
	Scope string `json:"scope,omitempty"`
}

// Entry is the normalised OSLog event exposed by the API and rendered by the UI.
type Entry struct {
	ID          uint64    `json:"id"`
	Timestamp   time.Time `json:"timestamp"`
	Level       string    `json:"level"`
	EventType   string    `json:"eventType"`
	Subsystem   string    `json:"subsystem"`
	Category    string    `json:"category"`
	Process     string    `json:"process"`
	ProcessID   int       `json:"processId"`
	ThreadID    int       `json:"threadId"`
	Message     string    `json:"message"`
	ProcessPath string    `json:"processPath,omitempty"`
	ActivityID  uint64    `json:"activityId,omitempty"`
	TraceID     uint64    `json:"traceId,omitempty"`
	Signpost    *Signpost `json:"signpost,omitempty"`
	Source      string    `json:"source,omitempty"`

	// search is the lowercased concatenation of the searchable fields, built once
	// when the entry is stored. Filter scans run over the whole buffer repeatedly
	// (facet polls, status polls, snapshots), and re-deriving this for every entry
	// on every scan dominated the cost of those scans. Unexported so it is neither
	// serialised nor part of the API.
	search string
}

// searchable is the text a free-text query is matched against.
func (e Entry) searchable() string {
	return e.Message + " " + e.Subsystem + " " + e.Category + " " + e.Process + " " + e.Level
}

// BuildSearch prepares the cached lowercase haystack. It is called when an entry
// enters the buffer and when one is loaded back from history.
func (e *Entry) BuildSearch() {
	e.search = strings.ToLower(e.searchable())
}

// ParseEvent decodes one ndjson line from `log stream`. It reports false for
// blank lines and for the human readable notices `log stream` writes to stdout
// (for example `Filtering the log data using "..."`), which must be ignored.
func ParseEvent(line []byte, source string) (Entry, bool) {
	trimmed := strings.TrimSpace(string(line))
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return Entry{}, false
	}
	var ev Event
	if err := json.Unmarshal([]byte(trimmed), &ev); err != nil {
		return Entry{}, false
	}
	return ev.toEntry(source), true
}

// DecodeEntry decodes an Entry that was previously serialised, which is how the
// on-disk history is stored.
func DecodeEntry(line []byte) (Entry, bool) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return Entry{}, false
	}
	var e Entry
	if err := json.Unmarshal(trimmed, &e); err != nil {
		return Entry{}, false
	}
	e.BuildSearch()
	return e, true
}

func (ev Event) toEntry(source string) Entry {
	e := Entry{
		Timestamp:   parseTimestamp(ev.Timestamp),
		Level:       ev.level(),
		EventType:   ev.EventType,
		Subsystem:   ev.Subsystem,
		Category:    ev.Category,
		ProcessID:   ev.ProcessID,
		ThreadID:    ev.ThreadID,
		ActivityID:  ev.ActivityID,
		TraceID:     ev.TraceID,
		Message:     ev.message(),
		ProcessPath: ev.ProcessImagePath,
		Source:      source,
	}
	e.Process = processName(ev.ProcessImagePath, ev.SenderImagePath)
	if ev.SignpostName != "" || ev.EventType == "signpostEvent" {
		e.Signpost = &Signpost{Name: ev.SignpostName, Type: ev.SignpostType, Scope: ev.SignpostScope}
	}
	return e
}

// level maps the OSLog message type to the severity label used for filtering.
// Events without a message type (signposts, activities, drops) fall back to a
// label derived from the event type.
func (ev Event) level() string {
	if ev.MessageType != "" {
		return ev.MessageType
	}
	switch ev.EventType {
	case "signpostEvent":
		return "Signpost"
	case "lossEvent":
		return "Loss"
	case "activityCreateEvent", "activityTransitionEvent", "userActionEvent":
		return "Activity"
	case "timesyncEvent":
		return "Timesync"
	case "stateEvent":
		return "State"
	case "traceEvent":
		return "Trace"
	}
	return "Default"
}

func (ev Event) message() string {
	for _, m := range []string{ev.EventMessage, ev.ComposedMessage, ev.FormatString} {
		if strings.TrimSpace(m) != "" {
			return m
		}
	}
	return ""
}

func parseTimestamp(s string) time.Time {
	if s == "" {
		return time.Now()
	}
	if t, err := time.Parse(logTimeLayout, s); err == nil {
		return t
	}
	return time.Now()
}

// processName derives a short process name from the executable path, e.g.
// "/usr/libexec/locationd" -> "locationd", ".../Foo.app/Foo" -> "Foo".
func processName(processImagePath, senderImagePath string) string {
	for _, p := range []string{processImagePath, senderImagePath} {
		if p == "" {
			continue
		}
		if base := path.Base(p); base != "" && base != "." && base != "/" {
			return base
		}
	}
	return "unknown"
}
