package oslog

import (
	"sort"
	"sync"
	"time"
)

// dimCount is the number of facet dimensions.
const dimCount = 4

// dimension names, used as map keys in API responses.
var dimensionNames = [dimCount]string{"subsystems", "categories", "processes", "levels"}

// Facet is a distinct value seen during a capture with its session total and when
// it was last observed.
type Facet struct {
	Value    string    `json:"value"`
	Count    int       `json:"count"`
	LastSeen time.Time `json:"lastSeen"`
}

// CatalogFacets is the set of values seen during a capture, per dimension.
// Distinct reports how many values each dimension has in total, which can exceed
// the returned list when limit truncates it.
type CatalogFacets struct {
	Subsystems []Facet        `json:"subsystems"`
	Categories []Facet        `json:"categories"`
	Processes  []Facet        `json:"processes"`
	Levels     []Facet        `json:"levels"`
	Distinct   map[string]int `json:"distinct"`
	Tracked    int            `json:"tracked"`
}

// maxValuesPerDimension bounds how many distinct values are remembered, so a
// long session cannot grow the catalog without limit.
const maxValuesPerDimension = 20000

type valueStat struct {
	count    int
	lastSeen time.Time
}

// Catalog remembers every value seen during a capture, independently of the
// in-memory buffer.
//
// The pickers need this because the buffer is a window measured in seconds: a
// subsystem that logs once every fifteen minutes is inside it for well under a
// second, so a buffer-scoped value list makes such a subsystem appear and then
// vanish. Counts here are session totals, so they only ever increase (until the
// next capture), and a value that has been seen once stays selectable.
type Catalog struct {
	mu     sync.RWMutex
	dims   [dimCount]map[string]*valueStat
	total  int
	resets uint64
}

// NewCatalog creates an empty catalog.
func NewCatalog() *Catalog {
	c := &Catalog{}
	c.Reset()
	return c
}

// Reset forgets every value. It is called when a new capture starts.
func (c *Catalog) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.dims {
		c.dims[i] = make(map[string]*valueStat)
	}
	c.total = 0
	c.resets++
}

// Record notes the values carried by an entry.
func (c *Catalog) Record(e Entry) {
	now := time.Now()

	c.mu.Lock()
	defer c.mu.Unlock()
	for dim, value := range [...]string{e.Subsystem, e.Category, e.Process, e.Level} {
		if value == "" {
			continue
		}
		stats := c.dims[dim]
		stat := stats[value]
		if stat == nil {
			if len(stats) >= maxValuesPerDimension {
				continue
			}
			stat = &valueStat{}
			stats[value] = stat
			c.total++
		}
		stat.count++
		stat.lastSeen = now
	}
}

// Facets returns each dimension ordered by descending count, truncated to limit
// when limit is positive.
func (c *Catalog) Facets(limit int) CatalogFacets {
	c.mu.RLock()
	defer c.mu.RUnlock()

	lists := make([][]Facet, dimCount)
	distinct := make(map[string]int, dimCount)
	for dim := range c.dims {
		lists[dim] = topFacets(c.dims[dim], limit)
		distinct[dimensionNames[dim]] = len(c.dims[dim])
	}
	return CatalogFacets{
		Subsystems: lists[0],
		Categories: lists[1],
		Processes:  lists[2],
		Levels:     lists[3],
		Distinct:   distinct,
		Tracked:    c.total,
	}
}

// Distinct reports the total number of rembered values across dimensions.
func (c *Catalog) Distinct() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.total
}

// topFacets converts a value->stat map into a slice ordered by descending count
// and then alphabetically. A limit <= 0 means "no limit".
func topFacets(stats map[string]*valueStat, limit int) []Facet {
	out := make([]Facet, 0, len(stats))
	for value, stat := range stats {
		out = append(out, Facet{Value: value, Count: stat.count, LastSeen: stat.lastSeen})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Value < out[j].Value
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}
