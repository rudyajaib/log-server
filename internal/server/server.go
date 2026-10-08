// Package server exposes the OSLog stream over HTTP: a small JSON API for
// controlling the capture and a Server-Sent Events feed for live log delivery.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rudy/log-server/internal/history"
	"github.com/rudy/log-server/internal/oslog"
)

// Options configures a Server.
type Options struct {
	WebFS      fs.FS
	BufferSize int
	Logger     *log.Logger
	// History persists captured entries to disk. Optional: when nil, only the
	// in-memory buffer is available.
	History *history.Store
}

// Status is the payload sent to clients describing the current capture.
type Status struct {
	oslog.Status
	Total             uint64        `json:"total"`
	Buffered          int           `json:"buffered"`
	Capacity          int           `json:"capacity"`
	Evicted           uint64        `json:"evicted"`
	BufferFrom        string        `json:"bufferFrom,omitempty"`
	BufferTo          string        `json:"bufferTo,omitempty"`
	Rate              float64       `json:"rate"`
	Clients           int           `json:"clients"`
	Matched           int           `json:"matched"`
	Scope             string        `json:"scope"`
	ScopeList         []string      `json:"scopeSubsystems,omitempty"`
	ScopeIgnoreApple  bool          `json:"scopeIgnoreApple"`
	ScopeIgnoreNoSubs bool          `json:"scopeIgnoreNoSubsystem"`
	ScopeDropped      uint64        `json:"scopeDropped"`
	Values            int           `json:"values"`
	Admin             bool          `json:"admin"`
	DeviceNotice      string        `json:"deviceNotice,omitempty"`
	History           history.Stats `json:"history"`
}

// Server implements http.Handler.
type Server struct {
	opts    Options
	logger  *log.Logger
	buf     *oslog.Buffer
	catalog *oslog.Catalog
	hub     *hub
	web     http.Handler

	ctx    context.Context
	cancel context.CancelFunc

	mu              sync.Mutex
	streamer        *oslog.Streamer
	scope           oslog.Scope
	scopeDropped    uint64
	rate            float64
	lastSampleTotal uint64
	lastSampleAt    time.Time
}

// New creates a server. Call Close to release resources.
func New(opts Options) *Server {
	if opts.Logger == nil {
		opts.Logger = log.Default()
	}
	if opts.BufferSize < 1 {
		opts.BufferSize = 20000
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		opts:         opts,
		logger:       opts.Logger,
		buf:          oslog.NewBuffer(opts.BufferSize),
		catalog:      oslog.NewCatalog(),
		hub:          newHub(),
		ctx:          ctx,
		cancel:       cancel,
		lastSampleAt: time.Now(),
	}
	s.streamer = oslog.NewStreamer(oslog.Options{Logger: s.logger})
	s.web = s.webHandler()
	go s.sampleLoop(ctx)
	return s
}

// Close stops any running capture and background workers.
func (s *Server) Close() {
	s.mu.Lock()
	streamer := s.streamer
	s.mu.Unlock()
	streamer.Stop()
	s.cancel()
}

// Handler returns the HTTP routes. The Go 1.22 method patterns are used so that
// / is only matched by GET/HEAD.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/targets", s.handleTargets)
	mux.HandleFunc("POST /api/stream/start", s.handleStart)
	mux.HandleFunc("POST /api/stream/stop", s.handleStop)
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("GET /api/facets", s.handleFacets)
	mux.HandleFunc("GET /api/logs", s.handleLogs)
	mux.HandleFunc("GET /api/stream", s.handleSSE)
	mux.HandleFunc("GET /api/export", s.handleExport)
	mux.HandleFunc("POST /api/buffer/clear", s.handleClear)
	mux.HandleFunc("POST /api/history/clear", s.handleHistoryClear)
	mux.HandleFunc("POST /api/simulators/boot", s.handleBoot)
	mux.Handle("GET /", s.web)
	return s.withCORS(mux)
}

// withCORS allows other local tools to read the API. Only safe methods get the
// header: mutating endpoints require a JSON content type, which forces browsers
// to preflight cross-origin requests and blocks drive-by CSRF from web pages.
func (s *Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Access-Control-Allow-Origin", "*")
		}
		if r.Method == http.MethodOptions {
			w.Header().Set("Allow", "GET, POST")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) webHandler() http.Handler {
	fileServer := http.FileServer(http.FS(s.opts.WebFS))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		fileServer.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------- capture --

type startRequest struct {
	Kind      string `json:"kind"`
	ID        string `json:"id"`
	Name      string `json:"name"`
	Level     string `json:"level"`
	Predicate string `json:"predicate"`
	Runtime   string `json:"runtime"`
	Model     string `json:"model"`
	// The capture scope. It is pushed into the `log stream` predicate where that is
	// possible and enforced again as entries arrive, so everything outside it never
	// reaches the buffer, the catalog or the history file.
	Subsystems        []string `json:"subsystems"`
	IgnoreApple       bool     `json:"ignoreApple"`
	IgnoreNoSubsystem bool     `json:"ignoreNoSubsystem"`
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	var req startRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	req.Kind = strings.ToLower(strings.TrimSpace(req.Kind))
	if req.Kind != oslog.KindSimulator && req.Kind != oslog.KindDevice {
		writeError(w, http.StatusBadRequest, `kind must be "simulator" or "device"`)
		return
	}
	if strings.TrimSpace(req.ID) == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	level := strings.ToLower(strings.TrimSpace(req.Level))
	if level == "" {
		level = "default"
	}
	if !oslog.ValidLevel(level) {
		writeError(w, http.StatusBadRequest, "level must be one of debug, info, default")
		return
	}
	predicate := strings.TrimSpace(req.Predicate)
	if len(predicate) > 4000 {
		writeError(w, http.StatusBadRequest, "predicate is too long")
		return
	}
	scope, err := oslog.NewScope(req.Subsystems)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	scope.IgnoreApple = req.IgnoreApple
	scope.IgnoreNoSubsystem = req.IgnoreNoSubsystem

	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = req.ID
	}
	target := oslog.Target{Kind: req.Kind, ID: req.ID, Name: name, Runtime: req.Runtime, Model: req.Model, Ready: true}

	s.mu.Lock()
	streamer := s.streamer
	s.mu.Unlock()

	// Starting a new capture replaces the previous one.
	streamer.Stop()

	// A new capture starts a fresh buffer so counts and facets match the session,
	// and a fresh history file so scrolling back stays inside that session.
	s.buf.Clear()
	if s.opts.History != nil {
		if err := s.opts.History.Restart(); err != nil {
			s.logger.Printf("resetting history: %v", err)
		}
	}
	s.mu.Lock()
	s.scope = scope
	s.scopeDropped = 0
	s.catalog.Reset()
	s.rate = 0
	s.lastSampleTotal = 0
	s.lastSampleAt = time.Now()
	s.mu.Unlock()

	err = streamer.Start(s.ctx, oslog.Options{
		Target:    target,
		Level:     level,
		Predicate: scope.Predicate(predicate),
		OnEntry:   s.handleEntry,
		OnStatus:  func(oslog.Status) { s.hub.broadcastStatus(s.status()) },
		Logger:    s.logger,
	})
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}

	s.logger.Printf("capture started: %s %q (%s, level=%s, scope=%s)", target.Kind, target.Name, target.ID, level, scope.Describe())
	if effective := scope.Predicate(predicate); effective != "" {
		s.logger.Printf("capture predicate: %s", effective)
	}
	if scope.IgnoreNoSubsystem {
		s.logger.Printf("entries without a subsystem are dropped on arrival (not expressible in a predicate)")
	}
	writeJSON(w, http.StatusOK, s.status())
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	if err := requireJSON(r); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.mu.Lock()
	streamer := s.streamer
	s.mu.Unlock()

	streamer.Stop()
	s.logger.Printf("capture stopped")
	writeJSON(w, http.StatusOK, s.status())
}

func (s *Server) handleEntry(e oslog.Entry) {
	// The scope is applied again here, not only in the `log stream` predicate, so
	// that out-of-scope entries cannot reach the buffer, the catalog or the
	// history file even if the predicate was ignored or unsupported.
	if !s.inScope(e) {
		s.mu.Lock()
		s.scopeDropped++
		s.mu.Unlock()
		return
	}

	stored := s.buf.Add(e)
	s.catalog.Record(stored)
	if s.opts.History != nil {
		if err := s.opts.History.Append(stored); err != nil {
			s.logger.Printf("history append failed: %v", err)
		}
	}
	s.hub.publish(stored)
}

// ---------------------------------------------------------------- queries --

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	status := s.status()
	// A caller may pass a filter so the match count describes the view it is
	// showing. Only the count is needed, so this takes the cheap single-pass path
	// rather than building the facet lists as well.
	if len(r.URL.Query()) > 0 {
		if filter, err := oslog.NewFilter(r.URL.Query()); err == nil {
			status.Matched = s.buf.Count(filter)
		}
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleFacets(w http.ResponseWriter, r *http.Request) {
	filter, err := oslog.NewFilter(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	limit := intParam(r, "limit", 1000, 1, 20000)
	writeJSON(w, http.StatusOK, s.facets(filter, limit))
}

// facetsResponse is the value catalog plus the context a viewer needs to read it.
type facetsResponse struct {
	oslog.CatalogFacets
	Matched  int    `json:"matched"`
	Buffered int    `json:"buffered"`
	Evicted  uint64 `json:"evicted"`
	Scope    string `json:"scope"`
}

// facets reports the values seen during the whole capture rather than only those
// in the buffer. A buffer-scoped list made rare subsystems flicker in and out as
// the window slid past them; a session list stays put and stays selectable.
func (s *Server) facets(filter oslog.Filter, limit int) facetsResponse {
	return facetsResponse{
		CatalogFacets: s.catalog.Facets(limit),
		Matched:       s.buf.Count(filter),
		Buffered:      s.buf.Len(),
		Evicted:       s.buf.Evicted(),
		Scope:         s.scopeDescription(),
	}
}

func (s *Server) scopeDescription() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.scope.Describe()
}

// inScope reports whether an entry is inside the capture scope.
func (s *Server) inScope(e oslog.Entry) bool {
	s.mu.Lock()
	scope := s.scope
	s.mu.Unlock()
	return scope.Match(e)
}

type logsResponse struct {
	Entries   []oslog.Entry `json:"entries"`
	Matched   int           `json:"matched"`
	Returned  int           `json:"returned"`
	Truncated bool          `json:"truncated"`
	HasMore   bool          `json:"hasMore"`
	Source    string        `json:"source"`
	LastID    uint64        `json:"lastId"`
}

// handleLogs pages through matching entries. Paging backwards with `before`
// walks the in-memory buffer first and then continues into the on-disk history,
// which is what lets a viewer scroll back past the buffer into entries that were
// evicted minutes ago.
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	filter, err := oslog.NewFilter(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	limit := intParam(r, "limit", 1000, 1, 20000)
	after := uint64Param(r, "after", 0)
	before := uint64Param(r, "before", 0)
	useHistory := !strings.EqualFold(r.URL.Query().Get("scope"), "buffer")

	entries, matched := s.buf.Query(oslog.Query{Filter: filter, Limit: limit, After: after, Before: before})
	source := "buffer"
	if len(entries) == 0 {
		source = ""
	}
	hasMore := matched > len(entries)

	if useHistory && !hasMore && s.opts.History != nil && len(entries) < limit {
		oldest := before
		if len(entries) > 0 {
			oldest = entries[0].ID
		}
		older, more, err := s.opts.History.Query(filter, limit-len(entries), oldest)
		if err != nil {
			s.logger.Printf("history query failed: %v", err)
		} else if len(older) > 0 {
			entries = append(older, entries...)
			switch source {
			case "buffer":
				source = "buffer+history"
			case "":
				source = "history"
			}
			hasMore = more
		}
	}
	if source == "" {
		source = "empty"
	}

	writeJSON(w, http.StatusOK, logsResponse{
		Entries:   entries,
		Matched:   matched,
		Returned:  len(entries),
		Truncated: matched > len(entries),
		HasMore:   hasMore,
		Source:    source,
		LastID:    s.buf.LastID(),
	})
}

func (s *Server) handleHistoryClear(w http.ResponseWriter, r *http.Request) {
	if err := requireJSON(r); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.opts.History == nil {
		writeError(w, http.StatusNotFound, "history is disabled")
		return
	}
	if err := s.opts.History.Restart(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.opts.History.Stats())
}

func (s *Server) handleClear(w http.ResponseWriter, r *http.Request) {
	if err := requireJSON(r); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.buf.Clear()
	s.hub.broadcastStatus(s.status())
	writeJSON(w, http.StatusOK, s.status())
}

func (s *Server) handleTargets(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	simulators, devices, errs := oslog.ListTargets(ctx)
	payload := map[string]any{
		"simulators": simulators,
		"devices":    devices,
	}
	if len(errs) > 0 {
		payload["errors"] = errs
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *Server) handleBoot(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	if err := oslog.BootSimulator(ctx, req.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "booting", "id": req.ID})
}

// ----------------------------------------------------------------- status --

func (s *Server) sampleLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sample()
		}
	}
}

func (s *Server) sample() {
	now := time.Now()
	total := s.buf.Total()

	s.mu.Lock()
	elapsed := now.Sub(s.lastSampleAt).Seconds()
	var delta uint64
	if total >= s.lastSampleTotal {
		delta = total - s.lastSampleTotal
	}
	instant := 0.0
	if elapsed > 0 {
		instant = float64(delta) / elapsed
	}
	s.rate = s.rate*0.6 + instant*0.4
	s.lastSampleTotal = total
	s.lastSampleAt = now
	running := s.streamer.Status().Running
	s.mu.Unlock()

	// Only push periodic updates when something is happening.
	if running || delta > 0 || s.hub.clientCount() > 0 {
		s.hub.broadcastStatus(s.status())
	}
}

// status builds the combined capture + buffer snapshot.
func (s *Server) status() Status {
	s.mu.Lock()
	rate := s.rate
	st := s.streamer.Status()
	s.mu.Unlock()
	return s.buildStatus(st, rate)
}

func (s *Server) buildStatus(st oslog.Status, rate float64) Status {
	s.mu.Lock()
	scope, dropped := s.scope, s.scopeDropped
	s.mu.Unlock()

	out := Status{
		Status:            st,
		Total:             s.buf.Total(),
		Buffered:          s.buf.Len(),
		Capacity:          s.buf.Capacity(),
		Evicted:           s.buf.Evicted(),
		Rate:              rate,
		Clients:           s.hub.clientCount(),
		Scope:             scope.Describe(),
		ScopeList:         scope.Subsystems,
		ScopeIgnoreApple:  scope.IgnoreApple,
		ScopeIgnoreNoSubs: scope.IgnoreNoSubsystem,
		ScopeDropped:      dropped,
		Values:            s.catalog.Distinct(),
		Admin:             isAdmin(),
		DeviceNotice:      deviceNotice(),
	}
	if oldest, newest, ok := s.buf.TimeSpan(); ok {
		out.BufferFrom = oldest.Format(time.RFC3339Nano)
		out.BufferTo = newest.Format(time.RFC3339Nano)
	}
	if s.opts.History != nil {
		out.History = s.opts.History.Stats()
	}
	return out
}

func isAdmin() bool { return os.Geteuid() == 0 }

func deviceNotice() string {
	if isAdmin() {
		return ""
	}
	return "Connected devices can be listed, but streaming from a physical device requires admin " +
		"privileges: restart the server with sudo. Simulators stream without elevated privileges."
}

// ------------------------------------------------------------------ helpers --

func decodeJSON(r *http.Request, dst any) error {
	if err := requireJSON(r); err != nil {
		return err
	}
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("invalid request body: %w", err)
	}
	return nil
}

// requireJSON rejects bodies that are not JSON. Browsers must preflight a
// cross-origin request with an application/json content type, which keeps a web
// page from driving the capture on a local server it does not own.
func requireJSON(r *http.Request) error {
	ct := r.Header.Get("Content-Type")
	if mediatype, _, err := mime.ParseMediaType(ct); err == nil {
		if mediatype == "application/json" {
			return nil
		}
		return fmt.Errorf("expected Content-Type application/json, got %q", ct)
	}
	if ct == "" {
		return nil
	}
	return fmt.Errorf("expected Content-Type application/json, got %q", ct)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		// The response is already committed; nothing useful left to do.
		_ = err
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func intParam(r *http.Request, key string, def, min, max int) int {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	if v < min {
		v = min
	}
	if v > max {
		v = max
	}
	return v
}

func uint64Param(r *http.Request, key string, def uint64) uint64 {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return def
	}
	v, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return def
	}
	return v
}
