package common

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// RunVet runs go vet on compiler packages (excluding the generated parser),
// tools/build, and flows (when flows/go.mod and flow-sdk/go.mod are present).
func RunVet(root string) error {
	compilerDir := filepath.Join(root, "compiler")

	// Get the list of packages, then filter out internal/parser
	out, err := RunOutputIn(compilerDir, "go", "list", "./...")
	if err != nil {
		return fmt.Errorf("go list: %w", err)
	}

	var pkgs []string
	for _, pkg := range strings.Split(out, "\n") {
		pkg = strings.TrimSpace(pkg)
		if pkg != "" && !strings.HasSuffix(pkg, "/internal/parser") {
			pkgs = append(pkgs, pkg)
		}
	}

	if len(pkgs) == 0 {
		return fmt.Errorf("no packages found to vet")
	}

	args := append([]string{"vet"}, pkgs...)
	if err := RunIn(compilerDir, "go", args...); err != nil {
		return err
	}

	if err := RunToolsVet(root); err != nil {
		return err
	}

	if _, err := RunFlowsVet(root); err != nil {
		return err
	}

	return nil
}

// RunToolsVet runs go vet on the tools/build module.
func RunToolsVet(root string) error {
	toolsDir := filepath.Join(root, "tools", "build")
	return RunIn(toolsDir, "go", "vet", "./...")
}

// RunFlowsVet runs go vet on the flows module.
// Returns (skipped=true, nil) when flows/go.mod or flow-sdk/go.mod is absent.
func RunFlowsVet(root string) (skipped bool, err error) {
	if !Exists(filepath.Join(root, "flows", "go.mod")) {
		return true, nil
	}
	if !Exists(filepath.Join(root, "flow-sdk", "go.mod")) {
		fmt.Fprintf(os.Stderr, "warning: skipping flows vet — flow-sdk/ not present (run ./make to fetch)\n")
		return true, nil
	}
	flowsDir := filepath.Join(root, "flows")
	return false, RunIn(flowsDir, "go", "vet", "./...")
}
