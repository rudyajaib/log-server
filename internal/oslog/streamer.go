package oslog

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Stream states.
const (
	StateIdle       = "idle"
	StateRunning    = "running"
	StateRestarting = "restarting"
	StateError      = "error"
)

var validLevels = map[string]struct{}{"debug": {}, "info": {}, "default": {}}

// ValidLevel reports whether level is accepted by `log stream --level`.
func ValidLevel(level string) bool {
	_, ok := validLevels[strings.ToLower(strings.TrimSpace(level))]
	return ok
}

// Status is a snapshot of the streamer lifecycle.
type Status struct {
	State         string    `json:"state"`
	Running       bool      `json:"running"`
	Epoch         uint64    `json:"epoch"`
	Target        Target    `json:"target"`
	Level         string    `json:"level"`
	Predicate     string    `json:"predicate"`
	Command       string    `json:"command"`
	StartedAt     time.Time `json:"startedAt"`
	UptimeSeconds float64   `json:"uptimeSeconds"`
	Restarts      int       `json:"restarts"`
	Received      uint64    `json:"received"`
	LastError     string    `json:"lastError"`
	Notices       []string  `json:"notices,omitempty"`
}

// Options configures a Streamer.
type Options struct {
	Target    Target
	Level     string
	Predicate string
	// OnEntry is invoked for every parsed log entry.
	OnEntry func(Entry)
	// OnStatus is invoked whenever the streamer lifecycle changes.
	OnStatus func(Status)
	Logger   *log.Logger
}

const (
	maxRestarts    = 5
	restartBackoff = 1500 * time.Millisecond
	maxNotices     = 20
)

// Streamer runs a `log stream` subprocess and turns its ndjson output into
// Entry values. It restarts the subprocess if it terminates unexpectedly and
// records actionable diagnostics, including the "must be admin" failure that
// occurs when streaming from a physical device without elevated privileges.
type Streamer struct {
	opts   Options
	logger *log.Logger

	mu        sync.Mutex
	state     string
	epoch     uint64
	startedAt time.Time
	restarts  int
	lastError string
	command   string
	notices   []string
	fatal     error

	received atomic.Uint64

	rootCancel context.CancelFunc
	done       chan struct{}
}

// NewStreamer creates an idle streamer. Call Start to begin streaming.
func NewStreamer(opts Options) *Streamer {
	return &Streamer{
		opts:   opts,
		logger: opts.Logger,
		state:  StateIdle,
		done:   closedChan(),
	}
}

func closedChan() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

// Start begins streaming. It returns an error when a stream is already running.
func (s *Streamer) Start(parent context.Context, opts Options) error {
	if opts.Logger == nil {
		opts.Logger = s.logger
	}

	s.mu.Lock()
	if s.state == StateRunning || s.state == StateRestarting {
		s.mu.Unlock()
		return errors.New("a log stream is already running")
	}
	s.opts = opts
	s.state = StateRunning
	s.epoch++
	s.restarts = 0
	s.lastError = ""
	s.fatal = nil
	s.notices = nil
	s.startedAt = time.Now()
	s.received.Store(0)
	ctx, cancel := context.WithCancel(parent)
	s.rootCancel = cancel
	s.done = make(chan struct{})
	done := s.done
	status := s.statusLocked()
	s.mu.Unlock()

	go func() {
		defer close(done)
		s.runLoop(ctx)
	}()
	s.emit(status)
	return nil
}

// Stop terminates the stream and waits for its subprocess to exit. It is safe to
// call when no stream is running.
func (s *Streamer) Stop() {
	s.mu.Lock()
	cancel := s.rootCancel
	running := s.state == StateRunning || s.state == StateRestarting
	done := s.done
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if running {
		<-done
	}
}

// Status returns the current lifecycle snapshot.
func (s *Streamer) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statusLocked()
}

func (s *Streamer) statusLocked() Status {
	var uptime float64
	if !s.startedAt.IsZero() && (s.state == StateRunning || s.state == StateRestarting) {
		uptime = time.Since(s.startedAt).Seconds()
	}
	return Status{
		State:         s.state,
		Running:       s.state == StateRunning || s.state == StateRestarting,
		Epoch:         s.epoch,
		Target:        s.opts.Target,
		Level:         s.opts.Level,
		Predicate:     s.opts.Predicate,
		Command:       s.command,
		StartedAt:     s.startedAt,
		UptimeSeconds: uptime,
		Restarts:      s.restarts,
		Received:      s.received.Load(),
		LastError:     s.lastError,
		Notices:       append([]string(nil), s.notices...),
	}
}

func (s *Streamer) emit(status Status) {
	if s.opts.OnStatus != nil {
		s.opts.OnStatus(status)
	}
}

func (s *Streamer) logf(format string, args ...any) {
	if s.logger != nil {
		s.logger.Printf(format, args...)
	}
}

func (s *Streamer) runLoop(ctx context.Context) {
	attempt := 0
	for {
		err := s.runOnce(ctx)
		if ctx.Err() != nil {
			s.finish(nil)
			return
		}
		if isFatal(err) || attempt >= maxRestarts {
			s.finish(err)
			return
		}
		attempt++

		s.mu.Lock()
		s.state = StateRestarting
		s.restarts = attempt
		s.lastError = errorText(err)
		status := s.statusLocked()
		s.mu.Unlock()
		s.emit(status)
		s.logf("log stream exited (%v); restarting (attempt %d/%d)", err, attempt, maxRestarts)

		select {
		case <-time.After(restartBackoff):
		case <-ctx.Done():
			s.finish(nil)
			return
		}
	}
}

// runOnce builds the command, streams its output until exit, then returns.
func (s *Streamer) runOnce(ctx context.Context) error {
	name, args := s.buildCommand()
	cmd := exec.CommandContext(ctx, name, args...)
	setProcessAttrs(cmd)
	cmd.Cancel = func() error { return terminateProcess(cmd) }
	cmd.WaitDelay = 3 * time.Second

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("attaching to stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("attaching to stderr: %w", err)
	}

	fullCommand := strings.Join(append([]string{name}, args...), " ")
	s.mu.Lock()
	s.command = fullCommand
	s.mu.Unlock()
	s.logf("starting log stream: %s", fullCommand)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting %s: %w", name, err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		s.readEvents(stdout)
	}()
	go func() {
		defer wg.Done()
		s.readStderr(stderr)
	}()
	wg.Wait()
	waitErr := cmd.Wait()

	if fatal := s.fatalErr(); fatal != nil {
		return fatal
	}
	if waitErr != nil {
		return waitErr
	}
	return nil
}

// buildCommand returns the argv for the configured target.
func (s *Streamer) buildCommand() (string, []string) {
	level := strings.ToLower(strings.TrimSpace(s.opts.Level))
	if level == "" {
		level = "default"
	}
	predicate := strings.TrimSpace(s.opts.Predicate)

	if s.opts.Target.Kind == KindDevice {
		args := []string{"stream", "--device-udid", s.opts.Target.ID, "--style", "ndjson", "--level", level}
		if predicate != "" {
			args = append(args, "--predicate", predicate)
		}
		return "log", args
	}

	args := []string{"simctl", "spawn", s.opts.Target.ID, "log", "stream", "--style", "ndjson", "--level", level}
	if predicate != "" {
		args = append(args, "--predicate", predicate)
	}
	return "xcrun", args
}

func (s *Streamer) readEvents(r io.Reader) {
	// Options are immutable for the lifetime of a run, so read them once.
	s.mu.Lock()
	onEntry := s.opts.OnEntry
	source := s.opts.Target.Name
	s.mu.Unlock()

	reader := bufio.NewReaderSize(r, 64*1024)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			if entry, ok := ParseEvent(line, source); ok {
				s.received.Add(1)
				if onEntry != nil {
					onEntry(entry)
				}
			} else if notice := strings.TrimSpace(string(line)); notice != "" {
				s.addNotice(notice)
			}
		}
		if err != nil {
			return
		}
	}
}

// addNotice records non-JSON output such as "Filtering the log data using ...".
func (s *Streamer) addNotice(notice string) {
	if isBenignNotice(notice) {
		return
	}
	fatal := fatalForNotice(notice)

	s.mu.Lock()
	defer s.mu.Unlock()
	if fatal != nil && s.fatal == nil {
		s.fatal = fatal
	}
	for _, existing := range s.notices {
		if existing == notice {
			return
		}
	}
	s.notices = append(s.notices, notice)
	if len(s.notices) > maxNotices {
		s.notices = s.notices[len(s.notices)-maxNotices:]
	}
}

func (s *Streamer) readStderr(r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || isBenignNotice(line) {
			continue
		}
		s.logf("log stream stderr: %s", line)
		s.addNotice(line)
	}
}

func (s *Streamer) fatalErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fatal
}

// finish records the terminal state of the streamer.
func (s *Streamer) finish(cause error) {
	s.mu.Lock()
	if cause == nil {
		s.state = StateIdle
	} else {
		s.state = StateError
		s.lastError = errorText(cause)
	}
	status := s.statusLocked()
	s.mu.Unlock()

	if cause != nil {
		s.logf("log stream stopped: %v", cause)
	}
	s.emit(status)
}

// fatalError marks conditions that must not be retried.
type fatalError struct{ msg string }

func (e fatalError) Error() string { return e.msg }

func isFatal(err error) bool {
	var fe fatalError
	return errors.As(err, &fe)
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// fatalForNotice classifies subprocess output that makes further retries
// pointless, and turns it into an actionable message.
func fatalForNotice(line string) error {
	lower := strings.ToLower(line)
	switch {
	case strings.Contains(lower, "must be admin"):
		return fatalError{msg: "log stream: " + line + " — streaming from a physical device requires " +
			"admin privileges: restart the server with sudo (or configure passwordless sudo for /usr/bin/log)"}
	case strings.Contains(lower, "not booted"),
		strings.Contains(lower, "unable to find device"),
		strings.Contains(lower, "invalid device"),
		strings.Contains(lower, "bad or unknown session"),
		strings.Contains(lower, "no devices are available"):
		return fatalError{msg: "log stream: " + line + " — the selected target is not available; " +
			"boot the simulator (or reconnect the device) and start the capture again"}
	}
	return nil
}

// benignNotices are known-harmless diagnostics printed by the simulator tooling
// that would otherwise clutter the message shown in the UI.
var benignNotices = []string{
	"getpwuid_r did not find a match",
	"Child process terminated with signal 15",
}

func isBenignNotice(line string) bool {
	for _, benign := range benignNotices {
		if strings.Contains(line, benign) {
			return true
		}
	}
	return false
}
