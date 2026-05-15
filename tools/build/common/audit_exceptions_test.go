package common

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFilterExpired_ServerFlag(t *testing.T) {
	exceptions := []GateException{
		{Gate: "no-allow-leaks", BugID: "B0100", Expired: true},
		{Gate: "stress-mac", BugID: "B0200", Expired: false},
	}
	expired := filterExpired(exceptions)
	if len(expired) != 1 {
		t.Fatalf("expected 1 expired, got %d", len(expired))
	}
	if expired[0].BugID != "B0100" {
		t.Fatalf("expected B0100, got %s", expired[0].BugID)
	}
}

func TestFilterExpired_ParsedTime(t *testing.T) {
	past := time.Now().Add(-24 * time.Hour).Format(time.RFC3339)
	future := time.Now().Add(24 * time.Hour).Format(time.RFC3339)
	exceptions := []GateException{
		{Gate: "g1", BugID: "B0001", ExpiresAt: past},
		{Gate: "g2", BugID: "B0002", ExpiresAt: future},
	}
	expired := filterExpired(exceptions)
	if len(expired) != 1 {
		t.Fatalf("expected 1 expired, got %d", len(expired))
	}
	if expired[0].BugID != "B0001" {
		t.Fatalf("expected B0001, got %s", expired[0].BugID)
	}
}

func TestFilterExpired_Empty(t *testing.T) {
	expired := filterExpired(nil)
	if len(expired) != 0 {
		t.Fatalf("expected 0 expired, got %d", len(expired))
	}
}

func TestFilterExpired_InvalidTime(t *testing.T) {
	exceptions := []GateException{
		{Gate: "g1", BugID: "B0001", ExpiresAt: "not-a-date"},
	}
	expired := filterExpired(exceptions)
	if len(expired) != 0 {
		t.Fatalf("expected 0 expired for invalid date, got %d", len(expired))
	}
}

func TestFetchExceptions_OK(t *testing.T) {
	exceptions := []GateException{
		{Gate: "stress-mac", BugID: "B0050", Expired: true},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/gate-exceptions" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("include_expired") != "true" {
			t.Errorf("expected include_expired=true, got %s", r.URL.Query().Get("include_expired"))
		}
		json.NewEncoder(w).Encode(exceptions)
	}))
	defer srv.Close()

	got, err := fetchExceptions(srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].BugID != "B0050" {
		t.Fatalf("unexpected result: %+v", got)
	}
}

func TestFetchExceptions_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := fetchExceptions(srv.URL)
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

func TestFetchExceptions_Unreachable(t *testing.T) {
	_, err := fetchExceptions("http://127.0.0.1:1") // nothing listening
	if err == nil {
		t.Fatal("expected error for unreachable server")
	}
}

func TestFindTrackerBaseURL(t *testing.T) {
	dir := t.TempDir()
	mcpJSON := `{"mcpServers":{"tracker":{"type":"http","url":"http://localhost:9121/mcp"}}}`
	if err := os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(mcpJSON), 0644); err != nil {
		t.Fatal(err)
	}

	url, err := findTrackerBaseURL(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if url != "http://localhost:9121" {
		t.Fatalf("expected http://localhost:9121, got %s", url)
	}
}

func TestFindTrackerBaseURL_WalkUp(t *testing.T) {
	dir := t.TempDir()
	mcpJSON := `{"mcpServers":{"tracker":{"url":"http://host:8080/mcp"}}}`
	if err := os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(mcpJSON), 0644); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(dir, "sub", "deep")
	if err := os.MkdirAll(child, 0755); err != nil {
		t.Fatal(err)
	}

	url, err := findTrackerBaseURL(child)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if url != "http://host:8080" {
		t.Fatalf("expected http://host:8080, got %s", url)
	}
}

func TestFindTrackerBaseURL_NotFound(t *testing.T) {
	dir := t.TempDir()
	_, err := findTrackerBaseURL(dir)
	if err == nil {
		t.Fatal("expected error when .mcp.json not found")
	}
}

// --- AuditExceptions integration tests ---

// setupAuditEnv creates a temp dir with .mcp.json pointing to the given server URL.
func setupAuditEnv(t *testing.T, serverURL string) string {
	t.Helper()
	dir := t.TempDir()
	mcpJSON := fmt.Sprintf(`{"mcpServers":{"tracker":{"url":"%s/mcp"}}}`, serverURL)
	if err := os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(mcpJSON), 0644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestAuditExceptions_NoExpired(t *testing.T) {
	future := time.Now().Add(24 * time.Hour).Format(time.RFC3339)
	exceptions := []GateException{
		{Gate: "stress", BugID: "B0001", ExpiresAt: future},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(exceptions)
	}))
	defer srv.Close()

	root := setupAuditEnv(t, srv.URL)
	if err := AuditExceptions(root); err != nil {
		t.Fatalf("expected no error for non-expired exceptions, got: %v", err)
	}
}

func TestAuditExceptions_WithExpired(t *testing.T) {
	exceptions := []GateException{
		{Gate: "no-allow-leaks", BugID: "B0100", Metric: "leaks", Platform: "linux", Expired: true, ExpiresAt: "2025-01-01T00:00:00Z", GrantedBy: "test", Reason: "temp"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(exceptions)
	}))
	defer srv.Close()

	root := setupAuditEnv(t, srv.URL)
	err := AuditExceptions(root)
	if err == nil {
		t.Fatal("expected error for expired exceptions")
	}
	if !strings.Contains(err.Error(), "1 expired") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestAuditExceptions_WithExpiredNoOptionalFields(t *testing.T) {
	// Exercises the code path where Metric and Platform are empty (no optional print).
	exceptions := []GateException{
		{Gate: "g1", BugID: "B0200", Expired: true, ExpiresAt: "2025-01-01T00:00:00Z", GrantedBy: "ci", Reason: "flaky"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(exceptions)
	}))
	defer srv.Close()

	root := setupAuditEnv(t, srv.URL)
	err := AuditExceptions(root)
	if err == nil {
		t.Fatal("expected error for expired exceptions")
	}
}

func TestAuditExceptions_TrackerUnreachable(t *testing.T) {
	// Point .mcp.json at a server that's not listening.
	dir := t.TempDir()
	mcpJSON := `{"mcpServers":{"tracker":{"url":"http://127.0.0.1:1/mcp"}}}`
	if err := os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(mcpJSON), 0644); err != nil {
		t.Fatal(err)
	}

	err := AuditExceptions(dir)
	if err == nil {
		t.Fatal("expected error for unreachable tracker")
	}
	if !strings.Contains(err.Error(), "tracker unreachable") {
		t.Fatalf("expected 'tracker unreachable', got: %v", err)
	}
}

func TestAuditExceptions_NoMcpJSON(t *testing.T) {
	dir := t.TempDir()
	err := AuditExceptions(dir)
	if err == nil {
		t.Fatal("expected error when .mcp.json not found")
	}
	if !strings.Contains(err.Error(), "tracker unreachable") {
		t.Fatalf("expected 'tracker unreachable', got: %v", err)
	}
}

func TestFetchExceptions_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer srv.Close()

	_, err := fetchExceptions(srv.URL)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if !strings.Contains(err.Error(), "failed to parse") {
		t.Fatalf("expected parse error, got: %v", err)
	}
}
