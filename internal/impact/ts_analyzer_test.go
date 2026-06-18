// internal/impact/ts_analyzer_test.go
package impact

import (
	"os/exec"
	"testing"
)

func requireNode(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
	// typescript must be resolvable from CWD; the impact package dir has none,
	// so skip unless a global/local install resolves.
	if !(tsAnalyzer{}).nodeHasTypeScript() {
		t.Skip("typescript not resolvable from CWD")
	}
}

func TestTSAnalyzerChangedSymbols(t *testing.T) {
	requireNode(t)
	src := "export function foo() {}\n" + // line 1
		"export class Bar {}\n" // line 2
	a := tsAnalyzer{}
	syms, err := a.ChangedSymbols(src, map[int]bool{1: true})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(syms) != 1 || syms[0].Name != "foo" || syms[0].Kind != "function" {
		t.Fatalf("syms = %#v, want one function foo", syms)
	}
}

func TestTSAnalyzerReferencesExcludesStrings(t *testing.T) {
	requireNode(t)
	src := "const s = \"foo\";\n" + // string, not a ref
		"foo();\n" // call on line 2
	a := tsAnalyzer{}
	refs, err := a.References("x.ts", src, "foo")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(refs) != 1 || refs[0].Line != 2 || refs[0].Kind != "call" {
		t.Fatalf("refs = %#v, want one call on line 2", refs)
	}
}
