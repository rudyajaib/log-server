package history

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rudy/log-server/internal/oslog"
)

func newTempStore(t *testing.T, maxBytes int64) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "nested", "session.ndjson"), maxBytes)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func mustFilter(t *testing.T, values map[string][]string) oslog.Filter {
	t.Helper()
	f, err := oslog.NewFilter(values)
	if err != nil {
		t.Fatalf("NewFilter: %v", err)
	}
	return f
}

func appendEntries(t *testing.T, store *Store, entries []oslog.Entry) {
	t.Helper()
	for i, e := range entries {
		e.ID = uint64(i + 1)
		if e.Timestamp.IsZero() {
			e.Timestamp = time.Now()
		}
		if err := store.Append(e); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
}

// TestQuerySurvivesBeyondTheBuffer is the scenario the feature exists for: a
// rare subsystem must still be findable long after a busy stream would have
// pushed it out of the in-memory buffer.
func TestQuerySurvivesBeyondTheBuffer(t *testing.T) {
	store := newTempStore(t, 0)

	entries := []oslog.Entry{
		{Subsystem: "centrifugo-websocket.futures.open-order", Category: "order", Level: "Debug", Message: "order 1"},
	}
	for i := 0; i < 5000; i++ {
		entries = append(entries, oslog.Entry{Subsystem: "com.apple.network", Level: "Debug", Message: "noise"})
	}
	entries = append(entries, oslog.Entry{Subsystem: "centrifugo-websocket.futures.open-order", Category: "order", Level: "Debug", Message: "order 2"})
	appendEntries(t, store, entries)

	filter := mustFilter(t, map[string][]string{"subsystem": {"centrifugo-websocket.futures.open-order"}})
	found, hasMore, err := store.Query(filter, 10, 0)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(found) != 2 {
		t.Fatalf("found %d rare entries, want 2 (%v)", len(found), found)
	}
	if found[0].Message != "order 1" || found[1].Message != "order 2" {
		t.Errorf("entries = %q, %q; want oldest first", found[0].Message, found[1].Message)
	}
	if hasMore {
		t.Error("hasMore = true, want false when every match was returned")
	}
}

func TestQueryPagesBackwards(t *testing.T) {
	store := newTempStore(t, 0)

	var entries []oslog.Entry
	for i := 0; i < 20; i++ {
		entries = append(entries, oslog.Entry{Subsystem: "com.a", Message: "m", Level: "Info"})
	}
	appendEntries(t, store, entries)

	// Newest 5.
	page1, hasMore, err := store.Query(oslog.Filter{}, 5, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !hasMore {
		t.Error("hasMore = false, but older entries remain")
	}
	if got := page1[len(page1)-1].ID; got != 20 {
		t.Errorf("newest page ends at ID %d, want 20", got)
	}

	// The page before it.
	page2, _, err := store.Query(oslog.Filter{}, 5, page1[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 5 {
		t.Fatalf("second page has %d entries, want 5", len(page2))
	}
	if page2[len(page2)-1].ID != 15 || page2[0].ID != 11 {
		t.Errorf("second page IDs %d..%d, want 11..15", page2[0].ID, page2[len(page2)-1].ID)
	}

	// Reading past the start must not error or duplicate.
	page5, _, err := store.Query(oslog.Filter{}, 100, page2[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(page5) != 10 {
		t.Errorf("remaining page has %d entries, want 10", len(page5))
	}
}

// TestQueryAcrossBlockBoundaries exercises the chunked backwards reader with
// entries much larger than a single read block.
func TestQueryAcrossBlockBoundaries(t *testing.T) {
	store := newTempStore(t, 0)

	big := make([]byte, 8*1024)
	for i := range big {
		big[i] = 'x'
	}
	var entries []oslog.Entry
	for i := 0; i < 600; i++ { // ~5 MB, several 1 MB blocks
		e := oslog.Entry{Subsystem: "com.big", Level: "Info", Message: string(big)}
		if i == 137 {
			e.Subsystem = "com.needle"
			e.Message = "found me"
		}
		entries = append(entries, e)
	}
	appendEntries(t, store, entries)

	filter := mustFilter(t, map[string][]string{"subsystem": {"com.needle"}})
	found, _, err := store.Query(filter, 10, 0)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(found) != 1 || found[0].Message != "found me" {
		t.Fatalf("found %d entries (%v), want the needle across a block boundary", len(found), found)
	}
	if found[0].ID != 138 {
		t.Errorf("needle ID = %d, want 138", found[0].ID)
	}
}

func TestRestartClearsHistory(t *testing.T) {
	store := newTempStore(t, 0)
	appendEntries(t, store, []oslog.Entry{{Subsystem: "com.a", Message: "old"}})

	if err := store.Restart(); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if got := store.Stats().Entries; got != 0 {
		t.Errorf("entries after Restart = %d, want 0", got)
	}
	appendEntries(t, store, []oslog.Entry{{Subsystem: "com.a", Message: "new"}})

	found, _, err := store.Query(oslog.Filter{}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].Message != "new" {
		t.Errorf("entries after Restart = %v, want only the new one", found)
	}
}

func TestSizeLimitStopsRecording(t *testing.T) {
	store := newTempStore(t, 4096)
	big := make([]byte, 1024)
	for i := range big {
		big[i] = 'y'
	}
	var entries []oslog.Entry
	for i := 0; i < 50; i++ {
		entries = append(entries, oslog.Entry{Subsystem: "com.a", Level: "Info", Message: string(big)})
	}
	appendEntries(t, store, entries)

	stats := store.Stats()
	if !stats.Full {
		t.Error("Full = false, want true once the size limit is reached")
	}
	if stats.Bytes > 4096+2048 {
		t.Errorf("Bytes = %d, want the limit to be respected", stats.Bytes)
	}
	if stats.Entries >= 50 {
		t.Errorf("Entries = %d, want recording to stop before writing everything", stats.Entries)
	}
}

func TestQueryOnEmptyStore(t *testing.T) {
	store := newTempStore(t, 0)
	found, hasMore, err := store.Query(oslog.Filter{}, 10, 0)
	if err != nil {
		t.Fatalf("Query on empty store: %v", err)
	}
	if len(found) != 0 || hasMore {
		t.Errorf("got %d entries, hasMore = %v; want none", len(found), hasMore)
	}
}

func TestQueryMissingFileIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "session.ndjson"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "session.ndjson")); err != nil {
		t.Fatal(err)
	}

	found, _, err := store.Query(oslog.Filter{}, 10, 0)
	if err != nil {
		t.Fatalf("Query after the file disappeared: %v", err)
	}
	if len(found) != 0 {
		t.Errorf("found %d entries, want none", len(found))
	}
}
