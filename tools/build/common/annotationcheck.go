package common

import (
	"fmt"
	goast "go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// The three files the annotation reconciliation reads. The document is the
// specification; the two Go tables are the compiler's registration of the same
// closed set, and this check is what keeps them one set rather than two.
const (
	annotationsDoc  = "docs/annotations.md"
	builtinMetasGo  = "compiler/internal/sema/meta.go"
	metaParamSpecGo = "compiler/internal/sema/metaparams.go"
)

// metaTargetWords maps each sema.MetaTarget constant to the word annotations.md
// §6 spells it with, and metaValueWords does the same for sema.metaValueKind.
// Encountering a constant absent from either map is a hard error rather than a
// skip: a new declaration kind or value form is a change to what §6 can express,
// so it must force this check to be updated instead of silently passing.
var metaTargetWords = map[string]string{
	"TargetType":    "types",
	"TargetField":   "fields",
	"TargetMethod":  "methods",
	"TargetFunc":    "functions",
	"TargetEnum":    "enums",
	"TargetParam":   "parameters",
	"TargetVariant": "variants",
	// TargetReturn is declared in sema but no annotation targets a return
	// type, so §6 has no word for one. Registering the first such annotation
	// must fail here until §6 gains the spelling — which is the point.
}

var metaValueWords = map[string]string{
	"valString":      "string",
	"valBool":        "bool",
	"valInt":         "int",
	"valIdent":       "identifier",
	"valTargetCond":  "target-condition",
	"valExcludeCond": "exclude-condition",
}

// annotationGapKind is one way the document and the compiler can disagree.
type annotationGapKind int

const (
	// gapUndocumented — the compiler registers a name §6 has no row for.
	gapUndocumented annotationGapKind = iota
	// gapUnregistered — §6 has a row naming nothing the compiler registers.
	gapUnregistered
	// gapTargets — the row's targets disagree with builtinMetas.
	gapTargets
	// gapParameters — the row's parameters disagree with metaParamSpecs.
	gapParameters
)

func (k annotationGapKind) String() string {
	switch k {
	case gapUndocumented:
		return "registered by the compiler with no row in " + annotationsDoc + " §6"
	case gapUnregistered:
		return "has a row in " + annotationsDoc + " §6 but the compiler registers no such annotation"
	case gapTargets:
		return "targets in " + annotationsDoc + " §6 disagree with builtinMetas"
	case gapParameters:
		return "parameters in " + annotationsDoc + " §6 disagree with metaParamSpecs"
	}
	return "unknown gap"
}

// annotationGap is one known, accepted divergence, and the tracker item that
// closes it.
type annotationGap struct {
	kind annotationGapKind
	item string
	why  string
}

// annotationGaps records every divergence between docs/annotations.md and the
// compiler's tables that is known and already owned by an open tracker item.
// A gap listed here is not reported; a gap that is not listed is.
//
// It lives in this file rather than in the document because docs/org/normative.md
// §3 makes a specification a statement of the end state carrying no status, and
// no inline marker naming an item. The tag query is the status section, so the
// exception ledger belongs to the checker — which is the rule docs/normative.md
// §5 states in the other direction.
//
// The item ID is for the reader. Pre-commit runs offline and cannot reach the
// tracker, so nothing here verifies the item is still open — that is review's
// job. What *is* verified is the gap: an entry whose divergence no longer
// exists is itself a finding, so the ledger cannot outlive the work it excuses.
var annotationGaps = map[string][]annotationGap{
	// T1922 — registered, parameter-checked, and read by no compiler decision.
	"align":  {{gapUndocumented, "T1922", "layout directive with no codegen consumer; implement or delete"}},
	"inline": {{gapUndocumented, "T1922", "inlining hint with no codegen consumer; implement or delete"}},
	"packed": {{gapUndocumented, "T1922", "layout directive with no codegen consumer; implement or delete"}},

	// T1923 — the field-placement surface §9 does not admit.
	"instance": {{gapUndocumented, "T1923", "explicit spelling of the default placement; §16 rejects it"}},
	"variant":  {{gapUndocumented, "T1923", "per-monomorphization field placement; §9 and §16 forbid it"}},
	"value":    {{gapTargets, "T1923", "registered on methods too, where it is inert; §9 makes it a field placement"}},

	// T1564 — §13 requires the symbol; the contract still makes it optional.
	"extern": {{gapParameters, "T1564", "`symbol` is optional in metaParamSpecs; §13 requires it"}},

	// T2017 / T2018 — parameters the document does not admit.
	"deprecated": {{gapParameters, "T2017", "accepts named `since` and a second spelling of `message`; §11 specifies one positional `message`"}},
	"test":       {{gapParameters, "T2018", "accepts `allow_leaks`; §12 lists four parameters and §16 rejects it"}},

	// Specified ahead of implementation — the document leads, as it may.
	"builtin": {{gapUnregistered, "T1413", "specified in §10 with its full role table; not yet registered"}},
	"open":    {{gapUnregistered, "T1537", "specified in §10 with §5.4's transition table; not yet registered"}},
	"sealed":  {{gapUnregistered, "T1537", "specified in §10 with §5.4's transition table; not yet registered"}},
}

// annotationParam is one declared parameter, in the form both the document and
// metaParamSpecs can be reduced to.
type annotationParam struct {
	name     string
	kind     string
	optional bool // positional only; every named parameter may be omitted
}

// annotationSpec is one annotation's machine-checkable contract. Targets and
// named parameters are compared as sets; positional parameters are compared in
// order, because their order is what a call site depends on.
type annotationSpec struct {
	targets    []string // sorted
	positional []annotationParam
	named      []annotationParam // sorted by name
}

// checkAnnotationCoverage reconciles docs/annotations.md §6 — the normative
// list of annotations — against builtinMetas and metaParamSpecs, the compiler's
// registration of the same closed set. Eight assertions:
//
//  1. every registered annotation has a §6 row;
//  2. every §6 row names a registered annotation;
//  3. each row's targets and parameters match the Go tables;
//  4. every §6 name has an entry of its own (a heading, or a row in a grouped
//     entry table) — a row with no entry documents nothing;
//  5. every entry names a §6 row, so no entry documents a non-annotation;
//  6. each entry's Targets/Parameters line repeats its own §6 row — the
//     document renders the contract twice, and the two copies must agree;
//  7. §6 and §16 "Not annotations" are disjoint, so no name is both part of
//     the language and rejected by it;
//  8. builtinMetas and metaParamSpecs describe the same set of names.
//
// Known divergences are excused by annotationGaps, which is itself checked for
// staleness. Only assertions 1-3 are ledgerable: the rest are the document's
// internal consistency, which nothing outside the document can excuse. Without
// all of this the document drifts exactly as language-design.md §8.3 did:
// thirteen missing rows is what an unchecked table looks like after two years.
//
// A tree with no compiler source is not a Promise checkout (RunPreCommit is
// also exercised against bare temp repos), so the check scopes itself out. A
// tree that *has* the compiler but not the document is a finding — that is the
// document having been deleted, not the check being out of scope.
func checkAnnotationCoverage(root string) error {
	metaPath := filepath.Join(root, filepath.FromSlash(builtinMetasGo))
	if _, err := os.Stat(metaPath); os.IsNotExist(err) {
		return nil
	}

	targets, err := parseBuiltinMetas(metaPath)
	if err != nil {
		return err
	}
	params, err := parseMetaParamSpecs(filepath.Join(root, filepath.FromSlash(metaParamSpecGo)))
	if err != nil {
		return err
	}
	doc, err := parseAnnotationDoc(filepath.Join(root, filepath.FromSlash(annotationsDoc)))
	if err != nil {
		return err
	}

	var problems []string

	// (8) The two Go tables must describe one set of names. This mirrors
	// TestT1449SpecsExhaustive at the document layer, and matters here
	// because a name in only one table has only half a contract to compare.
	for _, name := range sortedNames(targets) {
		if _, ok := params[name]; !ok {
			problems = append(problems, fmt.Sprintf(
				"  `%s is in builtinMetas but declares no metaParamSpecs contract", name))
		}
	}
	for _, name := range sortedNames(params) {
		if _, ok := targets[name]; !ok {
			problems = append(problems, fmt.Sprintf(
				"  `%s declares a metaParamSpecs contract but is not in builtinMetas", name))
		}
	}

	registered := map[string]annotationSpec{}
	for name, t := range targets {
		spec := params[name] // zero value when only builtinMetas has it
		spec.targets = t
		registered[name] = spec
	}
	for name, spec := range params {
		if _, ok := registered[name]; !ok {
			registered[name] = spec
		}
	}

	// found records the divergences this run actually observed, so the
	// ledger can be checked both ways: an unledgered gap is reported, and a
	// ledgered gap that is absent means the ledger row has outlived its work.
	found := map[string]map[annotationGapKind]string{}
	note := func(name string, kind annotationGapKind, detail string) {
		if found[name] == nil {
			found[name] = map[annotationGapKind]string{}
		}
		found[name][kind] = detail
	}

	// (1) Registered with no row.
	for _, name := range sortedNames(registered) {
		if _, ok := doc.index[name]; !ok {
			note(name, gapUndocumented, "")
		}
	}

	for _, name := range doc.order {
		row := doc.index[name]
		// (2) A row naming nothing the compiler registers.
		have, ok := registered[name]
		if !ok {
			note(name, gapUnregistered, "")
			continue
		}
		// (3) Targets and parameters.
		if !equalStrings(have.targets, row.targets) {
			note(name, gapTargets, fmt.Sprintf("document says %q, builtinMetas says %q",
				renderTargets(row.targets), renderTargets(have.targets)))
		}
		if !equalParams(have.positional, row.positional) || !equalParams(have.named, row.named) {
			note(name, gapParameters, fmt.Sprintf("document says %q, metaParamSpecs says %q",
				renderParams(row), renderParams(have)))
		}
	}

	// Report every gap the ledger does not excuse.
	for _, name := range sortedNames(found) {
		for _, kind := range sortedGapKinds(found[name]) {
			if ledgeredGap(name, kind) {
				continue
			}
			line := fmt.Sprintf("  `%s %s", name, kind)
			if detail := found[name][kind]; detail != "" {
				line += ": " + detail
			}
			problems = append(problems, line+"\n    (a known divergence belongs in annotationGaps in "+
				"tools/build/common/annotationcheck.go, naming the open item that closes it)")
		}
	}

	// A ledger row whose divergence is gone must be deleted in the same
	// change that closed it — the same discipline CLAUDE.md states for
	// tracker markers in docs: a marker outliving its item is worse than no
	// marker, because it describes a compiler that no longer exists.
	for _, name := range sortedNames(annotationGaps) {
		for _, gap := range annotationGaps[name] {
			if _, ok := found[name][gap.kind]; ok {
				continue
			}
			problems = append(problems, fmt.Sprintf(
				"  annotationGaps entry `%s (%s, %s) no longer applies — the divergence is gone; delete the row",
				name, gap.item, gap.kind))
		}
	}

	// (4) A row with no entry of its own documents nothing but its own name,
	// and (6) an entry that does have one must repeat that row rather than
	// paraphrase it. The second is what keeps the document's two renderings
	// of one contract from drifting the way §6 and the Go tables would
	// without assertions 1-3.
	for _, name := range doc.order {
		entry, ok := doc.entry[name]
		if !ok {
			problems = append(problems, fmt.Sprintf(
				"  `%s has a §6 row but no entry in %s", name, annotationsDoc))
			continue
		}
		row := doc.index[name]
		if !equalStrings(entry.targets, row.targets) {
			problems = append(problems, fmt.Sprintf(
				"  `%s: the entry says targets %q, its §6 row says %q",
				name, renderTargets(entry.targets), renderTargets(row.targets)))
		}
		if !equalParams(entry.positional, row.positional) || !equalParams(entry.named, row.named) {
			problems = append(problems, fmt.Sprintf(
				"  `%s: the entry says parameters %q, its §6 row says %q",
				name, renderParams(entry), renderParams(row)))
		}
	}

	// (5) An entry for a name §6 does not list documents something that is
	// not an annotation — the mirror of (4).
	for _, name := range sortedNames(doc.entry) {
		if _, ok := doc.index[name]; !ok {
			problems = append(problems, fmt.Sprintf(
				"  `%s has an entry in %s but no §6 row", name, annotationsDoc))
		}
	}

	// (7) A name cannot be both part of the language and rejected by it.
	for _, name := range doc.order {
		if doc.rejected[name] {
			problems = append(problems, fmt.Sprintf(
				"  `%s is listed both in %s §6 and in its \"Not annotations\" table", name, annotationsDoc))
		}
	}

	if len(problems) > 0 {
		return fmt.Errorf("annotation coverage:\n%s", strings.Join(problems, "\n"))
	}
	return nil
}

// ledgeredGap reports whether annotationGaps excuses this exact divergence.
func ledgeredGap(name string, kind annotationGapKind) bool {
	for _, gap := range annotationGaps[name] {
		if gap.kind == kind {
			return true
		}
	}
	return false
}

// --- annotations.md parsing ---

// annotationDoc is what the reconciliation needs to read out of the document.
//
// index and entry are the document's two renderings of one contract — the §6
// row and the entry that expands it — so both are parsed into the same shape
// and compared. A name present in one and not the other is itself a finding.
type annotationDoc struct {
	index    map[string]annotationSpec // §6 rows, by annotation name
	order    []string                  // §6 row order, so findings read top to bottom
	entry    map[string]annotationSpec // the Targets/Parameters an entry declares
	rejected map[string]bool           // §16 "Not annotations"
}

// annotationIndexHeader and annotationRejectedHeader locate the document's
// tables by their column signature rather than by section number, so
// renumbering the document cannot silently disable half the check. A missing
// table is a finding, never a skip.
//
// Several tables carry the index signature: the first is §6 itself, and each
// later one is a *grouped entry* — annotations documented as a table row
// instead of a heading each, as §15's field annotations are. They are read the
// same way because they say the same thing, which is what lets one comparison
// cover both.
var (
	annotationIndexHeader    = []string{"Annotation", "Targets", "Parameters", "Effect"}
	annotationRejectedHeader = []string{"Name", "Why not, and what to use"}
)

// docParam matches one rendered parameter: `name` (kind, positional|named[, optional]).
var docParam = regexp.MustCompile("^`([A-Za-z_][A-Za-z0-9_]*)`\\s*\\(\\s*([a-z-]+)\\s*,\\s*(positional|named)\\s*(,\\s*optional\\s*)?\\)$")

// entrySchema matches an entry's opening line, which restates its §6 row:
//
//   - **Targets** types, enums · **Parameters** — none
//
// The line may wrap; entrySchemaLine reassembles it before this runs.
var entrySchema = regexp.MustCompile(`^- \*\*Targets\*\* (.+?) · \*\*Parameters\*\* (.+)$`)

// parseAnnotationDoc reads what the reconciliation compares out of
// annotations.md: the §6 index, the "Not annotations" table, and every entry —
// whether it is a `###` heading with a schema line or a row in a grouped entry
// table.
func parseAnnotationDoc(path string) (annotationDoc, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return annotationDoc{}, fmt.Errorf("annotation coverage: read %s: %w", annotationsDoc, err)
	}
	// A Windows checkout carries the document with CRLF endings (autocrlf), and
	// the schema regexps are anchored at end of line.
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")

	doc := annotationDoc{
		index:    map[string]annotationSpec{},
		entry:    map[string]annotationSpec{},
		rejected: map[string]bool{},
	}

	tables := findMarkdownTables(lines, annotationIndexHeader)
	if len(tables) == 0 {
		return annotationDoc{}, fmt.Errorf("annotation coverage: %s has no index table (a row reading %q)",
			annotationsDoc, "| "+strings.Join(annotationIndexHeader, " | ")+" |")
	}
	for _, row := range tables[0] {
		name := annotationName(row[0])
		if name == "" {
			return annotationDoc{}, fmt.Errorf("annotation coverage: %s: index row %q names no annotation",
				annotationsDoc, strings.Join(row, " | "))
		}
		if _, dup := doc.index[name]; dup {
			return annotationDoc{}, fmt.Errorf("annotation coverage: %s: `%s has two index rows", annotationsDoc, name)
		}
		spec, err := parseDocSpec(row[1], row[2])
		if err != nil {
			return annotationDoc{}, fmt.Errorf("annotation coverage: %s: `%s: %w", annotationsDoc, name, err)
		}
		doc.index[name] = spec
		doc.order = append(doc.order, name)
	}

	// Every later table with the index signature is a grouped entry: its rows
	// are entries, not index rows.
	for _, rows := range tables[1:] {
		for _, row := range rows {
			name := annotationName(row[0])
			if name == "" {
				return annotationDoc{}, fmt.Errorf("annotation coverage: %s: grouped entry row %q names no annotation",
					annotationsDoc, strings.Join(row, " | "))
			}
			spec, err := parseDocSpec(row[1], row[2])
			if err != nil {
				return annotationDoc{}, fmt.Errorf("annotation coverage: %s: `%s: %w", annotationsDoc, name, err)
			}
			if err := doc.addEntry(name, spec); err != nil {
				return annotationDoc{}, err
			}
		}
	}

	rejected := findMarkdownTables(lines, annotationRejectedHeader)
	if len(rejected) == 0 {
		return annotationDoc{}, fmt.Errorf("annotation coverage: %s has no \"Not annotations\" table (a row reading %q)",
			annotationsDoc, "| "+strings.Join(annotationRejectedHeader, " | ")+" |")
	}
	for _, rows := range rejected {
		for _, row := range rows {
			doc.rejected[annotationName(row[0])] = true
		}
	}

	// The other kind of entry is a `###` heading, which may name a pair —
	// "`sendable` / `sharable`" — in which case its schema line speaks for
	// both, and the two rows it repeats must therefore agree.
	for i, line := range lines {
		if !strings.HasPrefix(line, "### ") {
			continue
		}
		names := headingNames(line)
		if len(names) == 0 {
			continue // a section heading, not an entry
		}
		targetCell, paramCell, ok := entrySchemaLine(lines, i+1)
		if !ok {
			return annotationDoc{}, fmt.Errorf("annotation coverage: %s: the entry %q has no "+
				"\"- **Targets** … · **Parameters** …\" line", annotationsDoc, strings.TrimPrefix(line, "### "))
		}
		spec, err := parseDocSpec(targetCell, paramCell)
		if err != nil {
			return annotationDoc{}, fmt.Errorf("annotation coverage: %s: the entry %q: %w",
				annotationsDoc, strings.TrimPrefix(line, "### "), err)
		}
		for _, name := range names {
			if err := doc.addEntry(name, spec); err != nil {
				return annotationDoc{}, err
			}
		}
	}
	return doc, nil
}

// addEntry records one name's entry, rejecting a second one: two entries for
// the same annotation are two places to change it, which is the defect this
// whole check exists to prevent.
func (d *annotationDoc) addEntry(name string, spec annotationSpec) error {
	if _, dup := d.entry[name]; dup {
		return fmt.Errorf("annotation coverage: %s: `%s has two entries", annotationsDoc, name)
	}
	d.entry[name] = spec
	return nil
}

// entrySchemaLine returns the Targets and Parameters cells of the first entry
// schema line at or after `from`, stopping at the next heading. The line may be
// wrapped across several source lines — Markdown joins them — so continuations
// are folded back in before it is matched.
func entrySchemaLine(lines []string, from int) (string, string, bool) {
	for i := from; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "#") {
			return "", "", false
		}
		if !strings.HasPrefix(lines[i], "- **Targets**") {
			continue
		}
		joined := lines[i]
		for j := i + 1; j < len(lines); j++ {
			next := strings.TrimSpace(lines[j])
			if next == "" || strings.HasPrefix(next, "- ") || strings.HasPrefix(next, "#") {
				break
			}
			joined += " " + next
		}
		m := entrySchema.FindStringSubmatch(joined)
		if m == nil {
			return "", "", false
		}
		return m[1], m[2], true
	}
	return "", "", false
}

// parseDocSpec turns a Targets and a Parameters cell — from an index row, a
// grouped entry row, or an entry's schema line — into the same shape the Go
// tables reduce to, so all three compare directly.
func parseDocSpec(targetCell, paramCell string) (annotationSpec, error) {
	var spec annotationSpec
	for _, word := range strings.Split(targetCell, ",") {
		word = strings.TrimSpace(word)
		if word == "" {
			return spec, fmt.Errorf("empty target in %q", targetCell)
		}
		if !knownWord(metaTargetWords, word) {
			return spec, fmt.Errorf("unknown target %q; the vocabulary is %s", word, vocabulary(metaTargetWords))
		}
		spec.targets = append(spec.targets, word)
	}
	sort.Strings(spec.targets)

	// "— none" is how an entry's schema line spells an empty cell, "—" how a
	// table row does; they mean the same thing and must compare equal.
	if cell := strings.TrimSpace(paramCell); cell != "—" && cell != "— none" && cell != "-" && cell != "" {
		for _, part := range strings.Split(cell, ";") {
			part = strings.TrimSpace(part)
			m := docParam.FindStringSubmatch(part)
			if m == nil {
				return spec, fmt.Errorf("cannot read parameter %q; write `name` (kind, positional|named[, optional])", part)
			}
			if !knownWord(metaValueWords, m[2]) {
				return spec, fmt.Errorf("unknown parameter kind %q in %q; the vocabulary is %s",
					m[2], part, vocabulary(metaValueWords))
			}
			p := annotationParam{name: m[1], kind: m[2], optional: m[4] != ""}
			if m[3] == "named" {
				// Every named parameter may be omitted, so
				// "optional" on one says nothing and would match
				// no flag in metaParamSpecs.
				if p.optional {
					return spec, fmt.Errorf(
						"named parameter %q cannot be marked optional; every named parameter may be omitted", p.name)
				}
				spec.named = append(spec.named, p)
				continue
			}
			spec.positional = append(spec.positional, p)
		}
	}
	sort.Slice(spec.named, func(i, j int) bool { return spec.named[i].name < spec.named[j].name })
	return spec, nil
}

// findMarkdownTables returns the body rows of every table whose header row has
// exactly the given cells, in document order. Rows are collected until the
// first line that is not a table row, so the `|---|` rule is the only line
// skipped. Returning all of them rather than the first is what keeps a second
// table with the same signature from being silently ignored.
func findMarkdownTables(lines []string, header []string) [][][]string {
	var tables [][][]string
	for i := 0; i < len(lines); i++ {
		cells, ok := splitTableRow(lines[i])
		if !ok || !equalStrings(cells, header) {
			continue
		}
		var rows [][]string
		j := i + 1
		for ; j < len(lines); j++ {
			cells, ok := splitTableRow(lines[j])
			if !ok {
				break
			}
			if j == i+1 && isTableSeparator(cells) {
				continue
			}
			rows = append(rows, cells)
		}
		tables = append(tables, rows)
		i = j - 1
	}
	return tables
}

// splitTableRow splits a Markdown table row into its trimmed cells.
func splitTableRow(line string) ([]string, bool) {
	line = strings.TrimSpace(line)
	if len(line) < 2 || !strings.HasPrefix(line, "|") || !strings.HasSuffix(line, "|") {
		return nil, false
	}
	parts := strings.Split(line[1:len(line)-1], "|")
	cells := make([]string, len(parts))
	for i, p := range parts {
		cells[i] = strings.TrimSpace(p)
	}
	return cells, true
}

// isTableSeparator reports whether a row is the `|---|---|` rule under a header.
func isTableSeparator(cells []string) bool {
	for _, c := range cells {
		if strings.Trim(c, "-: ") != "" {
			return false
		}
	}
	return true
}

// annotationName extracts the bare name from a table cell — the "copy" in a
// cell reading `copy, and the "align" in one reading `align(N). Backticks are
// separators, not content, and a parameter list is not part of the name.
func annotationName(cell string) string {
	name := strings.ReplaceAll(cell, "`", " ")
	if i := strings.IndexByte(name, '('); i >= 0 {
		name = name[:i]
	}
	return strings.TrimSpace(name)
}

// headingNames extracts every name from an entry heading, so a heading covering
// a pair — one naming `sendable and `sharable together — documents both. A
// heading with no backticks names a section rather than an annotation ("###
// Field annotations") and contributes nothing.
func headingNames(line string) []string {
	var names []string
	for _, part := range strings.Split(strings.TrimPrefix(line, "### "), "/") {
		if !strings.Contains(part, "`") {
			continue
		}
		if name := annotationName(part); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// --- sema table parsing ---

// parseBuiltinMetas reduces sema's builtinMetas to name -> sorted target words.
func parseBuiltinMetas(path string) (map[string][]string, error) {
	lit, err := parseGoMapVar(path, "builtinMetas")
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, elt := range lit.Elts {
		name, value, err := goMapEntry(path, "builtinMetas", elt)
		if err != nil {
			return nil, err
		}
		list, ok := value.(*goast.CompositeLit)
		if !ok {
			return nil, fmt.Errorf("annotation coverage: %s: builtinMetas[%q] is not a target list",
				filepath.ToSlash(path), name)
		}
		var targets []string
		for _, t := range list.Elts {
			id, ok := t.(*goast.Ident)
			if !ok {
				return nil, fmt.Errorf("annotation coverage: %s: builtinMetas[%q] has a non-identifier target",
					filepath.ToSlash(path), name)
			}
			word, ok := metaTargetWords[id.Name]
			if !ok {
				return nil, fmt.Errorf("annotation coverage: %s: unknown MetaTarget %s — add it to "+
					"metaTargetWords in tools/build/common/annotationcheck.go, and give %s a spelling in %s §6",
					filepath.ToSlash(path), id.Name, id.Name, annotationsDoc)
			}
			targets = append(targets, word)
		}
		sort.Strings(targets)
		out[name] = targets
	}
	return out, nil
}

// parseMetaParamSpecs reduces sema's metaParamSpecs to name -> parameter
// contract, in the same shape parseDocSpec produces from the document.
func parseMetaParamSpecs(path string) (map[string]annotationSpec, error) {
	lit, err := parseGoMapVar(path, "metaParamSpecs")
	if err != nil {
		return nil, err
	}
	out := map[string]annotationSpec{}
	for _, elt := range lit.Elts {
		name, value, err := goMapEntry(path, "metaParamSpecs", elt)
		if err != nil {
			return nil, err
		}
		spec, err := parseGoParamSpec(path, name, value)
		if err != nil {
			return nil, err
		}
		out[name] = spec
	}
	return out, nil
}

// parseGoParamSpec reads one metaParamSpec value: either the shared `noParams`
// identifier or a composite literal with `positional` and `named` fields.
func parseGoParamSpec(path, name string, value goast.Expr) (annotationSpec, error) {
	var spec annotationSpec
	switch v := value.(type) {
	case *goast.Ident:
		if v.Name != "noParams" {
			return spec, fmt.Errorf("annotation coverage: %s: metaParamSpecs[%q] is the identifier %s, "+
				"which this check does not know how to read", filepath.ToSlash(path), name, v.Name)
		}
		return spec, nil
	case *goast.CompositeLit:
		for _, elt := range v.Elts {
			kv, ok := elt.(*goast.KeyValueExpr)
			if !ok {
				return spec, fmt.Errorf("annotation coverage: %s: metaParamSpecs[%q] has an unkeyed field",
					filepath.ToSlash(path), name)
			}
			field, ok := kv.Key.(*goast.Ident)
			if !ok {
				return spec, fmt.Errorf("annotation coverage: %s: metaParamSpecs[%q] has a non-identifier field",
					filepath.ToSlash(path), name)
			}
			switch field.Name {
			case "positional":
				positional, err := parseGoPositional(path, name, kv.Value)
				if err != nil {
					return spec, err
				}
				spec.positional = positional
			case "named":
				named, err := parseGoNamed(path, name, kv.Value)
				if err != nil {
					return spec, err
				}
				spec.named = named
			default:
				return spec, fmt.Errorf("annotation coverage: %s: metaParamSpecs[%q] has unknown field %q — "+
					"a new field is a change to what %s §6 must record",
					filepath.ToSlash(path), name, field.Name, annotationsDoc)
			}
		}
		return spec, nil
	}
	return spec, fmt.Errorf("annotation coverage: %s: metaParamSpecs[%q] is not a spec literal",
		filepath.ToSlash(path), name)
}

// parseGoPositional reads a []metaPositional literal, preserving slot order.
func parseGoPositional(path, name string, value goast.Expr) ([]annotationParam, error) {
	list, ok := value.(*goast.CompositeLit)
	if !ok {
		return nil, fmt.Errorf("annotation coverage: %s: metaParamSpecs[%q].positional is not a list",
			filepath.ToSlash(path), name)
	}
	var out []annotationParam
	for _, elt := range list.Elts {
		slot, ok := elt.(*goast.CompositeLit)
		if !ok {
			return nil, fmt.Errorf("annotation coverage: %s: metaParamSpecs[%q].positional has a non-literal slot",
				filepath.ToSlash(path), name)
		}
		var p annotationParam
		for _, f := range slot.Elts {
			kv, ok := f.(*goast.KeyValueExpr)
			if !ok {
				return nil, fmt.Errorf("annotation coverage: %s: metaParamSpecs[%q].positional has an unkeyed field",
					filepath.ToSlash(path), name)
			}
			field, ok := kv.Key.(*goast.Ident)
			if !ok {
				return nil, fmt.Errorf("annotation coverage: %s: metaParamSpecs[%q].positional has a non-identifier field",
					filepath.ToSlash(path), name)
			}
			switch field.Name {
			case "name":
				s, err := goStringLit(path, kv.Value)
				if err != nil {
					return nil, err
				}
				p.name = s
			case "kind":
				kind, err := goValueKind(path, name, kv.Value)
				if err != nil {
					return nil, err
				}
				p.kind = kind
			case "optional":
				id, ok := kv.Value.(*goast.Ident)
				if !ok || (id.Name != "true" && id.Name != "false") {
					return nil, fmt.Errorf("annotation coverage: %s: metaParamSpecs[%q].positional "+
						"has a non-boolean `optional`", filepath.ToSlash(path), name)
				}
				p.optional = id.Name == "true"
			default:
				return nil, fmt.Errorf("annotation coverage: %s: metaPositional has unknown field %q — "+
					"a new field is a change to what %s §6 must record",
					filepath.ToSlash(path), field.Name, annotationsDoc)
			}
		}
		out = append(out, p)
	}
	return out, nil
}

// parseGoNamed reads a map[string]metaValueKind literal, sorted by name — the
// map is unordered, so the document records it as a set.
func parseGoNamed(path, name string, value goast.Expr) ([]annotationParam, error) {
	list, ok := value.(*goast.CompositeLit)
	if !ok {
		return nil, fmt.Errorf("annotation coverage: %s: metaParamSpecs[%q].named is not a map literal",
			filepath.ToSlash(path), name)
	}
	var out []annotationParam
	for _, elt := range list.Elts {
		kv, ok := elt.(*goast.KeyValueExpr)
		if !ok {
			return nil, fmt.Errorf("annotation coverage: %s: metaParamSpecs[%q].named has an unkeyed entry",
				filepath.ToSlash(path), name)
		}
		key, err := goStringLit(path, kv.Key)
		if err != nil {
			return nil, err
		}
		kind, err := goValueKind(path, name, kv.Value)
		if err != nil {
			return nil, err
		}
		out = append(out, annotationParam{name: key, kind: kind})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

// goValueKind maps a metaValueKind constant to the word §6 spells it with.
func goValueKind(path, name string, value goast.Expr) (string, error) {
	id, ok := value.(*goast.Ident)
	if !ok {
		return "", fmt.Errorf("annotation coverage: %s: metaParamSpecs[%q] has a non-identifier value kind",
			filepath.ToSlash(path), name)
	}
	word, ok := metaValueWords[id.Name]
	if !ok {
		return "", fmt.Errorf("annotation coverage: %s: unknown metaValueKind %s — add it to metaValueWords "+
			"in tools/build/common/annotationcheck.go, and give %s a spelling in %s §6",
			filepath.ToSlash(path), id.Name, id.Name, annotationsDoc)
	}
	return word, nil
}

// parseGoMapVar returns the composite literal assigned to a package-level map
// variable. Go's own parser is used rather than a regexp because both tables
// carry interleaved comments that a line matcher would trip over.
func parseGoMapVar(path, name string) (*goast.CompositeLit, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("annotation coverage: parse %s: %w", filepath.ToSlash(path), err)
	}
	for _, d := range f.Decls {
		gd, ok := d.(*goast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, s := range gd.Specs {
			vs, ok := s.(*goast.ValueSpec)
			if !ok {
				continue
			}
			for i, n := range vs.Names {
				if n.Name != name || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*goast.CompositeLit)
				if !ok {
					return nil, fmt.Errorf("annotation coverage: %s: %s is not a map literal",
						filepath.ToSlash(path), name)
				}
				return lit, nil
			}
		}
	}
	return nil, fmt.Errorf("annotation coverage: %s declares no package-level %s", filepath.ToSlash(path), name)
}

// goMapEntry splits one `"key": value` element of a map literal.
func goMapEntry(path, table string, elt goast.Expr) (string, goast.Expr, error) {
	kv, ok := elt.(*goast.KeyValueExpr)
	if !ok {
		return "", nil, fmt.Errorf("annotation coverage: %s: %s has an unkeyed element",
			filepath.ToSlash(path), table)
	}
	key, err := goStringLit(path, kv.Key)
	if err != nil {
		return "", nil, err
	}
	return key, kv.Value, nil
}

// goStringLit unquotes a Go string literal expression.
func goStringLit(path string, e goast.Expr) (string, error) {
	lit, ok := e.(*goast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", fmt.Errorf("annotation coverage: %s: expected a string literal", filepath.ToSlash(path))
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", fmt.Errorf("annotation coverage: %s: cannot unquote %s: %w", filepath.ToSlash(path), lit.Value, err)
	}
	return s, nil
}

// --- comparison and rendering ---

// equalStrings compares two string slices element by element. Both sides are
// sorted before they reach here where order does not matter.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// equalParams compares parameter lists. Positional lists arrive in slot order,
// which is significant; named lists arrive sorted, which makes this a set
// comparison.
func equalParams(a, b []annotationParam) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// renderTargets and renderParams write a spec in the document's own spelling,
// so a finding can be pasted straight into the §6 row it is about.
func renderTargets(targets []string) string {
	if len(targets) == 0 {
		return "(none)"
	}
	return strings.Join(targets, ", ")
}

func renderParams(spec annotationSpec) string {
	var parts []string
	for _, p := range spec.positional {
		s := fmt.Sprintf("`%s` (%s, positional", p.name, p.kind)
		if p.optional {
			s += ", optional"
		}
		parts = append(parts, s+")")
	}
	for _, p := range spec.named {
		parts = append(parts, fmt.Sprintf("`%s` (%s, named)", p.name, p.kind))
	}
	if len(parts) == 0 {
		return "—"
	}
	return strings.Join(parts, "; ")
}

// knownWord and vocabulary read the canonical spellings straight out of the
// constant maps, so a diagnostic can never list a vocabulary the check does not
// actually accept.
func knownWord(words map[string]string, word string) bool {
	for _, w := range words {
		if w == word {
			return true
		}
	}
	return false
}

func vocabulary(words map[string]string) string {
	out := make([]string, 0, len(words))
	for _, w := range words {
		out = append(out, w)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// sortedNames returns a map's keys in a deterministic order, so findings are
// stable across runs.
func sortedNames[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sortedGapKinds orders the gaps observed for one annotation.
func sortedGapKinds(m map[annotationGapKind]string) []annotationGapKind {
	out := make([]annotationGapKind, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
