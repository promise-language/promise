package main

import (
	"os"
	"strings"
	"testing"
)

// requiredReleasePlatforms mirrors requiredPlatforms in
// tools/build/common/release_cut.go — the platforms the release pipeline gates
// on. It is duplicated (not imported) because tools/build is a separate module.
var requiredReleasePlatforms = []string{
	"linux-amd64",
	"linux-arm64",
	"darwin-arm64",
	"windows-amd64",
}

// TestReleaseEmbedFilesCoverRequiredPlatforms guards T1492: every required
// release platform must have BOTH per-target embed files —
// stub_embed_<os>_<arch>.go (thin variant, -tags=embed_stub) and
// llvm_<os>_<arch>.go (full variant, -tags=embed_llvm). ci.yml builds via
// bin/build with neither tag (stub_default.go / llvm_other.go supply the
// symbols), so a missing file stays green in CI and only explodes in
// `bin/release build` as "undefined: embeddedStub / embeddedLLVM / ...". This
// filesystem check is GOOS-independent so it runs everywhere bin/verify does.
func TestReleaseEmbedFilesCoverRequiredPlatforms(t *testing.T) {
	t.Parallel()
	for _, p := range requiredReleasePlatforms {
		suffix := strings.ReplaceAll(p, "-", "_")
		for _, name := range []string{"stub_embed_" + suffix + ".go", "llvm_" + suffix + ".go"} {
			if _, err := os.Stat(name); err != nil {
				t.Errorf("required release platform %s is missing per-target embed file %s "+
					"(the release-variant build will fail with undefined symbols even though ci.yml is green — see T1492): %v",
					p, name, err)
			}
		}
	}
}
