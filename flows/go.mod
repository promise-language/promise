module github.com/promise-language/promise/flows

go 1.26.1

// The flow substrate is split across two local modules, wired in via replaces so
// the flows build fully offline once the submodules are checked out:
//   - djabi.dev/go/flow_sdk (../flow-sdk) — the tracker-server backend (the
//     Backend/Agent/Worktree/Telemetry implementation the flows run on). Vanity
//     module path with no public proxy; a git submodule (see ../.gitmodules).
//   - github.com/promise-language/flow (../flow) — the OSS flow substrate
//     (cli.Run + the flow definition/step API). Also a git submodule.
// flow_sdk itself depends on github.com/promise-language/flow; the replace below
// makes both resolve to the in-tree submodules rather than the network.
require (
	djabi.dev/go/flow_sdk v0.0.0
	github.com/promise-language/flow v0.0.0
)

replace djabi.dev/go/flow_sdk => ../flow-sdk

replace github.com/promise-language/flow => ../flow
