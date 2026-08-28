package common

import (
	"testing"
)

func TestScanProtocolMethodsFindsHandle(t *testing.T) {
	source := `type Handler ` + "`" + `structural(protocol: true) ` + "`" + `public {
  handle!(this, ServerRequest request) ServerResponse ` + "`" + `abstract;
}
`
	table := &ProtocolTriggerTable{
		Triggers: make(map[string][]ProtocolTriggerEntry),
	}
	scanProtocolMethods(source, "http", table)

	entries, ok := table.Triggers["handle"]
	if !ok {
		t.Fatal("expected trigger for 'handle'")
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Module != "http" {
		t.Errorf("expected module 'http', got %q", entries[0].Module)
	}
	if entries[0].IsGetter {
		t.Error("expected IsGetter=false")
	}
	if entries[0].IsSetter {
		t.Error("expected IsSetter=false")
	}
}

func TestScanProtocolMethodsGetterAbstract(t *testing.T) {
	source := `type Proto ` + "`" + `structural(protocol: true) {
  get name string ` + "`" + `abstract;
}
`
	table := &ProtocolTriggerTable{
		Triggers: make(map[string][]ProtocolTriggerEntry),
	}
	scanProtocolMethods(source, "mymod", table)

	entries, ok := table.Triggers["name"]
	if !ok {
		t.Fatal("expected trigger for 'name'")
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if !entries[0].IsGetter {
		t.Error("expected IsGetter=true")
	}
}

func TestScanProtocolMethodsSkipsNonProtocol(t *testing.T) {
	source := `type NotProto ` + "`" + `structural {
  handle(this) string ` + "`" + `abstract;
}
`
	table := &ProtocolTriggerTable{
		Triggers: make(map[string][]ProtocolTriggerEntry),
	}
	scanProtocolMethods(source, "mymod", table)

	if len(table.Triggers) != 0 {
		t.Errorf("expected no triggers for non-protocol type, got %d", len(table.Triggers))
	}
}

func TestScanProtocolMethodsMultipleAbstract(t *testing.T) {
	source := `type Proto ` + "`" + `structural(protocol: true) {
  foo(this) string ` + "`" + `abstract;
  bar!(this, int x) int ` + "`" + `abstract;
}
`
	table := &ProtocolTriggerTable{
		Triggers: make(map[string][]ProtocolTriggerEntry),
	}
	scanProtocolMethods(source, "mymod", table)

	if _, ok := table.Triggers["foo"]; !ok {
		t.Error("expected trigger for 'foo'")
	}
	if _, ok := table.Triggers["bar"]; !ok {
		t.Error("expected trigger for 'bar'")
	}
}

func TestIsProtocolTypeDecl(t *testing.T) {
	tests := []struct {
		line string
		want bool
	}{
		{`type Handler ` + "`" + `structural(protocol: true) ` + "`" + `public {`, true},
		{`type Foo ` + "`" + `structural {`, false},
		{`type Bar {`, false},
		{`enum Baz ` + "`" + `structural(protocol: true) {`, false}, // enum, not type
	}
	for _, tt := range tests {
		if got := isProtocolTypeDecl(tt.line); got != tt.want {
			t.Errorf("isProtocolTypeDecl(%q) = %v, want %v", tt.line, got, tt.want)
		}
	}
}

func TestExtractAbstractMethodName(t *testing.T) {
	tests := []struct {
		line       string
		wantName   string
		wantGetter bool
		wantSetter bool
	}{
		{`handle!(this, Request r) Response ` + "`" + `abstract;`, "handle", false, false},
		{`process(this) string ` + "`" + `abstract;`, "process", false, false},
		{`get name string ` + "`" + `abstract;`, "name", true, false},
		{`set name(string value) ` + "`" + `abstract;`, "name", false, true},
		// No '(' or '!' → empty name.
		{`just_a_word ` + "`" + `abstract;`, "", false, false},
		// Spaces in name → empty (invalid).
		{`not a method(this) ` + "`" + `abstract;`, "", false, false},
		// Getter with empty rest → returns the backtick-bearing token as name (invalid, but no crash).
		{`get `, "", false, false},
	}
	for _, tt := range tests {
		name, isGetter, isSetter := extractAbstractMethodName(tt.line)
		if name != tt.wantName {
			t.Errorf("extractAbstractMethodName(%q) name = %q, want %q", tt.line, name, tt.wantName)
		}
		if isGetter != tt.wantGetter {
			t.Errorf("extractAbstractMethodName(%q) isGetter = %v, want %v", tt.line, isGetter, tt.wantGetter)
		}
		if isSetter != tt.wantSetter {
			t.Errorf("extractAbstractMethodName(%q) isSetter = %v, want %v", tt.line, isSetter, tt.wantSetter)
		}
	}
}

func TestScanProtocolMethodsSetterAbstract(t *testing.T) {
	source := `type Proto ` + "`" + `structural(protocol: true) {
  set value(int v) ` + "`" + `abstract;
}
`
	table := &ProtocolTriggerTable{
		Triggers: make(map[string][]ProtocolTriggerEntry),
	}
	scanProtocolMethods(source, "mymod", table)

	entries, ok := table.Triggers["value"]
	if !ok {
		t.Fatal("expected trigger for 'value'")
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if !entries[0].IsSetter {
		t.Error("expected IsSetter=true")
	}
}

func TestAppendUniqueTriggerDeduplicates(t *testing.T) {
	entry := ProtocolTriggerEntry{Module: "http", IsGetter: false, IsSetter: false}
	slice := []ProtocolTriggerEntry{entry}
	// Appending the same entry should not duplicate.
	result := appendUniqueTrigger(slice, entry)
	if len(result) != 1 {
		t.Errorf("expected 1 entry after dedup, got %d", len(result))
	}
	// Appending a different entry should add.
	different := ProtocolTriggerEntry{Module: "net", IsGetter: false, IsSetter: false}
	result = appendUniqueTrigger(result, different)
	if len(result) != 2 {
		t.Errorf("expected 2 entries, got %d", len(result))
	}
}

func TestScanProtocolMethodsSingleLineType(t *testing.T) {
	// A protocol type declared on a single line (opening and closing brace same line).
	// Unlikely for a real protocol but tests the braceDepth <= 0 exit path.
	source := `type Proto ` + "`" + `structural(protocol: true) {}
`
	table := &ProtocolTriggerTable{
		Triggers: make(map[string][]ProtocolTriggerEntry),
	}
	scanProtocolMethods(source, "mymod", table)
	if len(table.Triggers) != 0 {
		t.Errorf("expected no triggers for empty protocol, got %d", len(table.Triggers))
	}
}

func TestExtractFirstWordFullString(t *testing.T) {
	// When no delimiter is found, returns the whole string.
	result := extractFirstWord("identifier")
	if result != "identifier" {
		t.Errorf("expected 'identifier', got %q", result)
	}
}

func TestScanProtocolMethodsNonAbstractSkipped(t *testing.T) {
	// A protocol with a default (non-abstract) method — should not be in triggers.
	source := `type Proto ` + "`" + `structural(protocol: true) {
  handle(this) string ` + "`" + `abstract;
  helper(this) int { return 0; }
}
`
	table := &ProtocolTriggerTable{
		Triggers: make(map[string][]ProtocolTriggerEntry),
	}
	scanProtocolMethods(source, "mymod", table)
	if _, ok := table.Triggers["helper"]; ok {
		t.Error("non-abstract method 'helper' should not be in triggers")
	}
	if _, ok := table.Triggers["handle"]; !ok {
		t.Error("abstract method 'handle' should be in triggers")
	}
}
