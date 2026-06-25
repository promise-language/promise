package common

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// release_cut.go implements `bin/release cut next|stable` — the gated release
// orchestrator (T0943). It encodes the manual release procedure
// (docs/release-automation.md §6.2) as enforced preflight gates so neither a
// maintainer nor an agent can skip a step: a tag/push happens only when every
// gate is green, `--dry-run` changes nothing, and a gate may be bypassed only
// with `--reason "<text>"`, which is recorded into the tag/commit message so any
// override is auditable.
//
//   cut next   — refresh the moving `epoch-next` pre-release at HEAD.
//   cut stable — derive the epoch (cut stable owns the number — no --epoch flag),
//                run all gates, tag, push, then bump catalog.toml to the next
//                same-year epoch on main and push.
//
// `git`/`gh` are behind the cutGit/cutGH interfaces (mirroring the existing
// releaseUploader/blobFetcher stub pattern) so release_cut_test.go is hermetic.

// requiredPlatforms is the CI/release matrix gate-7 evaluates and gate-4 checks
// blob hosting for. darwin-amd64 is deferred (docs/release-automation.md §7).
var requiredPlatforms = []string{"linux-amd64", "darwin-arm64", "windows-amd64"}

// ── git/gh seams ────────────────────────────────────────────────────────────

// cutGit is the git surface the gates need. The production impl (shellGit)
// shells out to `git`; tests substitute a fake.
type cutGit interface {
	HeadSHA() (string, error)                             // git rev-parse HEAD
	CurrentBranch() (string, error)                       // git rev-parse --abbrev-ref HEAD
	CleanTree() (bool, error)                             // git status --porcelain == ""
	IsAncestor(sha, ref string) (bool, error)             // git merge-base --is-ancestor sha ref
	Fetch() error                                         // git fetch origin --tags
	TagExists(tag string) (bool, error)                   // git rev-parse -q --verify refs/tags/<tag>
	TagSHA(tag string) (string, error)                    // git rev-list -n1 <tag>
	ListEpochTags() ([]string, error)                     // git tag --list 'epoch-*'
	CreateTag(tag, message, sha string, force bool) error // git tag -a [-f] <tag> -m <msg> <sha>
	PushTag(tag string, force bool) error                 // git push [-f] origin <tag>
	CommitFile(path, message string) error                // git add <path> && git commit -m <msg>
	PushBranch(branch string) error                       // git push origin <branch>
	ResolveSHA(ref string) (string, error)                // git rev-parse <ref>^{commit}
	LogSubjects(fromRef, toSHA string) ([]string, error)  // git log --no-merges --pretty=%s <fromRef>..<toSHA>
}

// defaultCutGit builds the production cutGit for a repo root. Tests swap it.
var defaultCutGit = func(root string) cutGit { return shellGit{root: root} }

type shellGit struct{ root string }

func (g shellGit) HeadSHA() (string, error) { return RunOutputIn(g.root, "git", "rev-parse", "HEAD") }
func (g shellGit) CurrentBranch() (string, error) {
	return RunOutputIn(g.root, "git", "rev-parse", "--abbrev-ref", "HEAD")
}
func (g shellGit) CleanTree() (bool, error) {
	out, err := RunOutputIn(g.root, "git", "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "", nil
}
func (g shellGit) IsAncestor(sha, ref string) (bool, error) {
	err := RunIn(g.root, "git", "merge-base", "--is-ancestor", sha, ref)
	if err == nil {
		return true, nil
	}
	// `--is-ancestor` exits 1 for "not an ancestor" — a clean negative, not a
	// tool failure. Any other non-zero (or non-exit error) is a real error.
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}
func (g shellGit) Fetch() error { return RunIn(g.root, "git", "fetch", "origin", "--tags") }
func (g shellGit) TagExists(tag string) (bool, error) {
	if _, err := RunOutputIn(g.root, "git", "rev-parse", "-q", "--verify", "refs/tags/"+tag); err == nil {
		return true, nil
	} else {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return false, nil // non-zero with -q => ref absent
		}
		return false, err
	}
}
func (g shellGit) TagSHA(tag string) (string, error) {
	return RunOutputIn(g.root, "git", "rev-list", "-n", "1", tag)
}
func (g shellGit) ListEpochTags() ([]string, error) {
	out, err := RunOutputIn(g.root, "git", "tag", "--list", "epoch-*")
	if err != nil {
		return nil, err
	}
	var tags []string
	for _, l := range strings.Split(out, "\n") {
		if s := strings.TrimSpace(l); s != "" {
			tags = append(tags, s)
		}
	}
	return tags, nil
}
func (g shellGit) CreateTag(tag, message, sha string, force bool) error {
	args := []string{"tag", "-a"}
	if force {
		args = append(args, "-f")
	}
	args = append(args, tag, "-m", message, sha)
	return RunIn(g.root, "git", args...)
}
func (g shellGit) PushTag(tag string, force bool) error {
	args := []string{"push"}
	if force {
		args = append(args, "-f")
	}
	args = append(args, "origin", tag)
	return RunIn(g.root, "git", args...)
}
func (g shellGit) CommitFile(path, message string) error {
	if err := RunIn(g.root, "git", "add", path); err != nil {
		return err
	}
	return RunIn(g.root, "git", "commit", "-m", message)
}
func (g shellGit) PushBranch(branch string) error {
	return RunIn(g.root, "git", "push", "origin", branch)
}
func (g shellGit) ResolveSHA(ref string) (string, error) {
	// `^{commit}` peels annotated tags / branch refs to the underlying commit so
	// callers always get a full commit SHA regardless of what kind of ref they passed.
	return RunOutputIn(g.root, "git", "rev-parse", ref+"^{commit}")
}
func (g shellGit) LogSubjects(fromRef, toSHA string) ([]string, error) {
	// `<fromRef>..<toSHA>` when there is a previous epoch, else the whole history
	// reachable from toSHA (first release). `--no-merges` drops merge commits;
	// `%s` is the commit subject (first line).
	rangeArg := toSHA
	if fromRef != "" {
		rangeArg = fromRef + ".." + toSHA
	}
	out, err := RunOutputIn(g.root, "git", "log", "--no-merges", "--pretty=format:%s", rangeArg)
	if err != nil {
		return nil, err
	}
	var subjects []string
	for _, line := range strings.Split(out, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			subjects = append(subjects, s)
		}
	}
	return subjects, nil
}

// RemoteBranchSHA returns origin's tip for branch (or tag) via `git ls-remote`
// (an authoritative network query — no reliance on a possibly-stale local
// tracking ref), or "" if absent on origin. Prefers refs/heads over refs/tags
// when both match. `bin/release ci` compares this against local HEAD because
// workflow_dispatch checks out this tip, not the caller's local commit. Not part
// of cutGit — only the `ci` subcommand needs it.
func (g shellGit) RemoteBranchSHA(name string) (string, error) {
	out, err := RunOutputIn(g.root, "git", "ls-remote", "origin",
		"refs/heads/"+name, "refs/tags/"+name)
	if err != nil {
		return "", err
	}
	if out == "" {
		return "", nil
	}
	// Prefer refs/heads over refs/tags when both exist.
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.HasPrefix(fields[1], "refs/heads/") {
			return fields[0], nil
		}
	}
	// Fall back to first result (a tag match).
	line, _, _ := strings.Cut(out, "\n")
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return "", fmt.Errorf("unexpected `git ls-remote` output: %q", out)
	}
	return fields[0], nil
}

// PushTagAt pushes a lightweight tag directly to origin at sha without creating
// a local tag — used by `ci --commit-hash` to create a pin ref for dispatch.
func (g shellGit) PushTagAt(tag, sha string) error {
	return RunIn(g.root, "git", "push", "origin", sha+":refs/tags/"+tag)
}

// DeleteRemoteTag deletes a tag from origin — used to clean up pin tags after
// `ci --commit-hash` dispatches successfully.
func (g shellGit) DeleteRemoteTag(tag string) error {
	return RunIn(g.root, "git", "push", "origin", "--delete", "refs/tags/"+tag)
}

// ghRun is one GitHub Actions run row (`gh run list --json …`). ghJob is one
// per-run job (`gh run view --json jobs`).
type ghRun struct {
	DatabaseID int64  `json:"databaseId"`
	HeadSHA    string `json:"headSha"`
	HeadBranch string `json:"headBranch"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

type ghJob struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

// cutGH is the GitHub-Actions surface the gates need. The production impl
// (shellGH) shells out to `gh`; tests substitute a fake.
type cutGH interface {
	WorkflowRuns(workflow string, limit int) ([]ghRun, error)
	RunJobs(runID int64) ([]ghJob, error)
	DispatchWorkflow(workflow, ref string, inputs map[string]string) error
}

// defaultCutGH is the production cutGH. Tests swap it.
var defaultCutGH cutGH = shellGH{}

type shellGH struct{}

func (shellGH) WorkflowRuns(workflow string, limit int) ([]ghRun, error) {
	out, err := exec.Command("gh", "run", "list", "--workflow", workflow,
		"--limit", strconv.Itoa(limit),
		"--json", "databaseId,headSha,headBranch,status,conclusion").Output()
	if err != nil {
		return nil, fmt.Errorf("gh run list --workflow %s: %w", workflow, err)
	}
	var runs []ghRun
	if err := json.Unmarshal(out, &runs); err != nil {
		return nil, fmt.Errorf("parse gh run list: %w", err)
	}
	return runs, nil
}

func (shellGH) RunJobs(runID int64) ([]ghJob, error) {
	out, err := exec.Command("gh", "run", "view", strconv.FormatInt(runID, 10), "--json", "jobs").Output()
	if err != nil {
		return nil, fmt.Errorf("gh run view %d: %w", runID, err)
	}
	var wrap struct {
		Jobs []ghJob `json:"jobs"`
	}
	if err := json.Unmarshal(out, &wrap); err != nil {
		return nil, fmt.Errorf("parse gh run view: %w", err)
	}
	return wrap.Jobs, nil
}

func (shellGH) DispatchWorkflow(workflow, ref string, inputs map[string]string) error {
	args := []string{"workflow", "run", workflow, "--ref", ref}
	// Sort keys so the dispatched command is deterministic.
	keys := make([]string, 0, len(inputs))
	for k := range inputs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "-f", k+"="+inputs[k])
	}
	cmd := exec.Command("gh", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gh workflow run %s: %w", workflow, err)
	}
	return nil
}

// ── epoch model + derivation ────────────────────────────────────────────────

// epoch is a parsed `Y.N` release epoch (e.g. {2026, 1}).
type epoch struct{ Year, N int }

func parseEpochStr(s string) (epoch, error) {
	parts := strings.SplitN(strings.TrimSpace(s), ".", 2)
	if len(parts) != 2 {
		return epoch{}, fmt.Errorf("invalid epoch %q (want Y.N)", s)
	}
	y, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return epoch{}, fmt.Errorf("invalid epoch year in %q: %w", s, err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return epoch{}, fmt.Errorf("invalid epoch number in %q: %w", s, err)
	}
	return epoch{Year: y, N: n}, nil
}

func (e epoch) String() string { return fmt.Sprintf("%d.%d", e.Year, e.N) }

func (e epoch) less(o epoch) bool {
	if e.Year != o.Year {
		return e.Year < o.Year
	}
	return e.N < o.N
}

// currentYearFn is the device-clock year. A var so tests can pin it.
var currentYearFn = func() int { return time.Now().Year() }

// highestReleasedEpoch returns the highest `epoch-Y.N` tag (ignoring the moving
// `epoch-next` and any non-numeric tags), with ok=false when none exist.
func highestReleasedEpoch(g cutGit) (epoch, bool, error) {
	tags, err := g.ListEpochTags()
	if err != nil {
		return epoch{}, false, err
	}
	var best epoch
	found := false
	for _, t := range tags {
		name := strings.TrimSpace(t)
		if !strings.HasPrefix(name, "epoch-") {
			continue
		}
		rest := strings.TrimPrefix(name, "epoch-")
		if rest == "next" {
			continue
		}
		e, err := parseEpochStr(rest)
		if err != nil {
			continue // skip tags that are not numeric epochs
		}
		if !found || best.less(e) {
			best, found = e, true
		}
	}
	return best, found, nil
}

// errMultiYearGap / errClockBehind are the two derivation refusals. The gap is
// overridable with --reason; the clock-behind is never overridable.
var (
	errMultiYearGap = errors.New("multi-year gap since last released epoch")
	errClockBehind  = errors.New("device clock is behind the last released epoch")
)

// deriveStableTarget implements the epoch-derivation table (§6.3). It returns
// the target epoch, whether a year change must be confirmed, and an error for
// the gap/clock-behind refusals. For the multi-year-gap refusal it still returns
// the candidate target (Y.0) so the orchestrator can proceed under --reason.
func deriveStableTarget(last epoch, haveLast bool, year int) (target epoch, needYearConfirm bool, err error) {
	if !haveLast {
		return epoch{Year: year, N: 0}, false, nil // first release
	}
	switch {
	case last.Year == year:
		return epoch{Year: year, N: last.N + 1}, false, nil // same-year increment
	case last.Year == year-1:
		return epoch{Year: year, N: 0}, true, nil // year rollover
	case last.Year < year-1:
		return epoch{Year: year, N: 0}, true,
			fmt.Errorf("%w: last release epoch-%s, device-clock year %d", errMultiYearGap, last, year)
	default: // last.Year > year
		return epoch{}, false,
			fmt.Errorf("%w: last release epoch-%s, device-clock year %d; never cut backward", errClockBehind, last, year)
	}
}

// ── gate framework ──────────────────────────────────────────────────────────

// gateResult is one preflight check outcome. overridable=false marks a hard
// refusal that --reason cannot bypass (e.g. a tool error, or an immutable
// stable tag already present).
type gateResult struct {
	name        string
	passed      bool
	detail      string
	overridable bool
}

type gateFn func(ctx *cutContext) gateResult

// cutContext carries everything the gates and orchestration need.
type cutContext struct {
	root        string
	channel     string // "next" | "stable"
	pinnedRef   string // --sha <ref>: pin the cut to this commit instead of HEAD
	targetSHA   string
	targetEpoch epoch
	lastEpoch   epoch
	haveLast    bool

	dryRun      bool
	reason      string
	runCI       bool
	noCIWait    bool
	confirmYear bool
	notesFile   string // --notes-file <path> or "-" for stdin
	notesInline string // --notes "<text>"

	git      cutGit
	gh       cutGH
	uploader releaseUploader

	interactive bool
	stdin       io.Reader
	stdout      io.Writer
}

// preflight runs every gate, prints a checklist line per gate, then enforces the
// override policy: any non-overridable failure aborts; overridable failures
// abort unless --reason is set (each bypass is logged as an audit line).
func preflight(ctx *cutContext, gates []gateFn) error {
	fmt.Fprintf(ctx.stdout, "\nPreflight — cut %s (target %s @ %s):\n", ctx.channel, ctx.targetEpoch, short(ctx.targetSHA))
	var hardFails, softFails []gateResult
	for _, g := range gates {
		res := g(ctx)
		mark := "✓"
		if !res.passed {
			mark = "✗"
		}
		fmt.Fprintf(ctx.stdout, "  %s %s — %s\n", mark, res.name, res.detail)
		if res.passed {
			continue
		}
		if res.overridable {
			softFails = append(softFails, res)
		} else {
			hardFails = append(hardFails, res)
		}
	}
	if len(hardFails) > 0 {
		return fmt.Errorf("cut %s aborted: %d gate(s) cannot be bypassed: %s",
			ctx.channel, len(hardFails), strings.Join(gateNames(hardFails), ", "))
	}
	if len(softFails) > 0 {
		if ctx.reason == "" {
			return fmt.Errorf("cut %s aborted: %d gate(s) failed: %s (pass --reason \"<text>\" to override)",
				ctx.channel, len(softFails), strings.Join(gateNames(softFails), ", "))
		}
		for _, f := range softFails {
			fmt.Fprintf(ctx.stdout, "  GATE OVERRIDE: %s — %s\n    reason: %s\n", f.name, f.detail, ctx.reason)
		}
	}
	return nil
}

func gateNames(rs []gateResult) []string {
	names := make([]string, len(rs))
	for i, r := range rs {
		names[i] = r.name
	}
	return names
}

// ── discrete gates ──────────────────────────────────────────────────────────

func gateCleanTree(ctx *cutContext) gateResult {
	const name = "clean working tree"
	clean, err := ctx.git.CleanTree()
	if err != nil {
		return gateResult{name: name, detail: "git status failed: " + err.Error()}
	}
	if !clean {
		return gateResult{name: name, detail: "uncommitted changes present", overridable: true}
	}
	return gateResult{name: name, passed: true, detail: "no uncommitted changes"}
}

func gateReachable(ctx *cutContext) gateResult {
	const name = "tag commit reachable from origin/main"
	anc, err := ctx.git.IsAncestor(ctx.targetSHA, "origin/main")
	if err != nil {
		return gateResult{name: name, detail: "merge-base check failed: " + err.Error()}
	}
	if !anc {
		return gateResult{name: name, detail: short(ctx.targetSHA) + " is not reachable from origin/main", overridable: true}
	}
	return gateResult{name: name, passed: true, detail: short(ctx.targetSHA) + " reachable from origin/main"}
}

// gateCatalogEpoch (stable) requires catalog.toml's epoch to equal the target.
func gateCatalogEpoch(ctx *cutContext) gateResult {
	const name = "catalog epoch == target"
	cur, err := ParseEpoch(ctx.root)
	if err != nil {
		return gateResult{name: name, detail: err.Error()}
	}
	if cur != ctx.targetEpoch.String() {
		return gateResult{name: name, detail: fmt.Sprintf("catalog epoch is %s, expected target %s", cur, ctx.targetEpoch), overridable: true}
	}
	return gateResult{name: name, passed: true, detail: "catalog epoch " + cur}
}

// gateCatalogSanity (next) only reports the catalog epoch; it never blocks.
func gateCatalogSanity(ctx *cutContext) gateResult {
	const name = "catalog epoch (sanity)"
	cur, err := ParseEpoch(ctx.root)
	if err != nil {
		return gateResult{name: name, passed: true, detail: "catalog epoch unreadable: " + err.Error()}
	}
	return gateResult{name: name, passed: true, detail: "catalog epoch " + cur + " (informational for cut next)"}
}

// gateDepsHosted asserts every pinned dependency blob (each required platform ×
// each file) is recorded in blobs.json AND hosted on its deps-<dep>-<version>
// release.
func gateDepsHosted(ctx *cutContext) gateResult {
	const name = "all deps blobs hosted"
	pm, err := LoadPrebuiltsManifest(ctx.root)
	if err != nil {
		return gateResult{name: name, detail: err.Error()}
	}
	catalog, err := LoadBlobsCatalog(ctx.root)
	if err != nil {
		return gateResult{name: name, detail: err.Error()}
	}
	var missing []string
	depNames := make([]string, 0, len(pm.Binaries))
	for depName := range pm.Binaries {
		depNames = append(depNames, depName)
	}
	sort.Strings(depNames)
	for _, depName := range depNames {
		entry := pm.Binaries[depName]
		version := entry.Version
		tag := DepsReleaseTag(depName, version)
		assets, err := ctx.uploader.ListAssets(tag)
		if err != nil {
			return gateResult{name: name, detail: fmt.Sprintf("list assets for %s: %v", tag, err)}
		}
		hosted := make(map[string]bool, len(assets))
		for _, a := range assets {
			hosted[a] = true
		}
		for _, target := range requiredPlatforms {
			tEntry := entry.Targets[target]
			if tEntry == nil || tEntry.Unsupported != "" {
				continue // no buildable artifact for this target → nothing to host
			}
			for _, f := range tEntry.Files {
				be, ok := catalog.Lookup(depName, version, target, f.Out)
				if !ok {
					missing = append(missing, fmt.Sprintf("%s/%s/%s/%s (no catalog entry)", depName, version, target, f.Out))
					continue
				}
				suffix, err := assetSuffix(be.Compression)
				if err != nil {
					return gateResult{name: name, detail: fmt.Sprintf("%s/%s/%s/%s: %v", depName, version, target, f.Out, err)}
				}
				if asset := be.SHA256 + suffix; !hosted[asset] {
					missing = append(missing, fmt.Sprintf("%s/%s/%s/%s (asset %s not on %s)", depName, version, target, f.Out, asset, tag))
				}
			}
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return gateResult{name: name, detail: fmt.Sprintf("%d blob(s) not hosted: %s", len(missing), strings.Join(missing, "; ")), overridable: true}
	}
	return gateResult{name: name, passed: true, detail: "all required deps blobs hosted"}
}

// gateEpochRuleValid (stable) defensively re-checks monotonicity + the no-skip
// rule so an override cannot smuggle an illegal number past derivation.
func gateEpochRuleValid(ctx *cutContext) gateResult {
	const name = "epoch monotonic + rule-valid"
	if !ctx.haveLast {
		return gateResult{name: name, passed: true, detail: "first release (" + ctx.targetEpoch.String() + ")"}
	}
	t, last := ctx.targetEpoch, ctx.lastEpoch
	ok := (t.Year == last.Year && t.N == last.N+1) || (t.Year > last.Year && t.N == 0)
	if !ok {
		return gateResult{name: name, detail: fmt.Sprintf("target %s does not follow %s (no skip/jump)", t, last), overridable: true}
	}
	return gateResult{name: name, passed: true, detail: fmt.Sprintf("%s → %s", last, t)}
}

// gateStableTagAbsent (stable) enforces stable-tag immutability — never
// overridable.
func gateStableTagAbsent(ctx *cutContext) gateResult {
	const name = "stable tag not already present"
	tag := "epoch-" + ctx.targetEpoch.String()
	exists, err := ctx.git.TagExists(tag)
	if err != nil {
		return gateResult{name: name, detail: err.Error()}
	}
	if exists {
		return gateResult{name: name, detail: tag + " already exists (stable tags are immutable)"}
	}
	return gateResult{name: name, passed: true, detail: tag + " not present"}
}

// gateEpochNextValidated (stable) enforces validate-via-next-then-promote-the-
// same-hash: epoch-next must point at the release commit AND its release.yml run
// must have succeeded.
func gateEpochNextValidated(ctx *cutContext) gateResult {
	const name = "epoch-next validated this SHA"
	nextSHA, err := ctx.git.TagSHA("epoch-next")
	if err != nil {
		return gateResult{name: name, detail: "epoch-next tag missing or unreadable: " + err.Error(), overridable: true}
	}
	if !sameSHA(nextSHA, ctx.targetSHA) {
		return gateResult{name: name, detail: fmt.Sprintf("epoch-next is at %s, not the release commit %s", short(nextSHA), short(ctx.targetSHA)), overridable: true}
	}
	runs, err := ctx.gh.WorkflowRuns("release.yml", 50)
	if err != nil {
		return gateResult{name: name, detail: "query release.yml runs: " + err.Error()}
	}
	for _, run := range runs {
		if run.HeadBranch == "epoch-next" && sameSHA(run.HeadSHA, ctx.targetSHA) && run.Conclusion == "success" {
			return gateResult{name: name, passed: true, detail: "epoch-next release run succeeded at " + short(ctx.targetSHA)}
		}
	}
	return gateResult{name: name, detail: "no successful epoch-next release.yml run at " + short(ctx.targetSHA), overridable: true}
}

// resolveTargetSHA picks the commit a cut targets: the --sha ref when pinned
// (origin/main keeps moving under continuous development, so a cut names the
// commit it validated rather than whatever HEAD happens to be now), else HEAD.
func resolveTargetSHA(ctx *cutContext) (string, error) {
	if ctx.pinnedRef != "" {
		sha, err := ctx.git.ResolveSHA(ctx.pinnedRef)
		if err != nil {
			return "", fmt.Errorf("resolve --sha %q: %w", ctx.pinnedRef, err)
		}
		return strings.TrimSpace(sha), nil
	}
	return ctx.git.HeadSHA()
}

// installHeader leads every release-notes body so a reader lands on "how to
// get it" first, regardless of whether the body was auto-generated or supplied
// by the maintainer.
const installHeader = "**Install:** https://github.com/promise-language/promise/blob/main/docs/installing.md\n\n"

// generateReleaseNotes builds the mechanical release-notes body for a cut: a
// bulleted list of non-merge commit subjects in `epoch-<last>..<targetSHA>`,
// newest first, embedded into the annotated tag so release.yml can publish them
// with the artifacts (--notes-from-tag). It is deliberately dumb — no grouping,
// no enrichment. A future "smart" step (tracker-aware titles, gate/health status)
// can replace the body without changing callers; cutting a release must never
// depend on that step running.
func generateReleaseNotes(ctx *cutContext) (string, error) {
	var fromRef string
	if ctx.haveLast {
		fromRef = "epoch-" + ctx.lastEpoch.String()
	}
	subjects, err := ctx.git.LogSubjects(fromRef, ctx.targetSHA)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(installHeader)
	plural := "s"
	if len(subjects) == 1 {
		plural = ""
	}
	if fromRef != "" {
		fmt.Fprintf(&b, "Changes since %s (%d commit%s):\n", fromRef, len(subjects), plural)
	} else {
		fmt.Fprintf(&b, "Changes (%d commit%s):\n", len(subjects), plural)
	}
	if len(subjects) == 0 {
		b.WriteString("\n- (no commits since the previous epoch)\n")
		return b.String(), nil
	}
	b.WriteByte('\n')
	for _, s := range subjects {
		fmt.Fprintf(&b, "- %s\n", s)
	}
	return b.String(), nil
}

// resolveNotesBody returns the notes body to embed in the release tag.
// When a custom body is supplied (--notes-file / --notes), it replaces the
// mechanical git-log bullets but installHeader is always auto-prepended.
// Default (no flag) falls back to generateReleaseNotes, which already includes
// the install header.
func resolveNotesBody(ctx *cutContext) (string, error) {
	switch {
	case ctx.notesFile != "":
		b, err := readNotesFile(ctx)
		if err != nil {
			return "", fmt.Errorf("--notes-file: %w", err)
		}
		return installHeader + strings.TrimRight(b, "\n") + "\n", nil
	case ctx.notesInline != "":
		return installHeader + strings.TrimRight(ctx.notesInline, "\n") + "\n", nil
	default:
		return generateReleaseNotes(ctx)
	}
}

// readNotesFile reads the notes body from ctx.notesFile (a path or "-" for stdin).
func readNotesFile(ctx *cutContext) (string, error) {
	if ctx.notesFile == "-" {
		data, err := io.ReadAll(ctx.stdin)
		if err != nil {
			return "", fmt.Errorf("read stdin: %w", err)
		}
		return string(data), nil
	}
	data, err := os.ReadFile(ctx.notesFile)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// ── gate 7: three-state CI ──────────────────────────────────────────────────

type ciConclusion int

const (
	ciAbsent ciConclusion = iota // no run/conclusion covers this platform
	ciGreen                      // most-recent job at this SHA succeeded
	ciFailed                     // most-recent job at this SHA failed
)

// ciStatusAtSHA aggregates, per required platform, the conclusion of the job
// named for that platform across ALL ci.yml runs at `sha` (CI can be dispatched
// per-platform, so a platform's status may live in a different run). The most
// recent run that contains the job wins (gh returns runs newest-first).
func ciStatusAtSHA(gh cutGH, sha string) (map[string]ciConclusion, error) {
	runs, err := gh.WorkflowRuns("ci.yml", 50)
	if err != nil {
		return nil, err
	}
	status := make(map[string]ciConclusion, len(requiredPlatforms))
	for _, p := range requiredPlatforms {
		status[p] = ciAbsent
	}
	for _, run := range runs {
		if !sameSHA(run.HeadSHA, sha) {
			continue
		}
		jobs, err := gh.RunJobs(run.DatabaseID)
		if err != nil {
			return nil, err
		}
		for _, job := range jobs {
			plat := jobPlatform(job.Name)
			if plat == "" {
				continue
			}
			if status[plat] != ciAbsent {
				continue // a more recent run already decided this platform
			}
			switch job.Conclusion {
			case "success":
				status[plat] = ciGreen
			case "failure", "cancelled", "timed_out", "startup_failure":
				status[plat] = ciFailed
			default:
				// in-progress / queued / empty → leave absent
			}
		}
	}
	return status, nil
}

// jobPlatform maps a CI job name to a required platform (ci.yml sets
// `name: ${{ matrix.name }}`, so the job name is the platform).
func jobPlatform(jobName string) string {
	for _, p := range requiredPlatforms {
		if jobName == p || strings.Contains(jobName, p) {
			return p
		}
	}
	return ""
}

func splitCIStatus(status map[string]ciConclusion) (failed, absent []string) {
	for _, p := range requiredPlatforms {
		switch status[p] {
		case ciFailed:
			failed = append(failed, p)
		case ciAbsent:
			absent = append(absent, p)
		}
	}
	return failed, absent
}

// ciPollInterval/ciPollAttempts bound the cut's dispatch-and-watch loop;
// sleepFn/nowFn are vars so tests can no-op the wait and control the clock.
// ci --watch uses its own ciWatchTimeout (release_ci.go) instead of ciPollAttempts.
var (
	ciPollInterval = 20 * time.Second
	ciPollAttempts = 90 // ~30 min ceiling — used by cut's watchCI only
	sleepFn        = time.Sleep
	nowFn          = time.Now
)

func gateCI(ctx *cutContext) gateResult {
	const name = "green CI on all platforms"
	status, err := ciStatusAtSHA(ctx.gh, ctx.targetSHA)
	if err != nil {
		return gateResult{name: name, detail: "query CI status: " + err.Error()}
	}
	failed, absent := splitCIStatus(status)
	if len(failed) == 0 && len(absent) == 0 {
		return gateResult{name: name, passed: true, detail: "all platforms green at " + short(ctx.targetSHA)}
	}
	if len(failed) > 0 {
		// Real red signal — refuse; only --reason bypasses. Never auto-re-run.
		return gateResult{name: name, detail: fmt.Sprintf("CI failed at %s for: %s", short(ctx.targetSHA), strings.Join(failed, ", ")), overridable: true}
	}
	return resolveAbsentCI(ctx, name, absent) // only absent → fixable gap
}

// resolveAbsentCI handles the "no CI run covers this SHA" case: dispatch (on
// confirmation / --run-ci) and watch, else name the missing platforms.
func resolveAbsentCI(ctx *cutContext, name string, absent []string) gateResult {
	msg := fmt.Sprintf("CI has not run at %s for: %s", short(ctx.targetSHA), strings.Join(absent, ", "))
	// --dry-run must change nothing: never dispatch (a real ci.yml run) or prompt
	// — just report what a real run would do. The gate runs inside preflight,
	// which precedes the dry-run plan print, so the guard has to live here.
	if ctx.dryRun {
		return gateResult{name: name, detail: msg + " (dry-run: would dispatch ci.yml and wait)", overridable: true}
	}
	switch {
	case ctx.runCI:
		// dispatch below
	case ctx.interactive:
		fmt.Fprintf(ctx.stdout, "%s\nDispatch ci.yml now? [y/N] ", msg)
		if !readYes(ctx.stdin) {
			return gateResult{name: name, detail: msg + " (dispatch declined)", overridable: true}
		}
	default:
		return gateResult{name: name, detail: msg + " — re-run with --run-ci to dispatch, or --reason to override", overridable: true}
	}

	branch, err := ctx.git.CurrentBranch()
	if err != nil {
		return gateResult{name: name, detail: "current branch: " + err.Error()}
	}
	if err := dispatchCI(ctx, branch, absent); err != nil {
		return gateResult{name: name, detail: "dispatch ci.yml: " + err.Error()}
	}
	if ctx.noCIWait {
		// Dispatched but not waiting — a hard stop, not an override: come back
		// and re-run `cut` once CI is green.
		return gateResult{name: name, detail: "dispatched ci.yml for " + strings.Join(absent, ", ") + "; re-run `cut` once green"}
	}
	status, err := watchCI(ctx)
	if err != nil {
		return gateResult{name: name, detail: "watch CI: " + err.Error()}
	}
	failed, stillAbsent := splitCIStatus(status)
	switch {
	case len(failed) == 0 && len(stillAbsent) == 0:
		return gateResult{name: name, passed: true, detail: "all platforms green after dispatch"}
	case len(failed) > 0:
		return gateResult{name: name, detail: "CI failed after dispatch for: " + strings.Join(failed, ", "), overridable: true}
	default:
		// Still pending after the wait — e.g. origin/main moved past this SHA so
		// the dispatched run does not match. A hard stop, not an override.
		return gateResult{name: name, detail: "CI still pending after wait for: " + strings.Join(stillAbsent, ", ") + " (origin/main may have moved past " + short(ctx.targetSHA) + ")"}
	}
}

// dispatchCI dispatches ci.yml for the absent platforms on `branch` —
// platform=all when the whole matrix is missing, else one run per platform.
func dispatchCI(ctx *cutContext, branch string, absent []string) error {
	if len(absent) == len(requiredPlatforms) {
		return ctx.gh.DispatchWorkflow("ci.yml", branch, map[string]string{"platform": "all", "run_tests": "true"})
	}
	for _, p := range absent {
		if err := ctx.gh.DispatchWorkflow("ci.yml", branch, map[string]string{"platform": p, "run_tests": "true"}); err != nil {
			return err
		}
	}
	return nil
}

// watchCI re-evaluates ciStatusAtSHA until no required platform is absent (all
// runs finished) or the attempt ceiling is hit.
func watchCI(ctx *cutContext) (map[string]ciConclusion, error) {
	var status map[string]ciConclusion
	for i := 0; i < ciPollAttempts; i++ {
		s, err := ciStatusAtSHA(ctx.gh, ctx.targetSHA)
		if err != nil {
			return nil, err
		}
		status = s
		_, absent := splitCIStatus(s)
		if len(absent) == 0 {
			return s, nil
		}
		fmt.Fprintf(ctx.stdout, "  waiting on CI for %s...\n", strings.Join(absent, ", "))
		sleepFn(ciPollInterval)
	}
	return status, nil
}

// ── orchestration ───────────────────────────────────────────────────────────

// runReleaseCut parses the channel + flags and dispatches to cutNext/cutStable.
func runReleaseCut(root string, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("cut: expected `next` or `stable`\n%s", releaseUsage)
	}
	channel, rest := args[0], args[1:]
	fs := flag.NewFlagSet("cut", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "run all gates and print the checklist, change nothing")
	sha := fs.String("sha", "", "pin the cut to this commit/ref instead of HEAD (origin/main keeps moving)")
	reason := fs.String("reason", "", "override failed gate(s); recorded into the tag/commit message")
	runCI := fs.Bool("run-ci", false, "non-interactively dispatch ci.yml for platforms with no run at this SHA")
	noCIWait := fs.Bool("no-ci-wait", false, "with --run-ci: dispatch CI then stop (re-run `cut` once green)")
	confirmYear := fs.Bool("confirm-year", false, "non-interactively confirm a year-rollover epoch")
	notesFile := fs.String("notes-file", "", "read release-notes body from a local file (- for stdin); mutually exclusive with --notes")
	notesInline := fs.String("notes", "", "inline release-notes body; mutually exclusive with --notes-file")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if *notesFile != "" && *notesInline != "" {
		return fmt.Errorf("cut: --notes-file and --notes are mutually exclusive")
	}
	ctx := &cutContext{
		root:        root,
		channel:     channel,
		pinnedRef:   *sha,
		dryRun:      *dryRun,
		reason:      *reason,
		runCI:       *runCI,
		noCIWait:    *noCIWait,
		confirmYear: *confirmYear,
		notesFile:   *notesFile,
		notesInline: *notesInline,
		git:         defaultCutGit(root),
		gh:          defaultCutGH,
		uploader:    defaultReleaseUploader,
		interactive: isCutInteractive(),
		stdin:       os.Stdin,
		stdout:      os.Stdout,
	}
	switch channel {
	case "next":
		return cutNext(ctx)
	case "stable":
		return cutStable(ctx)
	default:
		return fmt.Errorf("cut: unknown channel %q (want `next` or `stable`)\n%s", channel, releaseUsage)
	}
}

// cutNext refreshes the moving epoch-next pre-release at HEAD.
func cutNext(ctx *cutContext) error {
	if err := ctx.git.Fetch(); err != nil {
		return fmt.Errorf("git fetch: %w", err)
	}
	sha, err := resolveTargetSHA(ctx)
	if err != nil {
		return err
	}
	ctx.targetSHA = sha
	cur, err := ParseEpoch(ctx.root)
	if err != nil {
		return err
	}
	e, err := parseEpochStr(cur)
	if err != nil {
		return err
	}
	ctx.targetEpoch = e
	// lastEpoch scopes the release-notes range (epoch-<last>..targetSHA).
	last, haveLast, err := highestReleasedEpoch(ctx.git)
	if err != nil {
		return err
	}
	ctx.lastEpoch, ctx.haveLast = last, haveLast

	gates := []gateFn{gateCleanTree, gateReachable, gateCatalogSanity, gateDepsHosted, gateCI}
	if err := preflight(ctx, gates); err != nil {
		return err
	}
	notes, err := resolveNotesBody(ctx)
	if err != nil {
		return fmt.Errorf("generate release notes: %w", err)
	}
	if ctx.dryRun {
		fmt.Fprintf(ctx.stdout, "\n[dry-run] would force-move tag epoch-next → %s and push (release.yml refreshes the pre-release).\nRelease notes:\n%s", short(ctx.targetSHA), notes)
		return nil
	}
	msg := tagMessage("epoch-next pre-release at "+short(ctx.targetSHA), notes, ctx)
	if err := ctx.git.CreateTag("epoch-next", msg, ctx.targetSHA, true); err != nil {
		return err
	}
	if err := ctx.git.PushTag("epoch-next", true); err != nil {
		return err
	}
	fmt.Fprintf(ctx.stdout, "\nepoch-next moved to %s and pushed; release.yml will refresh the pre-release.\n", short(ctx.targetSHA))
	return nil
}

// cutStable derives the epoch, runs all gates, then tags → pushes → bumps the
// catalog epoch on the current branch → pushes.
func cutStable(ctx *cutContext) error {
	if err := ctx.git.Fetch(); err != nil {
		return fmt.Errorf("git fetch: %w", err)
	}
	sha, err := resolveTargetSHA(ctx)
	if err != nil {
		return err
	}
	ctx.targetSHA = sha

	last, haveLast, err := highestReleasedEpoch(ctx.git)
	if err != nil {
		return err
	}
	ctx.lastEpoch, ctx.haveLast = last, haveLast
	year := currentYearFn()
	target, needYearConfirm, derr := deriveStableTarget(last, haveLast, year)
	if derr != nil {
		if errors.Is(derr, errMultiYearGap) {
			if ctx.reason == "" {
				return fmt.Errorf("%w — pass --reason \"<text>\" to proceed", derr)
			}
			fmt.Fprintf(ctx.stdout, "GATE OVERRIDE (epoch derivation): %v\n  reason: %s\n", derr, ctx.reason)
		} else {
			return derr // clock-behind: never overridable
		}
	}
	ctx.targetEpoch = target

	// A year change needs explicit confirmation; --reason (the audited override)
	// also satisfies it for the multi-year-gap path.
	if needYearConfirm && ctx.reason == "" {
		if !confirmYearChange(ctx, last, target) {
			return fmt.Errorf("year rollover %s → %s requires confirmation (--confirm-year)", last, target)
		}
	}

	fmt.Fprintf(ctx.stdout, "Deriving stable epoch: last=%s target=%s (device-clock year %d)\n", lastStr(last, haveLast), target, year)

	gates := []gateFn{
		gateCleanTree, gateReachable, gateCatalogEpoch, gateDepsHosted,
		gateEpochRuleValid, gateStableTagAbsent, gateCI, gateEpochNextValidated,
	}
	if err := preflight(ctx, gates); err != nil {
		return err
	}
	notes, err := resolveNotesBody(ctx)
	if err != nil {
		return fmt.Errorf("generate release notes: %w", err)
	}

	tag := "epoch-" + target.String()
	next := epoch{Year: target.Year, N: target.N + 1}
	if ctx.dryRun {
		fmt.Fprintf(ctx.stdout, "\n[dry-run] would: tag %s @ %s → push → bump catalog.toml to %s → commit → push current branch\nRelease notes:\n%s",
			tag, short(ctx.targetSHA), next, notes)
		return nil
	}

	// tag → push → bump catalog → commit → push.
	msg := tagMessage("Promise "+tag, notes, ctx)
	if err := ctx.git.CreateTag(tag, msg, ctx.targetSHA, false); err != nil {
		return err
	}
	if err := ctx.git.PushTag(tag, false); err != nil {
		return err
	}
	fmt.Fprintf(ctx.stdout, "\nTagged %s @ %s and pushed.\n", tag, short(ctx.targetSHA))

	if err := writeCatalogEpoch(ctx.root, next); err != nil {
		return err
	}
	bumpMsg := fmt.Sprintf("catalog: bump epoch to %s for ongoing development", next)
	if err := ctx.git.CommitFile("catalog.toml", bumpMsg); err != nil {
		return err
	}
	branch, err := ctx.git.CurrentBranch()
	if err != nil {
		return err
	}
	if err := ctx.git.PushBranch(branch); err != nil {
		return err
	}
	fmt.Fprintf(ctx.stdout, "Bumped catalog epoch to %s and pushed %s.\n", next, branch)
	return nil
}

// confirmYearChange returns true when a year rollover is authorized (via
// --confirm-year or an interactive y/N prompt).
func confirmYearChange(ctx *cutContext, last, target epoch) bool {
	if ctx.confirmYear {
		return true
	}
	if !ctx.interactive {
		return false
	}
	fmt.Fprintf(ctx.stdout, "Year rollover: last release %s, cutting %s. Confirm? [y/N] ", last, target)
	return readYes(ctx.stdin)
}

// writeCatalogEpoch rewrites the single `epoch = "…"` line in catalog.toml,
// preserving every other line (and the leading indentation) byte-for-byte.
func writeCatalogEpoch(root string, e epoch) error {
	path := filepath.Join(root, "catalog.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(string(data), "\n")
	replaced := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "epoch") && strings.Contains(line, "=") {
			indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
			lines[i] = fmt.Sprintf("%sepoch = %q", indent, e.String())
			replaced = true
			break
		}
	}
	if !replaced {
		return fmt.Errorf("catalog.toml: no epoch line to rewrite")
	}
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644)
}

// tagMessage builds an annotated-tag message: the title, the auto-generated
// release notes (published by release.yml via `gh release create --notes-from-tag`),
// then any override reason so a bypass is auditable in git history.
func tagMessage(title, notes string, ctx *cutContext) string {
	msg := title
	if n := strings.TrimRight(notes, "\n"); n != "" {
		msg += "\n\n" + n
	}
	if ctx.reason != "" {
		msg += "\n\nGate override reason: " + ctx.reason
	}
	return msg
}

// ── small helpers ───────────────────────────────────────────────────────────

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// sameSHA compares two commit hashes tolerating short/full forms.
func sameSHA(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if a == "" || b == "" {
		return false
	}
	return a == b || strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}

func lastStr(last epoch, haveLast bool) string {
	if !haveLast {
		return "(none)"
	}
	return last.String()
}

func readYes(r io.Reader) bool {
	if r == nil {
		return false
	}
	sc := bufio.NewScanner(r)
	if !sc.Scan() {
		return false
	}
	ans := strings.ToLower(strings.TrimSpace(sc.Text()))
	return ans == "y" || ans == "yes"
}

// isCutInteractive reports whether stdin is a terminal (so prompts make sense).
func isCutInteractive() bool {
	info, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
