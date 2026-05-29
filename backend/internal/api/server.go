package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"

	"github.com/rune/backend/internal/index"
	"github.com/rune/backend/internal/monitor"
	"github.com/rune/backend/internal/query"
)

// Server represents the local API server backing Rune.
type Server struct {
	store          *index.Store
	watcher        *monitor.Watcher
	indexingActive int32 // atomic flag
	listener       net.Listener
	mux            *http.ServeMux
	port           int
	addr           string
}

// NewServer creates a new Server instance.
func NewServer(store *index.Store, watcher *monitor.Watcher, port int) *Server {
	return &Server{
		store:   store,
		watcher: watcher,
		port:    port,
		addr:    fmt.Sprintf("127.0.0.1:%d", port),
		mux:     http.NewServeMux(),
	}
}

// Start listens and serves HTTP requests.
func (s *Server) Start() error {
	s.setupRoutes()

	// Implement the network transport abstraction allowing UDS migration.
	// Currently using localhost TCP, but net.Listen("unix", ...) can be swapped in.
	l, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("api: listen on %s failed: %w", s.addr, err)
	}
	s.listener = l

	log.Printf("api: server listening on http://%s", s.addr)

	// Run http server in the background
	go func() {
		err := http.Serve(s.listener, s.mux)
		if err != nil && err != http.ErrServerClosed {
			log.Printf("api: http serve error: %v", err)
		}
	}()

	return nil
}

// Stop closes the listener and halts the server.
func (s *Server) Stop() error {
	if s.listener != nil {
		return s.listener.Close()
	}
	return nil
}

// SetIndexingActive updates the internal status of the background crawler.
func (s *Server) SetIndexingActive(active bool) {
	var val int32
	if active {
		val = 1
	}
	atomic.StoreInt32(&s.indexingActive, val)
}

func (s *Server) setupRoutes() {
	s.mux.HandleFunc("/search", s.handleCors(s.handleSearch))
	s.mux.HandleFunc("/status", s.handleCors(s.handleStatus))
	s.mux.HandleFunc("/health", s.handleCors(s.handleHealth))
}

// handleCors applies standard CORS headers for local Tauri app access.
func (s *Server) handleCors(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		next(w, r)
	}
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	q := r.URL.Query().Get("q")
	genID := r.URL.Query().Get("gen")
	limitStr := r.URL.Query().Get("limit")

	limit := 50
	if limitStr != "" {
		if parsed, err := strconv.Atoi(limitStr); err == nil && parsed > 0 {
			limit = parsed
			if limit > 500 {
				limit = 500 // cap maximum to preserve sub-5ms performance
			}
		}
	}

	// Hot query path execution checking client context cancellation
	ctx := r.Context()
	select {
	case <-ctx.Done():
		// Client aborted before execution began
		return
	default:
	}

	engine := query.NewEngine(s.store)
	results, err := engine.Search(q, limit)
	if err != nil {
		log.Printf("api: search error: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	response := struct {
		Gen     string         `json:"gen"`
		Results []query.Result `json:"results"`
	}{
		Gen:     genID,
		Results: results,
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	stats := s.store.Stats()

	// Compute DB file size
	var dbSize int64
	dbMdbPath := filepath.Join(s.store.Path(), "data.mdb")
	if info, err := os.Stat(dbMdbPath); err == nil {
		dbSize = info.Size()
	}

	// Watch limits status
	watchLimitReached := false
	if s.watcher != nil {
		watchLimitReached = s.watcher.WatchLimitReached()
	}

	response := struct {
		IndexingActive    bool   `json:"indexing_active"`
		FileCount         int    `json:"file_count"`
		TrieNodes         int    `json:"trie_nodes"`
		TrigramCount      int    `json:"trigram_count"`
		DBSizeBytes       int64  `json:"db_size_bytes"`
		WatchLimitReached bool   `json:"watch_limit_reached"`
		DBPath            string `json:"db_path"`
	}{
		IndexingActive:    atomic.LoadInt32(&s.indexingActive) == 1,
		FileCount:         stats.PathCount,
		TrieNodes:         stats.TrieNodes,
		TrigramCount:      stats.TrigramCount,
		DBSizeBytes:       dbSize,
		WatchLimitReached: watchLimitReached,
		DBPath:            s.store.Path(),
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	response := map[string]string{"status": "healthy"}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}
