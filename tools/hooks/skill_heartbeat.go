// skill_heartbeat.go — Claude Code hook for automatic agent heartbeat updates.
//
// Handles both PreToolUse and PostToolUse events for the Skill tool:
//   - PreToolUse: detects which skill is starting and sends a heartbeat with
//     the mapped status (e.g., /do → "planning", /review → "reviewing").
//   - PostToolUse: detects when a "final" skill completes, sets the agent to
//     "idle", and cleans up items tagged "processing" that were assigned to
//     this agent (removes the tag and clears assigned_to).
//
// Usage (via hook config):
//
//	"command": "go run $(git rev-parse --show-toplevel)/tools/hooks/skill_heartbeat.go || true"
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// hookInput is the JSON structure Claude Code sends to tool-use hooks.
type hookInput struct {
	HookEventName string `json:"hook_event_name"`
	CWD           string `json:"cwd"`
	ToolInput     struct {
		Skill string `json:"skill"`
		Args  string `json:"args"`
	} `json:"tool_input"`
}

// mcpConfig mirrors the .mcp.json structure.
type mcpConfig struct {
	MCPServers map[string]struct {
		URL string `json:"url"`
	} `json:"mcpServers"`
}

// trackerItem is a minimal representation of an item from the tracker API.
type trackerItem struct {
	ID         string   `json:"id"`
	AssignedTo string   `json:"assigned_to"`
	Tags       []string `json:"tags"`
}

// skillStatus maps skill names to heartbeat status on PreToolUse (skill start).
var skillStatus = map[string]string{
	"do":       "planning",
	"plan":     "planning",
	"review":   "reviewing",
	"coverage": "coverage",
	"commit":   "committing",
	"inspect":  "reviewing",
	"stress":   "stress-testing",
}

// postSkillStatus maps skill names to heartbeat status on PostToolUse (skill exit).
// Only listed if the skill triggers a phase transition on completion.
var postSkillStatus = map[string]string{
	"plan": "implementing",
}

// finalSkills are skills whose completion means the agent is done working.
var finalSkills = map[string]bool{
	"do":      true,
	"inspect": true,
	"commit":  true,
	"stress":  true,
}

var itemIDRe = regexp.MustCompile(`[TBD]\d{4,}`)

func main() {
	var input hookInput
	if err := json.NewDecoder(os.Stdin).Decode(&input); err != nil {
		// Can't parse input — exit silently (non-blocking).
		os.Exit(0)
	}

	skill := input.ToolInput.Skill
	if skill == "" {
		os.Exit(0)
	}

	status, known := skillStatus[skill]
	if !known {
		os.Exit(0)
	}

	trackerURL := findTrackerURL(input.CWD)
	if trackerURL == "" {
		os.Exit(0)
	}

	agentName := filepath.Base(input.CWD)
	hostName, _ := os.Hostname()

	client := &http.Client{Timeout: 5 * time.Second}

	switch input.HookEventName {
	case "PreToolUse":
		itemID := extractItemID(input.ToolInput.Args)
		title := ""
		if itemID != "" {
			title = fetchItemTitle(client, trackerURL, itemID)
		}
		sendHeartbeat(client, trackerURL, agentName, hostName, status, itemID, title)

	case "PostToolUse":
		if finalSkills[skill] {
			sendHeartbeat(client, trackerURL, agentName, hostName, "idle", "", "")
			cleanupProcessingItems(client, trackerURL, agentName)
		} else if postStatus, ok := postSkillStatus[skill]; ok {
			sendHeartbeat(client, trackerURL, agentName, hostName, postStatus, "", "")
		}
	}

	// Output empty JSON — no hook decision needed (allow everything).
	fmt.Println("{}")
}

// findTrackerURL locates .mcp.json starting from cwd and walking up to the
// git root, then extracts the tracker server URL.
func findTrackerURL(cwd string) string {
	dir := cwd
	for {
		mcpPath := filepath.Join(dir, ".mcp.json")
		data, err := os.ReadFile(mcpPath)
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
			break
		}
		dir = parent
	}
	return ""
}

func extractItemID(args string) string {
	match := itemIDRe.FindString(args)
	return match
}

func fetchItemTitle(client *http.Client, baseURL, itemID string) string {
	resp, err := client.Get(baseURL + "/api/items/" + itemID)
	if err != nil || resp.StatusCode != 200 {
		return ""
	}
	defer resp.Body.Close()
	var item struct {
		Title string `json:"title"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&item); err != nil {
		return ""
	}
	return item.Title
}

func sendHeartbeat(client *http.Client, baseURL, agent, host, status, itemID, itemTitle string) {
	payload := map[string]string{
		"agent":  agent,
		"host":   host,
		"status": status,
	}
	if itemID != "" {
		payload["item_id"] = itemID
	}
	if itemTitle != "" {
		payload["item_title"] = itemTitle
	}

	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", baseURL+"/api/agents/heartbeat", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err == nil {
		resp.Body.Close()
	}
}

func cleanupProcessingItems(client *http.Client, baseURL, agentName string) {
	// List items tagged "processing".
	resp, err := client.Get(baseURL + "/api/items?tag=processing")
	if err != nil || resp.StatusCode != 200 {
		return
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return
	}

	var items []trackerItem
	if err := json.Unmarshal(data, &items); err != nil {
		return
	}

	for _, item := range items {
		if item.AssignedTo != agentName {
			continue
		}
		// Remove "processing" tag, clear assigned_to.
		newTags := make([]string, 0, len(item.Tags))
		for _, t := range item.Tags {
			if t != "processing" {
				newTags = append(newTags, t)
			}
		}
		update := map[string]any{
			"tags":        newTags,
			"assigned_to": "",
		}
		body, _ := json.Marshal(update)
		req, _ := http.NewRequest("PATCH", baseURL+"/api/items/"+item.ID, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r, err := client.Do(req)
		if err == nil {
			r.Body.Close()
		}
	}
}
