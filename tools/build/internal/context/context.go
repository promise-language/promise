// Package context implements the Claude Code guard hook's tracker
// notifications.
//
// The guard fires this package's Push/Pop on every PreToolUse / PostToolUse
// for Bash, Edit, Write, and Skill. The package forwards the event to the
// tracker's agent context API (POST /api/agent/context/push|pop) so the
// tracker can maintain a per-agent stack of in-flight tools and skills.
//
// All push/pop logic lives on the tracker. This package never inspects the
// skill name, never maps it to a status, and never extracts item IDs — it
// is a thin pass-through.
//
// Errors from push/pop are reported back to the tracker via the agent log
// API (POST /api/agent/log, level=error). If the log call itself fails,
// the package writes a single line to stderr and gives up.
package context

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Input is the data forwarded from the guard.
type Input struct {
	HookEventName string // "PreToolUse" | "PostToolUse"
	CWD           string
	Kind          string // "skill" | "tool"
	Name          string // skill name, or tool name (Bash | Edit | Write)
	InputText     string // skill args, bash command, or file path
}

type mcpConfig struct {
	MCPServers map[string]struct {
		URL string `json:"url"`
	} `json:"mcpServers"`
}

// httpClient is the shared client. The timeout sits in the PreToolUse
// critical path for Bash/Edit/Write — Claude Code waits for the hook to
// return before the tool runs, so a slow tracker would slow down every
// tool call. 2s is plenty for a healthy LAN tracker; if it's wrong, push
// + log fallback together (4s worst case) still leave headroom under the
// 10s hook timeout in settings.json.
var httpClient = &http.Client{Timeout: 2 * time.Second}

// Push forwards a PreToolUse event to the tracker.
func Push(in Input) {
	dispatch(in, "push", "/api/agent/context/push")
}

// Pop forwards a PostToolUse event to the tracker.
func Pop(in Input) {
	dispatch(in, "pop", "/api/agent/context/pop")
}

func dispatch(in Input, op, path string) {
	if in.Name == "" || in.Kind == "" {
		return
	}
	baseURL := findTrackerURL(in.CWD)
	if baseURL == "" {
		return
	}
	agent, host := identity(in.CWD)
	body, _ := json.Marshal(map[string]string{
		"agent": agent,
		"host":  host,
		"kind":  in.Kind,
		"name":  in.Name,
		"input": in.InputText,
		"cwd":   in.CWD,
	})
	if err := postJSON(baseURL+path, body); err != nil {
		msg := fmt.Sprintf("guard: context %s failed: %s %s -> %s",
			op, http.MethodPost, path, err.Error())
		logError(baseURL, agent, host, msg)
	}
}

func logError(baseURL, agent, host, message string) {
	body, _ := json.Marshal(map[string]string{
		"agent":   agent,
		"host":    host,
		"level":   "error",
		"message": message,
	})
	if err := postJSON(baseURL+"/api/agent/log", body); err != nil {
		fmt.Fprintf(os.Stderr, "guard: failed to log to tracker: %v\n", err)
		fmt.Fprintf(os.Stderr, "guard: original error: %s\n", message)
	}
}

func postJSON(url string, body []byte) error {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d %s", resp.StatusCode, resp.Status)
	}
	return nil
}

func identity(cwd string) (agent, host string) {
	agent = filepath.Base(cwd)
	host, _ = os.Hostname()
	return agent, host
}

func findTrackerURL(cwd string) string {
	dir := cwd
	for {
		data, err := os.ReadFile(filepath.Join(dir, ".mcp.json"))
		if err == nil {
			var cfg mcpConfig
			if json.Unmarshal(data, &cfg) == nil {
				if srv, ok := cfg.MCPServers["tracker"]; ok && srv.URL != "" {
					return strings.TrimSuffix(srv.URL, "/mcp")
				}
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}
