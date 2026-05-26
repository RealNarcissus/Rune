package query

import (
	"errors"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/lumina-search/backend/internal/index"
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
	meta     map[string]*index.FileMeta
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

// Search executes a query and returns ranked results according to the
// 4-tier ranking system:
//
//	Tier 1: Exact filename match
//	Tier 2: Prefix match (filename starts with query)
//	Tier 3: Word-boundary match (_, -, ., or CamelCase boundary)
//	Tier 4: Substring match (query appears anywhere in filename)
//
// Within each tier, directories rank above files (directory boost).
// Tiebreaking: shallower depth first, then alphabetical by filename (case-insensitive).
//
// The query is normalized (lowercased, trimmed) once at entry.
// limit must be >= 0. limit=0 returns an empty slice without scanning.
// Negative limit returns ErrNegativeLimit.
//
// Early termination: once `limit` results have been collected from higher
// tiers, lower tiers are skipped entirely.
func (e *Engine) Search(query string, limit int) ([]Result, error) {
	// Normalize query: trim whitespace and lowercase.
	query = strings.TrimSpace(strings.ToLower(query))

	if limit < 0 {
		return nil, ErrNegativeLimit
	}
	if query == "" || limit == 0 {
		return []Result{}, nil
	}

	seen := make(map[string]bool, 256)
	var results []Result

	// Phase 1: Collect prefix-based matches from the trie.
	// The trie returns all paths whose filenames start with the query.
	// From these we extract tier 1 (exact) and tier 2 (prefix) matches.
	trieResults := e.trie.Search(query)

	// ---- Tier 1: Exact match ----
	for path := range trieResults {
		meta, ok := e.meta[path]
		if !ok {
			continue
		}
		if meta.Filename == query {
			results = append(results, Result{
				Path:     meta.Path,
				Filename: meta.Filename,
				IsDir:    meta.IsDir,
				Depth:    meta.Depth,
				Tier:     TierExact,
			})
			seen[path] = true
		}
	}

	sortResults(results)
	if limit > 0 && len(results) >= limit {
		return results[:limit], nil
	}

	// ---- Tier 2: Prefix match ----
	for path := range trieResults {
		if seen[path] {
			continue
		}
		meta, ok := e.meta[path]
		if !ok {
			continue
		}
		if strings.HasPrefix(meta.Filename, query) {
			results = append(results, Result{
				Path:     meta.Path,
				Filename: meta.Filename,
				IsDir:    meta.IsDir,
				Depth:    meta.Depth,
				Tier:     TierPrefix,
			})
			seen[path] = true
		}
	}

	sortResults(results)
	if limit > 0 && len(results) >= limit {
		return results[:limit], nil
	}

	// Phase 2: Collect substring-based matches from the trigram index.
	// The trigram index returns all paths whose filenames contain ALL
	// trigrams of the query. From these we extract tier 3 (word-boundary)
	// and tier 4 (substring) matches.
	trigramResults := e.trigrams.Search(query)
	if trigramResults != nil {
		// ---- Tier 3: Word-boundary match ----
		for path := range trigramResults {
			if seen[path] {
				continue
			}
			meta, ok := e.meta[path]
			if !ok {
				continue
			}
			if isWordBoundaryMatch(meta.OriginalFilename, query) {
				results = append(results, Result{
					Path:     meta.Path,
					Filename: meta.Filename,
					IsDir:    meta.IsDir,
					Depth:    meta.Depth,
					Tier:     TierWordBoundary,
				})
				seen[path] = true
			}
		}

		sortResults(results)
		if limit > 0 && len(results) >= limit {
			return results[:limit], nil
		}

		// ---- Tier 4: Substring match ----
		for path := range trigramResults {
			if seen[path] {
				continue
			}
			meta, ok := e.meta[path]
			if !ok {
				continue
			}
			if strings.Contains(meta.Filename, query) {
				results = append(results, Result{
					Path:     meta.Path,
					Filename: meta.Filename,
					IsDir:    meta.IsDir,
					Depth:    meta.Depth,
					Tier:     TierSubstring,
				})
			}
		}

		sortResults(results)
	}

	if limit > 0 && len(results) > limit {
		results = results[:limit]
	}

	return results, nil
}

// isWordBoundaryMatch checks whether the lowercased query appears at a word
// boundary in the original (pre-lowercased) filename. This enables accurate
// CamelCase boundary detection that would be lost in the lowercased form.
//
// Word boundaries are: start of string, or after _, -, ., or at a lowercase
// to uppercase transition (CamelCase).
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
// A word boundary is defined as:
//   - Position 0 (start of string)
//   - After an underscore (_), hyphen (-), or dot (.)
//   - At a lowercase→uppercase transition (CamelCase boundary)
func isWordBoundary(s string, pos int) bool {
	if pos == 0 {
		return true
	}

	// Check for explicit separator characters (all ASCII, safe as bytes).
	prev := s[pos-1]
	if prev == '_' || prev == '-' || prev == '.' {
		return true
	}

	// Check for CamelCase boundary: lowercase→uppercase transition.
	// Use UTF-8 decoding for proper Unicode support.
	currRune, _ := utf8.DecodeRuneInString(s[pos:])
	prevRune, size := utf8.DecodeLastRuneInString(s[:pos])
	if size > 0 && prevRune != utf8.RuneError && currRune != utf8.RuneError {
		if unicode.IsLower(prevRune) && unicode.IsUpper(currRune) {
			return true
		}
	}

	return false
}

// sortResults sorts results by tier, then directory boost, then depth,
// then filename (case-insensitive alphabetical). This ensures:
//   - All tier 1 results come before all tier 2 results, etc.
//   - Within the same tier, directories come before files (directory boost).
//   - Within the same tier and type, shallower depth comes first.
//   - Within the same tier, type, and depth, alphabetical order by filename.
func sortResults(results []Result) {
	sort.Slice(results, func(i, j int) bool {
		a, b := results[i], results[j]

		// Tier: lower tier number = higher rank.
		if a.Tier != b.Tier {
			return a.Tier < b.Tier
		}

		// Directory boost: directories rank above files at same tier.
		if a.IsDir != b.IsDir {
			return a.IsDir
		}

		// Tiebreak 1: shallower depth first.
		if a.Depth != b.Depth {
			return a.Depth < b.Depth
		}

		// Tiebreak 2: alphabetical by filename (case-insensitive).
		return strings.ToLower(a.Filename) < strings.ToLower(b.Filename)
	})
}
