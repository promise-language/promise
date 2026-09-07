package common

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- fixtures ---

// The coherent baseline every test starts from: two annotations that agree
// across the document and both sema tables, plus one name the document rejects.
// A test states only what it varies.
const (
	copyMetas  = "\t\"copy\": {TargetType, TargetEnum},\n"
	copySpecs  = "\t\"copy\": noParams,\n"
	copyRow    = "| `` `copy `` | types, enums | — | Bitwise copy on assignment |"
	embedMetas = "\t\"embed\": {TargetFunc},\n"
	embedSpecs = "\t\"embed\": {\n" +
		"\t\tpositional: []metaPositional{{name: \"path\", kind: valString}},\n" +
		"\t\tnamed:      map[string]metaValueKind{\"compress\": valBool},\n" +
		"\t},\n"
	embedRow    = "| `` `embed `` | functions | `path` (string, positional); `compress` (bool, named) | Embed a file |"
	rejectedRow = "| `` `inline `` | Inlining is the optimizer's decision. |"
)

// annotationTree writes the two sema tables and the document into a temp tree,
// and installs an empty gap ledger so a fixture is reconciled on its own terms
// rather than against the repository's real divergences.
//
// metas and specs are the bodies of their map literals; doc is the whole
// document, or "" to leave it absent.
func annotationTree(t *testing.T, metas, specs, doc string) string {
	t.Helper()
	withAnnotationGaps(t, nil)

	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(builtinMetasGo, "package sema\n\nvar builtinMetas = map[string][]MetaTarget{\n"+metas+"}\n")
	write(metaParamSpecGo, "package sema\n\nvar noParams = metaParamSpec{}\n\n"+
		"var metaParamSpecs = map[string]metaParamSpec{\n"+specs+"}\n")
	if doc != "" {
		write(annotationsDoc, doc)
	}
	return root
}

// annotationDocument renders a document carrying both required tables. When
// entries is nil, every indexed name gets an entry repeating its own row, which
// is what a coherent document looks like; a test that varies an entry passes
// the whole entry text, heading and schema line together.
func annotationDocument(indexRows, rejectedRows, entries []string) string {
	var b strings.Builder
	b.WriteString("# Annotations\n\n## 6. Index\n\n")
	b.WriteString("| " + strings.Join(annotationIndexHeader, " | ") + " |\n|---|---|---|---|\n")
	for _, r := range indexRows {
		b.WriteString(r + "\n")
	}
	b.WriteString("\n## 7. Entries\n\n")
	if entries == nil {
		for _, r := range indexRows {
			entries = append(entries, entryFor(r))
		}
	}
	for _, e := range entries {
		b.WriteString(e + "\n\n- **Effect** something.\n\n")
	}
	b.WriteString("## 16. Not annotations\n\n")
	b.WriteString("| " + strings.Join(annotationRejectedHeader, " | ") + " |\n|---|---|\n")
	for _, r := range rejectedRows {
		b.WriteString(r + "\n")
	}
	return b.String()
}

// entryFor renders the entry an index row implies: a heading naming it, and a
// schema line repeating that row's own Targets and Parameters.
func entryFor(indexRow string) string {
	cells, ok := splitTableRow(indexRow)
	if !ok {
		return ""
	}
	return "### " + cells[0] + "\n\n" + schemaLine(cells[1], cells[2])
}

func schemaLine(targets, params string) string {
	return "- **Targets** " + targets + " · **Parameters** " + params
}

// coherentDocument is annotationDocument over the baseline rows.
func coherentDocument() string {
	return annotationDocument([]string{copyRow, embedRow}, []string{rejectedRow}, nil)
}

// withAnnotationGaps installs a gap ledger for the duration of one test.
func withAnnotationGaps(t *testing.T, gaps map[string][]annotationGap) {
	t.Helper()
	saved := annotationGaps
	annotationGaps = gaps
	t.Cleanup(func() { annotationGaps = saved })
}

func expectFinding(t *testing.T, err error, want ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a finding mentioning %q, got nil", want)
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("finding should mention %q, got:\n%v", w, err)
		}
	}
}

// --- the two directions of coverage ---

func TestAnnotationCoverageCoherentTreePasses(t *testing.T) {
	root := annotationTree(t, copyMetas+embedMetas, copySpecs+embedSpecs, coherentDocument())
	if err := checkAnnotationCoverage(root); err != nil {
		t.Fatalf("a coherent tree must produce no findings, got:\n%v", err)
	}
}

func TestAnnotationCoverageRegisteredAnnotationNeedsARow(t *testing.T) {
	// The failure mode this check exists for: language-design.md §8.3 was
	// missing thirteen rows because nothing asserted the compiler's table
	// against it.
	root := annotationTree(t,
		copyMetas+embedMetas+"\t\"mono\": {TargetMethod},\n",
		copySpecs+embedSpecs+"\t\"mono\": noParams,\n",
		coherentDocument())
	expectFinding(t, checkAnnotationCoverage(root), "`mono", "no row in docs/annotations.md §6")
}

func TestAnnotationCoverageRowNeedsARegisteredAnnotation(t *testing.T) {
	doc := annotationDocument(
		[]string{copyRow, embedRow, "| `` `sealed `` | types | — | Closed hierarchy |"},
		[]string{rejectedRow}, nil)
	root := annotationTree(t, copyMetas+embedMetas, copySpecs+embedSpecs, doc)
	expectFinding(t, checkAnnotationCoverage(root), "`sealed", "the compiler registers no such annotation")
}

func TestAnnotationCoverageTargetsMustMatch(t *testing.T) {
	// The document says fields; the compiler also accepts methods. This is
	// exactly the `value divergence T1923 owns in the real ledger.
	doc := annotationDocument([]string{copyRow, embedRow,
		"| `` `value `` | fields | — | Field lives in the value struct |"}, []string{rejectedRow}, nil)
	root := annotationTree(t,
		copyMetas+embedMetas+"\t\"value\": {TargetField, TargetMethod},\n",
		copySpecs+embedSpecs+"\t\"value\": noParams,\n", doc)
	expectFinding(t, checkAnnotationCoverage(root),
		"`value", "targets in docs/annotations.md §6 disagree",
		"document says \"fields\"", "builtinMetas says \"fields, methods\"")
}

func TestAnnotationCoverageParametersMustMatch(t *testing.T) {
	// Each row perturbs one dimension of `embed's contract. Order and the
	// optional flag are part of a positional slot's identity; a named
	// parameter is identified by name and kind alone.
	cases := []struct {
		name string
		row  string
		want string
	}{
		{
			"wrong kind",
			"| `` `embed `` | functions | `path` (int, positional); `compress` (bool, named) | Embed |",
			"(int, positional)",
		},
		{
			"positional written as named",
			"| `` `embed `` | functions | `path` (string, named); `compress` (bool, named) | Embed |",
			"`path` (string, named)",
		},
		{
			"missing named parameter",
			"| `` `embed `` | functions | `path` (string, positional) | Embed |",
			"`compress` (bool, named)",
		},
		{
			"named parameter the compiler does not accept",
			"| `` `embed `` | functions | `path` (string, positional); `compress` (bool, named); `strip` (bool, named) | Embed |",
			"`strip` (bool, named)",
		},
		{
			"spurious optional flag",
			"| `` `embed `` | functions | `path` (string, positional, optional); `compress` (bool, named) | Embed |",
			"`path` (string, positional, optional)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := annotationDocument([]string{copyRow, tc.row}, []string{rejectedRow}, nil)
			root := annotationTree(t, copyMetas+embedMetas, copySpecs+embedSpecs, doc)
			expectFinding(t, checkAnnotationCoverage(root),
				"`embed", "parameters in docs/annotations.md §6 disagree", tc.want)
		})
	}
}

func TestAnnotationCoveragePositionalOrderIsSignificant(t *testing.T) {
	// A call site depends on slot order, so two positional slots swapped is
	// a real disagreement even though the set is the same.
	specs := "\t\"wasm_import\": {positional: []metaPositional{" +
		"{name: \"module\", kind: valString}, {name: \"name\", kind: valString}}},\n"
	doc := annotationDocument([]string{copyRow,
		"| `` `wasm_import `` | functions | `name` (string, positional); `module` (string, positional) | Host import |"},
		[]string{rejectedRow}, nil)
	root := annotationTree(t, copyMetas+"\t\"wasm_import\": {TargetFunc},\n", copySpecs+specs, doc)
	expectFinding(t, checkAnnotationCoverage(root), "`wasm_import", "parameters in docs/annotations.md §6 disagree")
}

// --- the ledger ---

func TestAnnotationCoverageLedgerExcusesAKnownDivergence(t *testing.T) {
	root := annotationTree(t,
		copyMetas+embedMetas+"\t\"inline\": {TargetFunc, TargetMethod},\n",
		copySpecs+embedSpecs+"\t\"inline\": noParams,\n",
		coherentDocument())
	withAnnotationGaps(t, map[string][]annotationGap{
		"inline": {{gapUndocumented, "T1922", "read by nothing; implement or delete"}},
	})
	if err := checkAnnotationCoverage(root); err != nil {
		t.Fatalf("a ledgered divergence must not be reported, got:\n%v", err)
	}
}

func TestAnnotationCoverageLedgerExcusesOnlyTheGapItNames(t *testing.T) {
	// A ledger row is per gap kind, not per annotation: excusing `embed's
	// parameters must not also excuse its targets.
	doc := annotationDocument([]string{copyRow,
		"| `` `embed `` | types | `path` (int, positional); `compress` (bool, named) | Embed |"},
		[]string{rejectedRow}, nil)
	root := annotationTree(t, copyMetas+embedMetas, copySpecs+embedSpecs, doc)
	withAnnotationGaps(t, map[string][]annotationGap{
		"embed": {{gapParameters, "T9999", "known"}},
	})
	err := checkAnnotationCoverage(root)
	expectFinding(t, err, "targets in docs/annotations.md §6 disagree")
	if strings.Contains(err.Error(), "parameters in docs/annotations.md §6 disagree") {
		t.Errorf("the ledgered parameter gap must stay excused, got:\n%v", err)
	}
}

func TestAnnotationCoverageStaleLedgerEntryIsAFinding(t *testing.T) {
	// The self-cleaning property: once the divergence is fixed, the check
	// fails until the ledger row is deleted. A row outliving its work
	// describes a compiler that no longer exists.
	root := annotationTree(t, copyMetas+embedMetas, copySpecs+embedSpecs, coherentDocument())
	withAnnotationGaps(t, map[string][]annotationGap{
		"copy": {{gapTargets, "T1234", "targets used to disagree"}},
	})
	expectFinding(t, checkAnnotationCoverage(root),
		"annotationGaps entry `copy", "T1234", "no longer applies", "delete the row")
}

// --- structural assertions about the document ---

func TestAnnotationCoverageMissingIndexTableIsAFinding(t *testing.T) {
	// Located by column signature, not section number — a renumbered
	// document still reconciles, a deleted table never silently passes.
	doc := "# Annotations\n\n## 16. Not annotations\n\n| " +
		strings.Join(annotationRejectedHeader, " | ") + " |\n|---|---|\n" + rejectedRow + "\n"
	root := annotationTree(t, copyMetas, copySpecs, doc)
	expectFinding(t, checkAnnotationCoverage(root), "has no index table")
}

func TestAnnotationCoverageMissingRejectedTableIsAFinding(t *testing.T) {
	doc := annotationDocument([]string{copyRow}, nil, nil)
	doc = strings.Replace(doc, "| "+strings.Join(annotationRejectedHeader, " | ")+" |\n|---|---|\n", "", 1)
	root := annotationTree(t, copyMetas, copySpecs, doc)
	expectFinding(t, checkAnnotationCoverage(root), "has no \"Not annotations\" table")
}

func TestAnnotationCoverageNameInBothTablesIsAFinding(t *testing.T) {
	// A name cannot be both part of the language and rejected by it.
	doc := annotationDocument([]string{copyRow},
		[]string{"| `` `copy `` | Considered and rejected. |"}, nil)
	root := annotationTree(t, copyMetas, copySpecs, doc)
	expectFinding(t, checkAnnotationCoverage(root), "`copy is listed both in", "Not annotations")
}

func TestAnnotationCoverageRowWithoutAnEntryIsAFinding(t *testing.T) {
	// A row documents a name; the entry documents the annotation.
	doc := annotationDocument([]string{copyRow, embedRow}, []string{rejectedRow},
		[]string{entryFor(copyRow)})
	root := annotationTree(t, copyMetas+embedMetas, copySpecs+embedSpecs, doc)
	expectFinding(t, checkAnnotationCoverage(root), "`embed has a §6 row but no entry")
}

func TestAnnotationCoverageEntryWithoutARowIsAFinding(t *testing.T) {
	// The mirror: an entry for a name §6 does not list documents something
	// that is not an annotation.
	doc := annotationDocument([]string{copyRow}, []string{rejectedRow},
		[]string{entryFor(copyRow), entryFor(embedRow)})
	root := annotationTree(t, copyMetas, copySpecs, doc)
	expectFinding(t, checkAnnotationCoverage(root), "`embed has an entry in docs/annotations.md but no §6 row")
}

func TestAnnotationCoveragePairHeadingDocumentsBothNames(t *testing.T) {
	// One heading may cover a pair, in which case its schema line speaks for
	// both — so the two rows it repeats have to agree.
	metas := copyMetas + "\t\"sendable\": {TargetType},\n\t\"sharable\": {TargetType},\n"
	specs := copySpecs + "\t\"sendable\": noParams,\n\t\"sharable\": noParams,\n"
	doc := annotationDocument([]string{copyRow,
		"| `` `sendable `` | types | — | Assert transfer |",
		"| `` `sharable `` | types | — | Assert aliasing |"},
		[]string{rejectedRow},
		[]string{entryFor(copyRow),
			"### `` `sendable `` / `` `sharable ``\n\n" + schemaLine("types", "— none")})
	root := annotationTree(t, metas, specs, doc)
	if err := checkAnnotationCoverage(root); err != nil {
		t.Fatalf("one heading may document a pair, got:\n%v", err)
	}
}

func TestAnnotationCoveragePairHeadingCannotStraddleDifferingRows(t *testing.T) {
	// The other half of the pair rule: one schema line cannot be true of two
	// rows that disagree, so the mismatch surfaces against the row it is not.
	metas := copyMetas + "\t\"sendable\": {TargetType},\n\t\"sharable\": {TargetType, TargetEnum},\n"
	specs := copySpecs + "\t\"sendable\": noParams,\n\t\"sharable\": noParams,\n"
	doc := annotationDocument([]string{copyRow,
		"| `` `sendable `` | types | — | Assert transfer |",
		"| `` `sharable `` | types, enums | — | Assert aliasing |"},
		[]string{rejectedRow},
		[]string{entryFor(copyRow),
			"### `` `sendable `` / `` `sharable ``\n\n" + schemaLine("types", "— none")})
	root := annotationTree(t, metas, specs, doc)
	expectFinding(t, checkAnnotationCoverage(root),
		"`sharable: the entry says targets \"types\", its §6 row says \"enums, types\"")
}

func TestAnnotationCoverageGroupedEntryTableCountsAsAnEntry(t *testing.T) {
	// The serialization field annotations share one grouped entry rendered
	// as a table rather than a heading each. It carries the index signature,
	// so its rows are read — and reconciled — the same way.
	metas := copyMetas + "\t\"skip\": {TargetField},\n"
	specs := copySpecs + "\t\"skip\": noParams,\n"
	doc := annotationDocument([]string{copyRow, "| `` `skip `` | fields | — | Omit from serialization |"},
		[]string{rejectedRow}, []string{entryFor(copyRow)})
	doc += "\n### Field annotations\n\n| " + strings.Join(annotationIndexHeader, " | ") +
		" |\n|---|---|---|---|\n| `` `skip `` | fields | — | Omit the field. |\n"
	root := annotationTree(t, metas, specs, doc)
	if err := checkAnnotationCoverage(root); err != nil {
		t.Fatalf("a grouped entry row is an entry, got:\n%v", err)
	}
}

func TestAnnotationCoverageGroupedEntryRowIsReconciledToo(t *testing.T) {
	// A grouped entry is still an entry: its row must repeat §6's, not
	// paraphrase it.
	metas := copyMetas + "\t\"skip\": {TargetField},\n"
	specs := copySpecs + "\t\"skip\": noParams,\n"
	doc := annotationDocument([]string{copyRow, "| `` `skip `` | fields | — | Omit from serialization |"},
		[]string{rejectedRow}, []string{entryFor(copyRow)})
	doc += "\n### Field annotations\n\n| " + strings.Join(annotationIndexHeader, " | ") +
		" |\n|---|---|---|---|\n| `` `skip `` | fields, variants | — | Omit the field. |\n"
	root := annotationTree(t, metas, specs, doc)
	expectFinding(t, checkAnnotationCoverage(root),
		"`skip: the entry says targets \"fields, variants\", its §6 row says \"fields\"")
}

// --- the entry repeats its row ---

func TestAnnotationCoverageEntryMustRepeatItsRow(t *testing.T) {
	cases := []struct {
		name  string
		entry string
		want  string
	}{
		{
			"targets paraphrased",
			"### `` `embed ``\n\n" + schemaLine("module-level getters",
				"`path` (string, positional); `compress` (bool, named)"),
			"unknown target \"module-level getters\"",
		},
		{
			"targets narrowed",
			"### `` `embed ``\n\n" + schemaLine("functions, methods",
				"`path` (string, positional); `compress` (bool, named)"),
			"`embed: the entry says targets \"functions, methods\", its §6 row says \"functions\"",
		},
		{
			"parameters paraphrased",
			"### `` `embed ``\n\n" + schemaLine("functions",
				"`path` (string, positional); `compress` (bool, named); `strip` (bool, named)"),
			"`embed: the entry says parameters",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := annotationDocument([]string{copyRow, embedRow}, []string{rejectedRow},
				[]string{entryFor(copyRow), tc.entry})
			root := annotationTree(t, copyMetas+embedMetas, copySpecs+embedSpecs, doc)
			expectFinding(t, checkAnnotationCoverage(root), tc.want)
		})
	}
}

func TestAnnotationCoverageEntryWithoutASchemaLineIsAnError(t *testing.T) {
	doc := annotationDocument([]string{copyRow, embedRow}, []string{rejectedRow},
		[]string{entryFor(copyRow), "### `` `embed ``"})
	root := annotationTree(t, copyMetas+embedMetas, copySpecs+embedSpecs, doc)
	expectFinding(t, checkAnnotationCoverage(root), "has no", "**Targets**")
}

func TestAnnotationCoverageWrappedSchemaLineIsJoined(t *testing.T) {
	// A long contract wraps in the source; Markdown joins it and so must the
	// reconciliation, or every wide entry would read as malformed.
	wrapped := "### `` `embed ``\n\n- **Targets** functions · **Parameters** `path` (string, positional);\n" +
		"  `compress` (bool, named)"
	doc := annotationDocument([]string{copyRow, embedRow}, []string{rejectedRow},
		[]string{entryFor(copyRow), wrapped})
	root := annotationTree(t, copyMetas+embedMetas, copySpecs+embedSpecs, doc)
	if err := checkAnnotationCoverage(root); err != nil {
		t.Fatalf("a wrapped schema line must reconcile, got:\n%v", err)
	}
}

func TestAnnotationCoverageTwoEntriesForOneNameIsAnError(t *testing.T) {
	doc := annotationDocument([]string{copyRow, embedRow}, []string{rejectedRow},
		[]string{entryFor(copyRow), entryFor(embedRow), entryFor(embedRow)})
	root := annotationTree(t, copyMetas+embedMetas, copySpecs+embedSpecs, doc)
	expectFinding(t, checkAnnotationCoverage(root), "`embed has two entries")
}

func TestAnnotationCoverageDuplicateIndexRowIsAnError(t *testing.T) {
	doc := annotationDocument([]string{copyRow, copyRow}, []string{rejectedRow}, nil)
	root := annotationTree(t, copyMetas, copySpecs, doc)
	expectFinding(t, checkAnnotationCoverage(root), "`copy has two index rows")
}

// --- the document's own vocabulary ---

func TestAnnotationCoverageUnknownTargetWordIsAnError(t *testing.T) {
	// The Targets column is a closed vocabulary mapping onto MetaTarget; a
	// word outside it cannot be compared to anything.
	doc := annotationDocument([]string{"| `` `copy `` | widgets | — | Copy |"}, []string{rejectedRow}, nil)
	root := annotationTree(t, copyMetas, copySpecs, doc)
	expectFinding(t, checkAnnotationCoverage(root), "unknown target \"widgets\"", "the vocabulary is")
}

func TestAnnotationCoverageMalformedParameterCellIsAnError(t *testing.T) {
	doc := annotationDocument([]string{"| `` `copy `` | types | `x` (string, required) | Copy |"},
		[]string{rejectedRow}, nil)
	root := annotationTree(t, copyMetas, copySpecs, doc)
	expectFinding(t, checkAnnotationCoverage(root), "cannot read parameter", "positional|named")
}

func TestAnnotationCoverageNamedParameterCannotBeOptional(t *testing.T) {
	// Every named parameter may be omitted, so the flag would correspond to
	// no field in metaParamSpecs and could never be checked.
	doc := annotationDocument([]string{"| `` `copy `` | types | `x` (string, named, optional) | Copy |"},
		[]string{rejectedRow}, nil)
	root := annotationTree(t, copyMetas, copySpecs, doc)
	expectFinding(t, checkAnnotationCoverage(root), "cannot be marked optional")
}

// --- the compiler's own tables ---

func TestAnnotationCoverageUnknownMetaTargetIsAHardError(t *testing.T) {
	// A new declaration kind changes what §6 can express, so it must force
	// this check to be updated rather than silently skip the annotation.
	root := annotationTree(t, copyMetas+"\t\"getter\": {TargetGetter},\n",
		copySpecs+"\t\"getter\": noParams,\n", coherentDocument())
	expectFinding(t, checkAnnotationCoverage(root), "unknown MetaTarget TargetGetter", "metaTargetWords")
}

func TestAnnotationCoverageUnknownValueKindIsAHardError(t *testing.T) {
	root := annotationTree(t, copyMetas+"\t\"pin\": {TargetFunc},\n",
		copySpecs+"\t\"pin\": {positional: []metaPositional{{name: \"at\", kind: valDuration}}},\n",
		coherentDocument())
	expectFinding(t, checkAnnotationCoverage(root), "unknown metaValueKind valDuration", "metaValueWords")
}

func TestAnnotationCoverageUnknownSpecFieldIsAHardError(t *testing.T) {
	root := annotationTree(t, copyMetas+"\t\"pin\": {TargetFunc},\n",
		copySpecs+"\t\"pin\": {variadic: true},\n", coherentDocument())
	expectFinding(t, checkAnnotationCoverage(root), "unknown field \"variadic\"")
}

func TestAnnotationCoverageSemaTablesMustDescribeOneSet(t *testing.T) {
	// Mirrors TestT1449SpecsExhaustive at the document layer: a name in one
	// table only has half a contract, so there is nothing to compare.
	root := annotationTree(t,
		copyMetas+embedMetas+"\t\"raw\": {TargetField},\n",
		copySpecs+embedSpecs+"\t\"final\": noParams,\n",
		coherentDocument())
	err := checkAnnotationCoverage(root)
	expectFinding(t, err,
		"`raw is in builtinMetas but declares no metaParamSpecs contract",
		"`final declares a metaParamSpecs contract but is not in builtinMetas")
}

func TestAnnotationCoverageMissingSemaVarIsAnError(t *testing.T) {
	root := annotationTree(t, copyMetas, copySpecs, coherentDocument())
	p := filepath.Join(root, filepath.FromSlash(metaParamSpecGo))
	if err := os.WriteFile(p, []byte("package sema\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	expectFinding(t, checkAnnotationCoverage(root), "declares no package-level metaParamSpecs")
}

func TestAnnotationCoverageUnparsableSemaFileIsAnError(t *testing.T) {
	root := annotationTree(t, copyMetas, copySpecs, coherentDocument())
	p := filepath.Join(root, filepath.FromSlash(builtinMetasGo))
	if err := os.WriteFile(p, []byte("package sema\nvar builtinMetas = map[string][]MetaTarget{\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	expectFinding(t, checkAnnotationCoverage(root), "parse", "meta.go")
}

// --- scope ---

func TestAnnotationCoverageNoCompilerScopesItselfOut(t *testing.T) {
	// RunPreCommit is exercised against bare temp repos; a tree with no
	// compiler source is not a Promise checkout.
	withAnnotationGaps(t, nil)
	root := t.TempDir()
	if err := checkAnnotationCoverage(root); err != nil {
		t.Fatalf("a tree with no compiler source must be a no-op, got: %v", err)
	}
}

func TestAnnotationCoverageMissingDocumentIsAFinding(t *testing.T) {
	// The counterpart: a tree that has the compiler but not the document is
	// the document having been deleted, not the check being out of scope.
	root := annotationTree(t, copyMetas, copySpecs, "")
	expectFinding(t, checkAnnotationCoverage(root), "read docs/annotations.md")
}

// --- the real repository ---

func TestAnnotationGapsLedgerNamesATrackerItem(t *testing.T) {
	// Nothing here can reach the tracker, so the item's *status* is review's
	// job. What is checkable is that every row names one at all: a
	// divergence excused by an unattributed row is a divergence nobody owns.
	for name, gaps := range annotationGaps {
		if len(gaps) == 0 {
			t.Errorf("annotationGaps[%q] excuses nothing; delete the key", name)
		}
		for _, gap := range gaps {
			if !strings.HasPrefix(gap.item, "T") && !strings.HasPrefix(gap.item, "B") {
				t.Errorf("annotationGaps[%q] names %q, which is not a tracker item", name, gap.item)
			}
			if gap.why == "" {
				t.Errorf("annotationGaps[%q] (%s) says nothing about the divergence", name, gap.item)
			}
		}
	}
}

// --- the real repository ---

// TestAnnotationCoverageReconcilesThisCheckout is the only test that runs the
// reconciliation over the real document, the real sema tables, and the real
// annotationGaps ledger. Everything above it proves the check reports what it
// should on a fixture; this proves the repository it ships in is clean — and,
// because a stale ledger row is itself a finding, that every excused
// divergence still exists.
//
// Pre-commit runs the same check, but only at commit time. Here it fails in
// `go test ./common/`, which is where someone who has just added an annotation
// to builtinMetas is looking.
func TestAnnotationCoverageReconcilesThisCheckout(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(annotationsDoc))); err != nil {
		t.Fatalf("%s must be reachable from the package directory, or this test is a silent no-op: %v",
			annotationsDoc, err)
	}
	if err := checkAnnotationCoverage(root); err != nil {
		t.Fatalf("this checkout does not reconcile:\n%v", err)
	}
}

// --- malformed compiler tables ---

// overwriteSema replaces one of the two fixture sema files wholesale, for the
// cases that malform a table's shape rather than one of its entries.
func overwriteSema(t *testing.T, root, rel, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAnnotationCoverageMalformedBuiltinMetasIsAnError(t *testing.T) {
	// The check reads the tables with Go's own parser, so anything it cannot
	// reduce to name -> targets must be reported rather than skipped: an
	// entry silently dropped here is an annotation §6 is never asked about.
	cases := []struct {
		name string
		body string
		want string
	}{
		{"value is not a target list", "\t\"copy\": TargetType,\n", "builtinMetas[\"copy\"] is not a target list"},
		{"target is not an identifier", "\t\"copy\": {\"types\"},\n", "builtinMetas[\"copy\"] has a non-identifier target"},
		{"unkeyed element", "\t{TargetType},\n", "builtinMetas has an unkeyed element"},
		{"key is not a string", "\tcopy: {TargetType},\n", "expected a string literal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := annotationTree(t, tc.body, copySpecs, coherentDocument())
			expectFinding(t, checkAnnotationCoverage(root), tc.want)
		})
	}
}

func TestAnnotationCoverageMalformedMetaParamSpecsIsAnError(t *testing.T) {
	// The same for the parameter contract, one malformation per shape the
	// reader knows how to descend into. `optional` and the two "unknown
	// field" arms matter most: a field this check cannot read is a
	// parameter dimension §6 would have no way to record.
	cases := []struct {
		name string
		body string
		want string
	}{
		{"unkeyed element", "\t{positional: []metaPositional{}},\n", "metaParamSpecs has an unkeyed element"},
		{"key is not a string", "\tcopy: noParams,\n", "expected a string literal"},
		{"value is an unrecognized identifier", "\t\"copy\": someSpec,\n",
			"is the identifier someSpec, which this check does not know how to read"},
		{"value is neither identifier nor literal", "\t\"copy\": makeSpec(),\n", "is not a spec literal"},
		{"unkeyed field", "\t\"copy\": {[]metaPositional{}},\n", "metaParamSpecs[\"copy\"] has an unkeyed field"},
		{"field key is not an identifier", "\t\"copy\": {\"positional\": []metaPositional{}},\n",
			"metaParamSpecs[\"copy\"] has a non-identifier field"},
		{"unknown spec field", "\t\"copy\": {variadic: true},\n", "has unknown field \"variadic\""},
		{"positional is not a list", "\t\"copy\": {positional: noParams},\n", "positional is not a list"},
		{"positional slot is not a literal", "\t\"copy\": {positional: []metaPositional{pathSlot}},\n",
			"positional has a non-literal slot"},
		{"slot field is unkeyed", "\t\"copy\": {positional: []metaPositional{{\"path\", valString}}},\n",
			"positional has an unkeyed field"},
		{"slot field key is not an identifier",
			"\t\"copy\": {positional: []metaPositional{{\"name\": \"path\"}}},\n",
			"positional has a non-identifier field"},
		{"slot name is not a string",
			"\t\"copy\": {positional: []metaPositional{{name: 42, kind: valString}}},\n",
			"expected a string literal"},
		{"slot optional is not a boolean",
			"\t\"copy\": {positional: []metaPositional{{name: \"p\", kind: valString, optional: 1}}},\n",
			"has a non-boolean `optional`"},
		{"unknown slot field",
			"\t\"copy\": {positional: []metaPositional{{name: \"p\", kind: valString, repeated: true}}},\n",
			"metaPositional has unknown field \"repeated\""},
		{"named is not a map literal", "\t\"copy\": {named: noParams},\n", "named is not a map literal"},
		{"named entry is unkeyed", "\t\"copy\": {named: map[string]metaValueKind{valBool}},\n",
			"named has an unkeyed entry"},
		{"named key is not a string", "\t\"copy\": {named: map[string]metaValueKind{compress: valBool}},\n",
			"expected a string literal"},
		{"named value kind is not an identifier",
			"\t\"copy\": {named: map[string]metaValueKind{\"compress\": 1}},\n",
			"has a non-identifier value kind"},
		{"named value kind is unknown",
			"\t\"copy\": {named: map[string]metaValueKind{\"compress\": valDuration}},\n",
			"unknown metaValueKind valDuration"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := annotationTree(t, copyMetas, tc.body, coherentDocument())
			expectFinding(t, checkAnnotationCoverage(root), tc.want)
		})
	}
}

func TestAnnotationCoverageSemaVarThatIsNotAMapIsAnError(t *testing.T) {
	// A table rebuilt at run time cannot be reconciled against a document,
	// so the check says so rather than reporting every annotation missing.
	root := annotationTree(t, copyMetas, copySpecs, coherentDocument())
	overwriteSema(t, root, builtinMetasGo, "package sema\n\nvar builtinMetas = makeMetas()\n")
	expectFinding(t, checkAnnotationCoverage(root), "builtinMetas is not a map literal")
}

func TestAnnotationCoverageSemaFileMayCarryOtherDeclarations(t *testing.T) {
	// The table is found among whatever else the file declares — imports,
	// types, constants, functions — so a reorganized meta.go still
	// reconciles rather than reading as an absent table.
	root := annotationTree(t, copyMetas, copySpecs,
		annotationDocument([]string{copyRow}, []string{rejectedRow}, nil))
	overwriteSema(t, root, builtinMetasGo, "package sema\n\nimport \"fmt\"\n\n"+
		"type MetaTarget int\n\nconst TargetType MetaTarget = 0\n\n"+
		"func unrelated() { fmt.Println(\"x\") }\n\n"+
		"var builtinMetas = map[string][]MetaTarget{\n"+copyMetas+"}\n")
	if err := checkAnnotationCoverage(root); err != nil {
		t.Fatalf("the table must be found among other declarations, got:\n%v", err)
	}
}

// --- malformed document tables ---

// groupedEntryTable renders a "### <heading>" section whose body is a table
// with the index signature — the shape §15's field annotations use, where
// several annotations share one entry.
func groupedEntryTable(heading string, rows ...string) string {
	return "\n### " + heading + "\n\n| " + strings.Join(annotationIndexHeader, " | ") +
		" |\n|---|---|---|---|\n" + strings.Join(rows, "\n") + "\n"
}

func TestAnnotationCoverageIndexRowMustNameAnAnnotation(t *testing.T) {
	// A row whose first cell carries no backticked name reconciles nothing,
	// so it is an error rather than an ignored row.
	doc := annotationDocument([]string{"|  | types | — | Copy |"}, []string{rejectedRow},
		[]string{entryFor(copyRow)})
	root := annotationTree(t, copyMetas, copySpecs, doc)
	expectFinding(t, checkAnnotationCoverage(root), "index row", "names no annotation")
}

func TestAnnotationCoverageGroupedEntryRowMustNameAnAnnotation(t *testing.T) {
	doc := annotationDocument([]string{copyRow}, []string{rejectedRow}, []string{entryFor(copyRow)}) +
		groupedEntryTable("Field annotations", "|  | fields | — | Omit the field. |")
	root := annotationTree(t, copyMetas, copySpecs, doc)
	expectFinding(t, checkAnnotationCoverage(root), "grouped entry row", "names no annotation")
}

func TestAnnotationCoverageGroupedEntryRowMustBeReadable(t *testing.T) {
	// A grouped row is held to the same vocabulary as an index row: it is
	// the entry, so an unreadable one is an entry nobody can reconcile.
	metas := copyMetas + "\t\"skip\": {TargetField},\n"
	specs := copySpecs + "\t\"skip\": noParams,\n"
	doc := annotationDocument([]string{copyRow, "| `` `skip `` | fields | — | Omit |"},
		[]string{rejectedRow}, []string{entryFor(copyRow)}) +
		groupedEntryTable("Field annotations", "| `` `skip `` | widgets | — | Omit the field. |")
	root := annotationTree(t, metas, specs, doc)
	expectFinding(t, checkAnnotationCoverage(root), "`skip", "unknown target \"widgets\"")
}

func TestAnnotationCoverageTwoGroupedEntriesForOneNameIsAnError(t *testing.T) {
	// Two grouped tables listing the same name are two entries, which is
	// two places to change one annotation — the defect the whole check
	// exists to prevent, in the one shape a heading cannot express.
	metas := copyMetas + "\t\"skip\": {TargetField},\n"
	specs := copySpecs + "\t\"skip\": noParams,\n"
	row := "| `` `skip `` | fields | — | Omit the field. |"
	doc := annotationDocument([]string{copyRow, "| `` `skip `` | fields | — | Omit |"},
		[]string{rejectedRow}, []string{entryFor(copyRow)}) +
		groupedEntryTable("Field annotations", row) +
		groupedEntryTable("More field annotations", row)
	root := annotationTree(t, metas, specs, doc)
	expectFinding(t, checkAnnotationCoverage(root), "`skip has two entries")
}

func TestAnnotationCoverageEmptyTargetWordIsAnError(t *testing.T) {
	// A trailing comma in the Targets cell is a target the document does not
	// name, not an empty set.
	doc := annotationDocument([]string{"| `` `copy `` | types, | — | Copy |"}, []string{rejectedRow}, nil)
	root := annotationTree(t, copyMetas, copySpecs, doc)
	expectFinding(t, checkAnnotationCoverage(root), "empty target in \"types,\"")
}

func TestAnnotationCoverageUnknownParameterKindIsAnError(t *testing.T) {
	// The Parameters cell's kind column is as closed a vocabulary as the
	// Targets column: it maps onto metaValueKind and nothing else.
	doc := annotationDocument([]string{"| `` `copy `` | types | `x` (duration, positional) | Copy |"},
		[]string{rejectedRow}, nil)
	root := annotationTree(t, copyMetas, copySpecs, doc)
	expectFinding(t, checkAnnotationCoverage(root), "unknown parameter kind \"duration\"", "the vocabulary is")
}

func TestAnnotationCoverageRowMayCarryTheParameterListInItsName(t *testing.T) {
	// §16 spells one rejected name `` `align(N) ``, so a cell's name is what
	// precedes its parameter list. A §6 row written the same way still names
	// the annotation the compiler registers.
	doc := annotationDocument([]string{copyRow, "| `` `align(N) `` | types | `n` (int, positional) | Alignment |"},
		[]string{rejectedRow}, nil)
	root := annotationTree(t,
		copyMetas+"\t\"align\": {TargetType},\n",
		copySpecs+"\t\"align\": {positional: []metaPositional{{name: \"n\", kind: valInt}}},\n", doc)
	if err := checkAnnotationCoverage(root); err != nil {
		t.Fatalf("a row may spell its parameter list, got:\n%v", err)
	}
}

func TestAnnotationCoverageTableRuleIsOptional(t *testing.T) {
	// Tables are located by their header signature, and the `|---|` rule is
	// skipped only when it is actually a rule — a first body row is never
	// mistaken for one and silently dropped.
	doc := strings.Replace(coherentDocument(), "|---|---|---|---|\n", "", 1)
	root := annotationTree(t, copyMetas+embedMetas, copySpecs+embedSpecs, doc)
	if err := checkAnnotationCoverage(root); err != nil {
		t.Fatalf("the first body row must not be eaten as a rule, got:\n%v", err)
	}
}

func TestAnnotationCoverageUnreadableSchemaLineIsAnError(t *testing.T) {
	// Half a schema line is not a schema line: an entry that declares
	// targets but no parameters has nothing to reconcile against its row.
	doc := annotationDocument([]string{copyRow, embedRow}, []string{rejectedRow},
		[]string{entryFor(copyRow), "### `` `embed ``\n\n- **Targets** functions"})
	root := annotationTree(t, copyMetas+embedMetas, copySpecs+embedSpecs, doc)
	expectFinding(t, checkAnnotationCoverage(root), "has no", "**Parameters**")
}

func TestAnnotationCoverageEntryAtEndOfDocumentStillNeedsASchemaLine(t *testing.T) {
	// The scan for a schema line normally stops at the next heading; the
	// last entry in the file has none, and must not pass by running out of
	// document.
	doc := coherentDocument() + "\n### `` `mono ``\n"
	root := annotationTree(t,
		copyMetas+embedMetas+"\t\"mono\": {TargetMethod},\n",
		copySpecs+embedSpecs+"\t\"mono\": noParams,\n", doc)
	expectFinding(t, checkAnnotationCoverage(root), "the entry \"`` `mono ``\" has no", "**Targets**")
}

// --- the shape of a finding ---

func TestAnnotationCoverageFindingNamesAnEmptyContract(t *testing.T) {
	// Both halves of a disagreement are rendered in the document's own
	// spelling so a finding can be pasted into the row it is about — which
	// means "nothing at all" needs a spelling too, on either side.
	t.Run("no targets", func(t *testing.T) {
		doc := annotationDocument([]string{"| `` `copy `` | types | — | Copy |"}, []string{rejectedRow}, nil)
		root := annotationTree(t, "\t\"copy\": {},\n", copySpecs, doc)
		expectFinding(t, checkAnnotationCoverage(root),
			"document says \"types\", builtinMetas says \"(none)\"")
	})
	t.Run("no parameters", func(t *testing.T) {
		doc := annotationDocument([]string{"| `` `copy `` | types, enums | `x` (string, positional) | Copy |"},
			[]string{rejectedRow}, nil)
		root := annotationTree(t, copyMetas, copySpecs, doc)
		expectFinding(t, checkAnnotationCoverage(root),
			"document says \"`x` (string, positional)\", metaParamSpecs says \"—\"")
	})
}

func TestAnnotationCoverageOptionalPositionalIsPartOfTheContract(t *testing.T) {
	// `deprecated's `message` is optional. The flag is half of what a call
	// site may write, so it is checked in both directions — the existing
	// "spurious optional flag" case covers a document that claims one the
	// compiler does not grant; this covers dropping one it does.
	metas := copyMetas + "\t\"deprecated\": {TargetType},\n"
	specs := copySpecs +
		"\t\"deprecated\": {positional: []metaPositional{{name: \"message\", kind: valString, optional: true}}},\n"

	t.Run("declared", func(t *testing.T) {
		doc := annotationDocument([]string{copyRow,
			"| `` `deprecated `` | types | `message` (string, positional, optional) | Mark deprecated |"},
			[]string{rejectedRow}, nil)
		if err := checkAnnotationCoverage(annotationTree(t, metas, specs, doc)); err != nil {
			t.Fatalf("an optional slot must reconcile, got:\n%v", err)
		}
	})
	t.Run("omitted", func(t *testing.T) {
		doc := annotationDocument([]string{copyRow,
			"| `` `deprecated `` | types | `message` (string, positional) | Mark deprecated |"},
			[]string{rejectedRow}, nil)
		expectFinding(t, checkAnnotationCoverage(annotationTree(t, metas, specs, doc)),
			"`deprecated", "parameters in docs/annotations.md §6 disagree",
			"metaParamSpecs says \"`message` (string, positional, optional)\"")
	})
}

func TestAnnotationCoverageNamedParametersAreASet(t *testing.T) {
	// metaParamSpecs stores named parameters in a Go map, which has no
	// order, so the document may list them in any order. Positional slots
	// are the opposite — TestAnnotationCoveragePositionalOrderIsSignificant
	// is the other half of this claim.
	metas := copyMetas + "\t\"test\": {TargetFunc},\n"
	specs := copySpecs + "\t\"test\": {named: map[string]metaValueKind{" +
		"\"expected\": valString, \"timeout\": valString, \"exclude\": valExcludeCond}},\n"
	doc := annotationDocument([]string{copyRow,
		"| `` `test `` | functions | `timeout` (string, named); `expected` (string, named); " +
			"`exclude` (exclude-condition, named) | Declare a test |"},
		[]string{rejectedRow}, nil)
	if err := checkAnnotationCoverage(annotationTree(t, metas, specs, doc)); err != nil {
		t.Fatalf("named parameters must compare as a set, got:\n%v", err)
	}
}

func TestAnnotationGapKindsAllHaveWording(t *testing.T) {
	// Every gap kind renders as the sentence a finding is built from. A new
	// kind added without a String arm would report every divergence it finds
	// as "unknown gap", which names neither the disagreement nor where to
	// fix it.
	for kind := gapUndocumented; kind <= gapParameters; kind++ {
		if s := kind.String(); s == "unknown gap" {
			t.Errorf("gap kind %d has no wording in String()", int(kind))
		}
	}
	if got := annotationGapKind(99).String(); got != "unknown gap" {
		t.Errorf("an out-of-range kind should render as %q, got %q", "unknown gap", got)
	}
}
