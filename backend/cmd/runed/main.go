package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/rune/backend/internal/api"
	"github.com/rune/backend/internal/crawler"
	"github.com/rune/backend/internal/index"
	"github.com/rune/backend/internal/monitor"
)

func main() {
	// 1. Resolve default directories
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}

	defaultRoot := home
	defaultDB := filepath.Join(home, ".local", "share", "rune")

	// 2. Parse command-line flags
	rootFlag := flag.String("root", defaultRoot, "The directory root to index and monitor recursively")
	dbFlag := flag.String("db", defaultDB, "The directory path where LMDB files are saved")
	portFlag := flag.Int("port", 10382, "Port to run the localhost HTTP server on")
	flag.Parse()

	absRoot, err := filepath.Abs(*rootFlag)
	if err != nil {
		log.Fatalf("runed: invalid root path '%s': %v", *rootFlag, err)
	}
	absRoot = filepath.Clean(absRoot)

	absDB, err := filepath.Abs(*dbFlag)
	if err != nil {
		log.Fatalf("runed: invalid db path '%s': %v", *dbFlag, err)
	}
	absDB = filepath.Clean(absDB)

	log.Printf("==============================================")
	log.Printf("   Rune Daemon (runed) starting up  ")
	log.Printf("==============================================")
	log.Printf("Crawl Root:   %s", absRoot)
	log.Printf("LMDB Store:   %s", absDB)
	log.Printf("API Port:     %d", *portFlag)
	log.Printf("==============================================")

	// 3. Create context for background jobs (crawler, watcher) and setup termination signal handling
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// A: Initialize/Open the on-disk LMDB database store.
	store, err := index.Open(absDB)
	if err != nil {
		log.Fatalf("runed: failed to open index store: %v", err)
	}
	defer func() {
		log.Println("runed: closing index store...")
		store.Close()
	}()

	// B: Start filesystem watcher FIRST, to queue all real-time modifications before crawling begins.
	watcher, err := monitor.NewWatcher(store, absRoot, nil)
	if err != nil {
		log.Fatalf("runed: failed to create watcher: %v", err)
	}
	if err := watcher.Start(); err != nil {
		log.Printf("runed: warning: watcher start encountered issues: %v", err)
	}
	defer func() {
		log.Println("runed: closing filesystem watcher...")
		watcher.Close()
	}()

	// C: Spin up localhost API server immediately so Svelte frontend can query state
	server := api.NewServer(store, watcher, *portFlag)
	server.SetIndexingActive(true)
	if err := server.Start(); err != nil {
		log.Fatalf("runed: failed to start API server: %v", err)
	}
	defer func() {
		log.Println("runed: halting API server...")
		server.Stop()
	}()

	// D: Perform background crawl of files to seed/update the search index
	crawlDone := make(chan struct{})
	go func() {
		defer close(crawlDone)
		log.Println("runed: starting background directory tree crawl...")
		c := crawler.New(store, crawler.Options{
			MaxConcurrency: 16,
			OnProgress: func(fileCount int64) {
				if fileCount%50000 == 0 && fileCount > 0 {
					log.Printf("runed: crawl in progress: %d files indexed.", fileCount)
				}
			},
		})

		count, err := c.Crawl(ctx, absRoot)
		if err != nil {
			if ctx.Err() != nil {
				log.Println("runed: background crawl cancelled gracefully.")
			} else {
				log.Printf("runed: error during background crawl: %v", err)
			}
			return
		}
		log.Printf("runed: directory tree crawl completed. Indexed %d files.", count)
	}()

	// Wait for background crawl to complete OR OS signal to abort
	select {
	case <-sigChan:
		log.Println("runed: shutdown signal received during initial crawl. Aborting background jobs...")
		cancel()
		<-crawlDone
		return
	case <-crawlDone:
		// Crawl finished normally
	}

	// E: Replay all filesystem mutations queued by watcher during crawl to prevent consistency gap races.
	watcher.Replay()

	// F: Steady state
	server.SetIndexingActive(false)
	log.Println("runed: initialization sequence finished. Ready for queries.")

	// Block until termination signal
	<-sigChan
	log.Println("runed: shutdown signal received. Halting services...")
}
