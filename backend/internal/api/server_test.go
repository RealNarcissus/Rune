package api

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/rune/backend/internal/index"
)

func TestServerEndpoints(t *testing.T) {
	// 1. Setup temp directory and store
	tmpDir, err := os.MkdirTemp("", "rune-api-test-*")
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

	// Put some sample files in the store to search
	err = store.Put(&index.FileMeta{
		Path:      "/home/user/documents/resume.pdf",
		Filename:  "resume.pdf",
		Parent:    "/home/user/documents",
		Extension: ".pdf",
		IsDir:     false,
		Modified:  1600000000,
		Depth:     3,
	})
	if err != nil {
		t.Fatalf("failed to put test file: %v", err)
	}

	// 2. Start server on ephemeral port (0)
	server := NewServer(store, nil, 0) // 0 lets OS assign free port
	if err := server.Start(); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	defer server.Stop()

	// Get actual listener address
	addr := server.listener.Addr().String()
	baseURL := "http://" + addr

	// 3. Test GET /health
	resp, err := http.Get(baseURL + "/health")
	if err != nil {
		t.Fatalf("GET /health failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status OK, got %v", resp.Status)
	}
	body, _ := io.ReadAll(resp.Body)
	var healthMap map[string]string
	if err := json.Unmarshal(body, &healthMap); err != nil {
		t.Fatalf("unmarshal health body: %v", err)
	}
	if healthMap["status"] != "healthy" {
		t.Errorf("expected status 'healthy', got '%s'", healthMap["status"])
	}

	// 4. Test GET /status
	server.SetIndexingActive(true)
	respStatus, err := http.Get(baseURL + "/status")
	if err != nil {
		t.Fatalf("GET /status failed: %v", err)
	}
	defer respStatus.Body.Close()

	if respStatus.StatusCode != http.StatusOK {
		t.Errorf("expected status OK, got %v", respStatus.Status)
	}
	bodyStatus, _ := io.ReadAll(respStatus.Body)
	var statusMap map[string]interface{}
	if err := json.Unmarshal(bodyStatus, &statusMap); err != nil {
		t.Fatalf("unmarshal status body: %v", err)
	}

	if statusMap["indexing_active"] != true {
		t.Errorf("expected indexing_active to be true")
	}
	if int(statusMap["file_count"].(float64)) != 1 {
		t.Errorf("expected file_count to be 1, got %v", statusMap["file_count"])
	}

	// 5. Test GET /search
	respSearch, err := http.Get(baseURL + "/search?q=res&gen=101&limit=10")
	if err != nil {
		t.Fatalf("GET /search failed: %v", err)
	}
	defer respSearch.Body.Close()

	if respSearch.StatusCode != http.StatusOK {
		t.Errorf("expected status OK, got %v", respSearch.Status)
	}
	bodySearch, _ := io.ReadAll(respSearch.Body)
	
	var searchResp struct {
		Gen     string                   `json:"gen"`
		Results []map[string]interface{} `json:"results"`
	}
	if err := json.Unmarshal(bodySearch, &searchResp); err != nil {
		t.Fatalf("unmarshal search body: %v", err)
	}

	if searchResp.Gen != "101" {
		t.Errorf("expected gen '101', got '%s'", searchResp.Gen)
	}
	if len(searchResp.Results) != 1 {
		t.Errorf("expected 1 search result, got %d", len(searchResp.Results))
	} else {
		res := searchResp.Results[0]
		if res["filename"] != "resume.pdf" {
			t.Errorf("expected result filename 'resume.pdf', got '%v'", res["filename"])
		}
	}
}
