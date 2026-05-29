package query

import (
	"container/heap"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/rune/backend/internal/index"
)

// Tier constants for the 4-tier ranking system.
const (
	TierExact        = 1 // query == filename (exact match)
	TierPrefix       = 2 // filename starts with query
	TierWordBoundary = 3 // query appears at _, -, ., or CamelCase boundary
	TierSubstring    = 4 // query appears anywhere in filename
)

// ErrNegativeLimit is returned when a negative limit is provided.
var ErrNegativeLimit = errors.New("limit must be non-negative")

// Result represents a single ranked search result.
type Result struct {
	Path     string `json:"path"`
	Filename string `json:"filename"`
	IsDir    bool   `json:"is_dir"`
	Depth    int    `json:"depth"`
	Tier     int    `json:"tier"`
}

// Engine performs search queries entirely in RAM against in-memory indexes.
// It accesses the prefix trie, trigram inverted index, and metadata cache
// from the index store. The query engine never touches LMDB at search time.
type Engine struct {
	trie     *index.PrefixTrie
	trigrams *index.TrigramIndex
	meta     map[uint32]*index.FileMeta
}

// NewEngine creates a new query engine backed by the given store's
// in-memory indexes. The engine holds references to the store's indexes,
// so the store must remain valid for the engine's lifetime.
func NewEngine(store *index.Store) *Engine {
	return &Engine{
		trie:     store.Trie(),
		trigrams: store.Trigrams(),
		meta:     store.MetaCache(),
	}
}

// ranksHigher reports whether Result a is higher ranking than Result b.
// Tiebreaking rules: directories rank above files, then shallower depth first,
// then alphabetical by filename (case-insensitive).
func ranksHigher(a, b Result) bool {
	if a.Tier != b.Tier {
		return a.Tier < b.Tier
	}
	if a.IsDir != b.IsDir {
		return a.IsDir
	}
	if a.Depth != b.Depth {
		return a.Depth < b.Depth
	}
	return strings.ToLower(a.Filename) < strings.ToLower(b.Filename)
}

// resultMinHeap implements heap.Interface and holds the top-N results.
// It acts as a min-heap based on ranking: the root of the heap is the
// WORST ranking result. This allows us to keep the N best results.
type resultMinHeap []Result

func (h resultMinHeap) Len() int           { return len(h) }
func (h resultMinHeap) Less(i, j int) bool { return !ranksHigher(h[i], h[j]) } // Worst item at index 0
func (h resultMinHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *resultMinHeap) Push(x interface{}) {
	*h = append(*h, x.(Result))
}
func (h *resultMinHeap) Pop() interface{} {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[0 : n-1]
	return x
}

// Search executes a query and returns ranked results according to the
// 4-tier ranking system.
//
// All query operations enforce the Hot Path Constraints:
// - Bounded Top-N min-heap ranking to maintain sub-5ms latency.
// - All operations run fully in-RAM with no JSON decoding or LMDB writes.
func (e *Engine) Search(query string, limit int) ([]Result, error) {
	// Normalize query: NFC Unicode normalization and lowercase.
	query = strings.TrimSpace(strings.ToLower(index.NormalizeUnicode(query)))

	if limit < 0 {
		return nil, ErrNegativeLimit
	}
	if query == "" || limit == 0 {
		return []Result{}, nil
	}

	h := &resultMinHeap{}
	heap.Init(h)

	seen := make(map[uint32]bool, 256)

	// Phase 1: Collect prefix-based matches from the trie.
	trieResults := e.trie.Search(query)

	// ---- Tier 1: Exact match ----
	for _, pathID := range trieResults {
		meta, ok := e.meta[pathID]
		if !ok {
			continue
		}
		if meta.Filename == query {
			r := Result{
				Path:     meta.Path,
				Filename: meta.Filename,
				IsDir:    meta.IsDir,
				Depth:    meta.Depth,
				Tier:     TierExact,
			}
			seen[pathID] = true
			pushBest(h, r, limit)
		}
	}

	// ---- Tier 2: Prefix match ----
	for _, pathID := range trieResults {
		if seen[pathID] {
			continue
		}
		meta, ok := e.meta[pathID]
		if !ok {
			continue
		}
		if strings.HasPrefix(meta.Filename, query) {
			r := Result{
				Path:     meta.Path,
				Filename: meta.Filename,
				IsDir:    meta.IsDir,
				Depth:    meta.Depth,
				Tier:     TierPrefix,
			}
			seen[pathID] = true
			pushBest(h, r, limit)
		}
	}

	// Phase 2: Collect substring-based matches from the trigram index.
	// For queries shorter than 3 characters, we skip trigrams entirely (Short Query Policy).
	if len([]rune(query)) >= 3 {
		trigramResults := e.trigrams.Search(query)
		if trigramResults != nil {
			// ---- Tier 3: Word-boundary match ----
			for _, pathID := range trigramResults {
				if seen[pathID] {
					continue
				}
				meta, ok := e.meta[pathID]
				if !ok {
					continue
				}
				if isWordBoundaryMatch(meta.OriginalFilename, query) {
					r := Result{
						Path:     meta.Path,
						Filename: meta.Filename,
						IsDir:    meta.IsDir,
						Depth:    meta.Depth,
						Tier:     TierWordBoundary,
					}
					seen[pathID] = true
					pushBest(h, r, limit)
				}
			}

			// ---- Tier 4: Substring match ----
			for _, pathID := range trigramResults {
				if seen[pathID] {
					continue
				}
				meta, ok := e.meta[pathID]
				if !ok {
					continue
				}
				if strings.Contains(meta.Filename, query) {
					r := Result{
						Path:     meta.Path,
						Filename: meta.Filename,
						IsDir:    meta.IsDir,
						Depth:    meta.Depth,
						Tier:     TierSubstring,
					}
					pushBest(h, r, limit)
				}
			}
		}
	}

	// Extract and sort results from the heap in descending order of quality
	resultLen := h.Len()
	results := make([]Result, resultLen)
	for i := resultLen - 1; i >= 0; i-- {
		results[i] = heap.Pop(h).(Result)
	}

	return results, nil
}

// pushBest inserts a new result into the min-heap. If the heap is full,
// it replaces the worst result in the heap if the new result is higher ranking.
func pushBest(h *resultMinHeap, r Result, limit int) {
	if h.Len() < limit {
		heap.Push(h, r)
	} else if ranksHigher(r, (*h)[0]) { // (*h)[0] is the worst item in the heap
		heap.Pop(h)
		heap.Push(h, r)
	}
}

// isWordBoundaryMatch checks whether the lowercased query appears at a word
// boundary in the original (pre-lowercased) filename. This enables accurate
// CamelCase boundary detection that would be lost in the lowercased form.
func isWordBoundaryMatch(originalFilename, lowerQuery string) bool {
	if originalFilename == "" {
		return false
	}

	lowerFilename := strings.ToLower(originalFilename)
	idx := 0
	for {
		pos := strings.Index(lowerFilename[idx:], lowerQuery)
		if pos < 0 {
			return false
		}
		absPos := idx + pos
		if isWordBoundary(originalFilename, absPos) {
			return true
		}
		idx = absPos + 1
	}
}

// isWordBoundary returns true if the byte position pos in s is at a word boundary.
func isWordBoundary(s string, pos int) bool {
	if pos == 0 {
		return true
	}

	prev := s[pos-1]
	if prev == '_' || prev == '-' || prev == '.' {
		return true
	}

	currRune, _ := utf8.DecodeRuneInString(s[pos:])
	prevRune, size := utf8.DecodeLastRuneInString(s[:pos])
	if size > 0 && prevRune != utf8.RuneError && currRune != utf8.RuneError {
		if unicode.IsLower(prevRune) && unicode.IsUpper(currRune) {
			return true
		}
	}

	return false
}
