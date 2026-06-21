package module

import (
	"fmt"
	"sort"
	"strings"
)

// EpochTag is a single `epoch-YYYY.N` tag on a module repo, dereferenced to the
// commit it points at. Epoch is the "YYYY.N" parsed out of the tag name; Tag is
// the full tag name ("epoch-2026.1"); Commit is the peeled commit SHA.
type EpochTag struct {
	Epoch  string
	Tag    string
	Commit string
}

const epochTagPrefix = "epoch-"

// ListRepoTags runs `git ls-remote --tags <url>` and parses the result into the
// repo's `epoch-*` tags (dereferenced to commits) plus the commit a `stable` tag
// points at, if present. Annotated tags appear twice in ls-remote output — once
// as `<sha> refs/tags/X` and once as `<sha> refs/tags/X^{}` (the peeled commit);
// the `^{}` line wins so the pinned commit is always the underlying commit, never
// the tag object.
func ListRepoTags(url string) (epochTags []EpochTag, stableCommit string, err error) {
	if err := requireGit(); err != nil {
		return nil, "", err
	}
	out, err := runGit("", "ls-remote", "--tags", url)
	if err != nil {
		return nil, "", fmt.Errorf("cannot list tags from %s: %w", url, err)
	}

	// Accumulate commit per tag name; the peeled ^{} line overrides the raw line.
	commits := make(map[string]string)
	var stableRaw, stablePeeled string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		sha, ref := fields[0], fields[1]
		name := strings.TrimPrefix(ref, "refs/tags/")
		if name == ref {
			continue // not a tag ref
		}
		peeled := strings.HasSuffix(name, "^{}")
		name = strings.TrimSuffix(name, "^{}")

		if name == "stable" {
			if peeled {
				stablePeeled = sha
			} else {
				stableRaw = sha
			}
			continue
		}
		if !strings.HasPrefix(name, epochTagPrefix) {
			continue
		}
		epoch := strings.TrimPrefix(name, epochTagPrefix)
		if _, _, ok := splitEpoch(epoch); !ok {
			continue // e.g. "epoch-next" or malformed — not a numeric epoch tag
		}
		if peeled || commits[name] == "" {
			commits[name] = sha
		}
	}

	for name, sha := range commits {
		epochTags = append(epochTags, EpochTag{
			Epoch:  strings.TrimPrefix(name, epochTagPrefix),
			Tag:    name,
			Commit: sha,
		})
	}
	// Stable order so callers (and the candidate walk-back) are deterministic.
	sort.Slice(epochTags, func(i, j int) bool {
		return CompareEpochs(epochTags[i].Epoch, epochTags[j].Epoch) > 0
	})

	stableCommit = stablePeeled
	if stableCommit == "" {
		stableCommit = stableRaw
	}
	return epochTags, stableCommit, nil
}

// Candidates filters epochTags down to those usable as a candidate for a project
// on projectEpoch — every tag whose epoch is ≤ the project's — and returns them
// sorted DESCENDING by epoch. The returned slice IS the §9.8 walk-back order:
// try the first (largest epoch-X ≤ E), and on verification failure step to the
// next-older entry.
func Candidates(epochTags []EpochTag, projectEpoch string) []EpochTag {
	var out []EpochTag
	for _, t := range epochTags {
		if CompareEpochs(t.Epoch, projectEpoch) <= 0 {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return CompareEpochs(out[i].Epoch, out[j].Epoch) > 0
	})
	return out
}

// HighestEpoch returns the largest epoch among the given tags (numeric compare),
// or "" when the list is empty. Used for the §9.10 "highest verified epoch"
// message — the newest epoch the module carries a tag for.
func HighestEpoch(epochTags []EpochTag) (epoch, tag string) {
	for _, t := range epochTags {
		if epoch == "" || CompareEpochs(t.Epoch, epoch) > 0 {
			epoch, tag = t.Epoch, t.Tag
		}
	}
	return epoch, tag
}

// NoCompatibleVersionError is the §9.10 gate: raised at resolve time, before any
// unverified dependency source reaches the compiler, so a project never sees raw
// compiler errors buried inside a dependency. Module is the URL or name as the
// user referred to it; Epoch is the project's epoch; HighestVerifiedEpoch/
// HighestTag describe the newest epoch the module carries a tag for (may be empty
// when the module has no `epoch-*` tags at all).
type NoCompatibleVersionError struct {
	Module               string
	Epoch                string
	HighestVerifiedEpoch string
	HighestTag           string
}

func (e *NoCompatibleVersionError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "module %q has no version compatible with epoch %s\n", e.Module, e.Epoch)
	if e.HighestVerifiedEpoch != "" {
		fmt.Fprintf(&b, "  highest verified epoch: %s   (tag %s)\n", e.HighestVerifiedEpoch, e.HighestTag)
		fmt.Fprintf(&b, "  newer tags fail to build under %s\n", e.Epoch)
	} else {
		fmt.Fprintf(&b, "  the module carries no epoch-* tags that build under %s\n", e.Epoch)
	}
	b.WriteString("  options:\n")
	if e.HighestVerifiedEpoch != "" {
		fmt.Fprintf(&b, "    - pin this project to epoch ≤ %s            (trades newer language features for %s)\n", e.HighestVerifiedEpoch, e.Module)
	} else {
		fmt.Fprintf(&b, "    - pin this project to an epoch the module supports\n")
	}
	b.WriteString("    - use a fork:   promise package add github.com/you/fork\n")
	b.WriteString("    - redirect locally while fixing:  [replace] " + e.Module + " = \"../...\"   (§9.7)\n")
	fmt.Fprintf(&b, "    - or wait for the module to publish an epoch-%s tag", e.Epoch)
	return b.String()
}
