// Package heartbeat implements the Skill-tool heartbeat hook logic.
//
// Originally lived in tools/hooks/skill_heartbeat.go and was invoked via
// `go run` on every Skill PreToolUse/PostToolUse. It now compiles into
// bin/guard (T0252) so hooks no longer pay the per-invocation go-run cost.
package heartbeat

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Input is the subset of Claude Code hook JSON the heartbeat cares about.
type Input struct {
	HookEventName string
	CWD           string
	Skill         string
	Args          string
}

type mcpConfig struct {
	MCPServers map[string]struct {
		URL string `json:"url"`
	} `json:"mcpServers"`
}

type trackerItem struct {
	ID         string   `json:"id"`
	AssignedTo string   `json:"assigned_to"`
	Tags       []string `json:"tags"`
}

// skillStatus maps skill names to heartbeat status on PreToolUse.
var skillStatus = map[string]string{
	"do":       "planning",
	"plan":     "planning",
	"review":   "reviewing",
	"coverage": "coverage",
	"commit":   "committing",
	"inspect":  "reviewing",
	"stress":   "stress-testing",
}

// postSkillStatus maps skill names to heartbeat status on PostToolUse
// (only entries that trigger a phase transition on completion).
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

// Run performs the heartbeat POST for the given hook input. All failures
// are silently swallowed — heartbeats are best-effort and must never block
// the Skill invocation.
func Run(in Input) {
	if in.Skill == "" {
		return
	}
	status, known := skillStatus[in.Skill]
	if !known {
		return
	}

	trackerURL := findTrackerURL(in.CWD)
	if trackerURL == "" {
		return
	}

	agentName := filepath.Base(in.CWD)
	hostName, _ := os.Hostname()
	client := &http.Client{Timeout: 5 * time.Second}

	switch in.HookEventName {
	case "PreToolUse":
		itemID := itemIDRe.FindString(in.Args)
		title := ""
		if itemID != "" {
			title = fetchItemTitle(client, trackerURL, itemID)
		}
		sendHeartbeat(client, trackerURL, agentName, hostName, status, itemID, title)

	case "PostToolUse":
		if finalSkills[in.Skill] {
			sendHeartbeat(client, trackerURL, agentName, hostName, "idle", "", "")
			cleanupProcessingItems(client, trackerURL, agentName)
		} else if postStatus, ok := postSkillStatus[in.Skill]; ok {
			sendHeartbeat(client, trackerURL, agentName, hostName, postStatus, "", "")
		}
	}
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
