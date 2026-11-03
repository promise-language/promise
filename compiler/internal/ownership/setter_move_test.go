package ownership

import "testing"

// #786: a setter owns its value parameter and frees it, so the assignment that
// calls it must move the right-hand side. A local still readable after
// `h.item = r` would be a use after free: the setter has already dropped it.
// Each setter form is a separate call path in codegen (an instance setter, a
// same-file module-level setter, a `global setter), so each is checked here.

func TestSetterAssignMovesLocal(t *testing.T) {
	errs := ownerErrs(t, `
		type Res { string name; }
		type Seen {
			int n;
			get item Res => Res(name: "");
			set item(Res value) { this.n = value.name.len; }
		}
		test() {
			h := Seen(n: 0);
			r := Res(name: "a" + "b");
			h.item = r;
			n := r.name.len;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'r'")
}

func TestModuleSetterAssignMovesLocal(t *testing.T) {
	errs := ownerErrs(t, `
		type Res { string name; }
		get sink Res => Res(name: "");
		set sink(Res value) { n := value.name.len; }
		test() {
			r := Res(name: "a" + "b");
			sink = r;
			n := r.name.len;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'r'")
}

func TestGlobalSetterAssignMovesLocal(t *testing.T) {
	errs := ownerErrs(t, `
		type Res { string name; }
		type Registry {
			int n;
			get last Res `+"`global"+` => Res(name: "");
			set last(Res value) `+"`global"+` { n := value.name.len; }
		}
		test() {
			r := Res(name: "a" + "b");
			Registry.last = r;
			n := r.name.len;
		}
	`)
	expectOwnerError(t, errs, "use of moved variable 'r'")
}

// The setter owns what it was handed, so it may move it on into a `move`
// parameter — an instance setter and a module-level one alike. A borrowed
// parameter would be refused here ("cannot move borrowed parameter").
func TestSetterMayMoveItsParameterOn(t *testing.T) {
	ownerOK(t, `
		type Res { string name; }
		keep(Res move r) {}
		type Seen {
			int n;
			get item Res => Res(name: "");
			set item(Res value) { keep(move value); }
		}
		get sink Res => Res(name: "");
		set sink(Res value) { keep(move value); }
		test() {
			h := Seen(n: 0);
			h.item = Res(name: "a" + "b");
			sink = Res(name: "c" + "d");
		}
	`)
}

// Owning its parameter does not let a setter use it after moving it on.
func TestSetterParameterUseAfterMove(t *testing.T) {
	errs := ownerErrs(t, `
		type Res { string name; }
		keep(Res move r) {}
		type Seen {
			int n;
			get item Res => Res(name: "");
			set item(Res value) {
				keep(move value);
				this.n = value.name.len;
			}
		}
		test() {}
	`)
	expectOwnerError(t, errs, "use of moved variable 'value'")
}
