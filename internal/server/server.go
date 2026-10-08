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

	"github.com/rudy/log-server/internal/oslog"
)

// Options configures a Server.
type Options struct {
	WebFS      fs.FS
	BufferSize int
	Logger     *log.Logger
}

// Status is the payload sent to clients describing the current capture.
type Status struct {
	oslog.Status
	Total        uint64  `json:"total"`
	Buffered     int     `json:"buffered"`
	Capacity     int     `json:"capacity"`
	Evicted      uint64  `json:"evicted"`
	Rate         float64 `json:"rate"`
	Clients      int     `json:"clients"`
	Admin        bool    `json:"admin"`
	DeviceNotice string  `json:"deviceNotice,omitempty"`
}

// Server implements http.Handler.
type Server struct {
	opts   Options
	logger *log.Logger
	buf    *oslog.Buffer
	hub    *hub
	web    http.Handler

	ctx    context.Context
	cancel context.CancelFunc

	mu              sync.Mutex
	streamer        *oslog.Streamer
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

	// A new capture starts a fresh buffer so counts and facets match the session.
	s.buf.Clear()
	s.mu.Lock()
	s.rate = 0
	s.lastSampleTotal = 0
	s.lastSampleAt = time.Now()
	s.mu.Unlock()

	err := streamer.Start(s.ctx, oslog.Options{
		Target:    target,
		Level:     level,
		Predicate: predicate,
		OnEntry:   s.handleEntry,
		OnStatus:  func(oslog.Status) { s.hub.broadcastStatus(s.status()) },
		Logger:    s.logger,
	})
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}

	s.logger.Printf("capture started: %s %q (%s, level=%s)", target.Kind, target.Name, target.ID, level)
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
	stored := s.buf.Add(e)
	s.hub.publish(stored)
}

// ---------------------------------------------------------------- queries --

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.status())
}

func (s *Server) handleFacets(w http.ResponseWriter, r *http.Request) {
	filter, err := oslog.NewFilter(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	limit := intParam(r, "limit", 500, 1, 5000)
	writeJSON(w, http.StatusOK, s.buf.ScopedFacets(filter, limit))
}

type logsResponse struct {
	Entries   []oslog.Entry `json:"entries"`
	Matched   int           `json:"matched"`
	Returned  int           `json:"returned"`
	Truncated bool          `json:"truncated"`
	LastID    uint64        `json:"lastId"`
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	filter, err := oslog.NewFilter(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	limit := intParam(r, "limit", 1000, 1, 5000)
	after := uint64Param(r, "after", 0)

	entries, matched := s.buf.Query(filter, limit, after)
	writeJSON(w, http.StatusOK, logsResponse{
		Entries:   entries,
		Matched:   matched,
		Returned:  len(entries),
		Truncated: matched > len(entries),
		LastID:    s.buf.LastID(),
	})
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
	return Status{
		Status:       st,
		Total:        s.buf.Total(),
		Buffered:     s.buf.Len(),
		Capacity:     s.buf.Capacity(),
		Evicted:      s.buf.Evicted(),
		Rate:         rate,
		Clients:      s.hub.clientCount(),
		Admin:        isAdmin(),
		DeviceNotice: deviceNotice(),
	}
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
