package index

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/bmatsuo/lmdb-go/lmdb"
)

// tempDir creates a temporary directory and returns its path.
// The caller is responsible for cleanup.
func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "luminasearch-index-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// TestCreateEnv verifies VAL-ENGINE-001: LMDB env created from empty state with schema version.
func TestCreateEnv(t *testing.T) {
	dir := tempDir(t)

	store, err := Open(dir)
	if err != nil {
		t.Fatalf("Open() failed: %v", err)
	}
	defer store.Close()

	// Verify the LMDB environment exists on disk.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir failed: %v", err)
	}
	if len(entries) == 0 {
		t.Error("LMDB environment directory is empty; expected data files")
	}

	// Verify the schema version is stored correctly.
	err = store.env.View(func(txn *lmdb.Txn) error {
		val, err := txn.Get(store.dbis.meta, []byte(metaKeySchemaVersion))
		if err != nil {
			return fmt.Errorf("schema version not found: %w", err)
		}
		var version int
		if _, scanErr := fmt.Sscanf(string(val), "%d", &version); scanErr != nil {
			return fmt.Errorf("invalid schema version: %q", string(val))
		}
		if version != SchemaVersion {
			return fmt.Errorf("expected schema version %d, got %d", SchemaVersion, version)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Verify all DBIs exist.
	err = store.env.View(func(txn *lmdb.Txn) error {
		for _, name := range []string{dbiNamesPaths, dbiNamesNames, dbiNamesTrigrams, dbiNamesMeta} {
			dbi, dbiErr := txn.OpenDBI(name, 0)
			if dbiErr != nil {
				return fmt.Errorf("DBI %q not found: %w", name, dbiErr)
			}
			_ = dbi
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestOpenExistingEnv verifies VAL-ENGINE-002: Existing LMDB env opened with schema validation.
func TestOpenExistingEnv(t *testing.T) {
	dir := tempDir(t)

	// Create a store and add an entry.
	store1, err := Open(dir)
	if err != nil {
		t.Fatalf("first Open() failed: %v", err)
	}

	meta := &FileMeta{
		Path:      "/home/user/test.txt",
		Filename:  "test.txt",
		Parent:    "/home/user",
		Extension: ".txt",
		IsDir:     false,
		Modified:  1234567890,
		Depth:     2,
	}
	if err := store1.Put(meta); err != nil {
		t.Fatalf("Put() failed: %v", err)
	}
	store1.Close()

	// Reopen and verify the entry persists.
	store2, err := Open(dir)
	if err != nil {
		t.Fatalf("second Open() failed: %v", err)
	}
	defer store2.Close()

	retrieved, err := store2.Get("/home/user/test.txt")
	if err != nil {
		t.Fatalf("Get() failed: %v", err)
	}
	if retrieved == nil {
		t.Fatal("entry not found after reopen")
	}
	if retrieved.Path != "/home/user/test.txt" {
		t.Errorf("expected path /home/user/test.txt, got %s", retrieved.Path)
	}
	if retrieved.Filename != "test.txt" {
		t.Errorf("expected filename test.txt, got %s", retrieved.Filename)
	}
}

// TestTrieBuildFromLMDB verifies VAL-ENGINE-003: In-memory prefix trie built from LMDB on load.
func TestTrieBuildFromLMDB(t *testing.T) {
	dir := tempDir(t)

	// Create store with entries.
	store1, err := Open(dir)
	if err != nil {
		t.Fatalf("Open() failed: %v", err)
	}

	entries := []*FileMeta{
		{Path: "/a/readme.md", Filename: "readme.md", Parent: "/a", Extension: ".md", Depth: 1},
		{Path: "/b/config.json", Filename: "config.json", Parent: "/b", Extension: ".json", Depth: 1},
		{Path: "/c/download.png", Filename: "download.png", Parent: "/c", Extension: ".png", Depth: 1},
	}
	for _, e := range entries {
		if err := store1.Put(e); err != nil {
			t.Fatalf("Put() failed: %v", err)
		}
	}
	store1.Close()

	// Reopen and verify the trie is rebuilt.
	store2, err := Open(dir)
	if err != nil {
		t.Fatalf("second Open() failed: %v", err)
	}
	defer store2.Close()

	if store2.trie.NodeCount() == 0 {
		t.Error("trie has zero nodes after load from LMDB")
	}

	// Every known path should be findable via trie lookup.
	result := store2.trie.Search("readme.md")
	if _, ok := result["/a/readme.md"]; !ok {
		t.Error("expected /a/readme.md in trie search results")
	}

	result = store2.trie.Search("config.json")
	if _, ok := result["/b/config.json"]; !ok {
		t.Error("expected /b/config.json in trie search results")
	}
}

// TestTrigramBuildFromLMDB verifies VAL-ENGINE-004: In-memory trigram index built from LMDB on load.
func TestTrigramBuildFromLMDB(t *testing.T) {
	dir := tempDir(t)

	// Create store with an entry.
	store1, err := Open(dir)
	if err != nil {
		t.Fatalf("Open() failed: %v", err)
	}

	meta := &FileMeta{
		Path:      "/home/user/readme.md",
		Filename:  "readme.md",
		Parent:    "/home/user",
		Extension: ".md",
		Depth:     2,
	}
	if err := store1.Put(meta); err != nil {
		t.Fatalf("Put() failed: %v", err)
	}
	store1.Close()

	// Reopen and verify trigrams are rebuilt.
	store2, err := Open(dir)
	if err != nil {
		t.Fatalf("second Open() failed: %v", err)
	}
	defer store2.Close()

	if store2.trigrams.Size() == 0 {
		t.Error("trigram index is empty after load from LMDB")
	}

	// For filename "readme.md", trigrams "rea", "ead", "adm", "dme" should all map to the entry.
	for _, trigram := range []string{"rea", "ead", "adm", "dme"} {
		result := store2.trigrams.Search(trigram)
		if result == nil {
			t.Errorf("trigram %q not found in index", trigram)
			continue
		}
		if _, ok := result["/home/user/readme.md"]; !ok {
			t.Errorf("expected /home/user/readme.md in search for trigram %q", trigram)
		}
	}
}

// TestPathNormalization verifies VAL-ENGINE-005: Paths normalized once at index time.
func TestPathNormalization(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"/a/./b//c/../d/", "/a/b/d"},
		{"/home/user//docs/./file.txt", "/home/user/docs/file.txt"},
		{"/var/./log/../tmp/test", "/var/tmp/test"},
		{"/simple/path", "/simple/path"},
		{"/trailing/slash/", "/trailing/slash"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := NormalizePath(tt.input)
			if result != tt.expected {
				t.Errorf("NormalizePath(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

// TestPathLowercasing verifies VAL-ENGINE-006: Paths lowercased once at index time.
func TestPathLowercasing(t *testing.T) {
	dir := tempDir(t)

	store, err := Open(dir)
	if err != nil {
		t.Fatalf("Open() failed: %v", err)
	}
	defer store.Close()

	meta := &FileMeta{
		Path:     "/Home/User/Documents/Report.PDF",
		Filename: "Report.PDF",
		Parent:   "/Home/User/Documents",
		Depth:    3,
	}
	if err := store.Put(meta); err != nil {
		t.Fatalf("Put() failed: %v", err)
	}

	// Verify the stored path is lowercased.
	retrieved, err := store.Get("/Home/User/Documents/Report.PDF")
	if err != nil {
		t.Fatalf("Get() failed: %v", err)
	}
	if retrieved == nil {
		t.Fatal("entry not found")
	}
	expectedPath := "/home/user/documents/report.pdf"
	if retrieved.Path != expectedPath {
		t.Errorf("expected path %q, got %q", expectedPath, retrieved.Path)
	}
	if retrieved.Filename != "report.pdf" {
		t.Errorf("expected filename report.pdf, got %s", retrieved.Filename)
	}
}

// TestIncrementalInsert verifies VAL-ENGINE-007: Incremental insert adds to LMDB + trie + trigram.
func TestIncrementalInsert(t *testing.T) {
	dir := tempDir(t)

	store, err := Open(dir)
	if err != nil {
		t.Fatalf("Open() failed: %v", err)
	}
	defer store.Close()

	// Insert entry.
	meta := &FileMeta{
		Path:      "/tmp/testfile.txt",
		Filename:  "testfile.txt",
		Parent:    "/tmp",
		Extension: ".txt",
		Depth:     1,
	}
	if err := store.Put(meta); err != nil {
		t.Fatalf("Put() failed: %v", err)
	}

	// Verify in LMDB.
	retrieved, err := store.Get("/tmp/testfile.txt")
	if err != nil {
		t.Fatalf("Get() failed: %v", err)
	}
	if retrieved == nil {
		t.Fatal("entry not found in LMDB")
	}

	// Verify in trie.
	trieResult := store.trie.Search("testfile.txt")
	if _, ok := trieResult["/tmp/testfile.txt"]; !ok {
		t.Error("entry not found in trie")
	}

	// Verify in trigram index.
	trigramResult := store.trigrams.Search("tes")
	if trigramResult == nil {
		t.Error("trigram 'tes' not found")
	} else if _, ok := trigramResult["/tmp/testfile.txt"]; !ok {
		t.Error("entry not found in trigram index")
	}

	// Verify in metadata cache.
	if _, ok := store.metaCache["/tmp/testfile.txt"]; !ok {
		t.Error("entry not found in metadata cache")
	}
}

// TestIncrementalDelete verifies VAL-ENGINE-008: Incremental delete removes from LMDB + trie + trigram.
func TestIncrementalDelete(t *testing.T) {
	dir := tempDir(t)

	store, err := Open(dir)
	if err != nil {
		t.Fatalf("Open() failed: %v", err)
	}
	defer store.Close()

	// Insert entry.
	meta := &FileMeta{
		Path:      "/tmp/to_delete.txt",
		Filename:  "to_delete.txt",
		Parent:    "/tmp",
		Extension: ".txt",
		Depth:     1,
	}
	if err := store.Put(meta); err != nil {
		t.Fatalf("Put() failed: %v", err)
	}

	// Delete the entry.
	if err := store.Delete("/tmp/to_delete.txt"); err != nil {
		t.Fatalf("Delete() failed: %v", err)
	}

	// Verify removed from LMDB.
	retrieved, err := store.Get("/tmp/to_delete.txt")
	if err != nil {
		t.Fatalf("Get() failed: %v", err)
	}
	if retrieved != nil {
		t.Error("entry still present in LMDB after delete")
	}

	// Verify removed from trie.
	trieResult := store.trie.Search("to_delete.txt")
	if _, ok := trieResult["/tmp/to_delete.txt"]; ok {
		t.Error("entry still present in trie after delete")
	}

	// Verify removed from trigram index.
	trigramResult := store.trigrams.Search("del")
	if trigramResult != nil {
		if _, ok := trigramResult["/tmp/to_delete.txt"]; ok {
			t.Error("entry still present in trigram index after delete")
		}
	}

	// Verify removed from metadata cache.
	if _, ok := store.metaCache["/tmp/to_delete.txt"]; ok {
		t.Error("entry still present in metadata cache after delete")
	}
}

// TestSchemaVersionMismatch verifies VAL-ENGINE-009: Schema version mismatch blocks startup.
func TestSchemaVersionMismatch(t *testing.T) {
	dir := tempDir(t)

	// Create a store normally.
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("Open() failed: %v", err)
	}

	// Tamper with the schema version to set it to 99.
	err = store.env.Update(func(txn *lmdb.Txn) error {
		return txn.Put(store.dbis.meta, []byte(metaKeySchemaVersion), []byte("99"), 0)
	})
	if err != nil {
		t.Fatalf("failed to tamper schema version: %v", err)
	}
	store.Close()

	// Attempt to reopen - should fail.
	_, err = Open(dir)
	if err == nil {
		t.Fatal("expected error for version mismatch, got nil")
	}

	errStr := err.Error()
	if !contains(errStr, "99") {
		t.Errorf("error message should contain version '99': %s", errStr)
	}
	if !contains(errStr, "1") && !contains(errStr, fmt.Sprintf("%d", SchemaVersion)) {
		t.Errorf("error message should contain supported version: %s", errStr)
	}
}

// TestConcurrentReadWrite verifies VAL-ENGINE-033: Concurrent read/write race-free.
func TestConcurrentReadWrite(t *testing.T) {
	dir := tempDir(t)

	store, err := Open(dir)
	if err != nil {
		t.Fatalf("Open() failed: %v", err)
	}
	defer store.Close()

	// Pre-populate with some entries.
	for i := 0; i < 10; i++ {
		path := fmt.Sprintf("/tmp/file_%d.txt", i)
		meta := &FileMeta{
			Path:      path,
			Filename:  fmt.Sprintf("file_%d.txt", i),
			Parent:    "/tmp",
			Extension: ".txt",
			Depth:     1,
		}
		if err := store.Put(meta); err != nil {
			t.Fatalf("Put() failed: %v", err)
		}
	}

	var wg sync.WaitGroup

	// Concurrent readers.
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				path := fmt.Sprintf("/tmp/file_%d.txt", j%10)
				_, _ = store.Get(path)
				_ = store.trie.Search(fmt.Sprintf("file_%d", id%10))
				_ = store.trigrams.Search(fmt.Sprintf("fil"))
				_ = store.Stats()
			}
		}(i)
	}

	// Concurrent writers.
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				path := fmt.Sprintf("/tmp/concurrent_%d_%d.txt", id, j)
				meta := &FileMeta{
					Path:      path,
					Filename:  fmt.Sprintf("concurrent_%d_%d.txt", id, j),
					Parent:    "/tmp",
					Extension: ".txt",
					Depth:     1,
				}
				_ = store.Put(meta)
			}
		}(i)
	}

	wg.Wait()
}

// TestPutAndGet verifies single entry insert and retrieval.
func TestPutAndGet(t *testing.T) {
	dir := tempDir(t)

	store, err := Open(dir)
	if err != nil {
		t.Fatalf("Open() failed: %v", err)
	}
	defer store.Close()

	meta := &FileMeta{
		Path:      "/home/user/document.pdf",
		Filename:  "document.pdf",
		Parent:    "/home/user",
		Extension: ".pdf",
		IsDir:     false,
		Modified:  1234567890,
		Depth:     2,
	}

	if err := store.Put(meta); err != nil {
		t.Fatalf("Put() failed: %v", err)
	}

	retrieved, err := store.Get("/home/user/document.pdf")
	if err != nil {
		t.Fatalf("Get() failed: %v", err)
	}
	if retrieved == nil {
		t.Fatal("entry not found")
	}
	if retrieved.Path != "/home/user/document.pdf" {
		t.Errorf("expected path /home/user/document.pdf, got %s", retrieved.Path)
	}
	if retrieved.Filename != "document.pdf" {
		t.Errorf("expected filename document.pdf, got %s", retrieved.Filename)
	}
	if retrieved.Extension != ".pdf" {
		t.Errorf("expected extension .pdf, got %s", retrieved.Extension)
	}
}

// TestDelete verifies single entry deletion.
func TestDelete(t *testing.T) {
	dir := tempDir(t)

	store, err := Open(dir)
	if err != nil {
		t.Fatalf("Open() failed: %v", err)
	}
	defer store.Close()

	meta := &FileMeta{
		Path:      "/tmp/delete_me.txt",
		Filename:  "delete_me.txt",
		Parent:    "/tmp",
		Extension: ".txt",
		Depth:     1,
	}
	if err := store.Put(meta); err != nil {
		t.Fatalf("Put() failed: %v", err)
	}

	if err := store.Delete("/tmp/delete_me.txt"); err != nil {
		t.Fatalf("Delete() failed: %v", err)
	}

	retrieved, err := store.Get("/tmp/delete_me.txt")
	if err != nil {
		t.Fatalf("Get() after delete failed: %v", err)
	}
	if retrieved != nil {
		t.Error("entry should be nil after delete")
	}
}

// TestMultipleEntries verifies that multiple entries with the same filename work correctly.
func TestMultipleEntries(t *testing.T) {
	dir := tempDir(t)

	store, err := Open(dir)
	if err != nil {
		t.Fatalf("Open() failed: %v", err)
	}
	defer store.Close()

	meta1 := &FileMeta{Path: "/a/readme.md", Filename: "readme.md", Parent: "/a", Extension: ".md", Depth: 1}
	meta2 := &FileMeta{Path: "/b/readme.md", Filename: "readme.md", Parent: "/b", Extension: ".md", Depth: 1}

	if err := store.Put(meta1); err != nil {
		t.Fatalf("Put() 1 failed: %v", err)
	}
	if err := store.Put(meta2); err != nil {
		t.Fatalf("Put() 2 failed: %v", err)
	}

	// Both should be findable via the trie.
	result := store.trie.Search("readme.md")
	if len(result) < 2 {
		t.Errorf("expected at least 2 results, got %d", len(result))
	}
	if _, ok := result["/a/readme.md"]; !ok {
		t.Error("expected /a/readme.md in results")
	}
	if _, ok := result["/b/readme.md"]; !ok {
		t.Error("expected /b/readme.md in results")
	}
}

// TestDirectoryEntry verifies directory entries are handled correctly.
func TestDirectoryEntry(t *testing.T) {
	dir := tempDir(t)

	store, err := Open(dir)
	if err != nil {
		t.Fatalf("Open() failed: %v", err)
	}
	defer store.Close()

	meta := &FileMeta{
		Path:     "/home/user/projects",
		Filename: "projects",
		Parent:   "/home/user",
		IsDir:    true,
		Depth:    2,
	}

	if err := store.Put(meta); err != nil {
		t.Fatalf("Put() failed: %v", err)
	}

	retrieved, err := store.Get("/home/user/projects")
	if err != nil {
		t.Fatalf("Get() failed: %v", err)
	}
	if retrieved == nil {
		t.Fatal("entry not found")
	}
	if !retrieved.IsDir {
		t.Error("expected IsDir=true for directory entry")
	}
}

// TestStats verifies the Stats method returns correct counts.
func TestStats(t *testing.T) {
	dir := tempDir(t)

	store, err := Open(dir)
	if err != nil {
		t.Fatalf("Open() failed: %v", err)
	}
	defer store.Close()

	for i := 0; i < 5; i++ {
		path := fmt.Sprintf("/tmp/file_%d.txt", i)
		meta := &FileMeta{
			Path:      path,
			Filename:  fmt.Sprintf("file_%d.txt", i),
			Parent:    "/tmp",
			Extension: ".txt",
			Depth:     1,
		}
		if err := store.Put(meta); err != nil {
			t.Fatalf("Put() failed: %v", err)
		}
	}

	stats := store.Stats()
	if stats.PathCount != 5 {
		t.Errorf("expected PathCount=5, got %d", stats.PathCount)
	}
	if stats.TrieNodes == 0 {
		t.Error("expected TrieNodes > 0")
	}
	if stats.TrigramCount == 0 {
		t.Error("expected TrigramCount > 0")
	}
}

// TestPersistAndRebuild verifies that entries persist across store close/reopen.
func TestPersistAndRebuild(t *testing.T) {
	dir := tempDir(t)

	// First instance.
	store1, err := Open(dir)
	if err != nil {
		t.Fatalf("first Open() failed: %v", err)
	}

	entries := []*FileMeta{
		{Path: "/a/alpha.txt", Filename: "alpha.txt", Parent: "/a", Extension: ".txt", Depth: 1},
		{Path: "/a/beta.txt", Filename: "beta.txt", Parent: "/a", Extension: ".txt", Depth: 1},
		{Path: "/b/gamma.txt", Filename: "gamma.txt", Parent: "/b", Extension: ".txt", Depth: 1},
		{Path: "/c/delta.dat", Filename: "delta.dat", Parent: "/c", Extension: ".dat", Depth: 1},
	}
	for _, e := range entries {
		if err := store1.Put(e); err != nil {
			t.Fatalf("Put() failed: %v", err)
		}
	}
	store1.Close()

	// Second instance - should rebuild from LMDB.
	store2, err := Open(dir)
	if err != nil {
		t.Fatalf("second Open() failed: %v", err)
	}
	defer store2.Close()

	// All entries should be retrievable.
	for _, e := range entries {
		retrieved, err := store2.Get(e.Path)
		if err != nil {
			t.Fatalf("Get(%s) failed: %v", e.Path, err)
		}
		if retrieved == nil {
			t.Errorf("entry %s not found after reopen", e.Path)
		}
		if retrieved.Filename != e.Filename {
			t.Errorf("expected filename %s, got %s", e.Filename, retrieved.Filename)
		}
	}

	// Trie should have all entries.
	stats := store2.Stats()
	if stats.PathCount != 4 {
		t.Errorf("expected PathCount=4 after reopen, got %d", stats.PathCount)
	}
}

// TestPutDuplicatePath verifies that re-Putting the same path updates rather than duplicates.
func TestPutDuplicatePath(t *testing.T) {
	dir := tempDir(t)

	store, err := Open(dir)
	if err != nil {
		t.Fatalf("Open() failed: %v", err)
	}
	defer store.Close()

	meta1 := &FileMeta{Path: "/tmp/test.txt", Filename: "test.txt", Parent: "/tmp", Extension: ".txt", Modified: 100, Depth: 1}
	meta2 := &FileMeta{Path: "/tmp/test.txt", Filename: "test.txt", Parent: "/tmp", Extension: ".txt", Modified: 200, Depth: 1}

	if err := store.Put(meta1); err != nil {
		t.Fatalf("first Put() failed: %v", err)
	}
	if err := store.Put(meta2); err != nil {
		t.Fatalf("second Put() failed: %v", err)
	}

	retrieved, err := store.Get("/tmp/test.txt")
	if err != nil {
		t.Fatalf("Get() failed: %v", err)
	}
	if retrieved == nil {
		t.Fatal("entry not found")
	}
	if retrieved.Modified != 200 {
		t.Errorf("expected Modified=200, got %d", retrieved.Modified)
	}

	// Should still have only one entry.
	stats := store.Stats()
	if stats.PathCount != 1 {
		t.Errorf("expected PathCount=1 after re-put, got %d", stats.PathCount)
	}
}

// TestTrigramSearch verifies the trigram index operations work correctly.
func TestTrigramSearch(t *testing.T) {
	ti := NewTrigramIndex()

	ti.Insert("document", "/path/to/document.pdf")
	ti.Insert("download", "/path/to/download.zip")

	// Search for "doc" trigrams: "doc", "ocu"
	result := ti.Search("doc")
	if result == nil {
		t.Error("expected results for trigram search 'doc'")
	}
	if _, ok := result["/path/to/document.pdf"]; !ok {
		t.Error("expected /path/to/document.pdf in results")
	}
	if _, ok := result["/path/to/download.zip"]; ok {
		t.Error("/path/to/download.zip should not be in 'doc' results")
	}

	// Search for "dow" trigrams: "dow", "own"
	result = ti.Search("dow")
	if result == nil {
		t.Error("expected results for trigram search 'dow'")
	}
	if _, ok := result["/path/to/download.zip"]; !ok {
		t.Error("expected /path/to/download.zip in 'dow' results")
	}
}

// TestTrigramDelete verifies trigram deletion works correctly.
func TestTrigramDelete(t *testing.T) {
	ti := NewTrigramIndex()

	ti.Insert("document", "/path/to/document.pdf")
	ti.Delete("document", "/path/to/document.pdf")

	result := ti.Search("doc")
	if result != nil {
		if _, ok := result["/path/to/document.pdf"]; ok {
			t.Error("entry should have been removed from trigram index")
		}
	}
}

// TestTrieDelete verifies trie deletion works correctly.
func TestTrieDelete(t *testing.T) {
	trie := NewPrefixTrie()

	trie.Insert("document.pdf", "/path/to/document.pdf")
	trie.Insert("download.zip", "/path/to/download.zip")

	result := trie.Search("document.pdf")
	if len(result) != 1 {
		t.Errorf("expected 1 result, got %d", len(result))
	}

	trie.Delete("document.pdf", "/path/to/document.pdf")

	result = trie.Search("document.pdf")
	if _, ok := result["/path/to/document.pdf"]; ok {
		t.Error("entry should have been removed from trie")
	}

	// The other entry should still be there.
	result = trie.Search("download.zip")
	if _, ok := result["/path/to/download.zip"]; !ok {
		t.Error("other entry should still be in trie")
	}
}

// TestExtractTrigrams verifies trigram extraction.
func TestExtractTrigrams(t *testing.T) {
	tests := []struct {
		input    string
		expected int
	}{
		{"hello", 3},            // hel, ell, llo
		{"ab", 0},               // too short
		{"abc", 1},              // abc
		{"document", 6},         // doc, ocu, cum, ume, men, ent
		{"aaa", 1},              // aaa (unique only)
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			trigrams := ExtractTrigrams(tt.input)
			if len(trigrams) != tt.expected {
				t.Errorf("ExtractTrigrams(%q) = %v (len=%d), want len=%d",
					tt.input, trigrams, len(trigrams), tt.expected)
			}
		})
	}
}

// TestTriePrefixSearch verifies prefix search in the trie.
func TestTriePrefixSearch(t *testing.T) {
	trie := NewPrefixTrie()

	trie.Insert("document.pdf", "/a/document.pdf")
	trie.Insert("download.zip", "/b/download.zip")
	trie.Insert("dockerfile", "/c/dockerfile")

	// Prefix "doc" should match document.pdf and dockerfile but not download.zip.
	result := trie.Search("doc")
	if result == nil {
		t.Fatal("expected results for prefix 'doc'")
	}
	if _, ok := result["/a/document.pdf"]; !ok {
		t.Error("expected /a/document.pdf in prefix results")
	}
	if _, ok := result["/c/dockerfile"]; !ok {
		t.Error("expected /c/dockerfile in prefix results")
	}
	if _, ok := result["/b/download.zip"]; ok {
		t.Error("download.zip should NOT be in 'doc' prefix results")
	}
}

// TestCloseIdempotent verifies that Close can be called multiple times safely.
func TestCloseIdempotent(t *testing.T) {
	dir := tempDir(t)

	store, err := Open(dir)
	if err != nil {
		t.Fatalf("Open() failed: %v", err)
	}

	if err := store.Close(); err != nil {
		t.Fatalf("first Close() failed: %v", err)
	}

	// Second close should not panic or error.
	if err := store.Close(); err != nil {
		t.Fatalf("second Close() failed: %v", err)
	}
}

func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// Ensure LMDB data files are cleaned up.
func TestMain(m *testing.M) {
	code := m.Run()
	// Clean up any leftover temp data directories.
	entries, _ := filepath.Glob(os.TempDir() + "/luminasearch-index-test-*")
	for _, e := range entries {
		os.RemoveAll(e)
	}
	os.Exit(code)
}
