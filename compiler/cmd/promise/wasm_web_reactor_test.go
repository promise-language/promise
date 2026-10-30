package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/promise-language/promise/compiler/cmd/promise/clitest"
	"github.com/promise-language/promise/compiler/internal/wasmweb"
)

// webReactorExterns declares the delivery externs modules/web builds on, so a
// program can subscribe without the web module. A program prints each ID the
// driver needs as "ID <name> <id>" (webReactorDriver).
const webReactorExterns = `_web_subscribe(i32 capacity, i32 policy) i32 ` + "`extern(\"promise_web_subscribe\") `target(web)" + `;
_web_channel(i32 sub_id) Channel[i32] ` + "`extern(\"promise_web_channel\") `target(web)" + `;
_web_unsubscribe(i32 sub_id) ` + "`extern(\"promise_web_unsubscribe\") `target(web)" + `;
_web_subscription_dropped(i32 sub_id) i32 ` + "`extern(\"promise_web_subscription_dropped\") `target(web)" + `;

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
  print_line("ID sub {sub_id}");
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
  print_line("ID sub {sub_id}");
  ch := _web_channel(sub_id);
  go {
    v := recv_from(ch);
    if v is present {
      print_line("GOT {v}");
    }
  };
}
`,
	// The goroutine is the helper itself: `go f(args)`.
	"go_call": webReactorExterns + `
consume(Channel[i32] ch) {
  v := recv_from(ch);
  if v is present {
    print_line("GOT {v}");
  }
}

main() {
  sub_id := _web_subscribe(16, 0);
  print_line("ID sub {sub_id}");
  go consume(_web_channel(sub_id));
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
  print_line("ID sub {sub_id}");
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
  print_line("ID sub {sub_id}");
  ch := _web_channel(sub_id);
  v := recv_from(ch);
  if v is present {
    print_line("MAIN GOT {v}");
  }
}
`,
	// A failable main's body is a function the main coroutine calls.
	"failable_main_awaits": webReactorExterns + `
main!() {
  sub_id := _web_subscribe(16, 0);
  print_line("ID sub {sub_id}");
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
// would (nodePageDriver), over a fake document.body that records each listener
// the program adds. Arguments: the build output's base name, then a mode:
//   - "init" only initializes;
//   - "enqueue V..." delivers each V to the subscription the program printed as
//     "ID sub <id>", and pumps after each;
//   - "click N" dispatches N clicks to every listener;
//   - "script OP..." runs each OP in order: "enq:<ref>:<handle>" enqueues to
//     <ref> — a name the program printed as "ID <name> <id>", or "#<id>" — and
//     does not pump; "pump" pumps; "pump?" pumps and, if the pump ends the
//     program, records the exit code in exits and goes on; "click:<kind>"
//     dispatches one event to the listeners of that kind, as the page's own
//     event would; "await:<line>" waits until the program has printed line —
//     what a continuation the loader scheduled prints — and holds nothing
//     open meanwhile, so the wait ends only if the loader keeps node alive
//     for that continuation.
//
// It reports one RESULT: line. Every enqueue — the driver's own, and those the
// loader's event listeners make — is recorded as [sub_id, handle, returned] by
// wrapping the instance's exports, and every event handle Promise code releases
// is recorded by supplying promise_env's "[resource-drop]event" to init(), and
// so is the number of MessagePorts still holding node open at the end. It
// also supplies promise_env's "poke": called by Promise code while it runs, it
// enqueues 7 to the subscription it is given and pumps, as a page's event
// dispatched synchronously from inside a Promise→JS call would, and records
// that nested pump's result as nested.
const webReactorDriver = nodePageDriver + `
const [name, mode, ...vals] = process.argv.slice(2);

const enqueues = [];
let raw;
const instantiateStreaming = WebAssembly.instantiateStreaming;
WebAssembly.instantiateStreaming = async (source, imports) => {
  const { module, instance } = await instantiateStreaming(source, imports);
  raw = instance.exports;
  const exports = { ...instance.exports };
  exports.promise_web_enqueue = (sub, handle) => {
    const returned = instance.exports.promise_web_enqueue(sub, handle);
    enqueues.push([sub, handle, returned]);
    return returned;
  };
  return { module, instance: { exports } };
};

const idLine = /^ID (\S+) (-?\d+)$/;
const ids = () => Object.fromEntries(logs.filter((l) => idLine.test(l)).map((l) => {
  const m = idLine.exec(l);
  return [m[1], Number(m[2])];
}));
const idOf = (ref) => {
  if (ref.startsWith("#")) return Number(ref.slice(1));
  const id = ids()[ref];
  if (id === undefined) throw new Error("the program printed no ID " + ref);
  return id;
};

let onLog;
const pageLog = console.log;
console.log = (...args) => {
  pageLog(...args);
  if (onLog) onLog(args.join(" "));
};
const printed = (line) => logs.includes(line) ? Promise.resolve() : new Promise((resolve) => {
  onLog = (l) => {
    if (l === line) {
      onLog = undefined;
      resolve();
    }
  };
});

(async () => {
  const result = { pumps: [], exits: [], listeners: 0 };
  const drops = [];
  const listeners = [];
  globalThis.document = { body: { addEventListener(kind, fn) { listeners.push({ kind, fn }); } } };
  const mod = await import(pathToFileURL(path.join(dir, name + ".js")).href);
  try {
    const exports = await mod.init({ promise_env: {
      "[resource-drop]event"(handle) { drops.push(handle); },
      poke(sub) {
        raw.promise_web_enqueue(sub, 7);
        result.nested = raw.promise_web_pump();
      },
    } });
    result.ok = true;
    if (mode === "enqueue") {
      for (const v of vals) {
        exports.promise_web_enqueue(idOf("sub"), Number(v));
        result.pumps.push(exports.promise_web_pump());
      }
    } else if (mode === "click") {
      result.listeners = listeners.length;
      for (let i = 0; i < Number(vals[0]); i++) {
        for (const l of listeners) l.fn({ type: "click" });
      }
    } else if (mode === "script") {
      for (const op of vals) {
        const [verb, arg, handle] = op.split(":");
        if (verb === "enq") {
          exports.promise_web_enqueue(idOf(arg), Number(handle));
        } else if (verb === "pump") {
          result.pumps.push(exports.promise_web_pump());
        } else if (verb === "pump?") {
          try {
            result.pumps.push(exports.promise_web_pump());
          } catch (e) {
            if (!(e instanceof mod._ExitSignal)) throw e;
            result.exits.push(e.code);
          }
        } else if (verb === "click") {
          for (const l of listeners) {
            if (l.kind === arg) l.fn({ type: arg });
          }
        } else if (verb === "await") {
          await printed(arg);
        } else {
          throw new Error("unknown driver op " + op);
        }
      }
    }
  } catch (e) {
    result.ok = false;
    result.exitSignal = e instanceof mod._ExitSignal;
    result.code = e && e.code;
    if (!result.exitSignal) result.driverError = String(e);
  }
  result.ids = ids();
  result.logs = logs.filter((l) => !idLine.test(l));
  result.errs = errs;
  result.enqueues = enqueues;
  result.drops = drops;
  result.openPorts = process.getActiveResourcesInfo().filter((r) => r === "MessagePort").length;
  process.stdout.write("RESULT:" + JSON.stringify(result) + "\n");
  // A port the loader left holding node open would hang the run; the test
  // reports openPorts instead.
  if (result.openPorts) process.exit(0);
})();
`

// webCoroCacheProgram's Box[int].get blocks only through helper, whose body is
// HELPER_BODY. The module use makes the build compile Box[int] as its own,
// cached instance object.
const webCoroCacheProgram = `use math;

type Box[T] {
  T v;

  get(Channel[int] ch) int {
    return helper(ch);
  }
}

helper(Channel[int] ch) int {
  HELPER_BODY
}

main() {
  ch := channel[int](1);
  t := go {
    b := Box[int](v: 1);
    b.get(ch)
  };
  ch.send(41);
  print_line("GOT {<-t} {math.sign(-3)}");
}
`

type webReactorResult struct {
	Logs        []string       `json:"logs"`
	Errs        []string       `json:"errs"`
	Pumps       []int          `json:"pumps"`
	Exits       []int          `json:"exits"`  // exit codes "pump?" recorded
	Nested      *int           `json:"nested"` // what poke's nested pump returned
	Listeners   int            `json:"listeners"`
	OK          bool           `json:"ok"`
	ExitSignal  bool           `json:"exitSignal"`
	Code        int            `json:"code"`
	DriverError string         `json:"driverError"`
	IDs         map[string]int `json:"ids"`
	Enqueues    [][3]int       `json:"enqueues"`  // [sub_id, handle, returned]
	Drops       []int          `json:"drops"`     // event handles Promise code released
	OpenPorts   int            `json:"openPorts"` // MessagePorts holding node open at the end
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
	nodePath, driver := buildWebReactorPrograms(t, webReactorPrograms)

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
		{"go f(args) of a helper", []string{"go_call", "enqueue", "777"},
			webReactorResult{Logs: []string{"GOT 777"}, Pumps: []int{0}, OK: true}, ""},
		{"for-in over next()", []string{"method", "enqueue", "1", "2"},
			webReactorResult{Logs: []string{"GOT 1", "GOT 2"}, Pumps: []int{0, 0}, OK: true}, ""},
		{"main parked in a helper", []string{"main_awaits", "enqueue", "5"},
			webReactorResult{Logs: []string{"MAIN GOT 5"}, Pumps: []int{0}, OK: true}, ""},
		{"failable main parked in a helper", []string{"failable_main_awaits", "enqueue", "5"},
			webReactorResult{Logs: []string{"MAIN GOT 5"}, Pumps: []int{0}, OK: true}, ""},
		{"web.on", []string{"web_on", "click", "2"},
			webReactorResult{Logs: []string{"click", "click"}, Listeners: 1, OK: true}, ""},
		{"deadlock through a helper", []string{"deadlock", "init"},
			webReactorResult{ExitSignal: true, Code: 2}, "all goroutines are asleep - deadlock!"},
		{"parked goroutine at a clean exit", []string{"clean_exit", "init"},
			webReactorResult{Logs: []string{"MAIN DONE"}, OK: true}, ""},
		// The program ended in _initialize, so the instance is terminated: a
		// later pump runs nothing.
		{"pump after a clean exit", []string{"clean_exit", "script", "pump"},
			webReactorResult{Logs: []string{"MAIN DONE"}, Pumps: []int{wasmweb.PumpTerminated}, OK: true}, ""},
	}
	for _, c := range cases {
		got := runNodeDriver[webReactorResult](t, nodePath, driver, c.args...)
		if got.DriverError != "" {
			t.Errorf("%s: the driver failed: %s", c.name, got.DriverError)
		}
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

// buildWebReactorPrograms builds each program for wasm32-web next to a copy of
// webReactorDriver, and returns the pinned node and the driver's path. It skips
// the test in short mode, or where the pinned node cannot be obtained.
func buildWebReactorPrograms(t *testing.T, programs map[string]string) (nodePath, driver string) {
	t.Helper()
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
	for name, src := range programs {
		srcFile := filepath.Join(dir, name, "main.pr")
		if err := os.MkdirAll(filepath.Dir(srcFile), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(srcFile, []byte(src), 0644); err != nil {
			t.Fatal(err)
		}
		clitest.RunOK(t, bin, nil, "build", "-target", "wasm32-web", "-o", filepath.Join(outDir, name+".wasm"), srcFile)
	}
	driver = filepath.Join(outDir, "driver.cjs")
	if err := os.WriteFile(driver, []byte(webReactorDriver), 0644); err != nil {
		t.Fatal(err)
	}
	return nodePath, driver
}

// webDeliveryProgram is a program of TestWebDeliveryQueueSemantics. It
// subscribes "sub" with capacity and policy, and "gate", prints both IDs, and
// runs before and after around that. Its consumer waits for an event on gate,
// then takes n values from sub, printing each (CLOSED for none), prints sub's
// drop count, runs report, and unsubscribes both — so once it is done the
// program has no live registration and ends cleanly.
func webDeliveryProgram(before string, capacity, policy int, after string, n int, report string) string {
	return webReactorExterns + fmt.Sprintf(`
main() {
%s  sub_id := _web_subscribe(%d, %d);
  ch := _web_channel(sub_id);
  gate_id := _web_subscribe(1, %d);
  gate := _web_channel(gate_id);
  print_line("ID sub {sub_id}");
  print_line("ID gate {gate_id}");
%s  go {
    opened := <-gate;
    if opened is present {
      for _ in 0..%d {
        v := <-ch;
        if v is present {
          print_line("GOT {v}");
        } else {
          print_line("CLOSED");
        }
      }
      dropped := _web_subscription_dropped(sub_id);
      print_line("DROPPED {dropped}");
%s    }
    _web_unsubscribe(sub_id);
    _web_unsubscribe(gate_id);
  };
}
`, before, capacity, policy, wasmweb.QueueDropOldest, after, n, report)
}

// webEventsProgram is a program of TestWebDeliveryQueueSemantics that
// subscribes to "click" through modules/web, with capacity and policy, and to
// "gate", and moves both into its consumer (s and g). On a gate event the
// consumer prints the click subscription's drop count, runs body, and prints
// count; both subscriptions drop when it ends.
func webEventsProgram(capacity, policy int, body string) string {
	return fmt.Sprintf(`use web;

main() {
  sub := web.events_with_policy(web.document.body, ["click"], %d, %d);
  gate := web.events(web.document.body, ["gate"]);
  go {
    s := sub;
    g := gate;
    opened := g.next();
    count := 0;
    dropped := s.dropped;
    print_line("DROPPED {dropped}");
%s    print_line("COUNT {count}");
  };
}
`, capacity, policy, body)
}

// webDeliveryPrograms are the programs of TestWebDeliveryQueueSemantics.
var webDeliveryPrograms = map[string]string{
	"fifo":        webDeliveryProgram("", 4, wasmweb.QueueDropOldest, "", 3, ""),
	"drop_oldest": webDeliveryProgram("", 2, wasmweb.QueueDropOldest, "", 2, ""),
	"drop_newest": webDeliveryProgram("", 2, wasmweb.QueueDropNewest, "", 2, ""),
	"coalesce":    webDeliveryProgram("", 2, wasmweb.QueueCoalesce, "", 2, ""),
	// "ended" takes the table's first slot and is unsubscribed, so ID 0 — what
	// a free slot holds — names a free slot. Neither an enqueue to it nor an
	// unsubscribe of it may act on that slot, or the live count goes wrong and
	// the program never ends cleanly.
	"rejected": webDeliveryProgram(`  ended := _web_subscribe(1, 0);
  ended_ch := _web_channel(ended);
`, 4, wasmweb.QueueDropOldest, `  _web_unsubscribe(ended);
  _web_unsubscribe(0);
  print_line("ID ended {ended}");
`, 1, ""),
	// "stale" is unsubscribed before sub is made, so sub takes its place in the
	// table; a second unsubscribe of stale must then leave sub alone, and
	// stale's drop count is not sub's.
	"reused": webDeliveryProgram(`  stale := _web_subscribe(4, 0);
  stale_ch := _web_channel(stale);
  _web_unsubscribe(stale);
`, 1, wasmweb.QueueDropOldest, `  _web_unsubscribe(stale);
  print_line("ID stale {stale}");
`, 1, `      stale_dropped := _web_subscription_dropped(stale);
      print_line("STALE DROPPED {stale_dropped}");
`),
	// The channel's owner closes it without unsubscribing.
	"closed": webDeliveryProgram("", 4, wasmweb.QueueDropOldest, "  ch.close();\n", 1, ""),
	// 1000 subscriptions come and go while sub and gate stay live. CHURN counts
	// the IDs that are not positive, repeat a live one, or repeat an earlier one.
	"churn": webDeliveryProgram("", 4, wasmweb.QueueDropOldest, `  seen := Set[i32]();
  bad := 0;
  for _ in 0..1000 {
    id := _web_subscribe(1, 0);
    id_ch := _web_channel(id);
    if id <= 0 || id == sub_id || id == gate_id || seen.contains(id) {
      bad = bad + 1;
    }
    seen.add(id);
    _web_unsubscribe(id);
  }
  print_line("CHURN {bad}");
`, 1, ""),
	"release_drop_oldest": webEventsProgram(2, wasmweb.QueueDropOldest, ""),
	"release_drop_newest": webEventsProgram(2, wasmweb.QueueDropNewest, ""),
	"release_coalesce":    webEventsProgram(2, wasmweb.QueueCoalesce, ""),
	// The consumer closes the subscription, then drains it.
	"close_drains": webEventsProgram(4, wasmweb.QueueDropOldest, `    s.close();
    for e in s {
      count = count + 1;
    }
`),
	// Nothing reads the subscription before it drops.
	"drop_drains": webEventsProgram(4, wasmweb.QueueDropOldest, ""),
	// The drop count stays readable once close() has released the ID, and a
	// second close() does not lose it.
	"dropped_after_close": webEventsProgram(2, wasmweb.QueueDropOldest, `    s.close();
    s.close();
    after := s.dropped;
    print_line("DROPPED AFTER CLOSE {after}");
    for e in s {
      count = count + 1;
    }
`),
}

// TestWebDeliveryQueueSemantics drives the delivery primitives under the pinned
// node, with the driver playing the host (docs/web-apps.md "Queue semantics and
// overflow", "The guest side primitive", "Resource lifetime across the
// boundary"). promise_web_enqueue must hand back exactly the handle the host
// has to release — the rejected one, the evicted one, the one a coalesce
// replaced, or 0 — and the loader's listener must release it. An ID whose
// subscription has ended must be rejected, even once its slot is reused.
// Events still queued when a subscription goes away must all be released.
func TestWebDeliveryQueueSemantics(t *testing.T) {
	t.Parallel()
	nodePath, driver := buildWebReactorPrograms(t, webDeliveryPrograms)

	// The raw programs: the driver enqueues with its own handles, then opens the
	// gate. Every program ends cleanly once its consumer has unsubscribed both
	// subscriptions — the pump that runs it exits with 0.
	gated := []struct {
		name    string
		ops     []string
		returns []int // what each enqueue returned, in order
		logs    []string
		differ  []string // printed IDs that must all differ
	}{
		{"fifo", []string{"enq:sub:11", "enq:sub:12", "enq:sub:13", "enq:gate:1", "pump"},
			[]int{0, 0, 0, 0}, []string{"GOT 11", "GOT 12", "GOT 13", "DROPPED 0"}, nil},
		{"drop_oldest", []string{"enq:sub:11", "enq:sub:12", "enq:sub:13", "enq:gate:1", "pump"},
			[]int{0, 0, 11, 0}, []string{"GOT 12", "GOT 13", "DROPPED 1"}, nil},
		{"drop_newest", []string{"enq:sub:11", "enq:sub:12", "enq:sub:13", "enq:gate:1", "pump"},
			[]int{0, 0, 13, 0}, []string{"GOT 11", "GOT 12", "DROPPED 1"}, nil},
		{"coalesce", []string{"enq:sub:11", "enq:sub:12", "enq:sub:13", "enq:gate:1", "pump"},
			[]int{0, 0, 12, 0}, []string{"GOT 11", "GOT 13", "DROPPED 1"}, nil},
		{"rejected", []string{"enq:#0:21", "enq:#-1:22", "enq:ended:23", "enq:sub:24", "enq:gate:1", "pump"},
			[]int{21, 22, 23, 0, 0}, []string{"GOT 24", "DROPPED 0"}, nil},
		{"reused", []string{"enq:stale:41", "enq:sub:42", "enq:sub:43", "enq:gate:1", "pump"},
			[]int{41, 0, 42, 0}, []string{"GOT 43", "DROPPED 1", "STALE DROPPED 0"}, []string{"stale", "sub"}},
		{"closed", []string{"enq:sub:51", "enq:gate:1", "pump"},
			[]int{51, 0}, []string{"CLOSED", "DROPPED 0"}, nil},
		{"churn", []string{"enq:sub:61", "enq:gate:1", "pump"},
			[]int{0, 0}, []string{"CHURN 0", "GOT 61", "DROPPED 0"}, nil},
	}
	for _, c := range gated {
		t.Run(c.name, func(t *testing.T) {
			got := runNodeDriver[webReactorResult](t, nodePath, driver, append([]string{c.name, "script"}, c.ops...)...)
			assertWebCleanExit(t, got)
			var returns []int
			for _, e := range got.Enqueues {
				returns = append(returns, e[2])
			}
			if !slices.Equal(returns, c.returns) {
				t.Errorf("enqueue returned %v, want %v", returns, c.returns)
			}
			if strings.Join(got.Logs, "\n") != strings.Join(c.logs, "\n") {
				t.Errorf("logs = %q, want %q", got.Logs, c.logs)
			}
			seen := map[int]string{}
			for _, name := range c.differ {
				if other, dup := seen[got.IDs[name]]; dup {
					t.Errorf("%s and %s share the ID %d (IDs %v)", other, name, got.IDs[name], got.IDs)
				}
				seen[got.IDs[name]] = name
			}
		})
	}

	// Through modules/web and the loader's listener: four clicks on a
	// capacity-2 subscription nobody reads. The third overflows and must hand
	// back the displaced handle (want is its index among the handles), and the
	// listener must release it — so the ref table hands that same handle to the
	// fourth click.
	for _, c := range []struct {
		name string
		want int
	}{
		{"release_drop_oldest", 0},
		{"release_drop_newest", 2},
		{"release_coalesce", 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := runNodeDriver[webReactorResult](t, nodePath, driver,
				c.name, "script", "click:click", "click:click", "click:click", "click:click")
			if got.DriverError != "" || !got.OK || len(got.Errs) != 0 {
				t.Fatalf("driver error %q, ok=%v, stderr %q", got.DriverError, got.OK, got.Errs)
			}
			if len(got.Enqueues) != 4 {
				t.Fatalf("enqueues = %v, want 4", got.Enqueues)
			}
			released := got.Enqueues[2][2]
			if want := got.Enqueues[c.want][1]; released != want {
				t.Errorf("the overflowing enqueue returned %d, want handle %d (enqueues %v)", released, want, got.Enqueues)
			}
			if got.Enqueues[3][1] != released {
				t.Errorf("the next event got handle %d, not the released %d: the listener did not release it (enqueues %v)",
					got.Enqueues[3][1], released, got.Enqueues)
			}
		})
	}

	// Through modules/web: three clicks queue, then the gate opens; the
	// subscription is drained by its consumer after close(), or by its own
	// drop. Every event handle the host enqueued — the gate's included — must
	// be released by Promise code unless enqueue handed it back to the host,
	// and the program then ends cleanly inside the gate listener's pump.
	for _, c := range []struct {
		name string
		logs []string
	}{
		{"close_drains", []string{"DROPPED 0", "COUNT 3"}},
		{"drop_drains", []string{"DROPPED 0", "COUNT 0"}},
		{"dropped_after_close", []string{"DROPPED 1", "DROPPED AFTER CLOSE 1", "COUNT 2"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := runNodeDriver[webReactorResult](t, nodePath, driver,
				c.name, "script", "click:click", "click:click", "click:click", "click:gate", "pump")
			assertWebListenerExit(t, got)
			if strings.Join(got.Logs, "\n") != strings.Join(c.logs, "\n") {
				t.Errorf("logs = %q, want %q", got.Logs, c.logs)
			}
			// The ref table reuses a handle the host released, so this is a
			// multiset difference.
			var want []int
			for _, e := range got.Enqueues {
				want = append(want, e[1])
			}
			for _, e := range got.Enqueues {
				if i := slices.Index(want, e[2]); e[2] != wasmweb.EnqueueNoRelease && i >= 0 {
					want = slices.Delete(want, i, i+1)
				}
			}
			slices.Sort(want)
			drops := slices.Sorted(slices.Values(got.Drops))
			if len(got.Enqueues) != 4 || !slices.Equal(drops, want) {
				t.Errorf("Promise code released %v, want %v (enqueues %v)", drops, want, got.Enqueues)
			}
		})
	}
}

// webHostEntryPrograms are the programs of TestWebReactorHostEntries.
var webHostEntryPrograms = map[string]string{
	// main makes a Promise→JS call that dispatches an event synchronously: the
	// driver's poke enqueues and pumps while main is still running.
	"reentrant_init": webReactorExterns + `
_poke(i32 sub_id) ` + "`extern(\"poke\") `wasm_import(\"promise_env\", \"poke\") `target(web)" + `;

main() {
  sub_id := _web_subscribe(4, 0);
  ch := _web_channel(sub_id);
  go {
    v := <-ch;
    if v is present {
      print_line("GOT {v}");
    }
    _web_unsubscribe(sub_id);
  };
  _poke(sub_id);
  print_line("AFTER POKE");
}
`,
	// docs/web-apps.md "It must be usable without bindgen": main's own loop
	// parks between events, and after three it ends its subscription and
	// returns, so the pump that runs it ends the program.
	"main_loop": webReactorExterns + `
main() {
  sub_id := _web_subscribe(4, 0);
  print_line("ID sub {sub_id}");
  ch := _web_channel(sub_id);
  print_line("ready");
  count := 0;
  for h in ch {
    print_line("{h}");
    count = count + 1;
    if count == 3 {
      _web_unsubscribe(sub_id);
      break;
    }
  }
}
`,
	// A handler in main panics on its second event.
	"handler_panics": webReactorExterns + `
main() {
  sub_id := _web_subscribe(4, 0);
  print_line("ID sub {sub_id}");
  ch := _web_channel(sub_id);
  for h in ch {
    print_line("{h}");
    assert(h != 2, "boom");
  }
}
`,
	// Through modules/web: main takes one click and returns, which drops its
	// subscription, so the click's pump ends the program.
	"listener_exit": `use web;

main() {
  sub := web.events(web.document.body, ["click"]);
  e := sub.next();
  print_line("CLICKED");
}
`,
	// As listener_exit, but the handler panics.
	"listener_panics": `use web;

main() {
  sub := web.events(web.document.body, ["click"]);
  e := sub.next();
  assert(e is absent, "boom");
}
`,
	// The event's handler starts more goroutine steps than one pump's step
	// budget (wasmweb.StepBudget), so DONE can only print in a continuation the
	// loader runs through schedule_pump, and that continuation ends the program.
	"continuation": webReactorExterns + `
main() {
  sub_id := _web_subscribe(4, 0);
  print_line("ID sub {sub_id}");
  ch := _web_channel(sub_id);
  v := <-ch;
  _web_unsubscribe(sub_id);
  done := channel[i32](5000);
  for i in 0..5000 {
    go { done.send(1); };
  }
  count := 0;
  while count < 5000 {
    r := <-done;
    count = count + 1;
  }
  print_line("DONE");
}
`,
	// A consumer ends the only registration while main waits on a channel no
	// one can send to: the pump that runs it finds nothing runnable, nothing
	// live and main not done — a deadlock.
	"pump_deadlock": webReactorExterns + `
main() {
  sub_id := _web_subscribe(4, 0);
  print_line("ID sub {sub_id}");
  ch := _web_channel(sub_id);
  go {
    v := <-ch;
    if v is present {
      print_line("GOT {v}");
    }
    _web_unsubscribe(sub_id);
  };
  never := channel[i32](1);
  w := <-never;
  print_line("never");
}
`,
}

// TestWebReactorHostEntries drives host entries into a live reactor under the
// pinned node (docs/web-apps.md "Budget and continuation", "Re entrancy",
// "Failure inside a handler", "Program lifetime and termination"). A host
// entry made while _initialize's drain runs must not nest a pump. However the
// instance exits — a clean drain, a panic or a deadlock — it is terminated: a
// later enqueue hands its handle back and a later pump returns PumpTerminated
// without running anything. The loader runs the continuation a pump schedules,
// swallows a clean exit inside a listener's pump or a continuation, surfaces
// any other, and after an exit makes no further call into the instance.
func TestWebReactorHostEntries(t *testing.T) {
	t.Parallel()
	nodePath, driver := buildWebReactorPrograms(t, webHostEntryPrograms)

	t.Run("reentrant_init", func(t *testing.T) {
		got := runNodeDriver[webReactorResult](t, nodePath, driver, "reentrant_init", "init")
		if got.DriverError != "" || !got.OK || len(got.Errs) != 0 {
			t.Fatalf("driver error %q, ok=%v, stderr %q; want init() to resolve cleanly", got.DriverError, got.OK, got.Errs)
		}
		if got.Nested == nil || *got.Nested != wasmweb.PumpMore {
			t.Errorf("the pump nested in main returned %v, want %d (an outer pump is running)", got.Nested, wasmweb.PumpMore)
		}
		// The consumer runs in _initialize's own drain, after main's call returns.
		if want := []string{"AFTER POKE", "GOT 7"}; !slices.Equal(got.Logs, want) {
			t.Errorf("logs = %q, want %q", got.Logs, want)
		}
	})

	for _, c := range []struct {
		name    string
		ops     []string
		pumps   []int
		exits   []int
		returns []int // what each enqueue returned, in order
		logs    []string
		wantErr string
	}{
		// The third pump ends the program; the instance then takes nothing more.
		{"main_loop", []string{"enq:sub:1", "pump", "enq:sub:2", "pump", "enq:sub:3", "pump?", "enq:sub:4", "pump"},
			[]int{wasmweb.PumpIdle, wasmweb.PumpIdle, wasmweb.PumpTerminated}, []int{0},
			[]int{0, 0, 0, 4}, []string{"ready", "1", "2", "3"}, ""},
		// The second pump panics; the instance is terminated, not left pumping.
		{"handler_panics", []string{"enq:sub:1", "pump", "enq:sub:2", "pump?", "enq:sub:3", "pump"},
			[]int{wasmweb.PumpIdle, wasmweb.PumpTerminated}, []int{1},
			[]int{0, 0, 3}, []string{"1", "2"}, "boom"},
		// The pump spends its budget and returns PumpMore; the continuation the
		// loader schedules runs without anything else holding node open, and its
		// clean exit is swallowed — nothing reaches stderr or the event loop —
		// leaving the instance terminated.
		{"continuation", []string{"enq:sub:1", "pump", "await:DONE", "pump"},
			[]int{wasmweb.PumpMore, wasmweb.PumpTerminated}, nil,
			[]int{0}, []string{"DONE"}, ""},
		// The pump's drain is a deadlock: it aborts with exit code 2, and the
		// instance is terminated.
		{"pump_deadlock", []string{"enq:sub:1", "pump?", "enq:sub:2", "pump"},
			[]int{wasmweb.PumpTerminated}, []int{2},
			[]int{0, 2}, []string{"GOT 1"}, "all goroutines are asleep - deadlock!"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := runNodeDriver[webReactorResult](t, nodePath, driver, append([]string{c.name, "script"}, c.ops...)...)
			if got.DriverError != "" || !got.OK {
				t.Fatalf("driver error %q, ok=%v, exitSignal=%v code=%d", got.DriverError, got.OK, got.ExitSignal, got.Code)
			}
			if !slices.Equal(got.Pumps, c.pumps) || !slices.Equal(got.Exits, c.exits) {
				t.Errorf("pumps = %v and exits = %v, want %v and %v", got.Pumps, got.Exits, c.pumps, c.exits)
			}
			var returns []int
			for _, e := range got.Enqueues {
				returns = append(returns, e[2])
			}
			if !slices.Equal(returns, c.returns) {
				t.Errorf("enqueue returned %v, want %v", returns, c.returns)
			}
			if !slices.Equal(got.Logs, c.logs) {
				t.Errorf("logs = %q, want %q", got.Logs, c.logs)
			}
			if errs := strings.Join(got.Errs, "\n"); (c.wantErr == "") != (errs == "") || !strings.Contains(errs, c.wantErr) {
				t.Errorf("stderr = %q, want %q", got.Errs, c.wantErr)
			}
			// Nothing is pending, so the loader holds nothing open: a ref'd
			// port would keep node from exiting at all.
			if got.OpenPorts != 0 {
				t.Errorf("%d MessagePorts still hold node open, want none", got.OpenPorts)
			}
		})
	}

	// The first click ends the program inside its listener's pump; the second
	// reaches a listener the page still holds, which must not call in again.
	t.Run("listener_exit", func(t *testing.T) {
		got := runNodeDriver[webReactorResult](t, nodePath, driver, "listener_exit", "script", "click:click", "click:click", "pump")
		assertWebListenerExit(t, got)
		if len(got.Enqueues) != 1 {
			t.Errorf("enqueues = %v, want only the first click's", got.Enqueues)
		}
		if want := []string{"CLICKED"}; !slices.Equal(got.Logs, want) {
			t.Errorf("logs = %q, want %q", got.Logs, want)
		}
	})

	// A panic in the handler surfaces to the dispatching page as init() would
	// surface it.
	t.Run("listener_panics", func(t *testing.T) {
		got := runNodeDriver[webReactorResult](t, nodePath, driver, "listener_panics", "script", "click:click")
		if got.DriverError != "" || !got.ExitSignal || got.Code != 1 {
			t.Errorf("driver error %q, exitSignal=%v code=%d; want the click to surface exit code 1", got.DriverError, got.ExitSignal, got.Code)
		}
		if !strings.Contains(strings.Join(got.Errs, "\n"), "boom") {
			t.Errorf("stderr %q does not contain the panic message", got.Errs)
		}
	})
}

// assertWebCleanExit requires the driven program to have ended with exit code 0
// and nothing on stderr.
func assertWebCleanExit(t *testing.T, got webReactorResult) {
	t.Helper()
	if got.DriverError != "" {
		t.Fatalf("the driver failed: %s", got.DriverError)
	}
	if !got.ExitSignal || got.Code != 0 || len(got.Errs) != 0 {
		t.Errorf("exitSignal=%v code=%d stderr=%q, want a clean exit", got.ExitSignal, got.Code, got.Errs)
	}
}

// assertWebListenerExit requires a program the driver ended through a loader
// listener, then pumped once more, to have ended cleanly: the listener's pump
// swallowed exit code 0 — nothing reached the dispatching page and nothing
// went to stderr — and the instance is terminated, so the last pump ran
// nothing.
func assertWebListenerExit(t *testing.T, got webReactorResult) {
	t.Helper()
	if got.DriverError != "" {
		t.Fatalf("the driver failed: %s", got.DriverError)
	}
	if !got.OK || got.ExitSignal || len(got.Errs) != 0 {
		t.Errorf("ok=%v exitSignal=%v code=%d stderr=%q, want the listener to end the program cleanly",
			got.OK, got.ExitSignal, got.Code, got.Errs)
	}
	if len(got.Pumps) == 0 || got.Pumps[len(got.Pumps)-1] != wasmweb.PumpTerminated {
		t.Errorf("pumps = %v, want the last one to return %d (terminated)", got.Pumps, wasmweb.PumpTerminated)
	}
}

// TestWebInstanceCacheTracksCoroVariants rebuilds one program into one build
// cache while a function outside Box's declaration starts and stops blocking
// (promise-language/promise#68). A cached instance object is reused by a key
// over its declaration, but on wasm32-web it also carries the coroutine
// variants its body reaches: Box[int].get has one only while helper blocks.
// Without them in the key, the rebuild after helper started blocking reused the
// variant-less Box[int] object, the link left Box[int].get$coro as an import no
// host provides, and init() rejected.
func TestWebInstanceCacheTracksCoroVariants(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping build+node integration test in short mode")
	}
	nodePath, err := resolveWasmRuntime("node")
	if err != nil {
		t.Skipf("the pinned node is not obtainable here: %v", err)
	}
	bin := clitest.Bin(t)
	dir := clitest.TempDir(t)
	env := []string{"PROMISE_CACHE=" + filepath.Join(dir, "cache")}
	outDir := filepath.Join(dir, "out")
	if err := os.MkdirAll(outDir, 0755); err != nil {
		t.Fatal(err)
	}
	driver := filepath.Join(outDir, "driver.cjs")
	if err := os.WriteFile(driver, []byte(webReactorDriver), 0644); err != nil {
		t.Fatal(err)
	}

	for _, step := range []struct{ name, helperBody, want string }{
		{"returns", "return 1;", "GOT 1 -1"},
		{"blocks", "return (<-ch) ?: 0;", "GOT 41 -1"},
		{"returns_again", "return 1;", "GOT 1 -1"},
	} {
		srcFile := filepath.Join(dir, step.name, "main.pr")
		if err := os.MkdirAll(filepath.Dir(srcFile), 0755); err != nil {
			t.Fatal(err)
		}
		src := strings.Replace(webCoroCacheProgram, "HELPER_BODY", step.helperBody, 1)
		if err := os.WriteFile(srcFile, []byte(src), 0644); err != nil {
			t.Fatal(err)
		}
		clitest.RunOK(t, bin, env, "build", "-target", "wasm32-web", "-o", filepath.Join(outDir, step.name+".wasm"), srcFile)
		got := runNodeDriver[webReactorResult](t, nodePath, driver, step.name, "init")
		if !got.OK || strings.Join(got.Logs, "\n") != step.want || len(got.Errs) != 0 {
			t.Errorf("%s: init() ok=%v logs=%q stderr=%q, want ok logs=[%q] and no stderr",
				step.name, got.OK, got.Logs, got.Errs, step.want)
		}
	}
}
