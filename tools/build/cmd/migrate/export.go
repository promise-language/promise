package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// idRef matches a tracker citation: T1234, B0043, D0009. Used to build the
// cross-reference graph, which the triage phase needs in order to catch a
// published item citing a withheld one.
var idRef = regexp.MustCompile(`\b([TBD][0-9]{4})\b`)

// Corpus is the export's index: everything the later phases read, in one file,
// so no phase has to go back to the tracker.
type Corpus struct {
	ExportedAt string         `json:"exported_at"`
	Counts     map[string]int `json:"counts"`
	Entries    []Entry        `json:"entries"`
}

// Entry is one item as the migration sees it.
type Entry struct {
	ID       string   `json:"id"`
	Type     string   `json:"type"`
	Status   string   `json:"status"`
	Priority string   `json:"priority"`
	Title    string   `json:"title"`
	Tags     []string `json:"tags"`
	Live     bool     `json:"live"`
	BodyPath string   `json:"body_path,omitempty"`
	RawPath  string   `json:"raw_path"`
	// Cites is every other tracker item this one's published body names.
	Cites []string `json:"cites,omitempty"`
	// Patches records captured worktree diffs — the only copy of work that
	// lives in an arena this tool cannot reach.
	Patches []PatchRecord `json:"patches,omitempty"`
}

// PatchRecord is a harvested patch, with where it landed on disk.
type PatchRecord struct {
	Step       string   `json:"step"`
	Bytes      int      `json:"bytes"`
	Hash       string   `json:"hash"`
	BaseSHA    string   `json:"base_sha"`
	BaseBranch string   `json:"base_branch"`
	Agent      string   `json:"agent"`
	UploadedAt string   `json:"uploaded_at"`
	Untracked  []string `json:"untracked,omitempty"`
	SavedPath  string   `json:"saved_path,omitempty"`
}

// patchFileName names a harvested patch so that no two can collide.
//
// One item commonly carries several patches — one per flow step, and sometimes
// several from the same step as a step was re-run. Naming a file by item and
// step alone silently overwrote all but the last: T2108 has three
// implementation diffs of 116 KB, 123 KB and 142 KB, and a name without the
// hash kept one of them. The content hash is what makes each patch distinct, so
// it is what distinguishes the files.
func patchFileName(id string, p Patch) string {
	step := p.Step
	if step == "" {
		step = "unknown"
	}
	h := p.Hash
	if len(h) > 12 {
		h = h[:12]
	}
	return fmt.Sprintf("%s.%s.%s.patch", id, step, h)
}

// renderBody builds the exact markdown a public issue would carry.
//
// It reads a CLOSED set of fields — description and plan — and nothing else.
// The tracker record also holds a lease naming an absolute home path, agent
// identifiers, per-account cost and an event log quoting other agents; none of
// them is read here, and none can be added by accident, because this function
// takes the typed Item rather than the raw JSON.
//
// The shape matches what the tracker's own publish_preview produces, so the 54
// items already published read the same as the ones this migration files.
func renderBody(it Item) string {
	var b strings.Builder
	b.WriteString(strings.TrimRight(it.Description, "\n"))
	if strings.TrimSpace(it.Plan) != "" {
		b.WriteString("\n\n## Plan\n\n")
		b.WriteString(strings.TrimRight(it.Plan, "\n"))
	}
	b.WriteString("\n\n---\n\n")
	// The marker is what a repair pass recognises an already-filed item by,
	// when a ledger line is lost. It carries the tracker ID and nothing else.
	fmt.Fprintf(&b, "<!-- tracker:%s -->\n", it.ID)
	return b.String()
}

// citations returns the tracker IDs a body names, excluding the item's own.
func citations(id, body string) []string {
	seen := map[string]bool{id: true}
	var out []string
	for _, m := range idRef.FindAllStringSubmatch(body, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	sort.Strings(out)
	return out
}

func runExport(root, dir string, liveOnly bool) error {
	base, err := trackerURL(root)
	if err != nil {
		return err
	}
	c := newClient(base)

	for _, sub := range []string{"raw", "bodies", "patches"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return err
		}
	}

	index, err := c.ListItems()
	if err != nil {
		return err
	}
	fmt.Printf("tracker holds %d items\n", len(index))

	corpus := Corpus{
		ExportedAt: nowStamp(),
		Counts:     map[string]int{},
	}

	var failed []string
	for i, stub := range index {
		if liveOnly && !stub.Live() {
			continue
		}
		it, raw, err := c.GetItem(stub.ID)
		if err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", stub.ID, err))
			continue
		}

		rawPath := filepath.Join("raw", it.ID+".json")
		if err := os.WriteFile(filepath.Join(dir, rawPath), raw, 0o644); err != nil {
			return err
		}

		e := Entry{
			ID: it.ID, Type: it.Type, Status: it.Status, Priority: it.Priority,
			Title: it.Title, Tags: it.Tags, Live: it.Live(), RawPath: rawPath,
		}

		// A body is rendered for every item, live or not: the archive export
		// (decision B) needs the closed corpus in readable form, and the guard
		// phase screens whatever might be committed, not only what is filed.
		body := renderBody(it)
		bodyPath := filepath.Join("bodies", it.ID+".md")
		if err := os.WriteFile(filepath.Join(dir, bodyPath), []byte(body), 0o644); err != nil {
			return err
		}
		e.BodyPath = bodyPath
		e.Cites = citations(it.ID, body)

		for _, p := range it.Patches {
			rec := PatchRecord{
				Step: p.Step, Bytes: p.Size, BaseSHA: p.BaseSHA,
				BaseBranch: p.BaseBranch, Agent: p.Agent, UploadedAt: p.UploadedAt,
				Hash: p.Hash, Untracked: p.Untracked,
			}
			if p.Size > 0 && p.Hash != "" {
				data, err := c.GetPatch(it.ID, p.Hash)
				if err != nil {
					failed = append(failed, fmt.Sprintf("%s patch %s: %v", it.ID, p.Step, err))
				} else {
					sp := filepath.Join("patches", patchFileName(it.ID, p))
					if err := os.WriteFile(filepath.Join(dir, sp), data, 0o644); err != nil {
						return err
					}
					rec.SavedPath = sp
				}
			}
			e.Patches = append(e.Patches, rec)
		}

		corpus.Entries = append(corpus.Entries, e)
		corpus.Counts[it.Status]++
		if it.Live() {
			corpus.Counts["_live"]++
		}
		if len(e.Patches) > 0 {
			corpus.Counts["_with_patch"]++
		}

		if (i+1)%200 == 0 {
			fmt.Printf("  exported %d/%d\n", i+1, len(index))
		}
	}

	sort.Slice(corpus.Entries, func(a, b int) bool { return corpus.Entries[a].ID < corpus.Entries[b].ID })
	corpus.Counts["_total"] = len(corpus.Entries)

	out, err := json.MarshalIndent(corpus, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "corpus.json"), out, 0o644); err != nil {
		return err
	}

	fmt.Printf("\nexported %d items to %s\n", len(corpus.Entries), dir)
	fmt.Printf("  live (filed as issues): %d\n", corpus.Counts["_live"])
	fmt.Printf("  carrying a patch:       %d\n", corpus.Counts["_with_patch"])
	if len(failed) > 0 {
		fmt.Printf("\n%d fetch failures:\n", len(failed))
		for _, f := range failed {
			fmt.Printf("  %s\n", f)
		}
		return fmt.Errorf("export incomplete: %d items or patches could not be read", len(failed))
	}
	return nil
}
