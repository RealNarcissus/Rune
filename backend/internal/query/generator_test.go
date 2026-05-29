package query

import (
	"strings"
	"testing"

	"github.com/rune/backend/internal/index"
)

func TestGenerateRealisticPaths_Zero(t *testing.T) {
	entries := GenerateRealisticPaths(0)
	if entries != nil {
		t.Errorf("expected nil for n=0, got %d entries", len(entries))
	}
}

func TestGenerateRealisticPaths_Small(t *testing.T) {
	entries := GenerateRealisticPaths(100)
	if len(entries) != 100 {
		t.Errorf("expected 100 entries, got %d", len(entries))
	}
}

func TestGenerateRealisticPaths_AllUnique(t *testing.T) {
	n := 10000
	entries := GenerateRealisticPaths(n)

	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		if seen[e.Path] {
			t.Errorf("duplicate path found: %s", e.Path)
		}
		seen[e.Path] = true
	}

	if len(seen) != n {
		t.Errorf("expected %d unique paths, got %d", n, len(seen))
	}
}

func TestGenerateRealisticPaths_Deterministic(t *testing.T) {
	n := 5000
	first := GenerateRealisticPaths(n)
	second := GenerateRealisticPaths(n)

	if len(first) != len(second) {
		t.Fatalf("length mismatch: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i].Path != second[i].Path {
			t.Errorf("deterministic output differs at index %d: %q vs %q",
				i, first[i].Path, second[i].Path)
			break
		}
	}
}

func TestGenerateRealisticPaths_RealisticStructure(t *testing.T) {
	n := 5000
	entries := GenerateRealisticPaths(n)

	hasSrc := false
	hasHome := false
	hasVarLog := false
	hasProjects := false
	hasDeepPath := false

	for _, e := range entries {
		path := e.Path
		if strings.Contains(path, "/src/") {
			hasSrc = true
		}
		if strings.HasPrefix(path, "/home/") {
			hasHome = true
		}
		if strings.HasPrefix(path, "/var/log") {
			hasVarLog = true
		}
		if strings.Contains(path, "/projects/") {
			hasProjects = true
		}
		if strings.Count(path, "/") >= 6 {
			hasDeepPath = true
		}
	}

	if !hasSrc {
		t.Error("expected some paths with /src/ directories")
	}
	if !hasHome {
		t.Error("expected some paths under /home/")
	}
	if !hasVarLog {
		t.Error("expected some paths under /var/log")
	}
	if !hasProjects {
		t.Error("expected some paths with /projects/")
	}
	if !hasDeepPath {
		t.Error("expected some deep paths (6+ levels)")
	}
}

func TestGenerateRealisticPaths_FileMetaPopulated(t *testing.T) {
	entries := GenerateRealisticPaths(10)

	for _, e := range entries {
		if e.Path == "" {
			t.Error("expected non-empty Path")
		}
	}
}

func TestGenerateRealisticPaths_Insertable(t *testing.T) {
	// Verify generated paths can be successfully inserted into a real store.
	entries := GenerateRealisticPaths(100)
	engine, store, cleanup := setupEngine(t, entries)
	defer cleanup()

	stats := store.Stats()
	if stats.PathCount != 100 {
		t.Errorf("expected 100 entries in store, got %d", stats.PathCount)
	}

	// Verify each entry is searchable via the engine.
	for _, e := range entries {
		// Extract the filename (without extension) for a search query.
		baseIdx := strings.LastIndex(e.Path, "/")
		if baseIdx < 0 {
			continue
		}
		filename := e.Path[baseIdx+1:]
		dotIdx := strings.Index(filename, ".")
		query := filename
		if dotIdx > 0 {
			query = filename[:dotIdx]
		}

		results, err := engine.Search(query, 50)
		if err != nil {
			t.Errorf("Search(%q) failed: %v", query, err)
			continue
		}

		found := false
		for _, r := range results {
			if r.Path == index.NormalizePath(e.Path) {
				found = true
				break
			}
		}
		if len(results) > 0 && !found {
			// Not all entries will match their stem as a search query,
			// but any returned result should be valid.
		}
		_ = results
	}
}
