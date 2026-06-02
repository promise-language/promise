package common

import (
	"fmt"
	"os/exec"
	"strings"
)

// RunSetup configures git hooks and submodule-safety git config for the
// repository. Idempotent — each setting is only written (and only printed) on
// the transition from unset/other to the desired value, so frequent ./make
// invocations stay quiet once everything is in place.
func RunSetup(root string) error {
	if currentHooksPath(root) != ".githooks" {
		if err := RunSilent("git", "-C", root, "config", "core.hooksPath", ".githooks"); err != nil {
			return fmt.Errorf("configure git hooks: %w", err)
		}
		fmt.Println("Git hooks enabled (.githooks/)")
	}
	if err := ensureSubmoduleConfig(root); err != nil {
		return err
	}
	return nil
}

// submoduleSafetyConfig is the set of repo-local git settings that make working
// across the flow / flow-sdk submodules safe and visible. See
// docs/working-with-submodules.md for the rationale:
//   - push.recurseSubmodules=check  — refuse to push the superproject if it
//     references a submodule commit that isn't pushed (no dangling gitlinks).
//   - status.submoduleSummary / diff.submodule=log — surface gitlink drift in
//     `git status` / `git diff` instead of a bare SHA.
//   - fetch.recurseSubmodules=on-demand — fetch submodule commits referenced by
//     fetched superproject commits, so a gitlink bump is always resolvable.
var submoduleSafetyConfig = [][2]string{
	{"push.recurseSubmodules", "check"},
	{"status.submoduleSummary", "true"},
	{"diff.submodule", "log"},
	{"fetch.recurseSubmodules", "on-demand"},
}

// ensureSubmoduleConfig applies submoduleSafetyConfig idempotently, writing only
// the keys that differ from the desired value and printing a single summary line
// when it changed anything (so steady-state ./make runs stay silent).
func ensureSubmoduleConfig(root string) error {
	changed := false
	for _, kv := range submoduleSafetyConfig {
		key, want := kv[0], kv[1]
		if gitConfigValue(root, key) == want {
			continue
		}
		if err := RunSilent("git", "-C", root, "config", key, want); err != nil {
			return fmt.Errorf("configure %s: %w", key, err)
		}
		changed = true
	}
	if changed {
		fmt.Println("Submodule-safety git config applied (see docs/working-with-submodules.md)")
	}
	return nil
}

// gitConfigValue reads a single repo-local git config key, returning "" if it is
// unset or git fails. Stderr is discarded so a missing key prints no noise.
func gitConfigValue(root, key string) string {
	cmd := exec.Command("git", "-C", root, "config", "--get", key)
	cmd.Stderr = nil
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// currentHooksPath reads core.hooksPath from the repo's git config, returning
// "" if the key is unset or git fails.
func currentHooksPath(root string) string {
	return gitConfigValue(root, "core.hooksPath")
}
