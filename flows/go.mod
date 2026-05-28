module github.com/p5e-ia/promise-lang/flows

go 1.25.6

// The flow SDK (djabi.dev/go/flow_sdk) is a vanity module path with no public
// proxy / go-get resolver, so it cannot be `go get`'d here. It is fetched on
// demand by ./make into ../flow-sdk (gitignored, NOT a git submodule) and wired
// in via this local replace — so the flows build fully offline once present.
require djabi.dev/go/flow_sdk v0.0.0

replace djabi.dev/go/flow_sdk => ../flow-sdk
