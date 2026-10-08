package oslog

import "sort"

// topFacets converts a value->count map into a slice ordered by descending count
// and then alphabetically. A limit <= 0 means "no limit".
func topFacets(counts map[string]int, limit int) []Facet {
	out := make([]Facet, 0, len(counts))
	for value, count := range counts {
		out = append(out, Facet{Value: value, Count: count})
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
