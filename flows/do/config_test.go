package main

import (
	"strings"
	"testing"

	"djabi.dev/go/flow_sdk/doflow"
)

// TestPromiseConfig_WindowsCommandsAreCmdSafe guards T0827: the flow runner
// shells VerifyCmd and FormatCmd out through cmd.exe on Windows, which cannot
// resolve a forward-slash command path ("bin/verify" → "'bin' is not
// recognized"). Every command the Promise config supplies must therefore carry
// a Windows variant whose program path uses backslashes (a path cmd.exe can
// resolve), and must not lead with a forward-slash "bin/" path.
func TestPromiseConfig_WindowsCommandsAreCmdSafe(t *testing.T) {
	cfg := promiseConfig()
	cmds := map[string]doflow.Command{
		"VerifyCmd": cfg.VerifyCmd,
		"FormatCmd": cfg.FormatCmd,
	}
	for name, c := range cmds {
		if c.Default == "" {
			t.Errorf("%s.Default is empty", name)
		}
		if c.Windows == "" {
			t.Errorf("%s.Windows is empty — Windows runs cmd.exe and needs a backslash path (T0827)", name)
			continue
		}
		// The cross-platform default uses a forward-slash bin/ path; the Windows
		// form must not (cmd.exe rejects it before PATHEXT lookup).
		if strings.HasPrefix(c.Windows, "bin/") {
			t.Errorf("%s.Windows %q uses a forward-slash path cmd.exe cannot resolve (T0827)", name, c.Windows)
		}
		if !strings.HasPrefix(c.Windows, `bin\`) {
			t.Errorf("%s.Windows %q should invoke the executable via a backslash bin\\ path", name, c.Windows)
		}
	}
}
