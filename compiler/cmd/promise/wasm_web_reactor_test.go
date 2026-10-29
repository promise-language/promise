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
//     does not pump; "pump" pumps; "click:<kind>" dispatches one event to the
//     listeners of that kind, as the page's own event would.
//
// It reports one RESULT: line. Every enqueue — the driver's own, and those the
// loader's event listeners make — is recorded as [sub_id, handle, returned] by
// wrapping the instance's exports, and every event handle Promise code releases
// is recorded by supplying promise_env's "[resource-drop]event" to init().
const webReactorDriver = nodePageDriver + `
const [name, mode, ...vals] = process.argv.slice(2);

const enqueues = [];
const instantiateStreaming = WebAssembly.instantiateStreaming;
WebAssembly.instantiateStreaming = async (source, imports) => {
  const { module, instance } = await instantiateStreaming(source, imports);
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

(async () => {
  const result = { pumps: [], listeners: 0 };
  const drops = [];
  const listeners = [];
  globalThis.document = { body: { addEventListener(kind, fn) { listeners.push({ kind, fn }); } } };
  const mod = await import(pathToFileURL(path.join(dir, name + ".js")).href);
  try {
    const exports = await mod.init({ promise_env: { "[resource-drop]event"(handle) { drops.push(handle); } } });
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
        } else if (verb === "click") {
          for (const l of listeners) {
            if (l.kind === arg) l.fn({ type: arg });
          }
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
  process.stdout.write("RESULT:" + JSON.stringify(result) + "\n");
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
	Listeners   int            `json:"listeners"`
	OK          bool           `json:"ok"`
	ExitSignal  bool           `json:"exitSignal"`
	Code        int            `json:"code"`
	DriverError string         `json:"driverError"`
	IDs         map[string]int `json:"ids"`
	Enqueues    [][3]int       `json:"enqueues"` // [sub_id, handle, returned]
	Drops       []int          `json:"drops"`    // event handles Promise code released
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
	// and the program then ends cleanly.
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
				c.name, "script", "click:click", "click:click", "click:click", "click:gate")
			assertWebCleanExit(t, got)
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
