package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// guardRefusal is a refusal the supplied guard returned. It is an error so a
// caller cannot mistake it for a pass, and it carries the guard's own reason
// because a refusal that does not say what it found is indistinguishable from
// a guard that gave up.
type guardRefusal struct {
	ID     string
	Reason string
}

func (g guardRefusal) Error() string {
	return fmt.Sprintf("%s: disclosure refused: %s", g.ID, g.Reason)
}

// Verdict is one item's screening result.
type Verdict struct {
	ID string `json:"id"`
	// Outcome is clean, refused, or unexamined. There is no fourth value and
	// no confidence score: a guard that returns a degree of concern has moved
	// the decision to the party trying to publish.
	Outcome string `json:"outcome"`
	Reason  string `json:"reason,omitempty"`
	Live    bool   `json:"live"`
	Status  string `json:"status"`
}

// Verdicts is the guard phase's output.
type Verdicts struct {
	ScreenedAt string         `json:"screened_at"`
	Guard      string         `json:"guard"`
	Counts     map[string]int `json:"counts"`
	Items      []Verdict      `json:"items"`
}

// runGuard screens every exported body through the guard the workspace
// supplies.
//
// This tool does not implement a guard and must not: a guard authored by the
// party it constrains is a guard that party grants itself. It shells out to
// bin/tool-guard — installed by workspace, hardlinked with the flow binaries,
// digest-tracked in .workspace/project.json — handing it the same PreToolUse
// payload the harness hands it for an agent's own `gh issue create`.
//
// This is a SCREEN, not the guard on the write. The bodies here still carry
// T#### citations that the publishing phase rewrites to #N, so the bytes are
// not yet final. The publishing phase examines the final bytes again, at the
// seam where they are both final and not yet sent. Screening early is worth
// doing anyway: asking what a write would disclose is the same question the
// write asks, one step earlier, and it publishes nothing.
//
// It fails closed. A guard that cannot answer — missing binary, non-zero exit
// that is not a refusal, a timeout — is recorded as unexamined and counted with
// the refusals, never as a pass. Not publishing something publishable wastes a
// step; publishing something unpublishable cannot be undone.
func runGuard(root, dir string, jobs int) error {
	guard := filepath.Join(root, "bin", "tool-guard")
	if runtime.GOOS == "windows" {
		guard += ".exe"
	}
	if _, err := os.Stat(guard); err != nil {
		return fmt.Errorf("the supplied disclosure guard is not installed at bin/tool-guard: %w\n"+
			"nothing is screened and nothing may be published; run `workspace update` to install it", err)
	}

	corpus, err := loadCorpus(dir)
	if err != nil {
		return err
	}
	if jobs < 1 {
		jobs = 1
	}

	type job struct{ e Entry }
	in := make(chan job)
	out := make(chan Verdict, len(corpus.Entries))

	var wg sync.WaitGroup
	for range jobs {
		wg.Go(func() {
			for j := range in {
				out <- screen(guard, dir, j.e)
			}
		})
	}
	go func() {
		for _, e := range corpus.Entries {
			if e.BodyPath == "" {
				continue
			}
			in <- job{e}
		}
		close(in)
	}()
	wg.Wait()
	close(out)

	v := Verdicts{ScreenedAt: nowStamp(), Guard: "bin/tool-guard", Counts: map[string]int{}}
	for r := range out {
		v.Items = append(v.Items, r)
		v.Counts[r.Outcome]++
		if r.Live {
			v.Counts["live_"+r.Outcome]++
		}
	}
	sort.Slice(v.Items, func(a, b int) bool { return v.Items[a].ID < v.Items[b].ID })

	blob, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "verdicts.json"), blob, 0o644); err != nil {
		return err
	}

	fmt.Printf("screened %d bodies through %s\n\n", len(v.Items), guard)
	fmt.Printf("  clean       %5d   (live: %d)\n", v.Counts["clean"], v.Counts["live_clean"])
	fmt.Printf("  refused     %5d   (live: %d)\n", v.Counts["refused"], v.Counts["live_refused"])
	fmt.Printf("  unexamined  %5d   (live: %d)  <- treated as refused\n", v.Counts["unexamined"], v.Counts["live_unexamined"])
	fmt.Printf("\nverdicts written to %s\n", filepath.Join(dir, "verdicts.json"))
	return nil
}

// screen asks the guard about one item, about a write it does not perform.
func screen(guard, dir string, e Entry) Verdict {
	v := Verdict{ID: e.ID, Live: e.Live, Status: e.Status}

	bodyAbs, err := filepath.Abs(filepath.Join(dir, e.BodyPath))
	if err != nil {
		v.Outcome, v.Reason = "unexamined", err.Error()
		return v
	}

	// The command names the title, the labels and the body file — every string
	// the create would publish. The guard resolves --body-file itself rather
	// than stopping at the literal arguments, which is what keeps a refusal
	// from being one `--body-file` away from avoidable.
	args := []string{"gh", "issue", "create", "--title", e.Title, "--body-file", bodyAbs}
	for _, t := range e.Tags {
		args = append(args, "--label", t)
	}
	payload, err := json.Marshal(map[string]any{
		"hook_event_name": "PreToolUse",
		"tool_name":       "Bash",
		"tool_input":      map[string]any{"command": shellJoin(args)},
	})
	if err != nil {
		v.Outcome, v.Reason = "unexamined", err.Error()
		return v
	}

	// No -agent flag: it names the harness whose tool vocabulary the payload is
	// written in, and it already defaults to claude, which is the vocabulary
	// built above. Spelling the default out added nothing and made a disclosure
	// check read like the spawning of a model turn.
	cmd := exec.Command(guard)
	cmd.Stdin = bytes.NewReader(payload)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		v.Outcome, v.Reason = "unexamined", "starting the guard: "+err.Error()
		return v
	}
	go func() { done <- cmd.Wait() }()

	select {
	case err = <-done:
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		v.Outcome, v.Reason = "unexamined", "the guard did not answer within 30s"
		return v
	}

	reason := strings.TrimSpace(stdout.String() + "\n" + stderr.String())
	reason = strings.TrimSpace(reason)

	switch code := cmd.ProcessState.ExitCode(); {
	case err == nil && code == 0:
		v.Outcome = "clean"
	case code == 2:
		v.Outcome = "refused"
		v.Reason = firstLine(reason)
	default:
		// Any other exit is an answer the guard did not give. Fail closed.
		v.Outcome = "unexamined"
		v.Reason = fmt.Sprintf("guard exited %d: %s", code, firstLine(reason))
	}
	return v
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// shellJoin renders an argv as a POSIX shell command line. The guard parses a
// command string, so the payload has to carry one; single-quoting with the
// standard '\” escape is exact for every byte.
func shellJoin(args []string) string {
	var b strings.Builder
	for i, a := range args {
		if i > 0 {
			b.WriteByte(' ')
		}
		if a != "" && !strings.ContainsAny(a, " \t\n\"'\\$`&;|<>()*?[]{}!#~=") {
			b.WriteString(a)
			continue
		}
		b.WriteByte('\'')
		b.WriteString(strings.ReplaceAll(a, "'", `'\''`))
		b.WriteByte('\'')
	}
	return b.String()
}

func loadCorpus(dir string) (Corpus, error) {
	var c Corpus
	b, err := os.ReadFile(filepath.Join(dir, "corpus.json"))
	if err != nil {
		return c, fmt.Errorf("no corpus at %s: run `migrate export` first (%w)", dir, err)
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("parsing corpus: %w", err)
	}
	return c, nil
}

func nowStamp() string { return time.Now().UTC().Format(time.RFC3339) }
