package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// runReport prints what export and guard established. It reads the corpus and
// (when present) the verdicts, and writes nothing.
func runReport(dir string) error {
	corpus, err := loadCorpus(dir)
	if err != nil {
		return err
	}

	fmt.Printf("corpus %s  (exported %s)\n\n", dir, corpus.ExportedAt)

	// ---- inventory by status ----
	statuses := make([]string, 0, len(corpus.Counts))
	for k := range corpus.Counts {
		if !strings.HasPrefix(k, "_") {
			statuses = append(statuses, k)
		}
	}
	sort.Slice(statuses, func(a, b int) bool {
		return corpus.Counts[statuses[a]] > corpus.Counts[statuses[b]]
	})
	fmt.Println("INVENTORY")
	for _, s := range statuses {
		live := ""
		if s == "open" || s == "needs_answer" || s == "in_progress" {
			live = "  <- filed as an issue"
		}
		fmt.Printf("  %-18s %5d%s\n", s, corpus.Counts[s], live)
	}
	fmt.Printf("  %-18s %5d\n", "TOTAL", corpus.Counts["_total"])
	fmt.Printf("  %-18s %5d\n\n", "live", corpus.Counts["_live"])

	// ---- captured work ----
	// Captured work. Only the LIVE items are listed one by one: those are the
	// diffs that still have somewhere to go. A closed item's patch is history —
	// it is counted, kept on disk, and not printed, because 669 lines of it
	// would bury the handful that need a decision.
	type pw struct {
		id, step, base, path string
		bytes                int
	}
	var liveP []pw
	closedN, closedBytes, liveBytes := 0, 0, 0
	for _, e := range corpus.Entries {
		for _, p := range e.Patches {
			if p.Bytes == 0 {
				continue
			}
			if e.Live {
				liveBytes += p.Bytes
				liveP = append(liveP, pw{e.ID, p.Step, p.BaseSHA, p.SavedPath, p.Bytes})
			} else {
				closedN++
				closedBytes += p.Bytes
			}
		}
	}
	if len(liveP)+closedN > 0 {
		sort.Slice(liveP, func(a, b int) bool { return liveP[a].bytes > liveP[b].bytes })
		fmt.Println("CAPTURED WORK — arena patches recovered locally")
		fmt.Printf("  on LIVE items   %4d patches  %9d B   <- still has somewhere to go\n", len(liveP), liveBytes)
		fmt.Printf("  on closed items %4d patches  %9d B   (history; kept on disk)\n\n", closedN, closedBytes)
		for _, p := range liveP {
			base := p.base
			if len(base) > 8 {
				base = base[:8]
			}
			fmt.Printf("    %-7s %-15s base=%-9s %8d B  %s\n", p.id, p.step, base, p.bytes, p.path)
		}
		fmt.Println()
	}

	// ---- tag drift ----
	tagCount := map[string]int{}
	for _, e := range corpus.Entries {
		if !e.Live {
			continue
		}
		for _, t := range e.Tags {
			tagCount[t]++
		}
	}
	fmt.Printf("TAGS on live items: %d distinct\n\n", len(tagCount))

	// ---- cross references into the closed corpus ----
	live := map[string]bool{}
	known := map[string]bool{}
	for _, e := range corpus.Entries {
		known[e.ID] = true
		if e.Live {
			live[e.ID] = true
		}
	}
	danglingTo := map[string]int{}
	toClosed := 0
	for _, e := range corpus.Entries {
		if !e.Live {
			continue
		}
		for _, c := range e.Cites {
			switch {
			case live[c]:
			case known[c]:
				toClosed++
			default:
				danglingTo[c]++
			}
		}
	}
	fmt.Println("CROSS REFERENCES from live bodies")
	fmt.Printf("  to another live item     %5d  -> rewritten to #N at publish\n", countLiveCites(corpus, live))
	fmt.Printf("  to a closed item         %5d  -> left as T####, resolved by the archive index\n", toClosed)
	fmt.Printf("  to an unknown id         %5d\n\n", len(danglingTo))

	// ---- verdicts, if the guard has run ----
	vb, err := os.ReadFile(filepath.Join(dir, "verdicts.json"))
	if err != nil {
		fmt.Println("GUARD — not run yet (`migrate guard`)")
		return nil
	}
	var v Verdicts
	if err := json.Unmarshal(vb, &v); err != nil {
		return fmt.Errorf("parsing verdicts: %w", err)
	}
	fmt.Printf("GUARD — screened %s via %s\n", v.ScreenedAt, v.Guard)
	fmt.Printf("  clean       %5d   (live: %d)\n", v.Counts["clean"], v.Counts["live_clean"])
	fmt.Printf("  refused     %5d   (live: %d)\n", v.Counts["refused"], v.Counts["live_refused"])
	fmt.Printf("  unexamined  %5d   (live: %d)\n", v.Counts["unexamined"], v.Counts["live_unexamined"])

	reasons := map[string]int{}
	var blocked []Verdict
	for _, it := range v.Items {
		if it.Outcome == "clean" {
			continue
		}
		reasons[normalizeReason(it.Reason)]++
		if it.Live {
			blocked = append(blocked, it)
		}
	}
	if len(reasons) > 0 {
		fmt.Println("\n  reasons:")
		type rc struct {
			r string
			n int
		}
		var rs []rc
		for r, n := range reasons {
			rs = append(rs, rc{r, n})
		}
		sort.Slice(rs, func(a, b int) bool { return rs[a].n > rs[b].n })
		for _, r := range rs {
			fmt.Printf("    %4d  %s\n", r.n, r.r)
		}
	}
	if len(blocked) > 0 {
		fmt.Printf("\n  LIVE items needing revision before they can be filed (%d):\n", len(blocked))
		for _, it := range blocked {
			fmt.Printf("    %-7s %s\n", it.ID, it.Reason)
		}
	}
	return nil
}

func countLiveCites(c Corpus, live map[string]bool) int {
	n := 0
	for _, e := range c.Entries {
		if !e.Live {
			continue
		}
		for _, cite := range e.Cites {
			if live[cite] {
				n++
			}
		}
	}
	return n
}

// normalizeReason strips the quoted fragment from a guard refusal so reasons
// can be counted by kind.
//
// The fragment is what makes a refusal actionable, and it is also the refused
// text itself — so it stays in verdicts.json for the person revising the item,
// and never reaches a summary that might be pasted somewhere public. A refusal
// does not travel.
func normalizeReason(r string) string {
	if head, _, ok := strings.Cut(r, " — found "); ok {
		return strings.TrimSpace(head)
	}
	if head, _, ok := strings.Cut(r, `found "`); ok {
		return strings.TrimSpace(head)
	}
	return r
}
