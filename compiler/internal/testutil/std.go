// Package testutil provides shared test infrastructure for compiler tests.
package testutil

import (
	"embed"
	"sort"
	"strings"
)

// Embed std/*.pr files copied by `make resources` into testdata/std/.
// This ensures Go's build cache tracks std file changes — when std files
// are modified and `make resources` re-copies them, tests are rebuilt.
//
//go:embed testdata/std/*.pr
var stdFS embed.FS

// LoadStdFiles reads all embedded std/*.pr files and concatenates them
// into a single source string suitable for parsing by ANTLR.
func LoadStdFiles() string {
	entries, err := stdFS.ReadDir("testdata/std")
	if err != nil {
		panic("cannot read embedded std directory: " + err.Error())
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
		data, err := stdFS.ReadFile("testdata/std/" + name)
		if err != nil {
			panic("cannot read embedded std file " + name + ": " + err.Error())
		}
		b.Write(data)
		b.WriteByte('\n')
	}
	return b.String()
}
