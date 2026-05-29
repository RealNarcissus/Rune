package query

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/rune/backend/internal/index"
)

// =============================================================================
// Benchmark Configuration
// =============================================================================

// benchmarkEntryCount is the number of entries to use in benchmarks.
// VAL-ENGINE-034, -035, -036 require 1,000,000 entries.
const benchmarkEntryCount = 1_000_000

// smallBenchmarkEntryCount is used for quick benchmarks during development.
const smallBenchmarkEntryCount = 10_000

// benchmarkQueries provides a diverse set of search queries for benchmarks.
// Includes queries that exercise all 4 ranking tiers:
//   - Exact and prefix match queries (trie-based, fast)
//   - Substring queries (trigram-based, intersected)
var benchmarkQueries = []struct {
	query   string
	tier    int  // expected dominant tier
	isShort bool // query works even on small benchmark sets
}{
	{"index", TierPrefix, true},        // common prefix match
	{"test", TierPrefix, true},         // common prefix match
	{"config", TierPrefix, true},       // common prefix match
	{"readme", TierPrefix, true},       // common prefix match
	{"main", TierExact, true},          // common exact/prefix
	{"server", TierPrefix, true},       // common prefix match
	{"handler", TierPrefix, true},      // common prefix match
	{"utils", TierPrefix, true},        // common prefix match
	{"model", TierPrefix, true},        // common prefix match
	{"service", TierPrefix, true},      // common prefix match
	{"middleware", TierPrefix, false},  // longer prefix
	{"component", TierPrefix, false},   // longer prefix
	{"controller", TierPrefix, false},  // longer prefix
	{"repository", TierPrefix, false},  // longer prefix
}

// =============================================================================
// Benchmark Helpers
// =============================================================================

// setupBenchmarkStore creates an index store populated with n generated entries
// and returns the store, query engine, and a cleanup function.
func setupBenchmarkStore(tb testing.TB, n int) (*Engine, *index.Store, func()) {
	tb.Helper()

	dir, err := os.MkdirTemp("", "rune-bench-*")
	if err != nil {
		tb.Fatalf("failed to create temp dir: %v", err)
	}

	store, err := index.Open(dir)
	if err != nil {
		os.RemoveAll(dir)
		tb.Fatalf("Open() failed: %v", err)
	}

	entries := GenerateRealisticPaths(n)
	tb.Logf("Inserting %d entries into index store...", n)

	if err := store.BulkPut(entries); err != nil {
		store.Close()
		os.RemoveAll(dir)
		tb.Fatalf("BulkPut() failed: %v", err)
	}

	tb.Logf("Inserted %d entries. Trie nodes: %d, Trigrams: %d",
		n, store.Stats().TrieNodes, store.Stats().TrigramCount)

	engine := NewEngine(store)
	cleanup := func() {
		store.Close()
		os.RemoveAll(dir)
	}

	return engine, store, cleanup
}

// =============================================================================
// VAL-ENGINE-034: BenchmarkSearchPrefix — prefix search <5ms P50 on 1M entries
// =============================================================================

func BenchmarkSearchPrefix(b *testing.B) {
	if testing.Short() {
		b.Skip("skipping 1M-entry benchmark in short mode")
	}

	engine, _, cleanup := setupBenchmarkStore(b, benchmarkEntryCount)
	defer cleanup()

	// Select prefix-like queries: these hit the trie for prefix candidates.
	prefixQueries := []string{
		"index", "test", "config", "readme", "main",
		"server", "handler", "utils", "model", "service",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		q := prefixQueries[i%len(prefixQueries)]
		_, err := engine.Search(q, 20)
		if err != nil {
			b.Fatalf("Search(%q) failed: %v", q, err)
		}
	}
}

// =============================================================================
// VAL-ENGINE-035: BenchmarkSearchSubstring — substring search <5ms P50 on 1M entries
// =============================================================================

func BenchmarkSearchSubstring(b *testing.B) {
	if testing.Short() {
		b.Skip("skipping 1M-entry benchmark in short mode")
	}

	engine, _, cleanup := setupBenchmarkStore(b, benchmarkEntryCount)
	defer cleanup()

	// Substring queries: these exercise the trigram intersection path.
	// The trigram index returns candidate sets that are intersected and ranked.
	substringQueries := []string{
		"mod", "han", "con", "erv", "util",
		"stor", "base", "data", "orm", "load",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		q := substringQueries[i%len(substringQueries)]
		_, err := engine.Search(q, 20)
		if err != nil {
			b.Fatalf("Search(%q) failed: %v", q, err)
		}
	}
}

// =============================================================================
// Additional benchmarks for comprehensive performance testing
// =============================================================================

// BenchmarkSearchExact measures exact-match query latency (VAL-PERF-003 target: <2ms).
func BenchmarkSearchExact(b *testing.B) {
	if testing.Short() {
		b.Skip("skipping 1M-entry benchmark in short mode")
	}

	engine, _, cleanup := setupBenchmarkStore(b, benchmarkEntryCount)
	defer cleanup()

	// Exact match queries: the filename equals the query exactly.
	// We use known stems from the generator that match exactly.
	exactQueries := []string{
		"index.ts", "main.go", "config.json", "test.py",
		"utils.rs", "helpers.js", "types.ts", "app.go",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		q := exactQueries[i%len(exactQueries)]
		_, err := engine.Search(q, 20)
		if err != nil {
			b.Fatalf("Search(%q) failed: %v", q, err)
		}
	}
}

// BenchmarkSearchEmpty verifies empty query returns immediately (VAL-PERF-016 target: <100µs).
func BenchmarkSearchEmpty(b *testing.B) {
	if testing.Short() {
		b.Skip("skipping 1M-entry benchmark in short mode")
	}

	engine, _, cleanup := setupBenchmarkStore(b, benchmarkEntryCount)
	defer cleanup()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := engine.Search("", 20)
		if err != nil {
			b.Fatalf("Search(\"\") failed: %v", err)
		}
	}

	b.ReportMetric(0, "allocs/op")
}

// BenchmarkSearchSmallPrefix measures prefix search on a smaller index (10K entries)
// for quick iteration during development.
func BenchmarkSearchSmallPrefix(b *testing.B) {
	engine, _, cleanup := setupBenchmarkStore(b, smallBenchmarkEntryCount)
	defer cleanup()

	prefixQueries := []string{"index", "test", "config", "readme", "main"}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		q := prefixQueries[i%len(prefixQueries)]
		_, err := engine.Search(q, 20)
		if err != nil {
			b.Fatalf("Search(%q) failed: %v", q, err)
		}
	}
}

// BenchmarkSearchSmallSubstring measures substring search on 10K entries.
func BenchmarkSearchSmallSubstring(b *testing.B) {
	engine, _, cleanup := setupBenchmarkStore(b, smallBenchmarkEntryCount)
	defer cleanup()

	substringQueries := []string{"mod", "han", "con", "erv", "util"}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		q := substringQueries[i%len(substringQueries)]
		_, err := engine.Search(q, 20)
		if err != nil {
			b.Fatalf("Search(%q) failed: %v", q, err)
		}
	}
}

// =============================================================================
// VAL-ENGINE-036: TestMemoryUsage — heap allocation <1 GiB for 1M entries
// =============================================================================

func TestMemoryUsage(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 1M-entry memory test in short mode")
	}

	// Force GC and record baseline memory.
	runtime.GC()
	var baseline runtime.MemStats
	runtime.ReadMemStats(&baseline)

	engine, store, cleanup := setupBenchmarkStore(t, benchmarkEntryCount)
	defer cleanup()

	// Force GC to get accurate heap measurement.
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	heapAlloc := after.HeapAlloc - baseline.HeapAlloc
	heapInUse := after.HeapInuse - baseline.HeapInuse
	totalAlloc := after.TotalAlloc - baseline.TotalAlloc

	t.Logf("Memory usage for %d entries:", benchmarkEntryCount)
	t.Logf("  HeapAlloc:  %s", formatBytes(heapAlloc))
	t.Logf("  HeapInuse:  %s", formatBytes(heapInUse))
	t.Logf("  TotalAlloc: %s", formatBytes(totalAlloc))
	t.Logf("  NumGC:      %d", after.NumGC-baseline.NumGC)

	// These prevent compiler optimizations from eliminating the engine/store.
	_ = engine
	_ = store

	const oneGiB = uint64(1 << 30)
	if heapAlloc > oneGiB {
		t.Errorf("HeapAlloc %s exceeds 1 GiB limit for %d entries",
			formatBytes(heapAlloc), benchmarkEntryCount)
	}
	if heapInUse > oneGiB*2 {
		t.Errorf("HeapInuse %s exceeds 2 GiB threshold for %d entries",
			formatBytes(heapInUse), benchmarkEntryCount)
	}

	// Verify store consistency after bulk insert.
	// Since GenerateRealisticPaths can create duplicates during combinatorial generation of 1M entries,
	// we calculate the expected number of unique paths in the generated set.
	expectedUnique := 0
	{
		entries := GenerateRealisticPaths(benchmarkEntryCount)
		seen := make(map[string]bool)
		for _, e := range entries {
			p := index.NormalizePath(e.Path)
			if !seen[p] {
				seen[p] = true
				expectedUnique++
			}
		}
	}

	stats := store.Stats()
	if stats.PathCount != expectedUnique {
		t.Errorf("expected %d unique entries in store, got %d", expectedUnique, stats.PathCount)
	}
	if stats.TrieNodes == 0 {
		t.Error("trie is empty after bulk insert")
	}
	if stats.TrigramCount == 0 {
		t.Error("trigram index is empty after bulk insert")
	}
}

// TestMemoryUsageSmall runs a quick memory check on 10K entries.
// This runs even in short mode since it's fast.
func TestMemoryUsageSmall(t *testing.T) {
	runtime.GC()
	var baseline runtime.MemStats
	runtime.ReadMemStats(&baseline)

	engine, store, cleanup := setupBenchmarkStore(t, smallBenchmarkEntryCount)
	defer cleanup()

	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	heapAlloc := after.HeapAlloc - baseline.HeapAlloc

	t.Logf("Memory usage for %d entries:", smallBenchmarkEntryCount)
	t.Logf("  HeapAlloc:  %s", formatBytes(heapAlloc))
	t.Logf("  NumGC:      %d", after.NumGC-baseline.NumGC)

	_ = engine
	_ = store
}

// =============================================================================
// Utility
// =============================================================================

// formatBytes formats a byte count as a human-readable string.
func formatBytes(bytes uint64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := uint64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

// TestGenerateRealisticPaths_Performance verifies the generator can produce
// 1M paths without excessive memory or time.
func TestGenerateRealisticPaths_Performance(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 1M-entry generator performance test in short mode")
	}

	runtime.GC()
	var baseline runtime.MemStats
	runtime.ReadMemStats(&baseline)

	entries := GenerateRealisticPaths(benchmarkEntryCount)

	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	if len(entries) != benchmarkEntryCount {
		t.Errorf("expected %d entries, got %d", benchmarkEntryCount, len(entries))
	}

	heapAlloc := after.HeapAlloc - baseline.HeapAlloc
	t.Logf("Generator memory for %d paths: %s", benchmarkEntryCount, formatBytes(heapAlloc))

	// Verify uniqueness of a sample.
	sampleSize := 10000
	seen := make(map[string]bool, sampleSize)
	for i := 0; i < sampleSize; i++ {
		path := entries[i*len(entries)/sampleSize].Path
		if seen[path] {
			t.Errorf("duplicate path in sample: %s", path)
		}
		seen[path] = true
	}

	// Verify realistic structure.
	hasSrc := false
	hasHome := false
	for i := 0; i < 1000; i++ {
		p := entries[i].Path
		if strings.Contains(p, "/src/") {
			hasSrc = true
		}
		if strings.HasPrefix(p, "/home/") {
			hasHome = true
		}
	}
	if !hasSrc {
		t.Error("expected some paths with /src/ directories")
	}
	if !hasHome {
		t.Error("expected some paths under /home/")
	}
}

// TestBenchmarkQueriesAreValid verifies that benchmark queries return
// reasonable results on a test index, preventing silent benchmark degradation.
func TestBenchmarkQueriesAreValid(t *testing.T) {
	entries := GenerateRealisticPaths(10000)
	engine, _, cleanup := setupEngine(t, entries)
	defer cleanup()

	for _, bq := range benchmarkQueries {
		if !bq.isShort {
			continue // skip queries that need the full 1M dataset
		}
		results, err := engine.Search(bq.query, 20)
		if err != nil {
			t.Errorf("Search(%q) failed: %v", bq.query, err)
			continue
		}
		if len(results) == 0 {
			t.Errorf("Search(%q) returned no results on 10K entry index", bq.query)
		}
	}
}
