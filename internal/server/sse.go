package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rudy/log-server/internal/oslog"
)

const (
	clientQueueSize    = 16384
	flushInterval      = 100 * time.Millisecond
	keepAliveInterval  = 20 * time.Second
	maxEntriesPerBatch = 250
)

// message is a unit of work for a subscribed client.
type message struct {
	entries []oslog.Entry
	status  *Status
}

// client is a single SSE subscriber with its own filter.
type client struct {
	filter  oslog.Filter
	ch      chan message
	dropped atomic.Uint64
}

// hub fans log entries out to subscribed clients, applying each client's filter
// before delivery so that a filtered view never depends on the browser keeping
// up with an unfiltered firehose.
type hub struct {
	mu      sync.RWMutex
	clients map[*client]struct{}
}

func newHub() *hub {
	return &hub{clients: map[*client]struct{}{}}
}

func (h *hub) subscribe(f oslog.Filter) *client {
	c := &client{filter: f, ch: make(chan message, clientQueueSize)}
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
	return c
}

func (h *hub) unsubscribe(c *client) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
}

func (h *hub) clientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// publish delivers one entry to every client whose filter matches. Clients that
// cannot keep up drop entries instead of blocking the ingest path; the drop
// count is reported back to the client so the UI can warn the user.
func (h *hub) publish(e oslog.Entry) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.clients {
		if !c.filter.Match(e) {
			continue
		}
		select {
		case c.ch <- message{entries: []oslog.Entry{e}}:
		default:
			c.dropped.Add(1)
		}
	}
}

func (h *hub) broadcastStatus(s Status) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.clients {
		select {
		case c.ch <- message{status: &s}:
		default:
		}
	}
}

type snapshotPayload struct {
	Mode      string              `json:"mode"` // full | resume
	Entries   []oslog.Entry       `json:"entries"`
	Matched   int                 `json:"matched"`
	Truncated bool                `json:"truncated"`
	Older     bool                `json:"older"`
	Facets    oslog.CatalogFacets `json:"facets"`
	Filter    oslog.Filter        `json:"filter"`
	Status    Status              `json:"status"`
}

// handleSSE streams log entries to the browser as Server-Sent Events.
//
// Semantics:
//   - A connection without Last-Event-ID receives a `snapshot` event with
//     mode=full containing the matching buffered entries (capped by the
//     `snapshot` parameter), followed by live `log` events.
//   - A reconnection carrying Last-Event-ID receives mode=resume with only the
//     entries missed while disconnected, so nothing is duplicated.
//   - Entries are batched to keep the event rate low on busy streams.
func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	filter, err := oslog.NewFilter(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming is not supported by this connection")
		return
	}

	snapshotLimit := intParam(r, "snapshot", 5000, 0, 20000)
	resumeID := lastEventID(r)

	header := w.Header()
	header.Set("Content-Type", "text/event-stream; charset=utf-8")
	header.Set("Cache-Control", "no-cache, no-transform")
	header.Set("Connection", "keep-alive")
	header.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Subscribe before reading the buffer so no entry can slip through the gap;
	// duplicates are removed with the ID watermark below.
	client := s.hub.subscribe(filter)
	defer s.hub.unsubscribe(client)

	mode := "full"
	entries, matched := s.buf.Snapshot(filter, snapshotLimit, resumeID)
	if resumeID > 0 {
		mode = "resume"
	}
	watermark := resumeID
	for _, e := range entries {
		if e.ID > watermark {
			watermark = e.ID
		}
	}
	// Older matching entries may live further back in the buffer, or on disk.
	older := matched > len(entries)
	if !older && s.opts.History != nil && s.opts.History.Stats().Entries > 0 {
		older = true
	}

	s.writeEvent(w, "snapshot", strconv.FormatUint(watermark, 10), snapshotPayload{
		Mode:      mode,
		Entries:   entries,
		Matched:   matched,
		Truncated: matched > len(entries),
		Older:     older,
		Facets:    s.catalog.Facets(1000),
		Filter:    filter,
		Status:    s.status(),
	})
	flusher.Flush()
	s.logger.Printf("sse client connected (clients=%d, mode=%s, buffered=%d/%d)", s.hub.clientCount(), mode, len(entries), matched)

	pending := make([]oslog.Entry, 0, maxEntriesPerBatch)
	flush := func() {
		if len(pending) == 0 {
			return
		}
		s.writeEvent(w, "log", strconv.FormatUint(pending[len(pending)-1].ID, 10), map[string]any{"entries": pending})
		pending = pending[:0]
		flusher.Flush()
	}

	flushTicker := time.NewTicker(flushInterval)
	defer flushTicker.Stop()
	keepAlive := time.NewTicker(keepAliveInterval)
	defer keepAlive.Stop()

	for {
		select {
		case <-r.Context().Done():
			flush()
			s.logger.Printf("sse client disconnected (clients=%d)", s.hub.clientCount()-1)
			return
		case <-s.ctx.Done():
			// The server is shutting down: returning promptly lets Shutdown
			// finish instead of waiting for the idle timeout.
			flush()
			return
		case msg := <-client.ch:
			if msg.status != nil {
				flush()
				st := *msg.status
				st.Clients = s.hub.clientCount()
				s.writeEvent(w, "status", "", st)
				flusher.Flush()
			}
			for _, e := range msg.entries {
				if e.ID <= watermark {
					continue
				}
				watermark = e.ID
				pending = append(pending, e)
				if len(pending) >= maxEntriesPerBatch {
					flush()
				}
			}
			if dropped := client.dropped.Swap(0); dropped > 0 {
				flush()
				s.writeEvent(w, "warning", "", map[string]any{
					"message": fmt.Sprintf("%d log entries were dropped for this viewer because it could not keep up", dropped),
					"dropped": dropped,
				})
				flusher.Flush()
			}
		case <-flushTicker.C:
			flush()
		case <-keepAlive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

// writeEvent serialises one SSE frame. The event ID is omitted when empty, which
// is the case for status updates that must not advance the resume watermark.
func (s *Server) writeEvent(w http.ResponseWriter, event, id string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		s.logger.Printf("encoding %s event: %v", event, err)
		return
	}
	if id != "" {
		fmt.Fprintf(w, "id: %s\n", id)
	}
	fmt.Fprintf(w, "event: %s\n", event)
	fmt.Fprintf(w, "data: %s\n\n", data)
}

// lastEventID returns the resume watermark supplied by the client, either in the
// standard Last-Event-ID header or as a query parameter for manual clients.
func lastEventID(r *http.Request) uint64 {
	raw := r.Header.Get("Last-Event-ID")
	if raw == "" {
		raw = r.URL.Query().Get("lastEventId")
	}
	if raw == "" {
		return 0
	}
	v, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0
	}
	return v
}
