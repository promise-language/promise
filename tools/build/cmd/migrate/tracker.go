package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// trackerURL resolves the tracker's base address without ever compiling one in.
//
// Order: PROMISE_TRACKER_URL, then .flow/context.json. Both sit outside git.
// A private address written into a tracked file is published by the next push
// — the guard refuses that string for exactly this reason — so the address is
// read at runtime and never written to a tracked path by this tool.
func trackerURL(root string) (string, error) {
	if v := strings.TrimSpace(os.Getenv("PROMISE_TRACKER_URL")); v != "" {
		return strings.TrimRight(v, "/"), nil
	}
	b, err := os.ReadFile(filepath.Join(root, ".flow", "context.json"))
	if err != nil {
		return "", fmt.Errorf("no tracker address: set PROMISE_TRACKER_URL, or provide .flow/context.json (%w)", err)
	}
	var ctx struct {
		TrackerURL string `json:"tracker_url"`
	}
	if err := json.Unmarshal(b, &ctx); err != nil {
		return "", fmt.Errorf("parsing .flow/context.json: %w", err)
	}
	if ctx.TrackerURL == "" {
		return "", fmt.Errorf(".flow/context.json has no tracker_url; set PROMISE_TRACKER_URL")
	}
	if _, err := url.Parse(ctx.TrackerURL); err != nil {
		return "", fmt.Errorf("tracker_url is not a URL: %w", err)
	}
	return strings.TrimRight(ctx.TrackerURL, "/"), nil
}

// Item is the closed set of fields this migration reads.
//
// It is deliberately NOT the whole record. The tracker's item JSON also carries
// flow_lease.worktree (an absolute home path), agent identifiers (machine
// names), per-account cost, and an event log quoting other agents — every one
// of them a disclosure category, and none of them anything a public issue
// needs. Fields that are not declared here cannot reach a rendered body by
// accident, which is the point of declaring them rather than passing a map
// around.
type Item struct {
	ID          string   `json:"id"`
	Type        string   `json:"type"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Plan        string   `json:"plan"`
	Status      string   `json:"status"`
	Priority    string   `json:"priority"`
	Tags        []string `json:"tags"`
	CreatedAt   string   `json:"created_at"`
	UpdatedAt   string   `json:"updated_at"`
	Patches     []Patch  `json:"patches"`
}

// Patch is a worktree diff the flow captured from an arena. The content is not
// inlined: it is fetched separately and written beside the corpus, because a
// captured patch is the only copy of work that lives on a machine this tool
// cannot reach.
type Patch struct {
	Hash     string `json:"hash"`
	Filename string `json:"filename"`
	Size     int    `json:"size"`
	// Step is the flow step that captured the diff — plan, implementation,
	// review, coverage, park, release. One item commonly has several, one per
	// step, and they are different diffs rather than revisions of one.
	Step       string   `json:"step"`
	BaseSHA    string   `json:"base_sha"`
	BaseBranch string   `json:"base_branch"`
	Agent      string   `json:"agent"`
	UploadedAt string   `json:"uploaded_at"`
	Untracked  []string `json:"untracked"`
}

// Live reports whether the item is one this migration files as a GitHub issue.
// Decision A: live items only — open and needs_answer. Everything else is
// preserved by the archive index instead.
func (it Item) Live() bool {
	return it.Status == "open" || it.Status == "in_progress" || it.Status == "needs_answer"
}

type client struct {
	base string
	http *http.Client
}

func newClient(base string) *client {
	return &client{base: base, http: &http.Client{Timeout: 120 * time.Second}}
}

func (c *client) get(path string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "*/*")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("GET %s: reading body: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s: %s", path, resp.Status, strings.TrimSpace(string(body)))
	}
	return body, nil
}

// ListItems returns every item the tracker holds, newest field set and all.
func (c *client) ListItems() ([]Item, error) {
	body, err := c.get("/api/items")
	if err != nil {
		return nil, err
	}
	var items []Item
	if err := json.Unmarshal(body, &items); err != nil {
		return nil, fmt.Errorf("parsing item list: %w", err)
	}
	return items, nil
}

// GetItem returns one item in full. The list endpoint omits description and
// plan, so the body of every item has to be fetched individually.
func (c *client) GetItem(id string) (Item, []byte, error) {
	body, err := c.get("/api/items/" + url.PathEscape(id))
	if err != nil {
		return Item{}, nil, err
	}
	var it Item
	if err := json.Unmarshal(body, &it); err != nil {
		return Item{}, nil, fmt.Errorf("parsing item %s: %w", id, err)
	}
	return it, body, nil
}

// GetPatch returns a captured worktree diff.
func (c *client) GetPatch(id, hash string) ([]byte, error) {
	return c.get("/api/items/" + url.PathEscape(id) + "/patches/" + url.PathEscape(hash))
}
