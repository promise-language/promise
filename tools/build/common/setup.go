package common

import (
	"fmt"
	"os/exec"
	"strings"
)

// RunSetup configures git hooks for the repository. Idempotent — when hooks
// are already pointed at .githooks (the common case after the first ./make),
// this returns silently. Only prints on the transition from unset/other to
// .githooks so frequent ./make invocations stay quiet.
func RunSetup(root string) error {
	if currentHooksPath(root) == ".githooks" {
		return nil
	}
	if err := RunSilent("git", "-C", root, "config", "core.hooksPath", ".githooks"); err != nil {
		return fmt.Errorf("configure git hooks: %w", err)
	}
	fmt.Println("Git hooks enabled (.githooks/)")
	return nil
}

// currentHooksPath reads core.hooksPath from the repo's git config, returning
// "" if the key is unset or git fails. Stderr is discarded so a missing key
// doesn't print "key does not contain a section: core" noise.
func currentHooksPath(root string) string {
	cmd := exec.Command("git", "-C", root, "config", "--get", "core.hooksPath")
	cmd.Stderr = nil
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
