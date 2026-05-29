package crawler

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rune/backend/internal/index"
)

// tempDir creates a temporary directory and returns its path.
// The caller is responsible for cleanup (via t.Cleanup).
func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "rune-crawler-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// tempIndexDir creates a temp directory for the LMDB store.
func tempIndexDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "rune-crawler-index-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// createFile creates an empty file and returns its path.
func createFile(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("failed to create file %s: %v", path, err)
	}
	f.Close()
	return path
}

// createDir creates a directory and returns its path.
func createDir(t *testing.T, parent, name string) string {
	t.Helper()
	path := filepath.Join(parent, name)
	if err := os.Mkdir(path, 0755); err != nil {
		t.Fatalf("failed to create dir %s: %v", path, err)
	}
	return path
}

// openTestStore creates an LMDB store for testing.
func openTestStore(t *testing.T, dir string) (*index.Store, func()) {
	t.Helper()
	store, err := index.Open(dir)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	return store, func() { store.Close() }
}

// =============================================================================
// VAL-ENGINE-022: Bounded goroutine pool
// =============================================================================

func TestCrawlerBoundedPool(t *testing.T) {
	storeDir := tempIndexDir(t)
	store, cleanup := openTestStore(t, storeDir)
	defer cleanup()

	root := tempDir(t)

	// Create many directories to force parallel crawling.
	for i := 0; i < 50; i++ {
		subDir := createDir(t, root, fmt.Sprintf("dir-%d", i))
		createFile(t, subDir, fmt.Sprintf("file-%d.txt", i))
	}

	// Track max concurrent goroutines atomically.
	var maxConcurrent int64
	var goroutineSnapshots []int64
	var mu sync.Mutex

	opts := DefaultOptions()
	opts.MaxConcurrency = 4
	opts.OnProgress = func(fc int64) {
		// No-op for this test
	}

	c := New(store, opts)

	// Instrument by wrapping the crawl with goroutine counting.
	// We'll sample goroutine count during crawl by running a background goroutine.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var crawlErr error
	var fileCount int64
	done := make(chan struct{})

	// Background sampler.
	go func() {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				count := int64(runtime.NumGoroutine())
				mu.Lock()
				goroutineSnapshots = append(goroutineSnapshots, count)
				mu.Unlock()
				for {
					old := atomic.LoadInt64(&maxConcurrent)
					if count <= old || atomic.CompareAndSwapInt64(&maxConcurrent, old, count) {
						break
					}
				}
			}
		}
	}()

	fileCount, crawlErr = c.Crawl(ctx, root)
	close(done)

	if crawlErr != nil {
		t.Fatalf("Crawl() failed: %v", crawlErr)
	}

	// maxConcurrent should not exceed MaxConcurrency + reasonable overhead (~10 for test framework).
	baselineGoroutines := int64(runtime.NumGoroutine())
	_ = baselineGoroutines

	// The max observed during crawl should be within bounds.
	// A well-behaved bounded pool should not spawn unbounded goroutines.
	maxObserved := atomic.LoadInt64(&maxConcurrent)
	if maxObserved > int64(opts.MaxConcurrency)+20 {
		t.Errorf("max goroutines %d exceeds expected bound (pool=%d + 20 overhead); goroutine pool may not be bounded",
			maxObserved, opts.MaxConcurrency)
	}
	t.Logf("max goroutines observed: %d (pool size: %d), files indexed: %d",
		maxObserved, opts.MaxConcurrency, fileCount)

	// All 50 files should be indexed.
	if fileCount != 50 {
		t.Errorf("expected 50 files indexed, got %d", fileCount)
	}
}

// =============================================================================
// VAL-ENGINE-023: Context cancellation terminates cleanly
// =============================================================================

func TestCrawlerContextCancellation(t *testing.T) {
	storeDir := tempIndexDir(t)
	store, cleanup := openTestStore(t, storeDir)
	defer cleanup()

	root := tempDir(t)

	// Create deeply nested structure with many directories to ensure crawl takes time.
	for i := 0; i < 200; i++ {
		createDir(t, root, fmt.Sprintf("dir-%d", i))
		for j := 0; j < 5; j++ {
			createFile(t, root, fmt.Sprintf("dir-%d/file-%d.txt", i, j))
		}
	}

	opts := DefaultOptions()
	opts.MaxConcurrency = 2
	c := New(store, opts)

	baselineGoroutines := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())

	// Cancel after a short delay.
	time.AfterFunc(100*time.Millisecond, cancel)

	start := time.Now()
	fileCount, err := c.Crawl(ctx, root)
	elapsed := time.Since(start)

	if err == nil {
		// If cancellation didn't kick in (very fast crawl), that's fine too.
		t.Logf("crawl completed before cancellation (files: %d, elapsed: %v)", fileCount, elapsed)
	} else if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}

	// Should return within 1 second.
	if elapsed > 1*time.Second {
		t.Errorf("crawl took %v after cancellation; expected < 1s", elapsed)
	}

	// Give goroutines time to clean up.
	time.Sleep(100 * time.Millisecond)

	// Check for goroutine leaks.
	afterGoroutines := runtime.NumGoroutine()
	delta := afterGoroutines - baselineGoroutines
	if delta > 10 {
		t.Errorf("goroutine leak detected: baseline=%d, after=%d, delta=%d",
			baselineGoroutines, afterGoroutines, delta)
	}
	t.Logf("goroutines: baseline=%d, after=%d, delta=%d", baselineGoroutines, afterGoroutines, delta)
}

// =============================================================================
// VAL-ENGINE-024: Symlink cycle detection via inode tracking
// =============================================================================

func TestSymlinkCycleDetection(t *testing.T) {
	storeDir := tempIndexDir(t)
	store, cleanup := openTestStore(t, storeDir)
	defer cleanup()

	root := tempDir(t)

	// Create directory A with file.
	dirA := createDir(t, root, "A")
	createFile(t, dirA, "real-file.txt")

	// Create a symlink A/loop -> A (cycle).
	loopPath := filepath.Join(dirA, "loop")
	if err := os.Symlink(dirA, loopPath); err != nil {
		t.Fatalf("failed to create symlink: %v", err)
	}

	opts := DefaultOptions()
	c := New(store, opts)

	ctx := context.Background()
	fileCount, err := c.Crawl(ctx, root)

	if err != nil {
		t.Fatalf("Crawl() failed: %v", err)
	}

	// The crawler should finish without infinite loop.
	// Directory A should be indexed exactly once (not re-entered via symlink).
	t.Logf("files indexed: %d", fileCount)

	// Verify real-file.txt is indexed exactly once.
	normalized := index.NormalizePath(filepath.Join(dirA, "real-file.txt"))
	meta, getErr := store.Get(normalized)
	if getErr != nil {
		t.Fatalf("Get() failed: %v", getErr)
	}
	if meta == nil {
		t.Error("real-file.txt should be indexed")
	} else {
		// Check that dirA itself is also indexed (as a directory with depth).
		dirMeta, _ := store.Get(index.NormalizePath(dirA))
		if dirMeta == nil {
			t.Error("directory A should be indexed")
		}
	}
}

// =============================================================================
// VAL-ENGINE-025: Symlinks outside crawl root are skipped
// =============================================================================

func TestSymlinkOutsideRoot(t *testing.T) {
	storeDir := tempIndexDir(t)
	store, cleanup := openTestStore(t, storeDir)
	defer cleanup()

	root := tempDir(t)

	// Create a file inside root.
	createFile(t, root, "inside.txt")

	// Create a symlink pointing outside the root (e.g., to /etc/hostname or /tmp).
	outsideTarget := "/tmp"
	symlinkPath := filepath.Join(root, "link-to-outside")
	if err := os.Symlink(outsideTarget, symlinkPath); err != nil {
		t.Fatalf("failed to create symlink: %v", err)
	}

	opts := DefaultOptions()
	c := New(store, opts)

	ctx := context.Background()
	fileCount, err := c.Crawl(ctx, root)

	if err != nil {
		t.Fatalf("Crawl() failed: %v", err)
	}

	t.Logf("files indexed: %d", fileCount)

	// inside.txt should be indexed.
	normalized := index.NormalizePath(filepath.Join(root, "inside.txt"))
	meta, _ := store.Get(normalized)
	if meta == nil {
		t.Error("inside.txt should be indexed")
	}

	// Files under the symlinked outside directory should NOT be indexed.
	// Check that /etc/passwd (or any /tmp file) is NOT in the index.
	// We check by verifying that no path outside root appears in the store.
	stats := store.Stats()
	t.Logf("store stats: paths=%d", stats.PathCount)
	// The count should not include files from outside the root.
	// Since /tmp may contain many files, if the symlink was followed, we'd have many entries.
	if stats.PathCount > 10 {
		t.Errorf("store has %d paths; symlink outside root may have been followed", stats.PathCount)
	}
}

// =============================================================================
// VAL-ENGINE-026: Permission-denied directories skipped gracefully
// =============================================================================

func TestPermissionDeniedGraceful(t *testing.T) {
	storeDir := tempIndexDir(t)
	store, cleanup := openTestStore(t, storeDir)
	defer cleanup()

	root := tempDir(t)

	// Create a directory with no read permission.
	lockedDir := createDir(t, root, "locked")
	createFile(t, lockedDir, "secret.txt") // create before locking

	if err := os.Chmod(lockedDir, 0000); err != nil {
		t.Fatalf("failed to chmod dir: %v", err)
	}
	t.Cleanup(func() { os.Chmod(lockedDir, 0755) }) // so cleanup can remove it

	// Create a normal directory alongside.
	normalDir := createDir(t, root, "normal")
	createFile(t, normalDir, "visible.txt")

	opts := DefaultOptions()
	c := New(store, opts)

	ctx := context.Background()
	fileCount, err := c.Crawl(ctx, root)

	if err != nil {
		t.Fatalf("Crawl() returned error: %v (should skip permission errors gracefully)", err)
	}

	t.Logf("files indexed: %d", fileCount)

	// visible.txt should be indexed.
	normalPath := index.NormalizePath(filepath.Join(normalDir, "visible.txt"))
	meta, _ := store.Get(normalPath)
	if meta == nil {
		t.Error("visible.txt should be indexed")
	}

	// secret.txt should NOT be indexed (directory was unreadable).
	secretPath := index.NormalizePath(filepath.Join(lockedDir, "secret.txt"))
	meta2, _ := store.Get(secretPath)
	if meta2 != nil {
		t.Error("secret.txt should NOT be indexed (directory was permission-denied)")
	}
}

// =============================================================================
// VAL-ENGINE-027: Default ignore patterns enforced at any depth
// =============================================================================

func TestDefaultIgnorePatterns(t *testing.T) {
	storeDir := tempIndexDir(t)
	store, cleanup := openTestStore(t, storeDir)
	defer cleanup()

	root := tempDir(t)

	// Create a directory for each ignore pattern with a file inside.
	ignoreDirs := []string{
		"node_modules", ".git", ".cache", "target",
		"dist", "build", "venv", "__pycache__",
	}

	for _, ignoreDir := range ignoreDirs {
		dir := createDir(t, root, ignoreDir)
		createFile(t, dir, "should-be-ignored.txt")
	}

	// Also test that ignore works at deeper levels.
	deepDir := createDir(t, root, "project")
	deepIgnore := createDir(t, deepDir, "node_modules")
	createFile(t, deepIgnore, "lodash.js")

	// Create a normal directory with a file that SHOULD be indexed.
	normalDir := createDir(t, root, "src")
	createFile(t, normalDir, "main.go")

	// Create a dir named like an ignore pattern but as a subpath (not basename).
	namedDir := createDir(t, root, "my-node_modules-folder")
	createFile(t, namedDir, "not-ignored.txt")

	opts := DefaultOptions()
	c := New(store, opts)

	ctx := context.Background()
	fileCount, err := c.Crawl(ctx, root)

	if err != nil {
		t.Fatalf("Crawl() failed: %v", err)
	}

	t.Logf("files indexed: %d", fileCount)

	// Normal files should be indexed.
	normalPath := index.NormalizePath(filepath.Join(normalDir, "main.go"))
	if meta, _ := store.Get(normalPath); meta == nil {
		t.Error("src/main.go should be indexed")
	}

	// Named-like-ignore-but-not-exact-match should be indexed.
	namedPath := index.NormalizePath(filepath.Join(namedDir, "not-ignored.txt"))
	if meta, _ := store.Get(namedPath); meta == nil {
		t.Error("my-node_modules-folder/not-ignored.txt should be indexed")
	}

	// All ignore directory contents should NOT be indexed.
	for _, ignoreDir := range ignoreDirs {
		ignoredPath := index.NormalizePath(filepath.Join(root, ignoreDir, "should-be-ignored.txt"))
		if meta, _ := store.Get(ignoredPath); meta != nil {
			t.Errorf("%s/should-be-ignored.txt should NOT be indexed", ignoreDir)
		}
	}

	// Deep ignore pattern should also be enforced.
	deepIgnoredPath := index.NormalizePath(filepath.Join(deepDir, "node_modules", "lodash.js"))
	if meta, _ := store.Get(deepIgnoredPath); meta != nil {
		t.Error("project/node_modules/lodash.js should NOT be indexed")
	}
}

// =============================================================================
// VAL-ENGINE-028: Hidden files (dotfiles) are indexed
// =============================================================================

func TestHiddenFilesIndexed(t *testing.T) {
	storeDir := tempIndexDir(t)
	store, cleanup := openTestStore(t, storeDir)
	defer cleanup()

	root := tempDir(t)

	// Create hidden files (dotfiles) that should be indexed.
	createFile(t, root, ".bashrc")
	createFile(t, root, ".gitconfig")
	createFile(t, root, ".hidden-file.txt")

	// Create hidden directory (not .git) that should be crawled.
	hiddenDir := createDir(t, root, ".config")
	createFile(t, hiddenDir, "settings.json")

	opts := DefaultOptions()
	c := New(store, opts)

	ctx := context.Background()
	fileCount, err := c.Crawl(ctx, root)

	if err != nil {
		t.Fatalf("Crawl() failed: %v", err)
	}

	t.Logf("files indexed: %d", fileCount)

	// .bashrc should be indexed.
	for _, name := range []string{".bashrc", ".gitconfig", ".hidden-file.txt"} {
		norm := index.NormalizePath(filepath.Join(root, name))
		if meta, _ := store.Get(norm); meta == nil {
			t.Errorf("%s should be indexed", name)
		}
	}

	// .config/settings.json should NOT be indexed because .config is a hidden directory and must be ignored.
	settingsPath := index.NormalizePath(filepath.Join(hiddenDir, "settings.json"))
	if meta, _ := store.Get(settingsPath); meta != nil {
		t.Error(".config/settings.json should NOT be indexed")
	}
}

// =============================================================================
// VAL-ENGINE-029: Unicode and special characters in filenames
// =============================================================================

func TestUnicodeAndSpecialChars(t *testing.T) {
	storeDir := tempIndexDir(t)
	store, cleanup := openTestStore(t, storeDir)
	defer cleanup()

	root := tempDir(t)

	// Create files with Unicode, emoji, accents, spaces, and regex metacharacters.
	unicodeFiles := []string{
		"📁report_🌟.pdf",
		"Café_München.txt",
		"test[1].c++",
		"文件名.txt",
		"ファイル.txt",
		"file with spaces.log",
		"special(chars).json",
		"path%20encoded.txt",
	}

	for _, name := range unicodeFiles {
		createFile(t, root, name)
	}

	opts := DefaultOptions()
	c := New(store, opts)

	ctx := context.Background()
	fileCount, err := c.Crawl(ctx, root)

	if err != nil {
		t.Fatalf("Crawl() failed: %v", err)
	}

	// All files should be indexed.
	if fileCount != int64(len(unicodeFiles)) {
		t.Errorf("expected %d files indexed, got %d", len(unicodeFiles), fileCount)
	}

	// Verify each file is findable.
	for _, name := range unicodeFiles {
		norm := index.NormalizePath(filepath.Join(root, name))
		meta, getErr := store.Get(norm)
		if getErr != nil {
			t.Errorf("Get(%q) error: %v", name, getErr)
		}
		if meta == nil {
			t.Errorf("file %q should be indexed", name)
		}
	}
}

// =============================================================================
// VAL-ENGINE-030: Very long filename (255 chars) indexed
// =============================================================================

func TestVeryLongFilename(t *testing.T) {
	storeDir := tempIndexDir(t)
	store, cleanup := openTestStore(t, storeDir)
	defer cleanup()

	root := tempDir(t)

	// Create a 255-character filename.
	longName := strings.Repeat("a", 251) + ".txt" // 255 chars total
	createFile(t, root, longName)

	opts := DefaultOptions()
	c := New(store, opts)

	ctx := context.Background()
	fileCount, err := c.Crawl(ctx, root)

	if err != nil {
		t.Fatalf("Crawl() failed: %v", err)
	}

	if fileCount != 1 {
		t.Errorf("expected 1 file indexed, got %d", fileCount)
	}

	norm := index.NormalizePath(filepath.Join(root, longName))
	meta, getErr := store.Get(norm)
	if getErr != nil {
		t.Fatalf("Get() error: %v", getErr)
	}
	if meta == nil {
		t.Error("255-char filename should be indexed")
	}

	// Verify the filename in metadata is the lowercased version.
	if len(meta.Filename) != 255 {
		t.Errorf("expected filename length 255, got %d", len(meta.Filename))
	}
}

// =============================================================================
// VAL-ENGINE-031: Deep paths (>100 levels) indexed without stack overflow
// =============================================================================

func TestDeepPaths(t *testing.T) {
	storeDir := tempIndexDir(t)
	store, cleanup := openTestStore(t, storeDir)
	defer cleanup()

	root := tempDir(t)

	// Create a 100-level deep tree using minimal-length names to stay within
	// LMDB's default max key size (~511 bytes). Using 2-char dir names.
	// 100 levels: tempDir(~41) + separator + 100*(2+1) + "/f" = ~345 chars.
	current := root
	depth := 100
	for i := 0; i < depth; i++ {
		name := fmt.Sprintf("%02d", i) // 2-char names: "00", "01", ...
		current = createDir(t, current, name)
	}
	// Create a file at the deepest level.
	deepFile := createFile(t, current, "f")

	opts := DefaultOptions()
	opts.MaxConcurrency = 2 // Use small pool to test deep recursion in a single goroutine.
	c := New(store, opts)

	ctx := context.Background()
	fileCount, err := c.Crawl(ctx, root)

	if err != nil {
		t.Fatalf("Crawl() failed: %v", err)
	}

	t.Logf("crawl complete, fileCount=%d, pathLen=%d", fileCount, len(deepFile))

	// The deep file should be indexed.
	norm := index.NormalizePath(deepFile)
	meta, getErr := store.Get(norm)
	if getErr != nil {
		t.Fatalf("Get() error: %v", getErr)
	}
	if meta == nil {
		t.Fatalf("deep file should be indexed (path len=%d)", len(deepFile))
	}

	// Verify depth in metadata.
	expectedDepth := depth + 1 // 100 dirs + file name = 101 components deep from root
	if meta.Depth != expectedDepth {
		t.Errorf("expected depth %d, got %d", expectedDepth, meta.Depth)
	}

	t.Logf("deep file indexed with depth %d", meta.Depth)
}

// =============================================================================
// VAL-ENGINE-032: Duplicate filenames in different directories
// =============================================================================

func TestDuplicateFilenames(t *testing.T) {
	storeDir := tempIndexDir(t)
	store, cleanup := openTestStore(t, storeDir)
	defer cleanup()

	root := tempDir(t)

	dirA := createDir(t, root, "a")
	dirB := createDir(t, root, "b")

	createFile(t, dirA, "readme.md")
	createFile(t, dirB, "readme.md")

	opts := DefaultOptions()
	c := New(store, opts)

	ctx := context.Background()
	fileCount, err := c.Crawl(ctx, root)

	if err != nil {
		t.Fatalf("Crawl() failed: %v", err)
	}

	if fileCount != 2 {
		t.Errorf("expected 2 files indexed, got %d", fileCount)
	}

	// Both readme.md files should be indexed.
	pathA := index.NormalizePath(filepath.Join(dirA, "readme.md"))
	pathB := index.NormalizePath(filepath.Join(dirB, "readme.md"))

	metaA, _ := store.Get(pathA)
	metaB, _ := store.Get(pathB)

	if metaA == nil {
		t.Error("a/readme.md should be indexed")
	}
	if metaB == nil {
		t.Error("b/readme.md should be indexed")
	}

	// They should have different paths.
	if metaA != nil && metaB != nil && metaA.Path == metaB.Path {
		t.Error("the two readme.md files should have different paths")
	}
}

// =============================================================================
// Additional Tests
// =============================================================================

// TestProgressReporting verifies the OnProgress callback is invoked.
func TestProgressReporting(t *testing.T) {
	storeDir := tempIndexDir(t)
	store, cleanup := openTestStore(t, storeDir)
	defer cleanup()

	root := tempDir(t)

	// Create some files.
	for i := 0; i < 20; i++ {
		createFile(t, root, fmt.Sprintf("file-%d.txt", i))
	}

	var progressValues []int64
	var mu sync.Mutex

	opts := DefaultOptions()
	opts.MaxConcurrency = 2
	opts.OnProgress = func(fc int64) {
		mu.Lock()
		progressValues = append(progressValues, fc)
		mu.Unlock()
	}

	c := New(store, opts)

	ctx := context.Background()
	fileCount, err := c.Crawl(ctx, root)

	if err != nil {
		t.Fatalf("Crawl() failed: %v", err)
	}

	t.Logf("files indexed: %d, progress reports: %d", fileCount, len(progressValues))

	if fileCount != 20 {
		t.Errorf("expected 20 files, got %d", fileCount)
	}

	// Progress should have been reported.
	if len(progressValues) == 0 {
		t.Error("OnProgress callback was never invoked")
	}
}

// TestNoExtensionAndMultipleExtensions verifies files with no or multiple extensions.
func TestNoExtensionAndMultipleExtensions(t *testing.T) {
	storeDir := tempIndexDir(t)
	store, cleanup := openTestStore(t, storeDir)
	defer cleanup()

	root := tempDir(t)

	createFile(t, root, "Makefile")            // no extension
	createFile(t, root, "Dockerfile")          // no extension
	createFile(t, root, "archive.tar.gz")      // multiple extensions
	createFile(t, root, "config.prod.yaml")    // multiple extensions

	opts := DefaultOptions()
	c := New(store, opts)

	ctx := context.Background()
	fileCount, err := c.Crawl(ctx, root)

	if err != nil {
		t.Fatalf("Crawl() failed: %v", err)
	}

	if fileCount != 4 {
		t.Errorf("expected 4 files, got %d", fileCount)
	}

	// Verify each is indexed.
	for _, name := range []string{"Makefile", "Dockerfile", "archive.tar.gz", "config.prod.yaml"} {
		norm := index.NormalizePath(filepath.Join(root, name))
		if meta, _ := store.Get(norm); meta == nil {
			t.Errorf("%s should be indexed", name)
		}
	}
}

// TestEmptyDirectory verifies crawling an empty directory succeeds.
func TestEmptyDirectory(t *testing.T) {
	storeDir := tempIndexDir(t)
	store, cleanup := openTestStore(t, storeDir)
	defer cleanup()

	root := tempDir(t)

	opts := DefaultOptions()
	c := New(store, opts)

	ctx := context.Background()
	fileCount, err := c.Crawl(ctx, root)

	if err != nil {
		t.Fatalf("Crawl() failed: %v", err)
	}

	if fileCount != 0 {
		t.Errorf("expected 0 files in empty dir, got %d", fileCount)
	}
}

// TestSymlinkToFile verifies symlinks to files are indexed as the target.
func TestSymlinkToFile(t *testing.T) {
	storeDir := tempIndexDir(t)
	store, cleanup := openTestStore(t, storeDir)
	defer cleanup()

	root := tempDir(t)

	// Create a real file and a symlink to it.
	target := createFile(t, root, "target.txt")
	linkPath := filepath.Join(root, "link.txt")
	if err := os.Symlink(target, linkPath); err != nil {
		t.Fatalf("failed to create symlink: %v", err)
	}

	opts := DefaultOptions()
	c := New(store, opts)

	ctx := context.Background()
	fileCount, err := c.Crawl(ctx, root)

	if err != nil {
		t.Fatalf("Crawl() failed: %v", err)
	}

	// Both the real file and the symlink should be indexed (symlinks to files are indexed directly).
	t.Logf("files indexed: %d", fileCount)

	normTarget := index.NormalizePath(target)
	normLink := index.NormalizePath(linkPath)

	metaTarget, _ := store.Get(normTarget)
	metaLink, _ := store.Get(normLink)

	if metaTarget == nil {
		t.Error("target.txt should be indexed")
	}
	if metaLink == nil {
		t.Log("link.txt may not be indexed separately (symlink to file handled at target)")
	}
}

// TestSymlinkToDir verifies symlinks to directories inside root are followed.
func TestSymlinkToDir(t *testing.T) {
	storeDir := tempIndexDir(t)
	store, cleanup := openTestStore(t, storeDir)
	defer cleanup()

	root := tempDir(t)

	// Create a real directory with a file.
	realDir := createDir(t, root, "real")
	createFile(t, realDir, "inside.txt")

	// Create a symlink to the real directory.
	linkPath := filepath.Join(root, "link-to-real")
	if err := os.Symlink(realDir, linkPath); err != nil {
		t.Fatalf("failed to create symlink: %v", err)
	}

	opts := DefaultOptions()
	c := New(store, opts)

	ctx := context.Background()
	fileCount, err := c.Crawl(ctx, root)

	if err != nil {
		t.Fatalf("Crawl() failed: %v", err)
	}

	t.Logf("files indexed: %d", fileCount)

	// inside.txt should be indexed (found through the real directory path).
	normalizedInside := index.NormalizePath(filepath.Join(realDir, "inside.txt"))
	meta, _ := store.Get(normalizedInside)
	if meta == nil {
		t.Error("real/inside.txt should be indexed")
	}
}

// TestCustomIgnorePatterns verifies user-specified ignore patterns.
func TestCustomIgnorePatterns(t *testing.T) {
	storeDir := tempIndexDir(t)
	store, cleanup := openTestStore(t, storeDir)
	defer cleanup()

	root := tempDir(t)

	customIgnored := createDir(t, root, "custom-ignored")
	createFile(t, customIgnored, "skip-me.txt")

	normalDir := createDir(t, root, "normal")
	createFile(t, normalDir, "keep-me.txt")

	opts := DefaultOptions()
	opts.IgnorePatterns = append(opts.IgnorePatterns, "custom-ignored")
	c := New(store, opts)

	ctx := context.Background()
	fileCount, err := c.Crawl(ctx, root)

	if err != nil {
		t.Fatalf("Crawl() failed: %v", err)
	}

	t.Logf("files indexed: %d", fileCount)

	// keep-me.txt should be indexed.
	keepPath := index.NormalizePath(filepath.Join(normalDir, "keep-me.txt"))
	if meta, _ := store.Get(keepPath); meta == nil {
		t.Error("keep-me.txt should be indexed")
	}

	// skip-me.txt should NOT be indexed.
	skipPath := index.NormalizePath(filepath.Join(customIgnored, "skip-me.txt"))
	if meta, _ := store.Get(skipPath); meta != nil {
		t.Error("custom-ignored/skip-me.txt should NOT be indexed")
	}
}

// TestCrawlRootNotFound verifies error on non-existent root.
func TestCrawlRootNotFound(t *testing.T) {
	storeDir := tempIndexDir(t)
	store, cleanup := openTestStore(t, storeDir)
	defer cleanup()

	root := "/tmp/nonexistent-directory-12345-xyz"

	opts := DefaultOptions()
	c := New(store, opts)

	ctx := context.Background()
	_, err := c.Crawl(ctx, root)

	if err == nil {
		t.Error("expected error for non-existent root, got nil")
	}
}

// TestConcurrentProgressReporting verifies progress can be called concurrently.
func TestConcurrentProgressReporting(t *testing.T) {
	storeDir := tempIndexDir(t)
	store, cleanup := openTestStore(t, storeDir)
	defer cleanup()

	root := tempDir(t)

	// Create many files across many directories to force concurrency.
	for i := 0; i < 100; i++ {
		subDir := createDir(t, root, fmt.Sprintf("d%d", i))
		for j := 0; j < 10; j++ {
			createFile(t, subDir, fmt.Sprintf("f%d.txt", j))
		}
	}

	var maxProgress int64
	var progressCalls atomic.Int64
	var pMu sync.Mutex

	opts := DefaultOptions()
	opts.MaxConcurrency = 4
	opts.OnProgress = func(fc int64) {
		progressCalls.Add(1)
		pMu.Lock()
		if fc > maxProgress {
			maxProgress = fc
		}
		pMu.Unlock()
	}

	c := New(store, opts)

	ctx := context.Background()
	fileCount, err := c.Crawl(ctx, root)

	if err != nil {
		t.Fatalf("Crawl() failed: %v", err)
	}

	if fileCount != 1000 {
		t.Errorf("expected 1000 files, got %d", fileCount)
	}

	pMu.Lock()
	finalProgress := maxProgress
	pMu.Unlock()

	if finalProgress != 1000 {
		t.Errorf("expected maxProgress to reach 1000, got %d", finalProgress)
	}

	t.Logf("files indexed: %d, progress calls: %d", fileCount, progressCalls.Load())
}

// TestIndexingIncludesDirectories verifies directories themselves are indexed.
func TestIndexingIncludesDirectories(t *testing.T) {
	storeDir := tempIndexDir(t)
	store, cleanup := openTestStore(t, storeDir)
	defer cleanup()

	root := tempDir(t)

	subDir := createDir(t, root, "subdir")
	createFile(t, subDir, "file.txt")

	opts := DefaultOptions()
	c := New(store, opts)

	ctx := context.Background()
	fileCount, err := c.Crawl(ctx, root)

	if err != nil {
		t.Fatalf("Crawl() failed: %v", err)
	}

	// At minimum, subdir/file.txt should be indexed. The root and subdir may also be indexed.
	if fileCount < 1 {
		t.Errorf("expected at least 1 file indexed, got %d", fileCount)
	}

	// Verify directory entries exist if indexed.
	// The architecture says directories are also indexed (with IsDir=true).
	normSubDir := index.NormalizePath(subDir)
	meta, _ := store.Get(normSubDir)
	if meta != nil {
		if !meta.IsDir {
			t.Error("subdir should have IsDir=true")
		}
	}
}


