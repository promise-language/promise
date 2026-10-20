package main

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/cmd/promise/clitest"
)

// nodePageDriver is the start of every Node script that loads a generated
// wasm32-web loader the way a page would. It shims globalThis.fetch to read
// the file beside the script (Node's fetch takes no relative URLs) and
// captures console.log/console.error into logs/errs. A driver appends its own
// body, which reads its arguments from process.argv, imports the loader from
// dir, and reports one RESULT: line of JSON for runNodeDriver to decode.
const nodePageDriver = `
const fs = require("fs");
const path = require("path");
const { pathToFileURL } = require("url");

const dir = __dirname;

globalThis.fetch = async (url) => {
  const data = await fs.promises.readFile(path.join(dir, url));
  return new Response(data, { headers: { "content-type": "application/wasm" } });
};

const logs = [];
const errs = [];
console.log = (...args) => { logs.push(args.join(" ")); };
console.error = (...args) => { errs.push(args.join(" ")); };
`

// runNodeDriver runs the driver script at driverFile with args under the
// pinned node and decodes its RESULT line into a T.
func runNodeDriver[T any](t *testing.T, nodePath, driverFile string, args ...string) T {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), clitest.Budget())
	defer cancel()
	cmd := exec.CommandContext(ctx, nodePath, append([]string{driverFile}, args...)...)
	out, err := cmd.CombinedOutput()
	var r T
	for _, line := range strings.Split(string(out), "\n") {
		if payload, ok := strings.CutPrefix(line, "RESULT:"); ok {
			if jerr := json.Unmarshal([]byte(payload), &r); jerr != nil {
				t.Fatalf("cannot decode driver result %q: %v", payload, jerr)
			}
			return r
		}
	}
	t.Fatalf("driver produced no RESULT line (err=%v):\n%s", err, out)
	return r
}
