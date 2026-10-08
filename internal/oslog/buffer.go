package oslog

import (
	"sync"
	"time"
)

// Facet is a distinct value seen in the log stream together with how many
// matching entries carry it. It powers the subsystem/category pickers.
type Facet struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

// ScopedFacets summarises the values reachable through the current filter. Each
// dimension is counted with its own restriction removed, so selecting a
// subsystem narrows the category list but never empties the subsystem list.
type ScopedFacets struct {
	Subsystems []Facet `json:"subsystems"`
	Categories []Facet `json:"categories"`
	Processes  []Facet `json:"processes"`
	Levels     []Facet `json:"levels"`
	Matched    int     `json:"matched"`
	Total      uint64  `json:"total"`
	Buffered   int     `json:"buffered"`
	Capacity   int     `json:"capacity"`
	Evicted    uint64  `json:"evicted"`
}

// Buffer is a fixed capacity ring buffer of log entries. Entries keep
// monotonically increasing IDs so consumers can resume a stream without
// duplicates, even after the oldest entries have been evicted.
type Buffer struct {
	mu      sync.RWMutex
	entries []Entry
	head    int // index of the oldest entry
	count   int
	nextID  uint64
	total   uint64
	evicted uint64
}

// NewBuffer creates a ring buffer that keeps at most capacity entries.
func NewBuffer(capacity int) *Buffer {
	if capacity < 1 {
		capacity = 1
	}
	return &Buffer{entries: make([]Entry, capacity), nextID: 1}
}

// Add stores an entry, assigning it the next ID, and returns the stored copy.
// When the buffer is full the oldest entry is evicted; IDs are never reused.
func (b *Buffer) Add(e Entry) Entry {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.addLocked(e)
}

func (b *Buffer) addLocked(e Entry) Entry {
	e.BuildSearch()
	e.ID = b.nextID
	b.nextID++
	b.total++

	if b.count == len(b.entries) {
		b.evictOldestLocked()
	}
	idx := (b.head + b.count) % len(b.entries)
	b.entries[idx] = e
	b.count++
	return e
}

func (b *Buffer) evictOldestLocked() {
	if b.count == 0 {
		return
	}
	b.entries[b.head] = Entry{}
	b.head = (b.head + 1) % len(b.entries)
	b.count--
	b.evicted++
}

// All returns a copy of every buffered entry, oldest first.
func (b *Buffer) All() []Entry {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.snapshotLocked(0)
}

// Since returns buffered entries with an ID greater than id, oldest first.
func (b *Buffer) Since(id uint64) []Entry {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.snapshotLocked(id)
}

func (b *Buffer) snapshotLocked(afterID uint64) []Entry {
	out := make([]Entry, 0, b.count)
	for i := 0; i < b.count; i++ {
		e := b.entries[(b.head+i)%len(b.entries)]
		if e.ID > afterID {
			out = append(out, e)
		}
	}
	return out
}

// Query describes a request for buffered entries.
type Query struct {
	Filter Filter
	// Limit caps how many entries come back; 0 means no limit.
	Limit int
	// After is an exclusive lower ID bound, used when resuming a stream.
	After uint64
	// Before is an exclusive upper ID bound, used when paging backwards.
	Before uint64
}

// Query returns matching entries ordered oldest first, together with the total
// number of matches inside the bounds before Limit was applied. When Limit is
// positive the newest matches are kept, which is what a log view wants.
func (b *Buffer) Query(q Query) ([]Entry, int) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	entries, matched, _ := b.queryLocked(q)
	return entries, matched
}

// Snapshot returns the entries for a new subscriber together with the facets
// computed at the same instant, so the filter counts and the rows a viewer
// receives can never disagree.
func (b *Buffer) Snapshot(f Filter, limit int, afterID uint64) ([]Entry, int, ScopedFacets) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	entries, matched, facets := b.queryLocked(Query{Filter: f, Limit: limit, After: afterID})
	facets.Matched = b.countLocked(f)
	facets.Total = b.total
	facets.Buffered = b.count
	facets.Capacity = len(b.entries)
	facets.Evicted = b.evicted
	return entries, matched, facets
}

// queryLocked walks the ring once, keeping a sliding window of the newest Limit
// matches so a wide filter never materialises the whole buffer.
func (b *Buffer) queryLocked(q Query) ([]Entry, int, ScopedFacets) {
	limit := q.Limit
	window := []Entry{} // never nil, so it marshals as [] rather than null
	next := 0
	matched := 0

	add := func(e Entry) {
		if limit <= 0 {
			window = append(window, e)
			return
		}
		if len(window) < limit {
			window = append(window, e)
			return
		}
		window[next] = e
		next = (next + 1) % limit
	}

	for i := 0; i < b.count; i++ {
		e := b.entries[(b.head+i)%len(b.entries)]
		if e.ID <= q.After {
			continue
		}
		if q.Before > 0 && e.ID >= q.Before {
			continue
		}
		if !q.Filter.Match(e) {
			continue
		}
		matched++
		add(e)
	}

	if limit > 0 && next > 0 {
		// Rotate the ring so the entries read oldest first.
		rotated := make([]Entry, 0, len(window))
		rotated = append(rotated, window[next:]...)
		rotated = append(rotated, window[:next]...)
		window = rotated
	}
	return window, matched, b.scopedFacetsLocked(q.Filter, 500)
}

func (b *Buffer) countLocked(f Filter) int {
	if f.MatchAll() {
		return b.count
	}
	matched := 0
	for i := 0; i < b.count; i++ {
		if f.Match(b.entries[(b.head+i)%len(b.entries)]) {
			matched++
		}
	}
	return matched
}

// ScopedFacets returns the facet lists for the current filter. A limit <= 0
// means "no limit" per dimension.
func (b *Buffer) ScopedFacets(f Filter, limit int) ScopedFacets {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := b.scopedFacetsLocked(f, limit)
	out.Matched = b.countLocked(f)
	out.Total = b.total
	out.Buffered = b.count
	out.Capacity = len(b.entries)
	out.Evicted = b.evicted
	return out
}

// dimensionIndex identifies a facet dimension.
const (
	dimSubsystem = iota
	dimCategory
	dimProcess
	dimLevel
	dimCount
)

func dimensionValue(e Entry, dim int) string {
	switch dim {
	case dimSubsystem:
		return e.Subsystem
	case dimCategory:
		return e.Category
	case dimProcess:
		return e.Process
	default:
		return e.Level
	}
}

// scopedFacetsLocked builds every facet list in a single pass. The filter is
// evaluated once per entry into a dimensionMatch, and each relaxed view is then
// just a conjunction of the flags that are already known. Doing four independent
// passes over the whole buffer was measurably the most expensive thing the server
// did, and it held the buffer lock that ingest needs.
func (b *Buffer) scopedFacetsLocked(f Filter, limit int) ScopedFacets {
	var counts [dimCount]map[string]int
	matched := 0

	add := func(dim int, value string) {
		if value == "" {
			return
		}
		if counts[dim] == nil {
			counts[dim] = make(map[string]int)
		}
		counts[dim][value]++
	}

	for i := 0; i < b.count; i++ {
		e := b.entries[(b.head+i)%len(b.entries)]
		m := f.matches(e)

		if m.subsystem && m.category && m.process && m.level && m.text && m.hasSubsystem {
			matched++
			// Matching the full filter means matching every relaxed variant too.
			for dim := 0; dim < dimCount; dim++ {
				add(dim, dimensionValue(e, dim))
			}
			continue
		}
		if !m.text {
			// The text restriction applies to every dimension, so this entry is in
			// none of the lists.
			continue
		}
		if m.category && m.process && m.level && m.hasSubsystem {
			add(dimSubsystem, e.Subsystem)
		}
		if m.subsystem && m.process && m.level && m.hasSubsystem {
			add(dimCategory, e.Category)
		}
		if m.subsystem && m.category && m.level && m.hasSubsystem {
			add(dimProcess, e.Process)
		}
		if m.subsystem && m.category && m.process && m.hasSubsystem {
			add(dimLevel, e.Level)
		}
	}

	return ScopedFacets{
		Subsystems: topFacets(counts[dimSubsystem], limit),
		Categories: topFacets(counts[dimCategory], limit),
		Processes:  topFacets(counts[dimProcess], limit),
		Levels:     topFacets(counts[dimLevel], limit),
		Matched:    matched,
	}
}

// Count returns how many buffered entries match the filter. It is the cheap path
// for callers that need the number but not the facet lists.
func (b *Buffer) Count(f Filter) int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.countLocked(f)
}

// TimeSpan reports the timestamps of the oldest and newest buffered entries, so
// callers can tell viewers how much wall clock time the buffer currently covers.
func (b *Buffer) TimeSpan() (oldest, newest time.Time, ok bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.count == 0 {
		return time.Time{}, time.Time{}, false
	}
	return b.entries[b.head].Timestamp, b.entries[(b.head+b.count-1)%len(b.entries)].Timestamp, true
}

// Len reports how many entries are currently buffered.
func (b *Buffer) Len() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.count
}

// Capacity reports the maximum number of buffered entries.
func (b *Buffer) Capacity() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.entries)
}

// Total reports how many entries have been received in total.
func (b *Buffer) Total() uint64 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.total
}

// Evicted reports how many entries were dropped because the buffer was full.
func (b *Buffer) Evicted() uint64 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.evicted
}

// LastID returns the ID of the most recent entry, or 0 when empty.
func (b *Buffer) LastID() uint64 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.count == 0 {
		return 0
	}
	return b.entries[(b.head+b.count-1)%len(b.entries)].ID
}

// Clear removes all buffered entries. The entry ID sequence continues so live
// consumers keep a consistent watermark.
func (b *Buffer) Clear() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.entries = make([]Entry, len(b.entries))
	b.head = 0
	b.count = 0
}
