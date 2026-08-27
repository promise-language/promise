package codegen

import (
	"strings"
	"testing"
)

func TestT1711_ChainedVectorIndexDup(t *testing.T) {
	ir := generateIR(t, `
		main() {
			string url = "https://example.org/a";
			string authority = url.split("://")[1].split("/")[0];
			print_line(authority);
		}
	`)
	// Both index operations should emit string dups (promise_string_new).
	// The bug was that the inner index consumed the dupStringFieldAccess flag,
	// leaving the outer index without a dup → double-free.
	count := strings.Count(ir, "strdup.copy")
	if count < 2 {
		t.Errorf("expected at least 2 strdup.copy blocks for chained vector index, got %d", count)
	}
}

func TestT1711_NestedVectorOfVectorsIndex(t *testing.T) {
	// Exercises genVectorIndex with a nested index target (v[i][j]).
	ir := generateIR(t, `
		main() {
			string[][] v = [["a","b"],["c","d"]];
			string s = v[1][0];
			print_line(s);
		}
	`)
	// The nested index should still produce a string dup for the element.
	if !strings.Contains(ir, "strdup.copy") {
		t.Errorf("expected strdup.copy for nested vector-of-vectors index")
	}
}

func TestT1711_MapIndexDupFlagPreserved(t *testing.T) {
	// Exercises genMethodIndex (Map's [] operator) with a nested expression
	// that could consume dup flags if the save/restore is missing.
	ir := generateIR(t, `
		main() {
			map[string, string] m = {"k": "hello"};
			string s = m["k"]!;
			print_line(s);
		}
	`)
	// Map index produces an optional; the unwrap should still have proper IR.
	if !strings.Contains(ir, "promise_panic") {
		t.Errorf("expected panic call for map index unwrap")
	}
}

func TestT1711_ChainedIndexPreservesFlagsForOuter(t *testing.T) {
	// The inner index target evaluation must NOT consume dup flags meant
	// for the outer index. With the fix, each index level independently
	// produces its own strdup.copy for the extracted string element.
	ir := generateIR(t, `
		split_result(string s) string[] { return [s]; }
		main() {
			string url = "https://example.org/path/file";
			string a = url.split("://")[1].split("/")[0];
			string b = url.split("://")[0];
			print_line(a);
			print_line(b);
		}
	`)
	// The chained expression (a) needs 2 strdup.copy, the single (b) needs 1.
	// Total should be at least 3 strdup.copy blocks.
	count := strings.Count(ir, "strdup.copy")
	if count < 3 {
		t.Errorf("expected at least 3 strdup.copy blocks for mixed chained/single index, got %d", count)
	}
}
