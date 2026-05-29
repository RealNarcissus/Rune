package index

import (
	"sort"
	"sync"
)

// TrigramIndex is an inverted index mapping trigrams (3-character sequences)
// to the sorted list of path IDs whose filenames contain that trigram.
//
// Trigram extraction uses a sliding window of 3 runes over the filename.
// Filenames shorter than 3 characters are not indexed by trigram
// (they are findable via the prefix trie alone).
//
// All operations are safe for concurrent use.
type TrigramIndex struct {
	index map[string][]uint32 // trigram → sorted list of path IDs
	mu    sync.RWMutex
}

// NewTrigramIndex creates an empty trigram inverted index.
func NewTrigramIndex() *TrigramIndex {
	return &TrigramIndex{
		index: make(map[string][]uint32),
	}
}

// ExtractTrigrams extracts all 3-character trigrams from a string.
// The string should already be lowercased and Unicode-normalized.
// Returns unique trigrams only.
func ExtractTrigrams(s string) []string {
	runes := []rune(s)
	if len(runes) < 3 {
		return nil
	}

	seen := make(map[string]bool, len(runes)-2)
	result := make([]string, 0, len(runes)-2)

	for i := 0; i <= len(runes)-3; i++ {
		trigram := string(runes[i : i+3])
		if !seen[trigram] {
			seen[trigram] = true
			result = append(result, trigram)
		}
	}

	return result
}

// Insert adds a pathID to the trigram sets for all trigrams in the filename.
// The filename should already be lowercased.
func (ti *TrigramIndex) Insert(filename string, pathID uint32) {
	trigrams := ExtractTrigrams(filename)
	if len(trigrams) == 0 {
		return
	}

	ti.mu.Lock()
	defer ti.mu.Unlock()

	for _, trigram := range trigrams {
		ids := ti.index[trigram]
		idx := sort.Search(len(ids), func(i int) bool {
			return ids[i] >= pathID
		})
		if idx < len(ids) && ids[idx] == pathID {
			// Already present
		} else {
			// Insert in sorted order
			ids = append(ids, 0)
			copy(ids[idx+1:], ids[idx:])
			ids[idx] = pathID
			ti.index[trigram] = ids
		}
	}
}

// Delete removes a pathID from all trigram sets associated with the filename.
func (ti *TrigramIndex) Delete(filename string, pathID uint32) {
	trigrams := ExtractTrigrams(filename)
	if len(trigrams) == 0 {
		return
	}

	ti.mu.Lock()
	defer ti.mu.Unlock()

	for _, trigram := range trigrams {
		if ids, ok := ti.index[trigram]; ok {
			idx := sort.Search(len(ids), func(i int) bool {
				return ids[i] >= pathID
			})
			if idx < len(ids) && ids[idx] == pathID {
				ids = append(ids[:idx], ids[idx+1:]...)
				if len(ids) == 0 {
					delete(ti.index, trigram)
				} else {
					ti.index[trigram] = ids
				}
			}
		}
	}
}

// Search returns all path IDs that contain ALL trigrams in the query.
// The query should already be lowercased.
// Returns a sorted slice of uint32 IDs, or nil if no match.
func (ti *TrigramIndex) Search(query string) []uint32 {
	trigrams := ExtractTrigrams(query)
	if len(trigrams) == 0 {
		return nil
	}

	ti.mu.RLock()
	defer ti.mu.RUnlock()

	// If any trigram is missing, intersection is empty
	var lists [][]uint32
	for _, trigram := range trigrams {
		list, ok := ti.index[trigram]
		if !ok {
			return nil
		}
		lists = append(lists, list)
	}

	// Sort the lists by length to minimize intersection work (intersection order optimization)
	sort.Slice(lists, func(i, j int) bool {
		return len(lists[i]) < len(lists[j])
	})

	// Intersect the lists sequentially
	result := lists[0]
	for i := 1; i < len(lists); i++ {
		result = intersectSortedSlices(result, lists[i])
		if len(result) == 0 {
			return nil
		}
	}

	return result
}

func intersectSortedSlices(a, b []uint32) []uint32 {
	var result []uint32
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if a[i] == b[j] {
			result = append(result, a[i])
			i++
			j++
		} else if a[i] < b[j] {
			i++
		} else {
			j++
		}
	}
	return result
}

// Size returns the number of distinct trigrams in the index.
func (ti *TrigramIndex) Size() int {
	ti.mu.RLock()
	defer ti.mu.RUnlock()
	return len(ti.index)
}

// EntryCount returns the total number of path ID references across all trigrams.
func (ti *TrigramIndex) EntryCount() int {
	ti.mu.RLock()
	defer ti.mu.RUnlock()
	count := 0
	for _, ids := range ti.index {
		count += len(ids)
	}
	return count
}
