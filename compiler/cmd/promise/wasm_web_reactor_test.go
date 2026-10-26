package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/cmd/promise/clitest"
)

// webReactorExterns declares the delivery externs modules/web builds on, so a
// program can subscribe without the web module.
const webReactorExterns = `_web_subscribe(i32 capacity, i32 policy) i32 ` + "`extern(\"promise_web_subscribe\") `target(web)" + `;
_web_channel(i32 sub_id) Channel[i32] ` + "`extern(\"promise_web_channel\") `target(web)" + `;

recv_from(Channel[i32] ch) i32? {
  return <-ch;
}
`

// webReactorPrograms are the programs of TestWebReactorParksThroughCalls.
var webReactorPrograms = map[string]string{
	// The receive is in the goroutine's own body.
	"bare": webReactorExterns + `
main() {
  sub_id := _web_subscribe(16, 0);
  ch := _web_channel(sub_id);
  go {
    v := <-ch;
    if v is present {
      print_line("GOT {v}");
    }
  };
}
`,
	// The receive is one call down, in a helper.
	"helper": webReactorExterns + `
main() {
  sub_id := _web_subscribe(16, 0);
  ch := _web_channel(sub_id);
  go {
    v := recv_from(ch);
    if v is present {
      print_line("GOT {v}");
    }
  };
}
`,
	// for-in over a type whose next() receives: EventSubscription's shape.
	"method": webReactorExterns + `
type Sub {
  Channel[i32] _channel;

  next() i32? {
    return <-this._channel;
  }
}

main() {
  sub_id := _web_subscribe(16, 0);
  s := Sub(_channel: _web_channel(sub_id));
  go {
    for v in s {
      print_line("GOT {v}");
    }
  };
}
`,
	// main itself parks in a helper while a subscription is live.
	"main_awaits": webReactorExterns + `
main() {
  sub_id := _web_subscribe(16, 0);
  ch := _web_channel(sub_id);
  v := recv_from(ch);
  if v is present {
    print_line("MAIN GOT {v}");
  }
}
`,
	// No subscription and main parked in a helper: a genuine deadlock.
	"deadlock": webReactorExterns + `
main() {
  ch := channel[i32](1);
  v := recv_from(ch);
  print_line("never");
}
`,
	// No subscription, main returns, a goroutine stays parked in a helper: the
	// program is complete.
	"clean_exit": webReactorExterns + `
main() {
  ch := channel[i32](1);
  go {
    v := recv_from(ch);
    print_line("never");
  };
  print_line("MAIN DONE");
}
`,
	// web.on: for-in over EventSubscription.next() on the consumer goroutine.
	"web_on": `use web;

main() {
  web.on(web.document.body, "click", |web.Event e| {
    web.console.log("click");
  });
}
`,
}

// webReactorDriver loads a program through its generated loader the way a page
// would (nodePageDriver). Arguments: the build output's base name, then a mode:
// "init" only initializes; "enqueue V..." delivers each V to subscription 0 and
// pumps after each; "click N" installs a fake document.body and dispatches N
// clicks to every listener the program added. It reports one RESULT: line.
const webReactorDriver = nodePageDriver + `
const [name, mode, ...vals] = process.argv.slice(2);

(async () => {
  const result = { logs, errs, pumps: [], listeners: 0 };
  const listeners = [];
  globalThis.document = { body: { addEventListener(kind, fn) { listeners.push(fn); } } };
  const mod = await import(pathToFileURL(path.join(dir, name + ".js")).href);
  try {
    const exports = await mod.init();
    result.ok = true;
    if (mode === "enqueue") {
      for (const v of vals) {
        exports.promise_web_enqueue(0, Number(v));
        result.pumps.push(exports.promise_web_pump());
      }
    } else if (mode === "click") {
      result.listeners = listeners.length;
      for (let i = 0; i < Number(vals[0]); i++) {
        for (const fn of listeners) fn({ type: "click" });
      }
    }
  } catch (e) {
    result.ok = false;
    result.exitSignal = e instanceof mod._ExitSignal;
    result.code = e && e.code;
  }
  process.stdout.write("RESULT:" + JSON.stringify(result) + "\n");
})();
`

type webReactorResult struct {
	Logs       []string `json:"logs"`
	Errs       []string `json:"errs"`
	Pumps      []int    `json:"pumps"`
	Listeners  int      `json:"listeners"`
	OK         bool     `json:"ok"`
	ExitSignal bool     `json:"exitSignal"`
	Code       int      `json:"code"`
}

// TestWebReactorParksThroughCalls runs real wasm32-web reactor programs under
// the pinned node (promise-language/promise#68). A goroutine that blocks
// through a call — a helper, a method, the for-in next() protocol, web.on —
// must park, so _initialize returns while a subscription is live and a later
// event resumes the goroutine. Before #68 the callee ran a nested scheduler
// loop that could not return to the host, and init() rejected with
// "fatal: all goroutines are asleep - deadlock!". The liveness rule
// (docs/web-apps.md) still holds through calls: with no live registration a
// parked main is a deadlock, and a finished main ends the program.
func TestWebReactorParksThroughCalls(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping build+node integration test in short mode")
	}
	nodePath, err := resolveWasmRuntime("node")
	if err != nil {
		// Neither staged on this machine nor fetchable: a fact about the
		// environment, not the tree.
		t.Skipf("the pinned node is not obtainable here: %v", err)
	}
	bin := clitest.Bin(t)
	dir := clitest.TempDir(t)

	outDir := filepath.Join(dir, "out")
	if err := os.MkdirAll(outDir, 0755); err != nil {
		t.Fatal(err)
	}
	for name, src := range webReactorPrograms {
		srcFile := filepath.Join(dir, name, "main.pr")
		if err := os.MkdirAll(filepath.Dir(srcFile), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(srcFile, []byte(src), 0644); err != nil {
			t.Fatal(err)
		}
		clitest.RunOK(t, bin, nil, "build", "-target", "wasm32-web", "-o", filepath.Join(outDir, name+".wasm"), srcFile)
	}
	driver := filepath.Join(outDir, "driver.cjs")
	if err := os.WriteFile(driver, []byte(webReactorDriver), 0644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		args    []string
		want    webReactorResult
		wantErr string // a substring of the console.error output
	}{
		{"bare receive", []string{"bare", "enqueue", "777"},
			webReactorResult{Logs: []string{"GOT 777"}, Pumps: []int{0}, OK: true}, ""},
		{"receive in a helper", []string{"helper", "enqueue", "777"},
			webReactorResult{Logs: []string{"GOT 777"}, Pumps: []int{0}, OK: true}, ""},
		{"for-in over next()", []string{"method", "enqueue", "1", "2"},
			webReactorResult{Logs: []string{"GOT 1", "GOT 2"}, Pumps: []int{0, 0}, OK: true}, ""},
		{"main parked in a helper", []string{"main_awaits", "enqueue", "5"},
			webReactorResult{Logs: []string{"MAIN GOT 5"}, Pumps: []int{0}, OK: true}, ""},
		{"web.on", []string{"web_on", "click", "2"},
			webReactorResult{Logs: []string{"click", "click"}, Listeners: 1, OK: true}, ""},
		{"deadlock through a helper", []string{"deadlock", "init"},
			webReactorResult{ExitSignal: true, Code: 2}, "all goroutines are asleep - deadlock!"},
		{"parked goroutine at a clean exit", []string{"clean_exit", "init"},
			webReactorResult{Logs: []string{"MAIN DONE"}, OK: true}, ""},
	}
	for _, c := range cases {
		got := runNodeDriver[webReactorResult](t, nodePath, driver, c.args...)
		if got.OK != c.want.OK || got.ExitSignal != c.want.ExitSignal || got.Code != c.want.Code {
			t.Errorf("%s: init() ok=%v exitSignal=%v code=%d, want ok=%v exitSignal=%v code=%d (stderr %q)",
				c.name, got.OK, got.ExitSignal, got.Code, c.want.OK, c.want.ExitSignal, c.want.Code, got.Errs)
		}
		if strings.Join(got.Logs, "\n") != strings.Join(c.want.Logs, "\n") {
			t.Errorf("%s: logs = %q, want %q", c.name, got.Logs, c.want.Logs)
		}
		// promise_web_pump returns 0 (idle) once the delivered event is consumed
		// and the instance is back to waiting on its live subscription.
		if len(got.Pumps) != len(c.want.Pumps) {
			t.Errorf("%s: pumps = %v, want %v", c.name, got.Pumps, c.want.Pumps)
		} else {
			for i := range c.want.Pumps {
				if got.Pumps[i] != c.want.Pumps[i] {
					t.Errorf("%s: pump %d returned %d, want %d", c.name, i, got.Pumps[i], c.want.Pumps[i])
				}
			}
		}
		if got.Listeners != c.want.Listeners {
			t.Errorf("%s: %d listeners added, want %d", c.name, got.Listeners, c.want.Listeners)
		}
		if c.wantErr == "" && len(got.Errs) != 0 {
			t.Errorf("%s: unexpected stderr output %q", c.name, got.Errs)
		}
		if c.wantErr != "" && !strings.Contains(strings.Join(got.Errs, "\n"), c.wantErr) {
			t.Errorf("%s: stderr %q does not contain %q", c.name, got.Errs, c.wantErr)
		}
	}
}
