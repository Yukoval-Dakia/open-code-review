// internal/impact/go_analyzer_test.go
package impact

import "testing"

func TestGoAnalyzerChangedSymbols(t *testing.T) {
	src := "package p\n\n" + // line 1
		"func Foo() {}\n" + // line 3
		"type Bar struct{}\n" + // line 4
		"func Untouched() {}\n" + // line 5
		"var MyVar = 1\n" + // line 6
		"const MyConst = 2\n" // line 7
	a := goAnalyzer{}
	syms, err := a.ChangedSymbols(src, map[int]bool{3: true, 4: true, 6: true, 7: true})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	names := map[string]string{}
	for _, s := range syms {
		names[s.Name] = s.Kind
	}
	if names["Foo"] != "function" {
		t.Errorf("Foo kind = %q, want function (got %v)", names["Foo"], names)
	}
	if names["Bar"] != "type" {
		t.Errorf("Bar kind = %q, want type", names["Bar"])
	}
	if _, ok := names["Untouched"]; ok {
		t.Errorf("Untouched should not be reported (line 5 not changed)")
	}
	if names["MyVar"] != "var" {
		t.Errorf("MyVar kind = %q, want var", names["MyVar"])
	}
	if names["MyConst"] != "const" {
		t.Errorf("MyConst kind = %q, want const", names["MyConst"])
	}
}

func TestGoAnalyzerReferencesExcludesCommentsAndStrings(t *testing.T) {
	src := "package q\n" +
		"// Foo is great\n" + // comment, not a ref
		"var s = \"Foo\"\n" + // string literal, not a ref
		"func use() { Foo() }\n" // real call on line 4
	a := goAnalyzer{}
	refs, err := a.References("q.go", src, "Foo")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(refs) != 1 || refs[0].Line != 4 {
		t.Fatalf("refs = %#v, want one ref on line 4", refs)
	}
}
