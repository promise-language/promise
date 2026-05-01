// Package testutil provides shared test infrastructure for compiler tests.
package testutil

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// LoadStdFiles reads all std/*.pr files and concatenates them into a single source string.
// The path is relative to the calling test package (3 levels up from internal/<pkg>/).
func LoadStdFiles() string {
	stdDir := filepath.Join("..", "..", "..", "std")
	entries, err := os.ReadDir(stdDir)
	if err != nil {
		panic("cannot read std directory: " + err.Error())
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".pr") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(stdDir, name))
		if err != nil {
			panic("cannot read std file " + name + ": " + err.Error())
		}
		b.Write(data)
		b.WriteByte('\n')
	}
	return b.String()
}
