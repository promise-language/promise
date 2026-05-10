package common

import (
	"fmt"
	"path/filepath"
	"strings"
)

// RunVet runs go vet on compiler packages, excluding the auto-generated parser.
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
	return RunIn(compilerDir, "go", args...)
}
