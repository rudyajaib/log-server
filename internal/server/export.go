package server

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/rudy/log-server/internal/oslog"
)

// exportFormats lists the supported download formats.
var exportFormats = map[string]string{
	"json":   "json",
	"jsonl":  "ndjson",
	"ndjson": "ndjson",
	"csv":    "csv",
	"txt":    "txt",
	"log":    "txt",
}

type exportEnvelope struct {
	ExportedAt time.Time     `json:"exportedAt"`
	Target     string        `json:"target,omitempty"`
	Filter     string        `json:"filter"`
	Count      int           `json:"count"`
	Total      uint64        `json:"totalBuffered"`
	Entries    []oslog.Entry `json:"entries"`
}

// handleExport renders the matching buffered entries as a downloadable file so
// a filtered view can be shared with other people.
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	filter, err := oslog.NewFilter(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	raw := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	if raw == "" {
		raw = "json"
	}
	format, ok := exportFormats[raw]
	if !ok {
		writeError(w, http.StatusBadRequest, `format must be one of json, ndjson, csv, txt`)
		return
	}

	limit := intParam(r, "limit", 0, 0, 100000)
	entries, matched := s.buf.Query(oslog.Query{Filter: filter, Limit: limit})
	status := s.status()
	target := strings.TrimSpace(status.Target.Name)

	header := w.Header()
	header.Set("Content-Type", contentType(format))
	header.Set("X-Log-Count", fmt.Sprint(len(entries)))
	header.Set("X-Log-Matched", fmt.Sprint(matched))
	header.Set("X-Log-Filter", filter.Describe())
	header.Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", exportFilename(target, format)))

	switch format {
	case "csv":
		writeCSV(w, entries)
	case "ndjson":
		writeNDJSON(w, entries)
	case "txt":
		writeText(w, entries, filter, status, matched)
	default:
		writeJSONExport(w, exportEnvelope{
			ExportedAt: time.Now(),
			Target:     target,
			Filter:     filter.Describe(),
			Count:      len(entries),
			Total:      s.buf.Total(),
			Entries:    entries,
		})
	}
	s.logger.Printf("exported %d entries as %s (%s)", len(entries), format, filter.Describe())
}

// writeJSONExport streams the envelope without building the whole document in
// memory twice.
func writeJSONExport(w http.ResponseWriter, env exportEnvelope) {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(env); err != nil {
		_ = err // headers are already committed
	}
}

func writeNDJSON(w http.ResponseWriter, entries []oslog.Entry) {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	for _, e := range entries {
		if err := enc.Encode(e); err != nil {
			return
		}
	}
}

func writeCSV(w http.ResponseWriter, entries []oslog.Entry) {
	cw := csv.NewWriter(w)
	header := []string{"id", "timestamp", "level", "eventType", "process", "processId", "threadId", "subsystem", "category", "message"}
	if err := cw.Write(header); err != nil {
		return
	}
	for _, e := range entries {
		record := []string{
			fmt.Sprint(e.ID),
			e.Timestamp.Format(time.RFC3339Nano),
			e.Level,
			e.EventType,
			e.Process,
			fmt.Sprint(e.ProcessID),
			fmt.Sprint(e.ThreadID),
			e.Subsystem,
			e.Category,
			e.Message,
		}
		if err := cw.Write(record); err != nil {
			return
		}
	}
	cw.Flush()
}

// writeText renders a grep friendly plain text export with a metadata preamble.
func writeText(w http.ResponseWriter, entries []oslog.Entry, filter oslog.Filter, status Status, matched int) {
	fmt.Fprintf(w, "# OSLog export\n")
	fmt.Fprintf(w, "# exported:  %s\n", time.Now().Format(time.RFC3339))
	if status.Target.Name != "" {
		fmt.Fprintf(w, "# target:    %s (%s %s)\n", status.Target.Name, status.Target.Kind, status.Target.ID)
	}
	fmt.Fprintf(w, "# filter:    %s\n", filter.Describe())
	fmt.Fprintf(w, "# entries:   %d exported, %d matched, %d in buffer\n", len(entries), matched, status.Buffered)
	fmt.Fprintf(w, "#\n")

	for _, e := range entries {
		fmt.Fprintf(w, "%s [%-8s] %s(%d) %s/%s: %s\n",
			e.Timestamp.Format("2006-01-02 15:04:05.000-0700"),
			e.Level,
			e.Process,
			e.ProcessID,
			orDash(e.Subsystem),
			orDash(e.Category),
			e.Message,
		)
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func contentType(format string) string {
	switch format {
	case "csv":
		return "text/csv; charset=utf-8"
	case "txt":
		return "text/plain; charset=utf-8"
	case "ndjson":
		return "application/x-ndjson; charset=utf-8"
	default:
		return "application/json; charset=utf-8"
	}
}

// exportFilename builds a descriptive, filesystem safe download name.
func exportFilename(target, format string) string {
	name := "oslog"
	if target != "" {
		name += "-" + sanitizeFilename(target)
	}
	return fmt.Sprintf("%s-%s.%s", name, time.Now().Format("20060102-150405"), format)
}

func sanitizeFilename(s string) string {
	var sb strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			sb.WriteRune(r)
		case r == ' ' || r == '.':
			sb.WriteRune('-')
		}
	}
	out := strings.Trim(sb.String(), "-")
	if out == "" {
		return "target"
	}
	return out
}
