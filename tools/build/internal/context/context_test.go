package context

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

// trackerStub captures every request so tests can assert on them.
type trackerStub struct {
	mu          sync.Mutex
	requests    []capturedRequest
	pushStatus  int
	popStatus   int
	logStatus   int
	logRequests int32
}

type capturedRequest struct {
	Path string
	Body map[string]string
}

func newTrackerStub() *trackerStub {
	return &trackerStub{
		pushStatus: http.StatusOK,
		popStatus:  http.StatusOK,
		logStatus:  http.StatusOK,
	}
}

func (s *trackerStub) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/agent/context/push", func(w http.ResponseWriter, r *http.Request) {
		s.record(r)
		w.WriteHeader(s.pushStatus)
	})
	mux.HandleFunc("/api/agent/context/pop", func(w http.ResponseWriter, r *http.Request) {
		s.record(r)
		w.WriteHeader(s.popStatus)
	})
	mux.HandleFunc("/api/agent/log", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&s.logRequests, 1)
		s.record(r)
		w.WriteHeader(s.logStatus)
	})
	return mux
}

func (s *trackerStub) record(r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var decoded map[string]string
	_ = json.Unmarshal(body, &decoded)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, capturedRequest{Path: r.URL.Path, Body: decoded})
}

func (s *trackerStub) snapshot() []capturedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]capturedRequest, len(s.requests))
	copy(out, s.requests)
	return out
}

// makeWorkspace creates a temp dir with .mcp.json pointing tracker at the
// given URL, and returns the workspace path.
func makeWorkspace(t *testing.T, trackerURL string) string {
	t.Helper()
	dir := t.TempDir()
	mcp := map[string]any{
		"mcpServers": map[string]any{
			"tracker": map[string]any{"url": trackerURL + "/mcp"},
		},
	}
	body, _ := json.Marshal(mcp)
	if err := os.WriteFile(filepath.Join(dir, ".mcp.json"), body, 0o644); err != nil {
		t.Fatalf("writing .mcp.json: %v", err)
	}
	return dir
}

func TestPushSendsToContextPushEndpoint(t *testing.T) {
	stub := newTrackerStub()
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	ws := makeWorkspace(t, srv.URL)

	Push(Input{
		HookEventName: "PreToolUse",
		CWD:           ws,
		Kind:          "skill",
		Name:          "do",
		InputText:     "B0042",
	})

	got := stub.snapshot()
	if len(got) != 1 {
		t.Fatalf("got %d requests, want 1", len(got))
	}
	if got[0].Path != "/api/agent/context/push" {
		t.Errorf("path = %q, want /api/agent/context/push", got[0].Path)
	}
	want := map[string]string{"kind": "skill", "name": "do", "input": "B0042", "cwd": ws}
	for k, v := range want {
		if got[0].Body[k] != v {
			t.Errorf("body[%q] = %q, want %q", k, got[0].Body[k], v)
		}
	}
	if got[0].Body["agent"] == "" || got[0].Body["host"] == "" {
		t.Errorf("agent/host should be populated, got body=%v", got[0].Body)
	}
}

func TestPopSendsToContextPopEndpoint(t *testing.T) {
	stub := newTrackerStub()
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	ws := makeWorkspace(t, srv.URL)

	Pop(Input{
		HookEventName: "PostToolUse",
		CWD:           ws,
		Kind:          "tool",
		Name:          "Bash",
		InputText:     "ls",
	})

	got := stub.snapshot()
	if len(got) != 1 {
		t.Fatalf("got %d requests, want 1", len(got))
	}
	if got[0].Path != "/api/agent/context/pop" {
		t.Errorf("path = %q, want /api/agent/context/pop", got[0].Path)
	}
	if got[0].Body["kind"] != "tool" || got[0].Body["name"] != "Bash" {
		t.Errorf("body kind/name wrong: %v", got[0].Body)
	}
}

func TestPushFailureLogsError(t *testing.T) {
	stub := newTrackerStub()
	stub.pushStatus = http.StatusInternalServerError
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	ws := makeWorkspace(t, srv.URL)

	Push(Input{CWD: ws, Kind: "tool", Name: "Bash", InputText: "ls"})

	got := stub.snapshot()
	if len(got) != 2 {
		t.Fatalf("got %d requests, want 2 (push + log)", len(got))
	}
	if got[1].Path != "/api/agent/log" {
		t.Errorf("second request path = %q, want /api/agent/log", got[1].Path)
	}
	if got[1].Body["level"] != "error" {
		t.Errorf("log level = %q, want error", got[1].Body["level"])
	}
	if got[1].Body["message"] == "" {
		t.Errorf("log message should be non-empty")
	}
}

func TestPopFailureLogsError(t *testing.T) {
	stub := newTrackerStub()
	stub.popStatus = http.StatusBadGateway
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	ws := makeWorkspace(t, srv.URL)

	Pop(Input{CWD: ws, Kind: "tool", Name: "Edit", InputText: "/x"})

	got := stub.snapshot()
	if len(got) != 2 {
		t.Fatalf("got %d requests, want 2 (pop + log)", len(got))
	}
	if got[1].Path != "/api/agent/log" {
		t.Errorf("second request path = %q, want /api/agent/log", got[1].Path)
	}
}

// TestPushAndLogFailureDoesNotPanic ensures that when both the primary
// endpoint and the log endpoint fail, Push still returns cleanly.
func TestPushAndLogFailureDoesNotPanic(t *testing.T) {
	stub := newTrackerStub()
	stub.pushStatus = http.StatusInternalServerError
	stub.logStatus = http.StatusInternalServerError
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	ws := makeWorkspace(t, srv.URL)

	// Should not panic.
	Push(Input{CWD: ws, Kind: "tool", Name: "Bash", InputText: "ls"})

	if atomic.LoadInt32(&stub.logRequests) != 1 {
		t.Errorf("expected 1 log attempt even when log fails, got %d",
			atomic.LoadInt32(&stub.logRequests))
	}
}

func TestNoTrackerConfiguredIsNoop(t *testing.T) {
	// Workspace without .mcp.json (just an empty temp dir).
	dir := t.TempDir()
	// Should not panic and should not block.
	Push(Input{CWD: dir, Kind: "skill", Name: "do"})
	Pop(Input{CWD: dir, Kind: "skill", Name: "do"})
}

func TestEmptyKindOrNameIsNoop(t *testing.T) {
	stub := newTrackerStub()
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	ws := makeWorkspace(t, srv.URL)

	Push(Input{CWD: ws, Kind: "", Name: "do"})
	Push(Input{CWD: ws, Kind: "skill", Name: ""})
	Pop(Input{CWD: ws, Kind: "", Name: "do"})

	if got := stub.snapshot(); len(got) != 0 {
		t.Errorf("expected no requests when kind/name empty, got %d", len(got))
	}
}
