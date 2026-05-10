package common

import "fmt"

// RunSetup configures git hooks for the repository. Idempotent.
func RunSetup(root string) error {
	if err := RunSilent("git", "-C", root, "config", "core.hooksPath", ".githooks"); err != nil {
		return fmt.Errorf("configure git hooks: %w", err)
	}
	fmt.Println("Git hooks enabled (.githooks/)")
	return nil
}
