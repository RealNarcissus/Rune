package index

import (
	"sync"
)

// TrigramIndex is an inverted index mapping trigrams (3-character sequences)
// to the set of paths whose filenames contain that trigram.
//
// Trigram extraction uses a sliding window of 3 runes over the filename.
// Filenames shorter than 3 characters are not indexed by trigram
// (they are findable via the prefix trie alone).
//
// All operations are safe for concurrent use.
type TrigramIndex struct {
	index map[string]map[string]struct{} // trigram → set of paths
	mu    sync.RWMutex
}

// NewTrigramIndex creates an empty trigram inverted index.
func NewTrigramIndex() *TrigramIndex {
	return &TrigramIndex{
		index: make(map[string]map[string]struct{}),
	}
}

// ExtractTrigrams extracts all 3-character trigrams from a string.
// The string should already be lowercased.
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

// Insert adds a path to the trigram sets for all trigrams in the filename.
// The filename should already be lowercased.
func (ti *TrigramIndex) Insert(filename, fullPath string) {
	trigrams := ExtractTrigrams(filename)
	if len(trigrams) == 0 {
		return
	}

	ti.mu.Lock()
	defer ti.mu.Unlock()

	for _, trigram := range trigrams {
		set, ok := ti.index[trigram]
		if !ok {
			set = make(map[string]struct{})
			ti.index[trigram] = set
		}
		set[fullPath] = struct{}{}
	}
}

// Delete removes a path from all trigram sets associated with the filename.
func (ti *TrigramIndex) Delete(filename, fullPath string) {
	trigrams := ExtractTrigrams(filename)
	if len(trigrams) == 0 {
		return
	}

	ti.mu.Lock()
	defer ti.mu.Unlock()

	for _, trigram := range trigrams {
		if set, ok := ti.index[trigram]; ok {
			delete(set, fullPath)
			if len(set) == 0 {
				delete(ti.index, trigram)
			}
		}
	}
}

// Search returns all paths that contain ALL trigrams in the query.
// The query should already be lowercased.
// Returns nil if any trigram is not found (empty intersection).
func (ti *TrigramIndex) Search(query string) map[string]struct{} {
	trigrams := ExtractTrigrams(query)
	if len(trigrams) == 0 {
		return nil
	}

	ti.mu.RLock()
	defer ti.mu.RUnlock()

	// Start with the smallest candidate set for efficiency
	var result map[string]struct{}
	smallestSize := -1
	var smallestSet map[string]struct{}

	for _, trigram := range trigrams {
		set, ok := ti.index[trigram]
		if !ok {
			return nil // one trigram missing → no results
		}
		if smallestSize == -1 || len(set) < smallestSize {
			smallestSize = len(set)
			smallestSet = set
		}
	}

	// Copy the smallest set
	result = make(map[string]struct{}, len(smallestSet))
	for p := range smallestSet {
		result[p] = struct{}{}
	}

	// Intersect with remaining trigram sets
	for _, trigram := range trigrams {
		set := ti.index[trigram]
		if len(set) == smallestSize {
			// Skip the set we started with (same-size check is a heuristic;
			// in rare cases two sets may have the same size, but the intersection
			// is still correct even if we process the same set twice).
			isSameSet := true
			if len(set) != len(smallestSet) {
				isSameSet = false
			} else {
				for p := range smallestSet {
					if _, ok := set[p]; !ok {
						isSameSet = false
						break
					}
				}
			}
			if isSameSet {
				continue
			}
		}
		for p := range result {
			if _, ok := set[p]; !ok {
				delete(result, p)
			}
		}
	}

	if len(result) == 0 {
		return nil
	}
	return result
}

// Size returns the number of distinct trigrams in the index.
func (ti *TrigramIndex) Size() int {
	ti.mu.RLock()
	defer ti.mu.RUnlock()
	return len(ti.index)
}

// EntryCount returns the total number of path references across all trigrams.
func (ti *TrigramIndex) EntryCount() int {
	ti.mu.RLock()
	defer ti.mu.RUnlock()
	count := 0
	for _, set := range ti.index {
		count += len(set)
	}
	return count
}
