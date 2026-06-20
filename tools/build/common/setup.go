package common

import (
	"fmt"
	"os/exec"
	"strings"
)

// RunSetup configures git hooks for the repository. Idempotent — the hooks path
// is only written (and only printed) on the transition from unset/other to the
// desired value, so frequent ./make invocations stay quiet once it is in place.
func RunSetup(root string) error {
	if currentHooksPath(root) != ".githooks" {
		if err := RunSilent("git", "-C", root, "config", "core.hooksPath", ".githooks"); err != nil {
			return fmt.Errorf("configure git hooks: %w", err)
		}
		fmt.Println("Git hooks enabled (.githooks/)")
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
