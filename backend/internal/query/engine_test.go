package query

import (
	"os"
	"testing"

	"github.com/lumina-search/backend/internal/index"
)

// setupEngine creates a temporary index store, populates it with test data,
// and returns a query engine. The caller is responsible for cleanup.
func setupEngine(t *testing.T, entries []*index.FileMeta) (*Engine, *index.Store, func()) {
	t.Helper()

	dir, err := os.MkdirTemp("", "luminasearch-query-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}

	store, err := index.Open(dir)
	if err != nil {
		os.RemoveAll(dir)
		t.Fatalf("Open() failed: %v", err)
	}

	for _, meta := range entries {
		if err := store.Put(meta); err != nil {
			store.Close()
			os.RemoveAll(dir)
			t.Fatalf("Put(%q) failed: %v", meta.Path, err)
		}
	}

	engine := NewEngine(store)
	cleanup := func() {
		store.Close()
		os.RemoveAll(dir)
	}

	return engine, store, cleanup
}

// =============================================================================
// VAL-ENGINE-010: Exact filename match — tier 1
// =============================================================================

func TestExactMatchTier1(t *testing.T) {
	entries := []*index.FileMeta{
		{Path: "/home/user/report.pdf", Depth: 3},
		{Path: "/home/user/monthly_report.pdf", Depth: 3},
		{Path: "/home/user/reporting.pdf", Depth: 3},
	}

	engine, _, cleanup := setupEngine(t, entries)
	defer cleanup()

	results, err := engine.Search("report.pdf", 50)
	if err != nil {
		t.Fatalf("Search() failed: %v", err)
	}

	if len(results) == 0 {
		t.Fatal("expected at least one result")
	}

	// First result must be the exact match at tier 1.
	if results[0].Path != "/home/user/report.pdf" {
		t.Errorf("expected first result to be /home/user/report.pdf, got %s", results[0].Path)
	}
	if results[0].Tier != TierExact {
		t.Errorf("expected tier %d, got %d", TierExact, results[0].Tier)
	}

	// All tier 1 results must come before any other tier.
	for i, r := range results {
		if r.Tier == TierExact {
			continue
		}
		for j := i + 1; j < len(results); j++ {
			if results[j].Tier == TierExact {
				t.Errorf("tier 1 result at index %d appears after non-tier-1 result at index %d", j, i)
			}
		}
	}
}

// =============================================================================
// VAL-ENGINE-011: Prefix match — tier 2
// =============================================================================

func TestPrefixMatchTier2(t *testing.T) {
	entries := []*index.FileMeta{
		{Path: "/a/readme.md", Depth: 1},
		{Path: "/a/README.txt", Depth: 1},
		{Path: "/a/readings.docx", Depth: 1},
		{Path: "/a/exact_match", Depth: 1}, // exact match for query "exact_match"
	}

	engine, _, cleanup := setupEngine(t, entries)
	defer cleanup()

	// Query "read" should match readme.md, README.txt, readings.docx all at tier 2.
	results, err := engine.Search("read", 50)
	if err != nil {
		t.Fatalf("Search() failed: %v", err)
	}

	if len(results) < 3 {
		t.Fatalf("expected at least 3 results, got %d", len(results))
	}

	for _, r := range results {
		if r.Tier != TierPrefix {
			t.Errorf("expected all results at tier %d, got tier %d for %s", TierPrefix, r.Tier, r.Path)
		}
	}

	// Now query "exact_match" which should give an exact tier 1 match.
	results, err = engine.Search("exact_match", 50)
	if err != nil {
		t.Fatalf("Search() failed: %v", err)
	}
	if results[0].Tier != TierExact {
		t.Errorf("expected exact match at tier 1, got tier %d", results[0].Tier)
	}
	if results[0].Path != "/a/exact_match" {
		t.Errorf("expected /a/exact_match, got %s", results[0].Path)
	}
}

// =============================================================================
// VAL-ENGINE-012: Word-boundary match — tier 3 — CamelCase
// =============================================================================

func TestWordBoundaryCamelCaseTier3(t *testing.T) {
	entries := []*index.FileMeta{
		{Path: "/src/DataLoader.cpp", Depth: 2},
		{Path: "/src/DownloadUtil.cpp", Depth: 2},
	}

	engine, _, cleanup := setupEngine(t, entries)
	defer cleanup()

	results, err := engine.Search("load", 50)
	if err != nil {
		t.Fatalf("Search() failed: %v", err)
	}

	if len(results) < 2 {
		t.Fatalf("expected at least 2 results, got %d", len(results))
	}

	// Find DataLoader.cpp — should be tier 3.
	foundDL := false
	foundDU := false
	for _, r := range results {
		switch r.Path {
		case "/src/dataloader.cpp":
			if r.Tier != TierWordBoundary {
				t.Errorf("DataLoader.cpp should be tier 3 (word-boundary via CamelCase), got tier %d", r.Tier)
			}
			foundDL = true
		case "/src/downloadutil.cpp":
			if r.Tier != TierSubstring {
				t.Errorf("DownloadUtil.cpp should be tier 4 (substring only), got tier %d", r.Tier)
			}
			foundDU = true
		}
	}

	if !foundDL {
		t.Error("DataLoader.cpp not found in results")
	}
	if !foundDU {
		t.Error("DownloadUtil.cpp not found in results")
	}

	// Verify tier 3 results rank above tier 4.
	lastTier := 0
	for _, r := range results {
		if r.Tier < lastTier {
			t.Errorf("results not properly ordered: tier %d appears after tier %d", r.Tier, lastTier)
		}
		lastTier = r.Tier
	}
}

// =============================================================================
// VAL-ENGINE-013: Word-boundary match — tier 3 — snake_case/kebab-case/dot
// =============================================================================

func TestWordBoundarySeparatorsTier3(t *testing.T) {
	entries := []*index.FileMeta{
		{Path: "/py/data_loader.py", Depth: 2},
		{Path: "/cfg/config.prod.yaml", Depth: 2},
		{Path: "/js/my-component.js", Depth: 2},
		{Path: "/tmp/download.py", Depth: 1},
	}

	engine, _, cleanup := setupEngine(t, entries)
	defer cleanup()

	// Query "load" → data_loader.py at tier 3 (after _).
	results, err := engine.Search("load", 50)
	if err != nil {
		t.Fatalf("Search() failed: %v", err)
	}

	for _, r := range results {
		switch r.Path {
		case "/py/data_loader.py":
			if r.Tier != TierWordBoundary {
				t.Errorf("data_loader.py should be tier 3 (snake_case boundary), got tier %d", r.Tier)
			}
		case "/tmp/download.py":
			if r.Tier != TierSubstring {
				t.Errorf("download.py should be tier 4 (no boundary before 'load'), got tier %d", r.Tier)
			}
		}
	}

	// Query "yaml" → config.prod.yaml at tier 3 (after .).
	results, err = engine.Search("yaml", 50)
	if err != nil {
		t.Fatalf("Search() failed: %v", err)
	}

	found := false
	for _, r := range results {
		if r.Path == "/cfg/config.prod.yaml" {
			if r.Tier != TierWordBoundary {
				t.Errorf("config.prod.yaml should be tier 3 (dot boundary), got tier %d", r.Tier)
			}
			found = true
		}
	}
	if !found {
		t.Error("config.prod.yaml not found in results")
	}

	// Query "component" → my-component.js at tier 3 (after -).
	results, err = engine.Search("component", 50)
	if err != nil {
		t.Fatalf("Search() failed: %v", err)
	}

	for _, r := range results {
		if r.Path == "/js/my-component.js" {
			if r.Tier != TierWordBoundary {
				t.Errorf("my-component.js should be tier 3 (kebab-case boundary), got tier %d", r.Tier)
			}
		}
	}
}

// =============================================================================
// VAL-ENGINE-014: Substring match — tier 4
// =============================================================================

func TestSubstringMatchTier4(t *testing.T) {
	entries := []*index.FileMeta{
		{Path: "/img/download.png", Depth: 2},
	}

	engine, _, cleanup := setupEngine(t, entries)
	defer cleanup()

	// Query "own" — appears inside "download" but not at a boundary.
	results, err := engine.Search("own", 50)
	if err != nil {
		t.Fatalf("Search() failed: %v", err)
	}

	if len(results) == 0 {
		t.Fatal("expected at least one result")
	}

	if results[0].Tier != TierSubstring {
		t.Errorf("expected tier %d for substring 'own' in 'download.png', got tier %d", TierSubstring, results[0].Tier)
	}

	// Verify tier 4 is below tier 3: index both tier-3 and tier-4 entries and check ordering.
	entries2 := []*index.FileMeta{
		{Path: "/a/data_own.py", Depth: 1},    // '_own' → tier 3
		{Path: "/b/download.png", Depth: 1},    // 'own' in middle → tier 4
	}

	engine2, _, cleanup2 := setupEngine(t, entries2)
	defer cleanup2()

	results, err = engine2.Search("own", 50)
	if err != nil {
		t.Fatalf("Search() failed: %v", err)
	}

	if len(results) < 2 {
		t.Fatalf("expected at least 2 results, got %d", len(results))
	}

	// The tier 3 result must appear before the tier 4 result.
	foundT3 := false
	for _, r := range results {
		if r.Tier == TierWordBoundary {
			foundT3 = true
		} else if r.Tier == TierSubstring {
			if !foundT3 {
				t.Error("tier 4 result appears before tier 3 result")
			}
		}
	}
}

// =============================================================================
// VAL-ENGINE-015: Directory boost — directory ranks above file at same tier
// =============================================================================

func TestDirectoryBoostSameTier(t *testing.T) {
	entries := []*index.FileMeta{
		{Path: "/src/utils", IsDir: true, Depth: 2},     // directory
		{Path: "/src/utils.go", IsDir: false, Depth: 2},  // file
	}

	engine, _, cleanup := setupEngine(t, entries)
	defer cleanup()

	// Both entries are prefix matches for query "util" at tier 2.
	results, err := engine.Search("util", 50)
	if err != nil {
		t.Fatalf("Search() failed: %v", err)
	}

	if len(results) < 2 {
		t.Fatalf("expected at least 2 results, got %d", len(results))
	}

	// Directory should appear before file.
	if !results[0].IsDir {
		t.Error("expected directory to rank above file at same tier")
	}

	// The file should be second.
	if results[1].IsDir {
		t.Error("expected file to rank second at same tier")
	}
}

// =============================================================================
// VAL-ENGINE-016: Directory boost does not cross tier boundaries
// =============================================================================

func TestDirectoryBoostNoCrossTier(t *testing.T) {
	entries := []*index.FileMeta{
		{Path: "/home/downloads", IsDir: true, Depth: 2},   // prefix match tier 2
		{Path: "/home/download", IsDir: false, Depth: 2},    // exact match tier 1
	}

	engine, _, cleanup := setupEngine(t, entries)
	defer cleanup()

	// Query "download": exact match on "download" (tier 1 file), prefix match on "downloads" (tier 2 dir).
	results, err := engine.Search("download", 50)
	if err != nil {
		t.Fatalf("Search() failed: %v", err)
	}

	if len(results) < 2 {
		t.Fatalf("expected at least 2 results, got %d", len(results))
	}

	// The tier 1 file must rank above the tier 2 directory.
	if results[0].Tier != TierExact {
		t.Errorf("expected tier 1 result first, got tier %d", results[0].Tier)
	}
	if results[1].Tier != TierPrefix {
		t.Errorf("expected tier 2 result second, got tier %d", results[1].Tier)
	}

	// Even if the tier 2 result is a directory, it must not outrank the tier 1 file.
	if results[0].IsDir {
		t.Error("tier 1 file should rank above tier 2 directory")
	}
}

// =============================================================================
// VAL-ENGINE-017: Tiebreaking by depth then alphabetical
// =============================================================================

func TestTiebreakingDepthThenAlpha(t *testing.T) {
	entries := []*index.FileMeta{
		{Path: "/a/alpha.txt", Depth: 1},
		{Path: "/a/Zebra.txt", Depth: 1},
		{Path: "/deep/nested/alpha.txt", Depth: 3},
	}

	engine, _, cleanup := setupEngine(t, entries)
	defer cleanup()

	// Query "a" — all entries match as substring (tier 4).
	// Alpha is in "alpha.txt" as prefix, "Zebra.txt" as substring.
	// Actually "alpha.txt" contains "a" at position 0 → prefix match (tier 2).
	// "Zebra.txt" contains "a" at position 4 → substring (tier 4).
	// But we want to test tiebreaking at same tier...
	// Let's query something that gives both at same tier.
	// Actually for query "a", "alpha.txt" would be TierPrefix (starts with 'a')
	// and "Zebra.txt" would be TierSubstring (contains 'a' but not at start).
	// Different tiers, not a good test.
	//
	// Let's use query "lph" which is a substring of "alpha.txt" but not in "Zebra.txt".
	// Hmm, we need both at same tier. Let's use query "txt" → both contain ".txt".
	// But "txt" is after '.' → word boundary → tier 3 for both.

	results, err := engine.Search("txt", 50)
	if err != nil {
		t.Fatalf("Search() failed: %v", err)
	}

	// Both should be same tier (3, word-boundary after '.')
	// Shallow depth first: /a/alpha.txt (depth 1) before /deep/nested/alpha.txt (depth 3).
	shallowIdx := -1
	deepIdx := -1
	for i, r := range results {
		if r.Path == "/a/alpha.txt" {
			shallowIdx = i
		}
		if r.Path == "/deep/nested/alpha.txt" {
			deepIdx = i
		}
	}

	if shallowIdx < 0 || deepIdx < 0 {
		t.Fatal("both entries not found in results")
	}
	if shallowIdx > deepIdx {
		t.Errorf("shallow entry (depth 1) should rank before deep entry (depth 3), got indices %d and %d",
			shallowIdx, deepIdx)
	}

	// Also test alphabetical ordering at same depth + same tier.
	// "alpha.txt" vs "Zebra.txt" for query that matches both at same tier.
	// Query "a" would give "alpha" at prefix tier 2 and "Zebra" at substring tier 4. Different.
	// Query ".txt" gives both at word-boundary tier 3.
	results, err = engine.Search(".txt", 50)
	if err != nil {
		t.Fatalf("Search() failed: %v", err)
	}

	alphaIdx := -1
	zebraIdx := -1
	for i, r := range results {
		if r.Path == "/a/alpha.txt" {
			alphaIdx = i
		}
		if r.Path == "/a/Zebra.txt" {
			zebraIdx = i
		}
	}

	if alphaIdx >= 0 && zebraIdx >= 0 && alphaIdx > zebraIdx {
		t.Errorf("alphabetically, 'alpha.txt' should come before 'zebra.txt' (case-insensitive), got indices %d and %d",
			alphaIdx, zebraIdx)
	}
}

// =============================================================================
// VAL-ENGINE-018: Search is case-insensitive
// =============================================================================

func TestCaseInsensitiveSearch(t *testing.T) {
	entries := []*index.FileMeta{
		{Path: "/docs/ReadMe.md", Depth: 2},
	}

	engine, _, cleanup := setupEngine(t, entries)
	defer cleanup()

	queries := []string{"readme", "README", "ReadMe", "readme.md", "README.MD"}

	for _, q := range queries {
		t.Run(q, func(t *testing.T) {
			results, err := engine.Search(q, 50)
			if err != nil {
				t.Fatalf("Search(%q) failed: %v", q, err)
			}
			if len(results) == 0 {
				t.Errorf("Search(%q) returned no results", q)
				return
			}
			if results[0].Tier != TierExact && q == "readme.md" {
				// "readme.md" should be exact match
			}
			// Query "readme" should still find it (prefix match)
			if results[0].Path != "/docs/readme.md" {
				t.Errorf("expected /docs/readme.md, got %s", results[0].Path)
			}
		})
	}

	// All case variations of the exact filename should return tier 1.
	for _, q := range []string{"readme.md", "README.MD", "ReadMe.md"} {
		results, err := engine.Search(q, 50)
		if err != nil {
			t.Fatalf("Search(%q) failed: %v", q, err)
		}
		if len(results) == 0 || results[0].Tier != TierExact {
			t.Errorf("Search(%q): expected tier 1 exact match, got tier %d", q, results[0].Tier)
		}
	}
}

// =============================================================================
// VAL-ENGINE-019: Empty query returns empty result set
// =============================================================================

func TestEmptyQuery(t *testing.T) {
	entries := []*index.FileMeta{
		{Path: "/tmp/test.txt", Depth: 1},
	}

	engine, _, cleanup := setupEngine(t, entries)
	defer cleanup()

	// Empty string.
	results, err := engine.Search("", 50)
	if err != nil {
		t.Fatalf("Search(\"\") failed: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected empty results for empty query, got %d results", len(results))
	}

	// Whitespace only.
	results, err = engine.Search("   ", 50)
	if err != nil {
		t.Fatalf("Search(\"   \") failed: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected empty results for whitespace query, got %d results", len(results))
	}

	// Tab and newline.
	results, err = engine.Search("\t\n  ", 50)
	if err != nil {
		t.Fatalf("Search(whitespace) failed: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected empty results for whitespace-only query, got %d results", len(results))
	}
}

// =============================================================================
// VAL-ENGINE-020: Early termination after N results
// =============================================================================

func TestEarlyTermination(t *testing.T) {
	// Create entries at various tiers for query "test".
	entries := []*index.FileMeta{
		// Tier 1: Exact match.
		{Path: "/a/test", Depth: 1},
		// Tier 2: Prefix matches.
		{Path: "/a/testing", Depth: 1},
		{Path: "/a/tested", Depth: 1},
		{Path: "/a/testify", Depth: 1},
		// Tier 3: Word-boundary matches.
		{Path: "/a/my_test.go", Depth: 1},
		{Path: "/a/unit-test.rs", Depth: 1},
		// Tier 4: Substring matches.
		{Path: "/a/attest.txt", Depth: 1},
		{Path: "/a/contest.txt", Depth: 1},
		{Path: "/a/detest.txt", Depth: 1},
	}

	engine, _, cleanup := setupEngine(t, entries)
	defer cleanup()

	// With limit=3, we should get exactly 3 results: all tier 1 + top tier 2.
	results, err := engine.Search("test", 3)
	if err != nil {
		t.Fatalf("Search() failed: %v", err)
	}

	if len(results) != 3 {
		t.Fatalf("expected exactly 3 results with limit=3, got %d", len(results))
	}

	// All results should be from highest tiers (1 and 2 only).
	for _, r := range results {
		if r.Tier > TierPrefix {
			t.Errorf("with limit=3, expected only tier 1-2 results, but got tier %d", r.Tier)
		}
	}

	// With limit=1, we should get only the exact match.
	results, err = engine.Search("test", 1)
	if err != nil {
		t.Fatalf("Search() failed: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected exactly 1 result with limit=1, got %d", len(results))
	}
	if results[0].Tier != TierExact {
		t.Errorf("expected tier 1 exact match with limit=1, got tier %d", results[0].Tier)
	}

	// With limit=100 (more than total), should get all results.
	results, err = engine.Search("test", 100)
	if err != nil {
		t.Fatalf("Search() failed: %v", err)
	}
	if len(results) != len(entries) {
		t.Errorf("expected all %d results with limit=100, got %d", len(entries), len(results))
	}
}

// =============================================================================
// VAL-ENGINE-021: limit parameter handling (negative → error, 0 → empty)
// =============================================================================

func TestLimitParameter(t *testing.T) {
	entries := []*index.FileMeta{
		{Path: "/tmp/test.txt", Depth: 1},
	}

	engine, _, cleanup := setupEngine(t, entries)
	defer cleanup()

	// limit=0 returns empty array.
	results, err := engine.Search("test", 0)
	if err != nil {
		t.Fatalf("Search() with limit=0 failed: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected empty results with limit=0, got %d results", len(results))
	}

	// Negative limit returns error.
	_, err = engine.Search("test", -1)
	if err == nil {
		t.Fatal("expected error for negative limit, got nil")
	}
	if err != ErrNegativeLimit {
		t.Errorf("expected ErrNegativeLimit, got %v", err)
	}
}

// =============================================================================
// Additional edge case tests
// =============================================================================

func TestNoMatches(t *testing.T) {
	entries := []*index.FileMeta{
		{Path: "/tmp/hello.txt", Depth: 1},
	}

	engine, _, cleanup := setupEngine(t, entries)
	defer cleanup()

	results, err := engine.Search("zzzz_nonexistent", 50)
	if err != nil {
		t.Fatalf("Search() failed: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected empty results, got %d results", len(results))
	}
}

func TestDuplicateFilenamesDifferentDirs(t *testing.T) {
	entries := []*index.FileMeta{
		{Path: "/a/readme.md", Depth: 1},
		{Path: "/b/readme.md", Depth: 1},
	}

	engine, _, cleanup := setupEngine(t, entries)
	defer cleanup()

	results, err := engine.Search("readme", 50)
	if err != nil {
		t.Fatalf("Search() failed: %v", err)
	}

	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	// Both should be distinct.
	paths := make(map[string]bool)
	for _, r := range results {
		paths[r.Path] = true
	}
	if !paths["/a/readme.md"] || !paths["/b/readme.md"] {
		t.Error("expected both /a/readme.md and /b/readme.md in results")
	}
}

func TestShortFilenameTrigramFallback(t *testing.T) {
	// Filenames shorter than 3 characters are not in the trigram index.
	// They should still be found via the trie (prefix/exact match).
	entries := []*index.FileMeta{
		{Path: "/tmp/ab", Depth: 1}, // 2 chars, no trigram
	}

	engine, _, cleanup := setupEngine(t, entries)
	defer cleanup()

	// Exact match should still work.
	results, err := engine.Search("ab", 50)
	if err != nil {
		t.Fatalf("Search() failed: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result for exact match on short filename, got %d", len(results))
	}
	if results[0].Tier != TierExact {
		t.Errorf("expected tier %d, got tier %d", TierExact, results[0].Tier)
	}

	// Prefix match on "a" should also work (from trie).
	results, err = engine.Search("a", 50)
	if err != nil {
		t.Fatalf("Search() failed: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result for prefix match on 'a', got %d", len(results))
	}
	if results[0].Tier != TierPrefix {
		t.Errorf("expected tier %d for prefix match 'a', got tier %d", TierPrefix, results[0].Tier)
	}
}

func TestMultiWordBoundaryDetection(t *testing.T) {
	entries := []*index.FileMeta{
		{Path: "/src/Data_Loader_Util.cpp", Depth: 2},
	}

	engine, _, cleanup := setupEngine(t, entries)
	defer cleanup()

	// "loader" appears after '_' → tier 3.
	results, err := engine.Search("loader", 50)
	if err != nil {
		t.Fatalf("Search() failed: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected results for 'loader'")
	}
	found := false
	for _, r := range results {
		if r.Path == "/src/data_loader_util.cpp" && r.Tier == TierWordBoundary {
			found = true
		}
	}
	if !found {
		t.Error("expected Data_Loader_Util.cpp at tier 3 for query 'loader'")
	}
}

func TestResultsReturnedAsArray(t *testing.T) {
	entries := []*index.FileMeta{
		{Path: "/tmp/file.txt", Depth: 1},
	}

	engine, _, cleanup := setupEngine(t, entries)
	defer cleanup()

	// Empty query should return an empty array (not nil).
	results, err := engine.Search("", 50)
	if err != nil {
		t.Fatalf("Search() failed: %v", err)
	}
	if results == nil {
		t.Error("expected non-nil empty slice for empty query")
	}
}

func TestQueryNormalization(t *testing.T) {
	entries := []*index.FileMeta{
		{Path: "/tmp/my file.txt", Depth: 1},
	}

	engine, _, cleanup := setupEngine(t, entries)
	defer cleanup()

	// Query with leading/trailing whitespace should be normalized.
	results, err := engine.Search("  my file  ", 50)
	if err != nil {
		t.Fatalf("Search() failed: %v", err)
	}
	if len(results) < 1 {
		t.Error("expected results for 'my file' query even with surrounding whitespace")
	}

	// Uppercase query should be lowered.
	results, err = engine.Search("MY FILE", 50)
	if err != nil {
		t.Fatalf("Search() failed: %v", err)
	}
	if len(results) < 1 {
		t.Error("expected results for 'MY FILE' query (should be case-insensitive)")
	}
}
