// Package history persists captured log entries to disk so that rare events -
// a subsystem that logs once every fifteen minutes, say - stay retrievable long
// after they have been evicted from the in-memory buffer. That is the practical
// difference between a live stream window and the persistent store Xcode reads.
package history

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/rudy/log-server/internal/oslog"
)

const (
	readBlockSize = 1 << 20 // backwards scan block
	flushEvery    = 512     // entries buffered before a flush
)

// Stats describes the current history file.
type Stats struct {
	Enabled   bool   `json:"enabled"`
	Path      string `json:"path,omitempty"`
	Entries   uint64 `json:"entries"`
	Bytes     int64  `json:"bytes"`
	MaxBytes  int64  `json:"maxBytes"`
	Full      bool   `json:"full"`
	LastError string `json:"lastError,omitempty"`
}

// Store appends entries to a newline delimited JSON file and pages backwards
// through it, applying the same filter the live view uses.
type Store struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	file     *os.File
	writer   *bufio.Writer
	counting *countingWriter
	encoder  *json.Encoder
	entries  uint64
	pending  int
	full     bool
	lastErr  error
	closed   bool
}

// countingWriter tracks how many bytes have been handed to the file, including
// data still sitting in the bufio buffer, so the size cap is enforced promptly
// instead of only after a flush.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// Open prepares path for appending, creating parent directories as needed. A
// maxBytes of zero means no size limit.
func Open(path string, maxBytes int64) (*Store, error) {
	if path == "" {
		return nil, errors.New("history path is empty")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("creating history directory: %w", err)
		}
	}
	s := &Store{path: path, maxBytes: maxBytes}
	if err := s.reset(); err != nil {
		return nil, err
	}
	return s, nil
}

// reset truncates the file and starts a fresh writer. It runs at Open and
// whenever a new capture begins.
func (s *Store) reset() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.writer != nil {
		_ = s.writer.Flush()
	}
	if s.file != nil {
		_ = s.file.Close()
		s.file = nil
	}

	file, err := os.OpenFile(s.path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		s.lastErr = err
		return fmt.Errorf("opening history file: %w", err)
	}
	s.file = file
	s.writer = bufio.NewWriterSize(file, 1<<20)
	s.counting = &countingWriter{w: s.writer}
	s.encoder = json.NewEncoder(s.counting)
	s.encoder.SetEscapeHTML(false)
	s.entries = 0
	s.pending = 0
	s.full = false
	s.lastErr = nil
	s.closed = false
	return nil
}

// Restart clears the history file so it always describes the session that is
// currently in the buffer.
func (s *Store) Restart() error { return s.reset() }

// Append writes one entry. When the size limit is reached the store stops
// accepting new entries rather than growing without bound and reports Full.
func (s *Store) Append(e oslog.Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.full || s.encoder == nil {
		return nil
	}
	if s.maxBytes > 0 && s.bytesLocked() >= s.maxBytes {
		s.full = true
		return nil
	}
	if err := s.encoder.Encode(&e); err != nil {
		s.lastErr = err
		return err
	}
	s.entries++
	s.pending++
	if s.pending >= flushEvery {
		if err := s.writer.Flush(); err != nil {
			s.lastErr = err
			return err
		}
		s.pending = 0
	}
	return nil
}

func (s *Store) bytesLocked() int64 {
	if s.counting == nil {
		return 0
	}
	return s.counting.n
}

// Flush pushes buffered writes to the operating system.
func (s *Store) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writer == nil {
		return nil
	}
	if err := s.writer.Flush(); err != nil {
		s.lastErr = err
		return err
	}
	s.pending = 0
	return nil
}

// Close flushes and releases the file.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.writer != nil {
		_ = s.writer.Flush()
	}
	if s.file != nil {
		err := s.file.Close()
		s.file = nil
		return err
	}
	return nil
}

// Stats reports the current file state.
func (s *Store) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Stats{
		Enabled:   true,
		Path:      s.path,
		Entries:   s.entries,
		Bytes:     s.bytesLocked(),
		MaxBytes:  s.maxBytes,
		Full:      s.full,
		LastError: errText(s.lastErr),
	}
}

// Query returns up to limit matching entries older than before, oldest first. It
// scans the file backwards, so it reads only as much as it must, and reports
// whether older matching entries may remain.
func (s *Store) Query(f oslog.Filter, limit int, before uint64) ([]oslog.Entry, bool, error) {
	if err := s.Flush(); err != nil {
		return nil, false, err
	}

	file, err := os.Open(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return nil, false, err
	}

	var (
		out  = []oslog.Entry{} // never nil, so it marshals as [] rather than null
		tail []byte
		pos  = info.Size()
	)
	for pos > 0 && (limit <= 0 || len(out) < limit) {
		start := pos - readBlockSize
		if start < 0 {
			start = 0
		}
		size := int(pos - start)
		buf := make([]byte, size)
		if _, err := file.ReadAt(buf, start); err != nil && !errors.Is(err, io.EOF) {
			return nil, false, err
		}

		// Re-attach the partial line carried over from the later block.
		data := make([]byte, 0, len(buf)+len(tail))
		data = append(data, buf...)
		data = append(data, tail...)

		lines := bytes.Split(data, []byte{'\n'})
		if start > 0 {
			tail = append(tail[:0], lines[0]...)
			lines = lines[1:]
		} else {
			tail = nil
		}

		for i := len(lines) - 1; i >= 0; i-- {
			entry, ok := oslog.DecodeEntry(lines[i])
			if !ok {
				continue
			}
			if before > 0 && entry.ID >= before {
				continue
			}
			if !f.Match(entry) {
				continue
			}
			out = append(out, entry)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
		pos = start
	}

	// The backwards scan produced newest-first order.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	hasMore := limit > 0 && len(out) >= limit
	return out, hasMore, nil
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
