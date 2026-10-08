package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rudy/log-server/internal/oslog"
)

// TestCloseUnblocksOpenStreams ensures shutdown does not have to wait for the
// idle timeout of a live SSE connection.
func TestCloseUnblocksOpenStreams(t *testing.T) {
	s := newTestServer(t)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	s.handleEntry(oslog.Entry{Subsystem: "com.a", Message: "hello"})

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("opening the stream: %v", err)
	}
	defer resp.Body.Close()

	nextEvent(t, readEvents(resp.Body), "snapshot")

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		s.Close()
	}()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked while an SSE client was connected")
	}

	// The handler must return so the response body reaches EOF.
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(io.Discard, resp.Body)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the SSE response did not finish after Close")
	}
}
