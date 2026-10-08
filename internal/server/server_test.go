package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/rudy/log-server/internal/history"
	"github.com/rudy/log-server/internal/oslog"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	s := New(Options{
		WebFS:      fstest.MapFS{"index.html": {Data: []byte("<html>ok</html>")}},
		BufferSize: 100,
		Logger:     log.New(io.Discard, "", 0),
	})
	t.Cleanup(s.Close)
	return s
}

func TestStartValidation(t *testing.T) {
	s := newTestServer(t)
	handler := s.Handler()

	cases := []struct {
		name string
		body string
		want int
	}{
		{"unknown kind", `{"kind":"bogus","id":"x"}`, http.StatusBadRequest},
		{"missing id", `{"kind":"simulator"}`, http.StatusBadRequest},
		{"bad level", `{"kind":"simulator","id":"x","level":"verbose"}`, http.StatusBadRequest},
		{"malformed json", `{`, http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/stream/start", strings.NewReader(c.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != c.want {
				t.Errorf("status = %d, want %d (body %s)", rec.Code, c.want, rec.Body.String())
			}
		})
	}
}

// TestMutatingEndpointsRequireJSON guards against drive-by requests from web
// pages: a form-encoded body must be rejected.
func TestMutatingEndpointsRequireJSON(t *testing.T) {
	s := newTestServer(t)
	handler := s.Handler()

	for _, endpoint := range []string{"/api/stream/start", "/api/stream/stop", "/api/buffer/clear"} {
		req := httptest.NewRequest(http.MethodPost, endpoint, strings.NewReader("a=b"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s accepted a form body with status %d", endpoint, rec.Code)
		}
	}
}

func TestStatusAndFacetsReflectBuffer(t *testing.T) {
	s := newTestServer(t)
	handler := s.Handler()

	s.handleEntry(oslog.Entry{Subsystem: "com.a", Category: "net", Level: "Info", Message: "hello"})
	s.handleEntry(oslog.Entry{Subsystem: "com.b", Category: "db", Level: "Error", Message: "boom"})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status endpoint returned %d", rec.Code)
	}
	var status Status
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatalf("decoding status: %v", err)
	}
	if status.Total != 2 || status.Buffered != 2 || status.Capacity != 100 {
		t.Errorf("status = %+v, want 2 of 100 buffered", status)
	}
	if status.Running {
		t.Error("no capture was started, so Running should be false")
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/facets?subsystem=com.a", nil))
	var facets facetsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &facets); err != nil {
		t.Fatalf("decoding facets: %v", err)
	}
	// The match count respects the filter, but the value lists are session wide so
	// that a selection can always be changed.
	if facets.Matched != 1 {
		t.Errorf("Matched = %d, want 1", facets.Matched)
	}
	if len(facets.Subsystems) != 2 {
		t.Errorf("subsystems = %+v, want every value seen this session", facets.Subsystems)
	}
	if len(facets.Categories) != 2 {
		t.Errorf("categories = %+v, want every value seen this session", facets.Categories)
	}
}

func TestLogsEndpointFiltersAndLimits(t *testing.T) {
	s := newTestServer(t)
	handler := s.Handler()
	for i := 0; i < 6; i++ {
		level := "Info"
		if i%2 == 0 {
			level = "Error"
		}
		s.handleEntry(oslog.Entry{Level: level, Subsystem: "com.a", Message: "m"})
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/logs?level=Error&limit=2", nil))
	var payload logsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decoding logs: %v", err)
	}
	if payload.Matched != 3 {
		t.Errorf("Matched = %d, want 3", payload.Matched)
	}
	if payload.Returned != 2 || !payload.Truncated {
		t.Errorf("Returned = %d, Truncated = %v; want 2 and true", payload.Returned, payload.Truncated)
	}
	if !payload.HasMore {
		t.Error("HasMore = false, but older matches remain in the buffer")
	}
}

// TestLogsPagesBackPastTheBuffer covers the reason the history file exists: a
// rare entry that the buffer has already evicted must still be reachable.
func TestLogsPagesBackPastTheBuffer(t *testing.T) {
	dir := t.TempDir()
	store, err := history.Open(filepath.Join(dir, "session.ndjson"), 0)
	if err != nil {
		t.Fatalf("opening history: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	s := New(Options{
		WebFS:      fstest.MapFS{},
		BufferSize: 20, // deliberately small so most entries are evicted
		Logger:     log.New(io.Discard, "", 0),
		History:    store,
	})
	t.Cleanup(s.Close)
	handler := s.Handler()

	// One rare entry, then enough noise to evict it from the buffer.
	s.handleEntry(oslog.Entry{Subsystem: "centrifugo-websocket.futures.open-order", Message: "the rare one"})
	for i := 0; i < 200; i++ {
		s.handleEntry(oslog.Entry{Subsystem: "com.apple.network", Message: "noise"})
	}
	if got := s.buf.Len(); got != 20 {
		t.Fatalf("buffer holds %d entries, want the 20 entry cap", got)
	}

	r := httptest.NewRequest(http.MethodGet, "/api/logs?subsystem=centrifugo-websocket.futures.open-order", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, r)

	var payload logsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decoding logs: %v", err)
	}
	if payload.Matched != 0 {
		t.Errorf("the buffer should no longer match anything, got %d", payload.Matched)
	}
	if len(payload.Entries) != 1 || payload.Entries[0].Message != "the rare one" {
		t.Fatalf("history paging returned %+v, want the evicted entry", payload.Entries)
	}
	// The buffer no longer holds it, so this page came purely from history.
	if payload.Source != "history" {
		t.Errorf("Source = %q, want history", payload.Source)
	}
}

func TestSnapshotCarriesFacetsAndOlderFlag(t *testing.T) {
	s := newTestServer(t)
	handler := s.Handler()

	s.handleEntry(oslog.Entry{Subsystem: "com.a", Category: "net", Level: "Info", Message: "one"})
	s.handleEntry(oslog.Entry{Subsystem: "com.b", Category: "db", Level: "Error", Message: "two"})

	ts := httptest.NewServer(handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/stream?subsystem=com.a")
	if err != nil {
		t.Fatalf("opening the stream: %v", err)
	}
	defer resp.Body.Close()

	raw := nextEvent(t, readEvents(resp.Body), "snapshot")
	var payload snapshotPayload
	if idx := strings.Index(raw, "data="); idx >= 0 {
		if err := json.Unmarshal([]byte(raw[idx+len("data="):]), &payload); err != nil {
			t.Fatalf("decoding snapshot: %v\n%s", err, raw)
		}
	} else {
		t.Fatalf("no data field in %s", raw)
	}

	if payload.Matched != 1 || len(payload.Entries) != 1 {
		t.Errorf("matched %d with %d entries, want 1 and 1", payload.Matched, len(payload.Entries))
	}
	// The value lists are session wide, so a subsystem that stops logging stays
	// selectable instead of disappearing as the buffer window slides past it.
	if len(payload.Facets.Subsystems) != 2 {
		t.Errorf("facets subsystems = %+v, want every value seen this session", payload.Facets.Subsystems)
	}
	if len(payload.Facets.Categories) != 2 {
		t.Errorf("facets categories = %+v, want every value seen this session", payload.Facets.Categories)
	}
	if payload.Older {
		t.Error("Older = true, but nothing matches below the snapshot")
	}
}

func TestLogsAndExportReturnArraysWhenEmpty(t *testing.T) {
	s := newTestServer(t)
	handler := s.Handler()
	s.handleEntry(oslog.Entry{Subsystem: "com.a", Message: "only entry"})

	for _, path := range []string{
		"/api/logs?subsystem=com.nothing",
		"/api/export?format=json&subsystem=com.nothing",
	} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		body := rec.Body.String()
		if strings.Contains(body, "null") {
			t.Errorf("%s returned null instead of an empty array:\n%s", path, body)
		}
		if !strings.Contains(body, "[]") {
			t.Errorf("%s did not return an empty array:\n%s", path, body)
		}
	}
}

func TestHistoryClearEndpoint(t *testing.T) {
	s := newTestServer(t)
	handler := s.Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/history/clear", strings.NewReader("{}")))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when history is disabled", rec.Code)
	}
}

func TestExportFormats(t *testing.T) {
	s := newTestServer(t)
	handler := s.Handler()
	s.handleEntry(oslog.Entry{Subsystem: "com.a", Category: "net", Level: "Info", Process: "App", Message: "hello, \"world\""})
	s.handleEntry(oslog.Entry{Subsystem: "com.b", Category: "db", Level: "Error", Process: "App", Message: "boom"})

	cases := []struct {
		format      string
		contentType string
		contains    []string
	}{
		{"json", "application/json", []string{`"count": 1`, `com.a`, `hello`}},
		{"csv", "text/csv", []string{"id,timestamp,level", "com.a"}},
		{"ndjson", "application/x-ndjson", []string{`"subsystem":"com.a"`}},
		{"txt", "text/plain", []string{"# OSLog export", "subsystem=com.a", "com.a/net"}},
	}
	for _, c := range cases {
		t.Run(c.format, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/export?format="+c.format+"&subsystem=com.a", nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}
			if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, c.contentType) {
				t.Errorf("Content-Type = %q, want prefix %q", got, c.contentType)
			}
			if got := rec.Header().Get("X-Log-Matched"); got != "1" {
				t.Errorf("X-Log-Matched = %q, want 1", got)
			}
			if disposition := rec.Header().Get("Content-Disposition"); !strings.Contains(disposition, "attachment") {
				t.Errorf("Content-Disposition = %q, want an attachment", disposition)
			}
			body := rec.Body.String()
			for _, want := range c.contains {
				if !strings.Contains(body, want) {
					t.Errorf("body is missing %q\n%s", want, body)
				}
			}
			if strings.Contains(body, "com.b") {
				t.Errorf("export leaked an entry outside the filter:\n%s", body)
			}
		})
	}
}

func TestExportRejectsUnknownFormat(t *testing.T) {
	s := newTestServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/export?format=pdf", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestSSESnapshotAndLiveFiltering(t *testing.T) {
	s := newTestServer(t)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	s.handleEntry(oslog.Entry{Subsystem: "com.a", Category: "net", Level: "Info", Message: "buffered match"})
	s.handleEntry(oslog.Entry{Subsystem: "com.b", Category: "db", Level: "Info", Message: "buffered other"})

	resp, err := http.Get(ts.URL + "/api/stream?subsystem=com.a")
	if err != nil {
		t.Fatalf("opening the stream: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ct)
	}

	events := readEvents(resp.Body)

	snapshot := nextEvent(t, events, "snapshot")
	if !strings.Contains(snapshot, `"mode":"full"`) {
		t.Errorf("snapshot = %s", snapshot)
	}
	if !strings.Contains(snapshot, "buffered match") {
		t.Errorf("snapshot should contain the matching buffered entry: %s", snapshot)
	}
	if strings.Contains(snapshot, "buffered other") {
		t.Errorf("snapshot leaked an entry outside the filter: %s", snapshot)
	}
	if !strings.Contains(snapshot, "id=1 ") {
		t.Errorf("snapshot should carry the event id for resume: %s", snapshot)
	}

	// A live entry matching the filter must arrive.
	s.handleEntry(oslog.Entry{Subsystem: "com.a", Level: "Error", Message: "live match"})
	live := nextEvent(t, events, "log")
	if !strings.Contains(live, "live match") {
		t.Errorf("live event = %s", live)
	}

	// A live entry for another subsystem must not arrive within the window.
	s.handleEntry(oslog.Entry{Subsystem: "com.b", Level: "Error", Message: "live other"})
	select {
	case got := <-events:
		if strings.Contains(got, "live other") {
			t.Errorf("filtered event leaked to the client: %s", got)
		}
	case <-time.After(300 * time.Millisecond):
	}
}

func TestSSEResumeSkipsAlreadySeenEntries(t *testing.T) {
	s := newTestServer(t)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	for i := 0; i < 3; i++ {
		s.handleEntry(oslog.Entry{Subsystem: "com.a", Message: "m"})
	}

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Last-Event-ID", "2")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("opening the stream: %v", err)
	}
	defer resp.Body.Close()

	snapshot := nextEvent(t, readEvents(resp.Body), "snapshot")
	if !strings.Contains(snapshot, `"mode":"resume"`) {
		t.Errorf("resume snapshot mode = %s", snapshot)
	}
	if strings.Contains(snapshot, `"id":1`) || strings.Contains(snapshot, `"id":2`) {
		t.Errorf("resume must not resend entries at or below the watermark: %s", snapshot)
	}
	if !strings.Contains(snapshot, `"id":3`) {
		t.Errorf("resume should deliver the entry after the watermark: %s", snapshot)
	}
	if !strings.Contains(snapshot, "id=3 ") {
		t.Errorf("resume should advance the watermark to the newest entry: %s", snapshot)
	}
}

func TestWebRootServed(t *testing.T) {
	s := newTestServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "<html>") {
		t.Errorf("body = %q", rec.Body.String())
	}
}

// readEvents turns the SSE body into a channel of records formatted as
// "event=<name> id=<id> data=<json>", so tests can assert on the resume
// watermark as well as the payload.
func readEvents(body io.Reader) <-chan string {
	out := make(chan string, 64)
	go func() {
		defer close(out)
		scanner := bufio.NewScanner(body)
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		var event, data, id string
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			case strings.HasPrefix(line, "id: "):
				id = strings.TrimPrefix(line, "id: ")
			case line == "" && data != "":
				out <- fmt.Sprintf("event=%s id=%s data=%s", event, id, data)
				event, data, id = "", "", ""
			}
		}
	}()
	return out
}

func nextEvent(t *testing.T, events <-chan string, want string) string {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case got, ok := <-events:
			if !ok {
				t.Fatalf("stream closed while waiting for a %q event", want)
			}
			if strings.HasPrefix(got, "event="+want+" ") {
				return got
			}
		case <-deadline:
			t.Fatalf("timed out waiting for a %q event", want)
		}
	}
}

// TestCaptureScopeFiltersAtIngest covers the point of the subsystem allowlist:
// out-of-scope entries must not reach the buffer, the catalog or the history file,
// which is what keeps a chatty app from producing hundreds of megabytes.
func TestCaptureScopeFiltersAtIngest(t *testing.T) {
	dir := t.TempDir()
	store, err := history.Open(filepath.Join(dir, "session.ndjson"), 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	s := New(Options{WebFS: fstest.MapFS{}, BufferSize: 100, Logger: log.New(io.Discard, "", 0), History: store})
	t.Cleanup(s.Close)

	s.handleStartScope(t, []string{"centrifugo-websocket.futures.open-order", "com.a"})

	s.handleEntry(oslog.Entry{Subsystem: "centrifugo-websocket.futures.open-order", Level: "Debug", Message: "wanted"})
	s.handleEntry(oslog.Entry{Subsystem: "com.apple.network", Level: "Debug", Message: "noise"})
	s.handleEntry(oslog.Entry{Subsystem: "com.a", Level: "Info", Message: "also wanted"})

	status := s.status()
	if status.Buffered != 2 {
		t.Errorf("buffered %d entries, want 2 in scope", status.Buffered)
	}
	if status.ScopeDropped != 1 {
		t.Errorf("ScopeDropped = %d, want 1", status.ScopeDropped)
	}

	facets := s.facets(oslog.Filter{}, 0)
	if _, ok := findFacetValue(facets.Subsystems, "com.apple.network"); ok {
		t.Error("an out-of-scope subsystem leaked into the value list")
	}
	if _, ok := findFacetValue(facets.Subsystems, "centrifugo-websocket.futures.open-order"); !ok {
		t.Error("the scoped subsystem should be listed")
	}

	stored, _, err := store.Query(oslog.Filter{}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range stored {
		if e.Subsystem == "com.apple.network" {
			t.Error("an out-of-scope entry was written to the history file")
		}
	}
	if len(stored) != 2 {
		t.Errorf("history holds %d entries, want only the 2 in scope", len(stored))
	}
}

func TestStartRejectsInvalidScope(t *testing.T) {
	s := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/stream/start",
		strings.NewReader(`{"kind":"simulator","id":"x","subsystems":["com.a\" OR 1==1"]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an invalid subsystem name", rec.Code)
	}
}

func TestFacetsComeFromTheWholeSessionNotJustTheBuffer(t *testing.T) {
	s := newTestServer(t)
	// Buffer holds 100 entries; the rare value is pushed out of it long before it
	// leaves the session value list.
	s.handleEntry(oslog.Entry{Subsystem: "rare.subsystem", Level: "Debug", Message: "rare"})
	for i := 0; i < 300; i++ {
		s.handleEntry(oslog.Entry{Subsystem: "com.apple.network", Level: "Debug", Message: "noise"})
	}

	if got := s.buf.Len(); got != 100 {
		t.Fatalf("buffer holds %d entries, want the 100 entry cap", got)
	}
	inBuffer := false
	for _, e := range s.buf.All() {
		if e.Subsystem == "rare.subsystem" {
			inBuffer = true
		}
	}
	if inBuffer {
		t.Fatal("precondition: the rare subsystem should have been evicted")
	}

	facets := s.facets(oslog.Filter{}, 0)
	facet, ok := findFacetValue(facets.Subsystems, "rare.subsystem")
	if !ok {
		t.Fatalf("rare.subsystem is missing from the value list: %+v", facets.Subsystems)
	}
	if facet.Count != 1 {
		t.Errorf("session count = %d, want 1", facet.Count)
	}
}

// handleStartScope installs a capture scope the way handleStart does.
func (s *Server) handleStartScope(t *testing.T, subsystems []string) {
	t.Helper()
	scope, err := oslog.NewScope(subsystems)
	if err != nil {
		t.Fatalf("NewScope: %v", err)
	}
	s.mu.Lock()
	s.scope = scope
	s.scopeDropped = 0
	s.mu.Unlock()
	s.catalog.Reset()
}

func findFacetValue(facets []oslog.Facet, value string) (oslog.Facet, bool) {
	for _, f := range facets {
		if f.Value == value {
			return f, true
		}
	}
	return oslog.Facet{}, false
}

// TestStartAppliesCaptureSettings checks the three modal settings reach the
// scope. A device target is used because it fails immediately without admin
// rights - the scope is stored before the stream starts, so it can be asserted
// without spawning a simulator capture.
func TestStartAppliesCaptureSettings(t *testing.T) {
	s := newTestServer(t)
	handler := s.Handler()

	body := `{"kind":"device","id":"no-such-device","name":"Test","level":"debug",
	          "subsystems":["centrifugo-websocket.futures.open-order"],
	          "ignoreApple":true,"ignoreNoSubsystem":true}`
	req := httptest.NewRequest(http.MethodPost, "/api/stream/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	var status Status
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatalf("decoding status: %v", err)
	}

	if !status.ScopeIgnoreApple || !status.ScopeIgnoreNoSubs {
		t.Errorf("exclusion flags did not reach the status: %+v", status)
	}
	if len(status.ScopeList) != 1 || status.ScopeList[0] != "centrifugo-websocket.futures.open-order" {
		t.Errorf("ScopeList = %v, want the posted subsystem", status.ScopeList)
	}
	// The list is decisive, so the predicate must be the list alone.
	if strings.Contains(status.Predicate, "NOT subsystem") {
		t.Errorf("predicate = %q, want the list only", status.Predicate)
	}
	if !strings.Contains(status.Predicate, "centrifugo-websocket.futures.open-order") {
		t.Errorf("predicate = %q, want the posted subsystem", status.Predicate)
	}

	// And the settings must actually filter.
	s.handleEntry(oslog.Entry{Subsystem: "centrifugo-websocket.futures.open-order", Message: "keep"})
	s.handleEntry(oslog.Entry{Subsystem: "com.apple.network", Message: "drop: apple"})
	s.handleEntry(oslog.Entry{Subsystem: "", Message: "drop: no subsystem"})
	if got := s.buf.Len(); got != 1 {
		t.Errorf("buffered %d entries, want only the one inside the scope", got)
	}
}
