package monitor

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rune/backend/internal/index"
)

func TestWatcherQueueAndReplay(t *testing.T) {
	// 1. Setup temp directory and store
	tmpDir, err := os.MkdirTemp("", "rune-monitor-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "db")
	store, err := index.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	// 2. Create watcher
	w, err := NewWatcher(store, tmpDir, []string{"node_modules"})
	if err != nil {
		t.Fatalf("failed to create watcher: %v", err)
	}
	defer w.Close()

	// 3. Start watcher (starts in StateQueueing)
	if err := w.Start(); err != nil {
		t.Fatalf("failed to start watcher: %v", err)
	}

	if w.State() != StateQueueing {
		t.Errorf("expected state to be StateQueueing, got %v", w.State())
	}

	// 4. Perform mutation during queueing phase
	testFile := filepath.Join(tmpDir, "queued_file.txt")
	if err := os.WriteFile(testFile, []byte("hello queued"), 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	// Wait briefly to allow fsnotify inotify events to be captured and queued
	time.Sleep(100 * time.Millisecond)

	// Verify that in StateQueueing, the file is NOT yet in the search store
	meta, err := store.Get(testFile)
	if err != nil {
		t.Fatalf("store.Get failed: %v", err)
	}
	if meta != nil {
		t.Errorf("expected file to not be in store during queueing phase, but found: %+v", meta)
	}

	// Check if queue got items
	w.mu.Lock()
	queueLen := len(w.eventQueue)
	w.mu.Unlock()
	if queueLen == 0 {
		t.Errorf("expected some events in the queue, got 0")
	}

	// 5. Trigger Replay
	w.Replay()

	if w.State() != StateSteady {
		t.Errorf("expected state to transition to StateSteady, got %v", w.State())
	}

	// Verify file is now indexed
	meta, err = store.Get(testFile)
	if err != nil {
		t.Fatalf("store.Get failed after replay: %v", err)
	}
	if meta == nil {
		t.Fatalf("expected file to be in store after replay, but not found")
	}
	if meta.Filename != "queued_file.txt" {
		t.Errorf("expected filename queued_file.txt, got %s", meta.Filename)
	}

	// 6. Test steady-state realtime monitoring
	steadyFile := filepath.Join(tmpDir, "steady_file.txt")
	if err := os.WriteFile(steadyFile, []byte("hello steady"), 0644); err != nil {
		t.Fatalf("failed to write steady file: %v", err)
	}

	// Give watcher a split second to process the steady event
	time.Sleep(100 * time.Millisecond)

	meta, err = store.Get(steadyFile)
	if err != nil {
		t.Fatalf("store.Get steady failed: %v", err)
	}
	if meta == nil {
		t.Errorf("expected steady file to be indexed instantly in steady-state, but was not found")
	}
}

func TestWatcherDeletesAndIgnores(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "rune-monitor-ignores-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "db")
	store, err := index.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	w, err := NewWatcher(store, tmpDir, []string{"node_modules"})
	if err != nil {
		t.Fatalf("failed to create watcher: %v", err)
	}
	defer w.Close()

	if err := w.Start(); err != nil {
		t.Fatalf("failed to start watcher: %v", err)
	}
	w.Replay() // Enter steady state immediately

	// 1. Create a file, then delete it.
	targetFile := filepath.Join(tmpDir, "toBeDeleted.txt")
	if err := os.WriteFile(targetFile, []byte("del me"), 0644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}

	time.Sleep(100 * time.Millisecond)
	meta, _ := store.Get(targetFile)
	if meta == nil {
		t.Fatalf("expected file to be created and indexed")
	}

	// Now delete it
	if err := os.Remove(targetFile); err != nil {
		t.Fatalf("failed to delete file: %v", err)
	}

	time.Sleep(100 * time.Millisecond)
	meta, _ = store.Get(targetFile)
	if meta != nil {
		t.Errorf("expected file to be removed from the index, but it was found: %+v", meta)
	}

	// 2. Test Ignored Folder
	ignoredDir := filepath.Join(tmpDir, "node_modules")
	if err := os.Mkdir(ignoredDir, 0755); err != nil {
		t.Fatalf("failed to create ignored dir: %v", err)
	}

	ignoredFile := filepath.Join(ignoredDir, "package.json")
	if err := os.WriteFile(ignoredFile, []byte("{}"), 0644); err != nil {
		t.Fatalf("failed to write ignored file: %v", err)
	}

	time.Sleep(100 * time.Millisecond)
	meta, _ = store.Get(ignoredFile)
	if meta != nil {
		t.Errorf("expected ignored file inside 'node_modules' to NOT be indexed, but found: %+v", meta)
	}
}

func TestWatcherReconciliation(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "rune-monitor-recon-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "db")
	store, err := index.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	w, err := NewWatcher(store, tmpDir, []string{"node_modules"})
	if err != nil {
		t.Fatalf("failed to create watcher: %v", err)
	}
	defer w.Close()

	if err := w.Start(); err != nil {
		t.Fatalf("failed to start watcher: %v", err)
	}
	w.Replay()

	// Write file directly to filesystem bypassing the watcher (simulate a missed event)
	w.Close() // temporary close to block watch events

	missedFile := filepath.Join(tmpDir, "missed.txt")
	if err := os.WriteFile(missedFile, []byte("missed"), 0644); err != nil {
		t.Fatalf("failed to write missed file: %v", err)
	}

	// Recreate watcher
	w2, err := NewWatcher(store, tmpDir, []string{"node_modules"})
	if err != nil {
		t.Fatalf("failed to recreate watcher: %v", err)
	}
	defer w2.Close()
	w2.Start()
	w2.Replay()

	// Trigger reconciliation rescan manually
	w2.TriggerReconciliation()

	// Wait for background reconciliation to finish
	time.Sleep(200 * time.Millisecond)

	meta, err := store.Get(missedFile)
	if err != nil {
		t.Fatalf("store.Get failed: %v", err)
	}
	if meta == nil {
		t.Errorf("expected reconciliation to discover and index missed.txt, but it was not found")
	}
}
