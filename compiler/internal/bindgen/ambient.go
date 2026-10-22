package bindgen

// ambientRef is one host object the wasm32-web loaders seed into the ref table
// at load time. Its handle is its index in ambientRefs plus one (slot 0 is the
// null sentinel).
//
// ambientRefs is the one definition of which slot holds which object: the JS
// seed list (emitRefTable) and the Promise roots that name the slots
// (emitAmbientRoots) are both rendered from it, so the two cannot disagree on
// which slot is `document` (docs/web-apps.md §"One name one definition").
type ambientRef struct {
	global string // JS global stored in the slot
	root   string // module-level Promise getter naming it; "" for none
	typ    string // IDL definition the root wraps; the root is emitted only when the module binds it
	doc    string // the root's `doc text
}

var ambientRefs = []ambientRef{
	{global: "globalThis"},
	{global: "document", root: "document", typ: "Document",
		doc: "The page's document (WebIDL `Document`), seeded by the loader before the program runs. " +
			"Borrowed: dropping it never releases the host object."},
	{global: "console"}, // root: #672 (console is a WebIDL namespace, #170)
}

// ambientHandle returns the ref-table handle of ambientRefs[i].
func ambientHandle(i int) int { return i + 1 }
