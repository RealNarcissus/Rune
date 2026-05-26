package crawler

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/lumina-search/backend/internal/index"
)

// DefaultIgnorePatterns are directory basenames that are never entered
// during crawling. These match exactly (case-sensitive) at any depth.
var DefaultIgnorePatterns = []string{
	"node_modules", ".git", ".cache", "target",
	"dist", "build", "venv", "__pycache__",
}

// Options configures the directory crawler behavior.
type Options struct {
	// MaxConcurrency is the maximum number of directories processed
	// concurrently. Must be >= 1. Defaults to 8 if zero.
	MaxConcurrency int

	// IgnorePatterns is the list of directory basenames to skip.
	// Matching is exact and case-sensitive. Defaults to DefaultIgnorePatterns
	// if nil.
	IgnorePatterns []string

	// OnProgress is called periodically with the current number of files
	// indexed so far. It may be called from multiple goroutines concurrently;
	// the implementation must be safe for concurrent use. May be nil.
	OnProgress func(fileCount int64)
}

// DefaultOptions returns sensible default configuration.
func DefaultOptions() Options {
	return Options{
		MaxConcurrency: 8,
		IgnorePatterns: DefaultIgnorePatterns,
	}
}

// Crawler walks directory trees using a bounded goroutine pool and indexes
// all discovered files into the provided index.Store.
type Crawler struct {
	store     *index.Store
	opts      Options
	ignoreMap map[string]bool // O(1) lookup for ignore patterns
}

// New creates a new Crawler that writes discovered entries to store.
// If opts.MaxConcurrency <= 0, defaults to 8.
// If opts.IgnorePatterns is nil, defaults to DefaultIgnorePatterns.
func New(store *index.Store, opts Options) *Crawler {
	if opts.MaxConcurrency <= 0 {
		opts.MaxConcurrency = 8
	}
	if opts.IgnorePatterns == nil {
		opts.IgnorePatterns = DefaultIgnorePatterns
	}

	ignoreMap := make(map[string]bool, len(opts.IgnorePatterns))
	for _, p := range opts.IgnorePatterns {
		ignoreMap[p] = true
	}

	return &Crawler{
		store:     store,
		opts:      opts,
		ignoreMap: ignoreMap,
	}
}

// inodeKey uniquely identifies a filesystem object by (device, inode).
// Used for symlink cycle detection.
type inodeKey struct {
	dev uint64
	ino uint64
}

// Crawl walks the directory tree rooted at root and indexes all files.
// It returns the total number of files indexed.
//
// Crawl respects context cancellation: when ctx is cancelled, Crawl stops
// spawning new work and returns as soon as in-flight goroutines finish their
// current directory.
func (c *Crawler) Crawl(ctx context.Context, root string) (int64, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return 0, fmt.Errorf("crawler: resolve root: %w", err)
	}
	absRoot = filepath.Clean(absRoot)

	var (
		fileCount int64
		visited   sync.Map // inodeKey → struct{}
		wg        sync.WaitGroup
		mu        sync.Mutex
		firstErr  error
	)

	// Bounded goroutine pool via semaphore channel.
	sem := make(chan struct{}, c.opts.MaxConcurrency)

	// recordErr stores the first non-nil error encountered.
	recordErr := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		mu.Unlock()
	}

	// shouldIgnore reports whether a directory basename matches any
	// of the configured ignore patterns.
	shouldIgnore := func(name string) bool {
		return c.ignoreMap[name]
	}

	// crawlDir recursively processes a directory. It must be called while
	// holding a semaphore slot. It reads the directory contents and
	// dispatches subdirectories to new goroutines (each acquiring their
	// own semaphore slot). The recursive structure does not cause stack
	// overflow because each level is a separate goroutine.
	var crawlDir func(dir string)
	crawlDir = func(dir string) {
		// Check context before any significant work.
		if ctx.Err() != nil {
			return
		}

		// Stat the directory, following symlinks to get the real
		// underlying inode for cycle detection.
		info, err := os.Stat(dir)
		if err != nil {
			if os.IsPermission(err) {
				return // permission denied — skip gracefully
			}
			recordErr(err)
			return
		}

		if !info.IsDir() {
			return // not a directory (shouldn't happen for crawlDir calls)
		}

		// Cycle detection: track visited directories by (device, inode).
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			key := inodeKey{dev: stat.Dev, ino: stat.Ino}
			if _, loaded := visited.LoadOrStore(key, struct{}{}); loaded {
				return // cycle detected — already visited this inode
			}
		}

		// Index the directory itself so it appears in search results.
		// Note: directory entries are NOT counted in the file count
		// (fileCount tracks only non-directory entries).
		c.indexEntry(absRoot, dir, info, nil)

		// Read directory contents.
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsPermission(err) {
				return // permission denied
			}
			recordErr(err)
			return
		}

		for _, entry := range entries {
			// Check context frequently for prompt cancellation.
			if ctx.Err() != nil {
				return
			}

			name := entry.Name()
			fullPath := filepath.Join(dir, name)

			// Handle symlinks specially.
			if entry.Type()&os.ModeSymlink != 0 {
				c.processSymlink(ctx, dir, name, fullPath, absRoot, &visited,
					&fileCount, sem, &wg, crawlDir)
				continue
			}

			if entry.IsDir() {
				// Check ignore patterns against the directory basename.
				if shouldIgnore(name) {
					continue
				}

				// Dispatch subdirectory to a new goroutine (bounded by semaphore).
				// Use a non-blocking check with semaphore acquisition via select.
				c.dispatchDir(ctx, fullPath, sem, &wg, crawlDir)
			} else {
				// Regular file or non-symlink special: index it.
				info, err := entry.Info()
				if err != nil {
					continue // skip entries we can't stat
				}
				c.indexEntry(absRoot, fullPath, info, &fileCount)
			}
		}
	}

	// Start crawling from root. Acquire the first semaphore slot.
	select {
	case <-ctx.Done():
		return fileCount, ctx.Err()
	case sem <- struct{}{}:
		wg.Add(1)
		go func() {
			defer func() {
				<-sem // release root's slot
				wg.Done()
			}()
			crawlDir(absRoot)
		}()
	}

	// Wait for all goroutines to finish.
	wg.Wait()

	if firstErr != nil {
		return fileCount, firstErr
	}
	return fileCount, nil
}

// dispatchDir sends a directory path to be crawled in a new goroutine,
// bounded by the semaphore. It respects context cancellation.
func (c *Crawler) dispatchDir(
	ctx context.Context,
	dirPath string,
	sem chan struct{},
	wg *sync.WaitGroup,
	crawlDir func(string),
) {
	select {
	case <-ctx.Done():
		return
	case sem <- struct{}{}:
		wg.Add(1)
		go func(p string) {
			defer func() {
				<-sem
				wg.Done()
			}()
			crawlDir(p)
		}(dirPath)
	}
}

// processSymlink handles a single symlink entry. If the symlink target
// resolves within the crawl root, and it's a directory, the target is
// crawled. If it's a file, it is indexed directly. Symlinks outside the
// root are skipped.
func (c *Crawler) processSymlink(
	ctx context.Context,
	parentDir, name, symlinkPath, absRoot string,
	visited *sync.Map,
	fileCount *int64,
	sem chan struct{},
	wg *sync.WaitGroup,
	crawlDir func(string),
) {
	// Read the symlink target.
	target, err := os.Readlink(symlinkPath)
	if err != nil {
		return // broken or inaccessible symlink — skip
	}

	// Resolve the target to an absolute path.
	resolved := target
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(parentDir, resolved)
	}
	resolved = filepath.Clean(resolved)

	// Determine if the resolved target is within the crawl root.
	if !isWithinRoot(resolved, absRoot) {
		return // symlink outside root — skip
	}

	// Stat the resolved target (follows any further symlinks).
	targetInfo, err := os.Stat(resolved)
	if err != nil {
		return // target inaccessible — skip
	}

	if targetInfo.IsDir() {
		// Symlink to a directory inside root. Check cycle detection
		// using the target's real (device, inode). Only check, don't insert:
		// crawlDir will insert when it processes the directory.
		if stat, ok := targetInfo.Sys().(*syscall.Stat_t); ok {
			key := inodeKey{dev: stat.Dev, ino: stat.Ino}
			if _, loaded := visited.Load(key); loaded {
				return // cycle detected via symlink
			}
		}

		// Dispatch crawl of the resolved directory target.
		c.dispatchDir(ctx, resolved, sem, wg, crawlDir)
	} else {
		// Symlink to a file: index it at the symlink's own path.
		c.indexEntry(absRoot, symlinkPath, targetInfo, fileCount)
	}
}

// isWithinRoot reports whether the given path is within the crawl root.
// A path is within root if it equals root exactly or has root as a prefix
// followed by a path separator.
func isWithinRoot(path, root string) bool {
	if path == root {
		return true
	}
	return strings.HasPrefix(path, root+string(os.PathSeparator))
}

// indexEntry creates a FileMeta entry from file info and writes it to the
// store. If fileCount is non-nil, the count is atomically incremented and the
// OnProgress callback is invoked.
func (c *Crawler) indexEntry(root, fullPath string, info os.FileInfo, fileCount *int64) {
	// Determine relative path for depth calculation.
	relPath := fullPath
	if isWithinRoot(fullPath, root) && root != fullPath {
		relPath = strings.TrimPrefix(fullPath, root+string(os.PathSeparator))
	}

	// Depth is the number of path separators from root to this entry.
	depth := 0
	if relPath != root {
		depth = strings.Count(relPath, string(os.PathSeparator)) + 1
	}

	// Extract extension from the filename.
	ext := filepath.Ext(info.Name())

	// Determine parent directory.
	parent := filepath.Dir(fullPath)

	meta := &index.FileMeta{
		Path:      fullPath,
		Filename:  info.Name(),
		Parent:    parent,
		Extension: ext,
		IsDir:     info.IsDir(),
		Modified:  info.ModTime().Unix(),
		Depth:     depth,
	}

	// Write to store (normalization and lowercasing happen inside Put).
	if err := c.store.Put(meta); err != nil {
		return // skip entries that fail to store
	}

	// Only count non-directory entries in the file count.
	if fileCount != nil {
		newCount := atomic.AddInt64(fileCount, 1)
		if c.opts.OnProgress != nil {
			c.opts.OnProgress(newCount)
		}
	}
}
