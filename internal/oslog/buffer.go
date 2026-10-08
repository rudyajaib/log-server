package oslog

import "sync"

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

// Query returns the buffered entries that match the filter, considering only
// entries with an ID greater than afterID. When limit is positive, the most
// recent limit matches are returned, oldest first. The second return value is
// the number of matches before the limit was applied, so callers can tell
// whether the result was truncated.
func (b *Buffer) Query(f Filter, limit int, afterID uint64) ([]Entry, int) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	matched := 0
	capacity := b.count
	if limit > 0 && limit < capacity {
		capacity = limit
	}
	out := make([]Entry, 0, capacity)
	for i := 0; i < b.count; i++ {
		e := b.entries[(b.head+i)%len(b.entries)]
		if e.ID <= afterID || !f.Match(e) {
			continue
		}
		matched++
		out = append(out, e)
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, matched
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

	return ScopedFacets{
		Subsystems: b.facetLocked(f.WithoutDimension("subsystem"), limit, func(e Entry) string { return e.Subsystem }),
		Categories: b.facetLocked(f.WithoutDimension("category"), limit, func(e Entry) string { return e.Category }),
		Processes:  b.facetLocked(f.WithoutDimension("process"), limit, func(e Entry) string { return e.Process }),
		Levels:     b.facetLocked(f.WithoutDimension("level"), limit, func(e Entry) string { return e.Level }),
		Matched:    b.countLocked(f),
		Total:      b.total,
		Buffered:   b.count,
		Capacity:   len(b.entries),
		Evicted:    b.evicted,
	}
}

func (b *Buffer) facetLocked(f Filter, limit int, value func(Entry) string) []Facet {
	counts := map[string]int{}
	for i := 0; i < b.count; i++ {
		e := b.entries[(b.head+i)%len(b.entries)]
		if !f.Match(e) {
			continue
		}
		if v := value(e); v != "" {
			counts[v]++
		}
	}
	return topFacets(counts, limit)
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
