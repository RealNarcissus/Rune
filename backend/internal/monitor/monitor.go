package monitor

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/fsnotify/fsnotify"
	"github.com/rune/backend/internal/index"
)

// State represents the watcher's current lifecycle stage.
type State int

const (
	StateQueueing State = iota
	StateSteady
)

// DefaultIgnorePatterns matches directories we want to ignore.
var DefaultIgnorePatterns = []string{
	"node_modules", ".git", ".cache", "target",
	"dist", "build", "venv", "__pycache__",
}

// Watcher monitors a directory tree for real-time filesystem events,
// applying them to the index store while maintaining consistency.
type Watcher struct {
	store             *index.Store
	root              string
	fsWatcher         *fsnotify.Watcher
	ignorePatterns    []string
	ignoreMap         map[string]bool
	
	state             State
	eventQueue        []fsnotify.Event
	watchLimitReached bool
	reconciling       bool
	
	mu                sync.Mutex
	processingMu      sync.Mutex // Serializes all index mutations to avoid races and database contention

	ctx               context.Context
	cancel            context.CancelFunc
	wg                sync.WaitGroup
}

// NewWatcher creates a new Watcher instance for the given root directory.
func NewWatcher(store *index.Store, root string, ignorePatterns []string) (*Watcher, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("monitor: resolve absolute root: %w", err)
	}
	absRoot = filepath.Clean(absRoot)

	if ignorePatterns == nil {
		ignorePatterns = DefaultIgnorePatterns
	}

	ignoreMap := make(map[string]bool, len(ignorePatterns))
	for _, p := range ignorePatterns {
		ignoreMap[p] = true
	}

	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("monitor: create fsnotify watcher: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	return &Watcher{
		store:          store,
		root:           absRoot,
		fsWatcher:      fsw,
		ignorePatterns: ignorePatterns,
		ignoreMap:      ignoreMap,
		state:          StateQueueing,
		eventQueue:     make([]fsnotify.Event, 0, 1000),
		ctx:            ctx,
		cancel:         cancel,
	}, nil
}

// Start initiates the directory watching event loop and recursively
// registers watches for all subdirectories under root *before* crawl starts.
func (w *Watcher) Start() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	// 1. Start the event listening loops in background.
	w.wg.Add(2)
	go w.listenEvents()
	go w.listenErrors()

	// 2. Perform initial recursive watch registration.
	// Since this runs before the crawl begins, we construct the watch list.
	log.Printf("monitor: starting recursive watch registration from root: %s", w.root)
	if err := w.registerWatchRecursive(w.root); err != nil {
		log.Printf("monitor: warning during watch registration: %v", err)
	}

	return nil
}

// Replay transitions the watcher to steady-state and processes
// all accumulated queue events in strict chronological order.
func (w *Watcher) Replay() {
	w.processingMu.Lock()
	defer w.processingMu.Unlock()

	w.mu.Lock()
	if w.state == StateSteady {
		w.mu.Unlock()
		return
	}
	
	log.Printf("monitor: transitioning to steady-state. Replaying %d queued filesystem events...", len(w.eventQueue))
	
	// Copy and clear the queue
	queued := make([]fsnotify.Event, len(w.eventQueue))
	copy(queued, w.eventQueue)
	w.eventQueue = nil
	w.state = StateSteady
	w.mu.Unlock()

	// Replay events
	for _, ev := range queued {
		w.handleEvent(ev)
	}
	log.Printf("monitor: replay completed successfully. Now in steady-state real-time monitoring.")
}

// Close gracefully stops the watcher, releasing all registered watches.
func (w *Watcher) Close() error {
	w.cancel()
	err := w.fsWatcher.Close()
	w.wg.Wait()
	return err
}

// listenEvents processes events coming from fsnotify.
func (w *Watcher) listenEvents() {
	defer w.wg.Done()

	for {
		select {
		case <-w.ctx.Done():
			return
		case ev, ok := <-w.fsWatcher.Events:
			if !ok {
				return
			}

			w.mu.Lock()
			if w.state == StateQueueing {
				// Queue mutations during crawling phase to prevent races
				w.eventQueue = append(w.eventQueue, ev)
				w.mu.Unlock()
			} else {
				w.mu.Unlock()
				// Steady state mode: process immediately under processing lock
				w.processingMu.Lock()
				w.handleEvent(ev)
				w.processingMu.Unlock()
			}
		}
	}
}

// listenErrors monitors fsnotify errors, such as inotify queue overflow.
func (w *Watcher) listenErrors() {
	defer w.wg.Done()

	for {
		select {
		case <-w.ctx.Done():
			return
		case err, ok := <-w.fsWatcher.Errors:
			if !ok {
				return
			}
			log.Printf("monitor: fsnotify error received: %v", err)
			
			// Detect queue overflow or general failure to process events
			if errors.Is(err, fsnotify.ErrEventOverflow) || strings.Contains(err.Error(), "overflow") {
				log.Println("monitor: inotify queue overflow detected. Scheduling background reconciliation rescan.")
				w.TriggerReconciliation()
			}
		}
	}
}

// shouldIgnore reports whether a directory matches our ignore patterns.
func (w *Watcher) shouldIgnore(path string) bool {
	if path == w.root {
		return false
	}
	base := filepath.Base(path)
	if w.ignoreMap[base] {
		return true
	}
	
	// Also ensure we don't watch hidden directories/files starting with a dot
	if base != "." && base != ".." && strings.HasPrefix(base, ".") {
		return true
	}
	
	return false
}

// registerWatchRecursive traverses a directory tree recursively and adds watches.
func (w *Watcher) registerWatchRecursive(path string) error {
	if w.shouldIgnore(path) {
		return nil
	}

	// Add watch to directory
	err := w.fsWatcher.Add(path)
	if err != nil {
		if isWatchLimitError(err) {
			w.watchLimitReached = true
			log.Printf("monitor: WARNING: inotify watch limits exhausted adding '%s'. Gracefully falling back to partial watching + reconciliation mode.", path)
			return nil
		}
		return fmt.Errorf("add watch for '%s': %w", path, err)
	}

	// Recursively read subdirectories
	entries, err := os.ReadDir(path)
	if err != nil {
		if os.IsPermission(err) {
			return nil // skip permission denied
		}
		return err
	}

	for _, entry := range entries {
		if entry.IsDir() {
			sub := filepath.Join(path, entry.Name())
			if err := w.registerWatchRecursive(sub); err != nil {
				return err
			}
		}
	}

	return nil
}

// isWatchLimitError checks if an error indicates inotify watch limits have been exceeded.
func isWatchLimitError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	return strings.Contains(errStr, "no space left on device") || // ENOSPC
		strings.Contains(errStr, "too many open files") || // EMFILE
		strings.Contains(errStr, "limit")
}

// handleEvent processes a single filesystem event under steady state or during replay.
func (w *Watcher) handleEvent(ev fsnotify.Event) {
	path := filepath.Clean(ev.Name)
	
	// Apply ignore policies to the path
	if w.isIgnoredPath(path) {
		return
	}

	switch {
	case ev.Has(fsnotify.Create):
		w.handleCreate(path)
		
	case ev.Has(fsnotify.Write):
		w.handleWrite(path)
		
	case ev.Has(fsnotify.Remove) || ev.Has(fsnotify.Rename):
		w.handleDelete(path)
	}
}

// handleCreate processes a new file or directory creation.
func (w *Watcher) handleCreate(path string) {
	info, err := os.Stat(path)
	if err != nil {
		// Could have been deleted already, ignore
		return
	}

	if info.IsDir() {
		log.Printf("monitor: new directory detected: %s. Registering recursive watches.", path)
		w.mu.Lock()
		w.registerWatchRecursive(path)
		w.mu.Unlock()
		
		// Index all files inside the new directory
		w.indexTree(path)
	} else {
		w.indexFile(path, info)
	}
}

// handleWrite processes modifications.
func (w *Watcher) handleWrite(path string) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	if !info.IsDir() {
		w.indexFile(path, info)
	}
}

// handleDelete processes deletions.
func (w *Watcher) handleDelete(path string) {
	log.Printf("monitor: file or directory deleted: %s", path)
	
	// Remove from index
	if err := w.store.Delete(path); err != nil {
		log.Printf("monitor: error deleting '%s' from store: %v", path, err)
	}

	// fsnotify automatically removes deleted watches, but we can explicitly clean up
	_ = w.fsWatcher.Remove(path)
}

// indexFile puts a single file into the index store.
func (w *Watcher) indexFile(path string, info os.FileInfo) {
	// Calculate depth
	rel, err := filepath.Rel(w.root, path)
	depth := 0
	if err == nil && rel != "." {
		depth = strings.Count(rel, string(os.PathSeparator)) + 1
	}

	meta := &index.FileMeta{
		Path:      path,
		Filename:  info.Name(),
		Parent:    filepath.Dir(path),
		Extension: filepath.Ext(info.Name()),
		IsDir:     false,
		Modified:  info.ModTime().Unix(),
		Depth:     depth,
	}

	if err := w.store.Put(meta); err != nil {
		log.Printf("monitor: error putting file '%s' into store: %v", path, err)
	}
}

// indexTree walks a new directory and indexes all nested items.
func (w *Watcher) indexTree(path string) {
	_ = filepath.Walk(path, func(subPath string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if w.isIgnoredPath(subPath) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		// Calculate depth
		rel, err := filepath.Rel(w.root, subPath)
		depth := 0
		if err == nil && rel != "." {
			depth = strings.Count(rel, string(os.PathSeparator)) + 1
		}

		meta := &index.FileMeta{
			Path:      subPath,
			Filename:  info.Name(),
			Parent:    filepath.Dir(subPath),
			Extension: filepath.Ext(info.Name()),
			IsDir:     info.IsDir(),
			Modified:  info.ModTime().Unix(),
			Depth:     depth,
		}

		if err := w.store.Put(meta); err != nil {
			log.Printf("monitor: error putting tree entry '%s' in store: %v", subPath, err)
		}

		return nil
	})
}

// isIgnoredPath determines if any part of the path matches our ignore list.
func (w *Watcher) isIgnoredPath(path string) bool {
	if path == w.root {
		return false
	}
	rel, err := filepath.Rel(w.root, path)
	if err != nil {
		return true
	}
	
	parts := strings.Split(rel, string(os.PathSeparator))
	for _, part := range parts {
		if part == "." || part == ".." {
			continue
		}
		if w.ignoreMap[part] {
			return true
		}
		if strings.HasPrefix(part, ".") {
			return true
		}
	}
	return false
}

// TriggerReconciliation starts a background reconciliation to scan the root
// directory and align the index store with the current filesystem state.
func (w *Watcher) TriggerReconciliation() {
	w.mu.Lock()
	if w.reconciling {
		w.mu.Unlock()
		return
	}
	w.reconciling = true
	w.mu.Unlock()

	go func() {
		defer func() {
			w.mu.Lock()
			w.reconciling = false
			w.mu.Unlock()
		}()

		log.Println("monitor: background reconciliation crawl started...")
		
		// A lightweight directory scan to rebuild missing files in LMDB.
		// Since store.Put is idempotent, we can simply walk the tree and put all items.
		// We use w.processingMu to ensure we don't race with active steady state events.
		w.processingMu.Lock()
		defer w.processingMu.Unlock()

		err := filepath.Walk(w.root, func(subPath string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			if w.isIgnoredPath(subPath) {
				if info.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}

			// Add/refresh item in the store
			rel, rErr := filepath.Rel(w.root, subPath)
			depth := 0
			if rErr == nil && rel != "." {
				depth = strings.Count(rel, string(os.PathSeparator)) + 1
			}

			meta := &index.FileMeta{
				Path:      subPath,
				Filename:  info.Name(),
				Parent:    filepath.Dir(subPath),
				Extension: filepath.Ext(info.Name()),
				IsDir:     info.IsDir(),
				Modified:  info.ModTime().Unix(),
				Depth:     depth,
			}

			_ = w.store.Put(meta)
			return nil
		})

		if err != nil {
			log.Printf("monitor: error during reconciliation crawl: %v", err)
		} else {
			log.Println("monitor: background reconciliation crawl completed successfully.")
		}
	}()
}

// State returns the current watcher state.
func (w *Watcher) State() State {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.state
}

// WatchLimitReached returns whether the inotify watch limits were hit.
func (w *Watcher) WatchLimitReached() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.watchLimitReached
}
