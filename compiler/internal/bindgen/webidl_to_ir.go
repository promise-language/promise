package bindgen

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"unicode"

	"github.com/promise-language/promise/compiler/internal/wasmweb"
	"github.com/promise-language/promise/compiler/internal/webidl"
)

// WebIdlToIR converts a parsed WebIDL file into a slice of binding IR modules.
// Each WebIDL interface produces one IR Resource, which records its inheritance
// parent but carries only its own members — the generated `is` clause is how a
// derived interface reaches its parent's (T1507). A parent is converted before
// the interfaces deriving from it, because a derived member's name depends on
// what it inherits (docs/wasm-bindings.md, "Shadowed Members"); the resources
// still come out in input order. Dictionaries become Records carrying their
// inherited members as their own fields (flattened). Enums become IR Enums. A
// typedef names no type: each use of one lowers as its target written in its
// place (convertWebIdlTypeRef). The file produces a single module named from the
// first interface, or "web" if no interfaces are present.
//
// It fails when two values of one enum generate the same variant
// (convertEnum), when a dictionary's inheritance cannot be flattened — an
// ancestor that is not a dictionary in the file, or a cycle — when a typedef
// refers to itself (typedefTargets), when a type names a definition no bound
// IDL file defines (unboundRefs), or when an anonymous getter, setter or
// deleter cannot be reached by property access (anonymousSpecialError),
// reporting every such value, dictionary, typedef, declaration and operation.
//
// Call webidl.Merge(file) before this function to apply partials, mixins, and
// includes statements, and to record the members a derived interface
// redeclares (webidl.Interface.Redeclared) and those a named property may hide
// (webidl.Interface.OverriddenBuiltIns).
func WebIdlToIR(file *webidl.File) ([]*Module, error) {
	c := &webidlConverter{
		file:      file,
		dicts:     make(map[string]*webidl.Dictionary),
		ifaces:    interfacesByName(file),
		converted: make(map[*webidl.Interface]*convertedInterface),
	}
	for _, d := range file.Dictionaries {
		c.dicts[d.Name] = d
	}
	return c.convert()
}

type webidlConverter struct {
	file  *webidl.File
	dicts map[string]*webidl.Dictionary // every dictionary in file, by name
	// typedefs holds the target of every typedef a use lowers through, by name
	// (typedefTargets).
	typedefs map[string]*webidl.TypeRef
	// ifaces resolves an interface's parent by name (interfacesByName).
	ifaces map[string]*webidl.Interface
	// converted memoizes each interface's conversion, so a parent is converted
	// once, before its children, whatever the input order.
	converted map[*webidl.Interface]*convertedInterface
	// errs collects the members convertInterface cannot bind; convert reports
	// them with the dictionaries'.
	errs []error
}

// convertedInterface is one interface's conversion: its Resource, and the
// instance members visible on it — its own and those it inherits — which the
// interfaces deriving from it are named against. visible stays nil while the
// interface is being converted, so the back edge of an inheritance cycle
// inherits nothing instead of recursing forever.
type convertedInterface struct {
	res     Resource
	visible map[string][]slot
}

// interfacesByName indexes a merged file's interfaces by name, mixins excluded,
// for resolving an interface's parent.
func interfacesByName(file *webidl.File) map[string]*webidl.Interface {
	ifaces := make(map[string]*webidl.Interface)
	for _, iface := range file.Interfaces {
		if !isMixin(iface) {
			ifaces[iface.Name] = iface
		}
	}
	return ifaces
}

// resource converts iface, after its parent, once.
func (c *webidlConverter) resource(iface *webidl.Interface) *convertedInterface {
	if done, ok := c.converted[iface]; ok {
		return done
	}
	ci := &convertedInterface{}
	c.converted[iface] = ci
	var inherited map[string][]slot
	if parent := c.ifaces[iface.Parent]; parent != nil {
		inherited = c.resource(parent).visible
	}
	ci.res, ci.visible = c.convertInterface(iface, inherited)
	return ci
}

func (c *webidlConverter) convert() ([]*Module, error) {
	m := &Module{
		Name:         "web",
		ImportModule: wasmweb.ImportModule,
	}

	// Every lowering below inlines typedefs, so their table comes first.
	var errs []error
	c.typedefs, errs = typedefTargets(c.file)
	errs = append(errs, unboundRefs(c.file)...)

	// Convert enums
	for _, e := range c.file.Enums {
		t, enumErrs := c.convertEnum(e)
		errs = append(errs, enumErrs...)
		m.Types = append(m.Types, t)
	}

	// Convert dictionaries as records
	for _, d := range c.file.Dictionaries {
		t, err := c.convertDictionary(d)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		m.Types = append(m.Types, t)
	}

	// Convert typedefs as aliases
	for _, td := range c.file.Typedefs {
		m.Types = append(m.Types, c.convertTypedef(td))
	}

	// Convert interfaces as resources
	for _, iface := range c.file.Interfaces {
		// Skip mixin-tagged interfaces (they're merged into targets already)
		if isMixin(iface) {
			continue
		}
		m.Resources = append(m.Resources, c.resource(iface).res)
	}
	if errs = append(errs, c.errs...); len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	// Convert callbacks as function type aliases (skipped in IR for now —
	// callbacks would require lambda types which aren't in the IR)

	for _, iface := range c.file.Interfaces {
		if !isMixin(iface) {
			m.Name = idlToSnake(iface.Name)
			break
		}
	}

	// Scan all type references for JsValue usage
	m.HasJsValue = moduleHasJsValue(m)

	return []*Module{m}, nil
}

func isMixin(iface *webidl.Interface) bool {
	for _, ea := range iface.ExtAttrs {
		if ea.Name == "_mixin" {
			return true
		}
	}
	return false
}

// webidlDoc returns the `doc text of a declaration bound from WebIDL: its IDL
// comment, or — spec IDL carries almost none, its prose living outside the IDL
// block — a line naming the construct it binds, e.g. "`Element.setAttribute`
// (WebIDL operation)" (docs/wasm-bindings.md, "Generated Module Contents").
func webidlDoc(comment, construct, idlName string) string {
	if comment != "" {
		return comment
	}
	return "`" + idlName + "` (WebIDL " + construct + ")"
}

// convertEnum converts an enum to an IR Enum with one case per value, named by
// idlEnumValueToPascal. Two values that generate one variant cannot both be
// cases: each value after the first is reported, at the enum's position (the
// parser records none per value).
func (c *webidlConverter) convertEnum(e *webidl.Enum) (Type, []error) {
	t := Type{
		Name: e.Name,
		Kind: TypeEnum,
		Doc:  webidlDoc(e.Doc, "enum", e.Name),
	}
	var errs []error
	generatedBy := make(map[string]string) // variant name → the first value generating it
	for _, v := range e.Values {
		name := idlEnumValueToPascal(v)
		if first, ok := generatedBy[name]; ok {
			errs = append(errs, &webidl.ParseError{Pos: e.Pos, Msg: fmt.Sprintf("enum %s: values %q and %q both generate variant %s", e.Name, first, v, name)})
			continue
		}
		generatedBy[name] = v
		t.Cases = append(t.Cases, Case{Name: name})
	}
	return t, errs
}

// convertDictionary converts a dictionary to a Record holding every member the
// dictionary has: its inherited dictionaries' members first, from the root of
// the chain down, then its own, each dictionary's in declaration order. A
// dictionary crosses the boundary by value as one complete set of fields, so it
// is flattened rather than made an `is` child (docs/wasm-bindings.md,
// "Dictionary inheritance"). A flattened type never names its parent, so an
// ancestor that is not a dictionary here would silently drop members — it is
// an error, as is a cycle.
func (c *webidlConverter) convertDictionary(d *webidl.Dictionary) (Type, error) {
	chain := []*webidl.Dictionary{d} // d, then its parent, …, then the root
	for a := d; a.Parent != ""; {
		p, ok := c.dicts[a.Parent]
		if !ok {
			return Type{}, fmt.Errorf("dictionary %s inherits from %s, which is not a dictionary in the bound IDL files", d.Name, a.Parent)
		}
		if slices.Contains(chain, p) {
			return Type{}, fmt.Errorf("dictionary %s: inheritance cycle through %s", d.Name, p.Name)
		}
		chain = append(chain, p)
		a = p
	}

	t := Type{
		Name: d.Name,
		Kind: TypeRecord,
		Doc:  webidlDoc(d.Doc, "dictionary", d.Name),
	}
	for i := len(chain) - 1; i >= 0; i-- {
		for _, m := range chain[i].Members {
			f := Field{
				Name: idlToSnake(m.Name),
				Type: convertWebIdlTypeRef(m.Type, c.typedefs, c.file.WindowAliases),
			}
			if !m.Required {
				// Optional dictionary members map to Option type
				elem := f.Type
				f.Type = TypeRef{Kind: OptionKind, Elem: &elem}
			}
			t.Fields = append(t.Fields, f)
		}
	}
	return t, nil
}

func (c *webidlConverter) convertTypedef(td *webidl.Typedef) Type {
	ref := convertWebIdlTypeRef(td.Type, c.typedefs, c.file.WindowAliases)
	return Type{
		Name:   td.Name,
		Kind:   TypeAlias,
		Target: &ref,
		Doc:    td.Doc,
	}
}

// typedefTargets returns the target of every typedef in file, by name: the
// table convertWebIdlTypeRef inlines typedefs through. A typedef refers to the
// typedefs its target names where the lowering follows a name — never inside a
// union, Promise<T> or record<K, V>, each lowered to JsValue. One that refers to
// itself, directly or through other typedefs, cannot be inlined, and WebIDL
// forbids it: each typedef on such a cycle is reported and left out of the
// table, so its uses lower as its name and every lowering ends.
func typedefTargets(file *webidl.File) (map[string]*webidl.TypeRef, []error) {
	targets := make(map[string]*webidl.TypeRef, len(file.Typedefs))
	for _, td := range file.Typedefs {
		targets[td.Name] = td.Type
	}
	// refers returns the typedefs the target of the typedef name refers to.
	refers := func(name string) []string {
		var names []string
		eachName(convertWebIdlTypeRef(targets[name], nil, nil), func(n string) bool {
			if _, ok := targets[n]; ok {
				names = append(names, n)
			}
			return false
		})
		return names
	}
	// reaches reports whether the typedef from is the typedef to, or refers to
	// it through any number of typedefs.
	reaches := func(from, to string) bool {
		seen := make(map[string]bool)
		for pending := []string{from}; len(pending) > 0; {
			name := pending[len(pending)-1]
			pending = pending[:len(pending)-1]
			if name == to {
				return true
			}
			if !seen[name] {
				seen[name] = true
				pending = append(pending, refers(name)...)
			}
		}
		return false
	}

	var errs []error
	var cyclic []string
	for _, td := range file.Typedefs {
		for _, next := range refers(td.Name) {
			if reaches(next, td.Name) {
				errs = append(errs, &webidl.ParseError{Pos: td.Pos, Msg: fmt.Sprintf("typedef %s: cycle through %s", td.Name, next)})
				cyclic = append(cyclic, td.Name)
				break
			}
		}
	}
	for _, name := range cyclic {
		delete(targets, name)
	}
	return targets, errs
}

// definedTypes returns every name a reference (firstRef) may carry that resolves
// in the generated module: each interface (mixins excluded), dictionary, enum,
// typedef and callback the merged file defines, and JsValue, which the lowering
// itself produces. Any other name is defined in none of the bound IDL files
// (docs/wasm-bindings.md, "Supported subset").
func definedTypes(file *webidl.File) map[string]bool {
	defined := map[string]bool{"JsValue": true}
	for name := range interfacesByName(file) {
		defined[name] = true
	}
	for _, d := range file.Dictionaries {
		defined[d.Name] = true
	}
	for _, e := range file.Enums {
		defined[e.Name] = true
	}
	for _, td := range file.Typedefs {
		defined[td.Name] = true
	}
	for _, cb := range file.Callbacks {
		defined[cb.Name] = true
	}
	return defined
}

// firstRef returns the first name the types reference for which match reports
// true, or "". A reference is a name the type carries lowered with no typedef
// inlined (convertWebIdlTypeRef), aliases being the merged file's
// File.WindowAliases: a use of a typedef names the typedef, a name defined
// outside IDL names what it lowers to, and a name inside a union, Promise<T> or
// record<K, V> — each lowered to JsValue — is not one.
func firstRef(aliases map[string]string, match func(name string) bool, types ...*webidl.TypeRef) string {
	var hit string
	for _, t := range types {
		if t == nil {
			continue
		}
		if eachName(convertWebIdlTypeRef(t, nil, aliases), func(name string) bool {
			hit = name
			return match(name)
		}) {
			return hit
		}
	}
	return ""
}

// unboundRefs reports each declaration whose type references a definition no
// bound IDL file defines (definedTypes) — the typedefs, then the dictionary
// members, then the interface members, each in file order — at its position,
// naming the first such reference (firstRef). The generated module would name a
// type that does not exist. A mixin's members are reported on the interfaces
// that include it.
func unboundRefs(file *webidl.File) []error {
	defined := definedTypes(file)
	var errs []error
	report := func(pos webidl.Pos, what string, types ...*webidl.TypeRef) {
		if name := firstRef(file.WindowAliases, func(name string) bool { return !defined[name] }, types...); name != "" {
			errs = append(errs, &webidl.ParseError{Pos: pos, Msg: fmt.Sprintf("%s names %s, which is not defined in the bound IDL files", what, name)})
		}
	}
	for _, td := range file.Typedefs {
		report(td.Pos, "typedef "+td.Name, td.Type)
	}
	for _, d := range file.Dictionaries {
		for _, m := range d.Members {
			report(m.Pos, d.Name+"."+m.Name, m.Type)
		}
	}
	for _, iface := range file.Interfaces {
		if isMixin(iface) {
			continue
		}
		for _, m := range iface.Members {
			_, label, _, types, pos := keepUnit(m)
			report(pos, iface.Name+"."+label, types...)
		}
	}
	return errs
}

// convertInterface converts one interface, given the instance members it
// inherits by Promise name, and returns its Resource with the instance members
// visible on it for the interfaces deriving from it.
func (c *webidlConverter) convertInterface(iface *webidl.Interface, inherited map[string][]slot) (Resource, map[string][]slot) {
	res := Resource{
		Name:   iface.Name,
		Parent: iface.Parent,
		Drop:   true,
		Doc:    webidlDoc(iface.Doc, "interface", iface.Name),
	}

	// The ordinal of each member in the overload set of its Promise name, in
	// merged member order — see memberNamer and numberOverload.
	names := newMemberNamer(inherited)

	for _, member := range iface.Members {
		name, ok := memberName(member)
		if !ok {
			continue // nothing to emit
		}
		switch m := member.(type) {
		case *webidl.Attribute:
			idlName := iface.Name + "." + m.Name
			doc := webidlDoc(m.Doc, "attribute", idlName)
			viaPrototype := !m.Static && callsDeclaringMember(iface, m.Name)
			// Getter
			getter := Func{
				Name:         name,
				Kind:         FuncMethod,
				Accessor:     AccessorGetter,
				OwnerType:    iface.Name,
				ImportName:   idlName + ".get",
				Results:      []TypeRef{convertWebIdlTypeRef(m.Type, c.typedefs, c.file.WindowAliases)},
				Doc:          doc,
				ViaPrototype: viaPrototype,
			}
			if m.Static {
				getter.Kind = FuncStatic
			}
			// One ordinal names the attribute's getter and setter alike.
			k := names.declaredOnce(getter)
			numberOverload(&getter, k)
			res.Methods = append(res.Methods, getter)

			// Setter (unless readonly)
			if !m.Readonly {
				setter := Func{
					Name:         name,
					Kind:         FuncMethod,
					Accessor:     AccessorSetter,
					OwnerType:    iface.Name,
					ImportName:   idlName + ".set",
					Params:       []Param{{Name: "value", Type: convertWebIdlTypeRef(m.Type, c.typedefs, c.file.WindowAliases)}},
					Doc:          doc,
					ViaPrototype: viaPrototype,
				}
				if m.Static {
					setter.Kind = FuncStatic
				}
				numberOverload(&setter, k)
				res.Methods = append(res.Methods, setter)
			}

		case *webidl.Operation:
			f := c.convertOperation(iface.Name, m, name)
			if m.Name == "" {
				if err := anonymousSpecialError(m, f); err != nil {
					c.errs = append(c.errs, err)
					continue
				}
			}
			// Both marks are keyed by identifier; a static of the same one
			// (Response.json beside json()) is reached through the interface
			// object, not the prototype.
			f.ViaPrototype = f.Kind == FuncMethod && callsDeclaringMember(iface, m.Name)
			numberOverload(&f, names.operation(f))
			res.Methods = append(res.Methods, f)

		case *webidl.Constructor:
			f := c.convertConstructor(iface.Name, m)
			numberOverload(&f, names.operation(f))
			res.Methods = append(res.Methods, f)

		case *webidl.Const:
			// Constants become static getter-like functions
			idlName := iface.Name + "." + m.Name
			f := Func{
				Name:       name,
				Kind:       FuncStatic,
				OwnerType:  iface.Name,
				ImportName: idlName,
				Results:    []TypeRef{convertWebIdlTypeRef(m.Type, c.typedefs, c.file.WindowAliases)},
				Doc:        webidlDoc(m.Doc, "constant", idlName),
			}
			numberOverload(&f, names.declaredOnce(f))
			res.Methods = append(res.Methods, f)
		}
	}

	return res, names.visible()
}

// slot is an instance member a derived interface inherits: the ordinal the
// overload set of its Promise name gave it, and the member (an attribute's
// getter) whose signature a redeclaration is compared with.
type slot struct {
	ordinal int
	member  Func
}

// memberNamer gives each member of one interface its ordinal in the overload
// set of its Promise name (numberOverload spells it). The set spans the `is`
// chain (docs/wasm-bindings.md, "Shadowed Members"): an instance member that is
// exactly an inherited one — sameSignature — overrides it and takes its
// ordinal, and every other member is numbered after the instance members the
// interface inherits under that name, so a redeclaration never takes over an
// inherited slot the override rule would reject. With nothing inherited the
// numbering is the per-interface one of "Overloaded Operations".
type memberNamer struct {
	inherited  map[string][]slot  // the parent's visible instance members, by Promise name
	last       map[string]int     // the highest ordinal each name has been given so far
	overridden map[ordinalOf]bool // inherited slots an own member already overrides
	own        map[string][]slot  // own instance members that took a new ordinal
}

// ordinalOf identifies a slot: the ordinal a name's overload set gave it.
type ordinalOf struct {
	name    string
	ordinal int
}

func newMemberNamer(inherited map[string][]slot) *memberNamer {
	return &memberNamer{
		inherited:  inherited,
		last:       make(map[string]int),
		overridden: make(map[ordinalOf]bool),
		own:        make(map[string][]slot),
	}
}

// operation returns the ordinal of an operation or a constructor, f named by
// its base name. Static operations and constructors are `global`, so they never
// override an inherited slot, but they share their interface's set with its
// instance operations.
func (n *memberNamer) operation(f Func) int {
	if f.Kind == FuncMethod {
		if k, ok := n.override(f); ok {
			return k
		}
	}
	return n.add(f, n.next(f.Name))
}

// declaredOnce returns the ordinal of a member its interface declares once
// under its name, f named by its base name: an attribute, f its getter, whose
// setter shares the ordinal, or a constant, which is `global` and so never
// overrides an inherited slot. Such a member is numbered only when it
// redeclares an inherited name; otherwise it keeps its name and takes no
// ordinal from its interface's operations, as before shadows were named.
func (n *memberNamer) declaredOnce(f Func) int {
	if len(n.inherited[f.Name]) == 0 {
		return n.add(f, 1)
	}
	if f.Kind == FuncMethod {
		if k, ok := n.override(f); ok {
			return k
		}
	}
	return n.add(f, n.next(f.Name))
}

// override returns the ordinal of the first inherited slot of f's name that f
// is exactly and that no earlier member of this interface overrides.
func (n *memberNamer) override(f Func) (int, bool) {
	for _, s := range n.inherited[f.Name] {
		key := ordinalOf{name: f.Name, ordinal: s.ordinal}
		if !n.overridden[key] && sameSignature(s.member, f) {
			n.overridden[key] = true
			return s.ordinal, true
		}
	}
	return 0, false
}

// next returns the next ordinal of a name: one past the highest the interface
// has given it, which starts at the highest it inherits.
func (n *memberNamer) next(name string) int {
	if _, ok := n.last[name]; !ok {
		for _, s := range n.inherited[name] {
			n.last[name] = max(n.last[name], s.ordinal)
		}
	}
	n.last[name]++
	return n.last[name]
}

// add records an instance member as a slot the interfaces deriving from this
// one inherit, and returns its ordinal.
func (n *memberNamer) add(f Func, k int) int {
	if f.Kind == FuncMethod {
		n.own[f.Name] = append(n.own[f.Name], slot{ordinal: k, member: f})
	}
	return k
}

// visible returns the instance members visible on the interface — those it
// inherits, then its own — by name. No list is appended to in place, so
// siblings deriving from one parent never grow one they share.
func (n *memberNamer) visible() map[string][]slot {
	visible := make(map[string][]slot, len(n.inherited)+len(n.own))
	for name, slots := range n.inherited {
		visible[name] = slots
	}
	for name, slots := range n.own {
		visible[name] = slices.Concat(visible[name], slots)
	}
	return visible
}

// sameSignature reports whether a redeclared member is exactly the inherited
// one: the same kind of member, the same parameter types in order, and the same
// result. Parameter names are not part of it. This is identity, not the
// override rule's relaxed matching (docs/language-design.md, "How Inheritance
// Maps to Vtables"): identity is an override under every version of that rule,
// so a binding's names depend on the IDL alone.
func sameSignature(a, b Func) bool {
	if a.Accessor != b.Accessor || len(a.Params) != len(b.Params) || !reflect.DeepEqual(a.Results, b.Results) {
		return false
	}
	for i := range a.Params {
		if !reflect.DeepEqual(a.Params[i].Type, b.Params[i].Type) {
			return false
		}
	}
	return true
}

// numberOverload names the k-th member of an overload set (k is 1-based, in
// merged declaration order, counted across the `is` chain for a member that
// shadows an inherited one). Promise has no overloading (docs/language-design.md,
// "No Overloading"), so every member after the first becomes a member of its
// own: `<name>_<k>`, an operation imported as `<Interface>.<member>.<k>`. The
// import keeps the IDL member as its second component, so the JS glue calls
// that same member with this overload's own arguments and the host's overload
// resolution chooses among them (docs/wasm-bindings.md, "Overloaded
// Operations"). An attribute's import is left alone: an interface declares an
// attribute once, so its import is unique already, and the glue reads the
// accessor from its third component. The extern and link names derive from
// f.Name, so they follow automatically, and so does a constructor's factory
// (constructorFactoryName). Constructors are named constructorName, so they form
// one set — shared only with an operation whose name also snakes to it (an IDL
// `Constructor`), which one count for the set then keeps from colliding with
// them.
func numberOverload(f *Func, k int) {
	if k < 2 {
		return
	}
	f.Name = fmt.Sprintf("%s_%d", f.Name, k)
	if f.Accessor == AccessorNone {
		f.ImportName = fmt.Sprintf("%s.%d", f.ImportName, k)
	}
}

// memberName returns the name of the Promise member m lowers to before
// numberOverload numbers it — the name memberNamer counts its overload set by,
// and the unit a keep list keeps or drops it with (keepUnit). An anonymous
// getter, setter or deleter is named by its keyword; every constructor by
// constructorName. It returns false for a member that lowers to none, having no
// member to emit: an iterable declaration, or an anonymous stringifier or
// legacycaller.
func memberName(m webidl.Member) (string, bool) {
	switch m := m.(type) {
	case *webidl.Attribute:
		return idlToSnake(m.Name), true
	case *webidl.Const:
		return idlToSnake(m.Name), true
	case *webidl.Constructor:
		return constructorName, true
	case *webidl.Operation:
		if m.Name != "" {
			return idlToSnake(m.Name), true
		}
		if m.Special == "getter" || m.Special == "setter" || m.Special == "deleter" {
			return m.Special, true
		}
	}
	return "", false
}

// callsDeclaringMember reports whether the import of iface's instance member
// id calls iface's own member rather than resolving it on the object
// (Func.ViaPrototype): an interface inheriting from iface redeclares it, or a
// named property may hide it.
func callsDeclaringMember(iface *webidl.Interface, id string) bool {
	return iface.Redeclared[id] || iface.OverriddenBuiltIns[id]
}

// convertOperation converts op, named name (memberName).
func (c *webidlConverter) convertOperation(ifaceName string, op *webidl.Operation, name string) Func {
	// The import names the JS member the glue calls. An anonymous special
	// operation has no identifier and no JS member: it is named by its special
	// keyword on both sides — never by an empty member — and the glue reads that
	// keyword as the property access the host exposes it by (emitMethodBody;
	// docs/wasm-bindings.md, "Supported subset").
	member := op.Name
	if op.Special != "" && op.Name == "" {
		member = op.Special
	}

	idlName := ifaceName + "." + member
	f := Func{
		Name:       name,
		Kind:       FuncMethod,
		OwnerType:  ifaceName,
		ImportName: idlName,
		Doc:        webidlDoc(op.Doc, "operation", idlName),
	}
	if op.Static {
		f.Kind = FuncStatic
	}

	for _, p := range op.Params {
		param := Param{
			Name: idlToSnake(p.Name),
			Type: convertWebIdlTypeRef(p.Type, c.typedefs, c.file.WindowAliases),
		}
		if p.Optional {
			elem := param.Type
			param.Type = TypeRef{Kind: OptionKind, Elem: &elem}
		}
		f.Params = append(f.Params, param)
	}

	if op.Return != nil && op.Return.Builtin != "void" && op.Return.Builtin != "undefined" {
		f.Results = []TypeRef{convertWebIdlTypeRef(op.Return, c.typedefs, c.file.WindowAliases)}
	}

	return f
}

// anonymousSpecialError reports an anonymous getter, setter or deleter, op
// converted as f, that property access cannot express, or returns nil. The host
// exposes one only as property access on the object (docs/wasm-bindings.md,
// "Supported subset"), which passes exactly a key — and a setter, a value — and
// gives back no setter result; the glue's forms (emitMethodBody) rely on it.
func anonymousSpecialError(op *webidl.Operation, f Func) error {
	want, passes := 1, "property access passes exactly one: the key"
	if op.Special == "setter" {
		want, passes = 2, "property assignment passes exactly two: the key and the value"
	}
	var msg string
	switch {
	case len(f.Params) != want:
		noun := "arguments"
		if len(f.Params) == 1 {
			noun = "argument"
		}
		msg = fmt.Sprintf("takes %d %s, but %s", len(f.Params), noun, passes)
	case op.Special == "setter" && len(f.Results) > 0:
		msg = "declares a result, but property assignment returns none"
	default:
		return nil
	}
	return &webidl.ParseError{Pos: op.Pos, Msg: fmt.Sprintf("anonymous %s of %s %s", op.Special, f.OwnerType, msg)}
}

func (c *webidlConverter) convertConstructor(ifaceName string, ctor *webidl.Constructor) Func {
	f := Func{
		Name:       constructorName,
		Kind:       FuncConstructor,
		OwnerType:  ifaceName,
		ImportName: ifaceName + ".constructor",
		Doc:        ctor.Doc,
	}
	for _, p := range ctor.Params {
		param := Param{
			Name: idlToSnake(p.Name),
			Type: convertWebIdlTypeRef(p.Type, c.typedefs, c.file.WindowAliases),
		}
		if p.Optional {
			elem := param.Type
			param.Type = TypeRef{Kind: OptionKind, Elem: &elem}
		}
		f.Params = append(f.Params, param)
	}
	return f
}

// convertWebIdlTypeRef converts a WebIDL type reference to the binding IR TypeRef.
// A name typedefs holds is a typedef's, which names no type: it lowers as its
// target written in its place, through any number of typedefs
// (docs/wasm-bindings.md, "WebIDL to Promise", typedefs). A use written nullable
// over a target that is nullable already stays nullable once, as WebIDL forbids
// a nullable of a nullable type. A nil typedefs lowers every name as written
// (ApplyKeepList). Any other name lowers through convertWebIdlName, aliases being
// the merged file's File.WindowAliases.
func convertWebIdlTypeRef(ref *webidl.TypeRef, typedefs map[string]*webidl.TypeRef, aliases map[string]string) TypeRef {
	if ref == nil {
		return TypeRef{Kind: BuiltinKind, Builtin: "void"}
	}

	var result TypeRef

	switch ref.Kind {
	case webidl.BuiltinType:
		result = convertWebIdlBuiltin(ref.Builtin)
	case webidl.NamedType:
		if target, ok := typedefs[ref.Name]; ok {
			result = convertWebIdlTypeRef(target, typedefs, aliases)
			if result.Kind == OptionKind {
				return result
			}
		} else {
			result = convertWebIdlName(ref.Name, aliases)
		}
	case webidl.SequenceType:
		elem := convertWebIdlTypeRef(ref.Elem, typedefs, aliases)
		result = TypeRef{Kind: ListKind, Elem: &elem}
	case webidl.FrozenArrayType:
		elem := convertWebIdlTypeRef(ref.Elem, typedefs, aliases)
		result = TypeRef{Kind: ListKind, Elem: &elem}
	case webidl.ObservableArrayType:
		elem := convertWebIdlTypeRef(ref.Elem, typedefs, aliases)
		result = TypeRef{Kind: ListKind, Elem: &elem}
	case webidl.PromiseType:
		// Promise<T> → Named "JsValue" for now (tasks are separate)
		result = TypeRef{Kind: NamedKind, Name: "JsValue"}
	case webidl.RecordType:
		// record<K, V> → Named "JsValue"
		result = TypeRef{Kind: NamedKind, Name: "JsValue"}
	case webidl.UnionType:
		// Union types → Named "JsValue" (most general)
		result = TypeRef{Kind: NamedKind, Name: "JsValue"}
	default:
		result = TypeRef{Kind: BuiltinKind, Builtin: "u32"}
	}

	// Handle nullable: T? → option<T>
	if ref.Nullable {
		elem := result
		result = TypeRef{Kind: OptionKind, Elem: &elem}
	}

	return result
}

// convertWebIdlName converts a name used as a type. A name that specification
// prose or an extended attribute defines, rather than an IDL definition, lowers
// to what it names (docs/wasm-bindings.md, "Names defined outside IDL"): CSSOM's
// CSSOMString is a DOMString or a USVString at the implementation's choice,
// HTML's WindowProxy is the proxy for a Window, and an identifier a
// [LegacyWindowAlias] names (aliases) is the interface it annotates. Any other
// name is its own.
func convertWebIdlName(name string, aliases map[string]string) TypeRef {
	switch name {
	case "CSSOMString":
		return convertWebIdlBuiltin("DOMString")
	case "WindowProxy":
		return TypeRef{Kind: NamedKind, Name: "Window"}
	}
	if iface, ok := aliases[name]; ok {
		name = iface
	}
	return TypeRef{Kind: NamedKind, Name: name}
}

func convertWebIdlBuiltin(builtin string) TypeRef {
	switch builtin {
	case "DOMString", "USVString", "ByteString":
		return TypeRef{Kind: BuiltinKind, Builtin: "string"}
	case "boolean":
		return TypeRef{Kind: BuiltinKind, Builtin: "bool"}
	case "byte":
		return TypeRef{Kind: BuiltinKind, Builtin: "s8"}
	case "octet":
		return TypeRef{Kind: BuiltinKind, Builtin: "u8"}
	case "short":
		return TypeRef{Kind: BuiltinKind, Builtin: "s16"}
	case "unsigned short":
		return TypeRef{Kind: BuiltinKind, Builtin: "u16"}
	case "long":
		return TypeRef{Kind: BuiltinKind, Builtin: "s32"}
	case "unsigned long":
		return TypeRef{Kind: BuiltinKind, Builtin: "u32"}
	case "long long":
		return TypeRef{Kind: BuiltinKind, Builtin: "s64"}
	case "unsigned long long":
		return TypeRef{Kind: BuiltinKind, Builtin: "u64"}
	case "float", "unrestricted float":
		return TypeRef{Kind: BuiltinKind, Builtin: "f32"}
	case "double", "unrestricted double":
		return TypeRef{Kind: BuiltinKind, Builtin: "f64"}
	case "void", "undefined":
		return TypeRef{Kind: BuiltinKind, Builtin: "void"}
	case "any", "object":
		return TypeRef{Kind: NamedKind, Name: "JsValue"}
	case "bigint":
		return TypeRef{Kind: BuiltinKind, Builtin: "s64"}
	case "symbol":
		return TypeRef{Kind: NamedKind, Name: "JsValue"}
	case "ArrayBuffer", "DataView",
		"Int8Array", "Int16Array", "Int32Array",
		"Uint8Array", "Uint16Array", "Uint32Array", "Uint8ClampedArray",
		"Float32Array", "Float64Array":
		return TypeRef{Kind: NamedKind, Name: "JsValue"}
	default:
		return TypeRef{Kind: BuiltinKind, Builtin: "u32"}
	}
}

// Name conversion utilities: WebIDL camelCase → Promise snake_case

// idlToSnake converts "getElementById" → "get_element_by_id". A `-` separates
// words as `_` does, "margin-top" → "margin_top", and a leading one (a CSS
// vendor prefix) writes nothing: "-webkit-transform" → "webkit_transform", the
// name "webkitTransform" generates. Written as `_`, it would make the member
// private (docs/language-design.md, "Member Level Visibility").
func idlToSnake(s string) string {
	s = strings.TrimPrefix(s, "-")
	if s == "" {
		return s
	}
	var b strings.Builder
	prev := rune(0)
	for i, r := range s {
		if r == '-' {
			b.WriteByte('_')
		} else if r >= 'A' && r <= 'Z' {
			// Insert underscore before uppercase if:
			// 1. Not at start, AND
			// 2. Previous char was lowercase, OR
			// 3. Next char is lowercase (handles "getURL" → "get_url")
			if i > 0 && (unicode.IsLower(prev) || (i+1 < len(s) && unicode.IsLower(rune(s[i+1])))) {
				b.WriteByte('_')
			}
			b.WriteRune(unicode.ToLower(r))
		} else {
			b.WriteRune(r)
		}
		prev = r
	}
	return sanitizeIdent(b.String())
}

// promiseKeywords are reserved words that cannot appear as identifiers in
// generated Promise code. WebIDL freely uses some of these as parameter, member,
// and operation names (e.g. `type`), which would otherwise emit verbatim and fail
// to parse. sanitizeIdent rewrites any colliding identifier (T0724).
// Keep in sync with the keyword tokens in compiler/grammar/PromiseLexer.g4.
// `get`/`set` are contextual (getter/setter syntax) but are sanitized too to avoid
// ambiguity with a method/field of that name.
var promiseKeywords = map[string]bool{
	"absent": true, "as": true, "break": true, "continue": true, "else": true,
	"enum": true, "false": true, "for": true, "go": true, "if": true,
	"in": true, "is": true, "match": true, "move": true, "none": true,
	"present": true, "raise": true, "return": true, "select": true, "this": true,
	"true": true, "type": true, "use": true, "while": true,
	"yield": true, "get": true, "set": true,
}

// sanitizeIdent rewrites an identifier that collides with a Promise keyword by
// prefixing it with "_" (e.g. `type` -> `_type`). Applied inside idlToSnake so it
// covers every generated identifier position (params, dictionary fields,
// attribute/operation names). Type names stay PascalCase and never collide; extern
// link names and WASM import names derive from the raw IDL / PascalCase names, so
// wrapper, extern, and JS glue stay consistent.
func sanitizeIdent(s string) string {
	if promiseKeywords[s] {
		return "_" + s
	}
	return s
}

// idlEnumValueToPascal names the variant a WebIDL enum value generates
// (docs/wasm-bindings.md, "WebIDL to Promise", enums). A value is any string, so
// the rule is total: a word is a maximal run of ASCII letters and digits, every
// other character separates words and is dropped, and the variant is the words
// in order, each with its first letter upper-cased — "read-write" → "ReadWrite",
// "text/html" → "TextHtml". A name that would begin with a digit is prefixed
// with "V" ("2d" → "V2d"), and a value with no letter or digit is "Empty".
func idlEnumValueToPascal(s string) string {
	words := strings.FieldsFunc(s, func(r rune) bool {
		return !('a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9')
	})
	var b strings.Builder
	for _, w := range words {
		b.WriteString(strings.ToUpper(w[:1]))
		b.WriteString(w[1:])
	}
	name := b.String()
	switch {
	case name == "":
		return "Empty"
	case '0' <= name[0] && name[0] <= '9':
		return "V" + name
	}
	return name
}

// eachName calls fn with every NamedKind name in ref, depth-first, stopping at
// the first call that returns true; it reports whether one did. It is the one
// walk over a TypeRef's shape — a new TypeRefKind that can name a type extends
// it here.
func eachName(ref TypeRef, fn func(name string) bool) bool {
	if ref.Kind == NamedKind && fn(ref.Name) {
		return true
	}
	if ref.Elem != nil && eachName(*ref.Elem, fn) {
		return true
	}
	if ref.Ok != nil && eachName(*ref.Ok, fn) {
		return true
	}
	if ref.Err != nil && eachName(*ref.Err, fn) {
		return true
	}
	for _, e := range ref.Elements {
		if eachName(e, fn) {
			return true
		}
	}
	return false
}

// moduleHasJsValue returns true if any TypeRef in the module references JsValue.
func moduleHasJsValue(m *Module) bool {
	check := func(ref TypeRef) bool {
		return eachName(ref, func(name string) bool { return name == "JsValue" })
	}

	for _, t := range m.Types {
		for _, f := range t.Fields {
			if check(f.Type) {
				return true
			}
		}
		for _, c := range t.Cases {
			if c.Type != nil && check(*c.Type) {
				return true
			}
		}
		if t.Target != nil && check(*t.Target) {
			return true
		}
	}
	for _, f := range m.Functions {
		for _, p := range f.Params {
			if check(p.Type) {
				return true
			}
		}
		for _, r := range f.Results {
			if check(r) {
				return true
			}
		}
	}
	for _, r := range m.Resources {
		for _, method := range r.Methods {
			for _, p := range method.Params {
				if check(p.Type) {
					return true
				}
			}
			for _, res := range method.Results {
				if check(res) {
					return true
				}
			}
		}
	}
	return false
}
