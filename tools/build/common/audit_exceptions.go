package common

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// GateException represents an exception record from the tracker.
type GateException struct {
	Gate      string `json:"gate"`
	Metric    string `json:"metric"`
	BugID     string `json:"bug_id"`
	Platform  string `json:"platform"`
	GrantedBy string `json:"granted_by"`
	Reason    string `json:"reason"`
	ExpiresAt string `json:"expires_at"` // RFC3339
	Expired   bool   `json:"expired"`
}

// AuditExceptions queries the tracker for gate exceptions and reports any
// that have expired. Returns an error if expired exceptions exist or if the
// tracker is unreachable (fail-closed).
func AuditExceptions(root string) error {
	trackerURL, err := findTrackerBaseURL(root)
	if err != nil {
		return fmt.Errorf("tracker unreachable: %w", err)
	}

	exceptions, err := fetchExceptions(trackerURL)
	if err != nil {
		return fmt.Errorf("tracker unreachable: %w", err)
	}

	expired := filterExpired(exceptions)

	if len(expired) == 0 {
		fmt.Println("no expired exceptions — OK")
		return nil
	}

	fmt.Printf("%d expired exception(s):\n\n", len(expired))
	for _, e := range expired {
		fmt.Printf("  gate:       %s\n", e.Gate)
		if e.Metric != "" {
			fmt.Printf("  metric:     %s\n", e.Metric)
		}
		fmt.Printf("  bug:        %s\n", e.BugID)
		if e.Platform != "" {
			fmt.Printf("  platform:   %s\n", e.Platform)
		}
		fmt.Printf("  expired:    %s\n", e.ExpiresAt)
		fmt.Printf("  granted by: %s\n", e.GrantedBy)
		fmt.Printf("  reason:     %s\n", e.Reason)
		fmt.Println()
	}

	return fmt.Errorf("%d expired exception(s) found", len(expired))
}

// findTrackerBaseURL locates the tracker base URL from .mcp.json, walking up
// from root. Returns the base URL (without /mcp suffix).
func findTrackerBaseURL(root string) (string, error) {
	dir := root
	for {
		data, err := os.ReadFile(filepath.Join(dir, ".mcp.json"))
		if err == nil {
			var cfg struct {
				MCPServers map[string]struct {
					URL string `json:"url"`
				} `json:"mcpServers"`
			}
			if json.Unmarshal(data, &cfg) == nil {
				if srv, ok := cfg.MCPServers["tracker"]; ok && srv.URL != "" {
					return strings.TrimSuffix(srv.URL, "/mcp"), nil
				}
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no .mcp.json with tracker URL found")
		}
		dir = parent
	}
}

// fetchExceptions calls the tracker REST API to list all exceptions including
// expired ones. Returns an error if the tracker is unreachable or returns a
// non-200 status.
func fetchExceptions(trackerURL string) ([]GateException, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(trackerURL + "/api/gate-exceptions?include_expired=true")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tracker returned HTTP %d", resp.StatusCode)
	}

	var exceptions []GateException
	if err := json.NewDecoder(resp.Body).Decode(&exceptions); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}
	return exceptions, nil
}

// filterExpired returns exceptions that are expired, either by the server's
// expired flag or by checking expires_at against the current time.
func filterExpired(exceptions []GateException) []GateException {
	now := time.Now()
	var expired []GateException
	for _, e := range exceptions {
		if e.Expired {
			expired = append(expired, e)
			continue
		}
		if e.ExpiresAt != "" {
			if t, err := time.Parse(time.RFC3339, e.ExpiresAt); err == nil {
				if now.After(t) {
					expired = append(expired, e)
				}
			}
		}
	}
	return expired
}
