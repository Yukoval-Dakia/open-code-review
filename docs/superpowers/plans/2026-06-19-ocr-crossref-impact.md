# OCR Cross-Reference Impact Context — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Before the model reviews a file, automatically compute where the file's changed symbols are referenced elsewhere in the repo and inject a capped summary into the review prompt, so cross-file breakage is caught.

**Architecture:** A `reviewctx.ContextProvider` framework injects extra per-file context into the MAIN_TASK prompt. The first provider, `impact.CrossRefProvider`, extracts changed symbols (native per-language parsers), finds references via `git grep` + a confirming parse, and emits a capped summary. Wired into `agent.executeSubtask` as a new `{{extra_context}}` template variable.

**Tech Stack:** Go (stdlib `go/parser`/`go/ast`), an embedded Node helper using the TypeScript compiler for `.ts/.tsx`, `git grep`, `go:embed`.

## Global Constraints

- **No CGO.** OCR must stay a pure-Go static binary. Go parsing uses stdlib only; TS parsing shells out to Node. Never add a CGO dependency (no tree-sitter Go bindings).
- **Deterministic, side-effect-free providers.** No LLM calls in providers; no writes.
- **Graceful degradation.** Unsupported language, missing Node/`typescript`, or any parse/grep error must skip silently (stderr warning) and let the review proceed unchanged.
- **No silent truncation.** Whenever caps drop references, say so in the emitted summary.
- **Module path:** `github.com/open-code-review/open-code-review`.
- **Test command:** `go test ./...` from repo root. Repo convention: table/fixture tests next to code.

---

## File Structure

- Create `internal/reviewctx/provider.go` — `ContextProvider` interface, `FileReviewInput`, `Aggregate`.
- Create `internal/reviewctx/provider_test.go`.
- Create `internal/impact/analyzer.go` — `Symbol`, `Reference`, `LangAnalyzer` interface, `changedNewLines`, analyzer registry.
- Create `internal/impact/analyzer_test.go`.
- Create `internal/impact/go_analyzer.go` — `goAnalyzer` (go/parser).
- Create `internal/impact/go_analyzer_test.go`.
- Create `internal/impact/ts_analyzer.go` + `internal/impact/ts_refs.js` (embedded) — `tsAnalyzer`.
- Create `internal/impact/ts_analyzer_test.go`.
- Create `internal/impact/crossref.go` — `CrossRefProvider` (config, grep, confirm, summary).
- Create `internal/impact/crossref_test.go`.
- Modify `internal/agent/agent.go` — build providers, render `{{extra_context}}` in `executeSubtask` (~line 548-566).
- Modify `internal/config/template/task_template.json` — add the `{{extra_context}}` section to MAIN_TASK user message + a system-prompt instruction line.

---

### Task 1: ContextProvider framework

**Files:**
- Create: `internal/reviewctx/provider.go`
- Test: `internal/reviewctx/provider_test.go`

**Interfaces:**
- Produces: `reviewctx.FileReviewInput{RepoDir, Path, NewContent, Diff string; ChangedLines map[int]bool}`; `reviewctx.ContextProvider` interface with `Name() string` and `Context(ctx, FileReviewInput) (string, error)`; `reviewctx.Aggregate(ctx, []ContextProvider, FileReviewInput, warn func(string, error)) string`.

- [ ] **Step 1: Write the failing test**

```go
// internal/reviewctx/provider_test.go
package reviewctx

import (
	"context"
	"errors"
	"testing"
)

type stubProvider struct {
	name string
	out  string
	err  error
}

func (s stubProvider) Name() string { return s.name }
func (s stubProvider) Context(context.Context, FileReviewInput) (string, error) {
	return s.out, s.err
}

func TestAggregateJoinsNonEmptyAndSkipsErrors(t *testing.T) {
	var warned []string
	providers := []ContextProvider{
		stubProvider{name: "a", out: "block A"},
		stubProvider{name: "b", err: errors.New("boom")},
		stubProvider{name: "c", out: "  "}, // whitespace -> dropped
		stubProvider{name: "d", out: "block D"},
	}
	got := Aggregate(context.Background(), providers, FileReviewInput{}, func(p string, _ error) {
		warned = append(warned, p)
	})
	want := "block A\n\nblock D"
	if got != want {
		t.Errorf("Aggregate = %q, want %q", got, want)
	}
	if len(warned) != 1 || warned[0] != "b" {
		t.Errorf("warned = %v, want [b]", warned)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/reviewctx/ -run TestAggregate -v`
Expected: FAIL — package/identifiers undefined.

- [ ] **Step 3: Write minimal implementation**

```go
// internal/reviewctx/provider.go
package reviewctx

import (
	"context"
	"strings"
)

// FileReviewInput is the per-file context handed to each provider.
type FileReviewInput struct {
	RepoDir      string
	Path         string       // file under review (new path)
	NewContent   string       // full new content of the file
	Diff         string       // the file's unified diff
	ChangedLines map[int]bool // changed line numbers in the new file
}

// ContextProvider supplies extra, injectable review context for one file.
// Implementations must be deterministic and side-effect-free.
type ContextProvider interface {
	Name() string
	Context(ctx context.Context, in FileReviewInput) (string, error)
}

// Aggregate runs each provider and joins non-empty, trimmed outputs with a
// blank line. A provider error is reported via warn and skipped (never fatal).
func Aggregate(ctx context.Context, providers []ContextProvider, in FileReviewInput, warn func(provider string, err error)) string {
	var blocks []string
	for _, p := range providers {
		out, err := p.Context(ctx, in)
		if err != nil {
			if warn != nil {
				warn(p.Name(), err)
			}
			continue
		}
		if out = strings.TrimSpace(out); out != "" {
			blocks = append(blocks, out)
		}
	}
	return strings.Join(blocks, "\n\n")
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/reviewctx/ -run TestAggregate -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/reviewctx/
git commit -m "feat(reviewctx): ContextProvider framework for injectable review context"
```

---

### Task 2: Analyzer types + changed-line extraction

**Files:**
- Create: `internal/impact/analyzer.go`
- Test: `internal/impact/analyzer_test.go`

**Interfaces:**
- Produces: `impact.Symbol{Name, Kind string; DefLine int}`; `impact.Reference{File string; Line int; Kind string}`; `impact.LangAnalyzer` interface (`Supports(path string) bool`, `ChangedSymbols(content string, changed map[int]bool) ([]Symbol, error)`, `References(path, content, name string) ([]Reference, error)`); `impact.ChangedNewLines(diff string) map[int]bool`.

- [ ] **Step 1: Write the failing test**

```go
// internal/impact/analyzer_test.go
package impact

import "testing"

func TestChangedNewLines(t *testing.T) {
	diff := "" +
		"@@ -1,2 +1,3 @@\n" +
		" context\n" + // new line 1 (context)
		"+added a\n" + // new line 2 (added)
		"+added b\n" + // new line 3 (added)
		"@@ -10,1 +11,1 @@\n" +
		"-removed\n" + // not a new line
		"+changed\n" // new line 11 (added)
	got := ChangedNewLines(diff)
	for _, ln := range []int{2, 3, 11} {
		if !got[ln] {
			t.Errorf("line %d should be marked changed; got %v", ln, got)
		}
	}
	if got[1] { // context line is not "changed"
		t.Errorf("context line 1 should not be marked changed")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/impact/ -run TestChangedNewLines -v`
Expected: FAIL — undefined.

- [ ] **Step 3: Write minimal implementation**

```go
// internal/impact/analyzer.go
package impact

import (
	"regexp"
	"strconv"
	"strings"
)

// Symbol is a definition found in a changed file.
type Symbol struct {
	Name    string
	Kind    string // function | method | class | interface | type | enum | const | export
	DefLine int
}

// Reference is a confirmed use of a symbol in another file.
type Reference struct {
	File string
	Line int
	Kind string // call | import | type-use | ref
}

// LangAnalyzer parses one language's definitions and references.
type LangAnalyzer interface {
	Supports(path string) bool
	// ChangedSymbols returns definitions whose line intersects changed.
	ChangedSymbols(content string, changed map[int]bool) ([]Symbol, error)
	// References returns confirmed references to name in content (path is for kind hints).
	References(path, content, name string) ([]Reference, error)
}

var hunkHeader = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

// ChangedNewLines parses a unified diff and returns the set of NEW-file line
// numbers that were added (lines starting with '+', excluding the '+++' header).
func ChangedNewLines(diff string) map[int]bool {
	changed := map[int]bool{}
	newLine := 0
	for _, line := range strings.Split(diff, "\n") {
		if m := hunkHeader.FindStringSubmatch(line); m != nil {
			newLine, _ = strconv.Atoi(m[1])
			continue
		}
		switch {
		case strings.HasPrefix(line, "+++"):
			// file header, ignore
		case strings.HasPrefix(line, "+"):
			changed[newLine] = true
			newLine++
		case strings.HasPrefix(line, "-"):
			// removed from old file; new-file numbering unaffected
		default:
			newLine++ // context line
		}
	}
	return changed
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/impact/ -run TestChangedNewLines -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/impact/analyzer.go internal/impact/analyzer_test.go
git commit -m "feat(impact): analyzer types and changed-line extraction"
```

---

### Task 3: Go analyzer (go/parser)

**Files:**
- Create: `internal/impact/go_analyzer.go`
- Test: `internal/impact/go_analyzer_test.go`

**Interfaces:**
- Consumes: `Symbol`, `Reference`, `LangAnalyzer` (Task 2).
- Produces: `goAnalyzer` (implements `LangAnalyzer`).

- [ ] **Step 1: Write the failing test**

```go
// internal/impact/go_analyzer_test.go
package impact

import "testing"

func TestGoAnalyzerChangedSymbols(t *testing.T) {
	src := "package p\n\n" + // line 1
		"func Foo() {}\n" + // line 3
		"type Bar struct{}\n" + // line 4
		"func Untouched() {}\n" // line 5
	a := goAnalyzer{}
	syms, err := a.ChangedSymbols(src, map[int]bool{3: true, 4: true})
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/impact/ -run TestGoAnalyzer -v`
Expected: FAIL — `goAnalyzer` undefined.

- [ ] **Step 3: Write minimal implementation**

```go
// internal/impact/go_analyzer.go
package impact

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
)

type goAnalyzer struct{}

func (goAnalyzer) Supports(path string) bool { return strings.HasSuffix(path, ".go") }

func (goAnalyzer) ChangedSymbols(content string, changed map[int]bool) ([]Symbol, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", content, 0)
	if err != nil {
		return nil, err
	}
	var out []Symbol
	add := func(name string, kind string, pos token.Pos) {
		line := fset.Position(pos).Line
		if changed[line] {
			out = append(out, Symbol{Name: name, Kind: kind, DefLine: line})
		}
	}
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			kind := "function"
			if d.Recv != nil {
				kind = "method"
			}
			add(d.Name.Name, kind, d.Name.Pos())
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					add(s.Name.Name, "type", s.Name.Pos())
				case *ast.ValueSpec:
					for _, n := range s.Names {
						add(n.Name, "const", n.Pos())
					}
				}
			}
		}
	}
	return out, nil
}

func (goAnalyzer) References(path, content, name string) ([]Reference, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", content, 0)
	if err != nil {
		return nil, err
	}
	seen := map[int]bool{}
	var refs []Reference
	ast.Inspect(f, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok || id.Name != name {
			return true
		}
		// Skip the definition site itself and duplicate lines.
		line := fset.Position(id.Pos()).Line
		if seen[line] {
			return true
		}
		seen[line] = true
		refs = append(refs, Reference{File: path, Line: line, Kind: "ref"})
		return true
	})
	return refs, nil
}
```

> Note: `go/parser` with mode `0` drops comments from the AST, and string
> literals are `*ast.BasicLit` not `*ast.Ident`, so both are naturally excluded —
> that is what the test asserts.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/impact/ -run TestGoAnalyzer -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/impact/go_analyzer.go internal/impact/go_analyzer_test.go
git commit -m "feat(impact): Go analyzer via go/parser"
```

---

### Task 4: TypeScript analyzer (embedded Node helper)

**Files:**
- Create: `internal/impact/ts_refs.js`
- Create: `internal/impact/ts_analyzer.go`
- Test: `internal/impact/ts_analyzer_test.go`

**Interfaces:**
- Consumes: `Symbol`, `Reference`, `LangAnalyzer` (Task 2).
- Produces: `tsAnalyzer` (implements `LangAnalyzer`). Uses `nodeAvailable()` and runs the embedded `ts_refs.js` with a JSON request on stdin, JSON response on stdout.

- [ ] **Step 1: Write the embedded Node helper**

```javascript
// internal/impact/ts_refs.js
// Reads a JSON request on stdin, writes a JSON response on stdout.
// Request:  {mode:"symbols", content, changed:[lineNums]} |
//           {mode:"refs", content, name}
// Response: {symbols:[{name,kind,line}]} | {refs:[{line,kind}]} | {error}
// Resolves 'typescript' from the CWD's node_modules (the repo under review).
const chunks = [];
process.stdin.on('data', c => chunks.push(c));
process.stdin.on('end', () => {
  try {
    const ts = require('typescript');
    const req = JSON.parse(Buffer.concat(chunks).toString('utf8'));
    const sf = ts.createSourceFile('f.tsx', req.content, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
    const lineOf = pos => sf.getLineAndCharacterOfPosition(pos).line + 1;
    if (req.mode === 'symbols') {
      const changed = new Set(req.changed || []);
      const symbols = [];
      const kindFor = n => {
        if (ts.isFunctionDeclaration(n)) return 'function';
        if (ts.isMethodDeclaration(n)) return 'method';
        if (ts.isClassDeclaration(n)) return 'class';
        if (ts.isInterfaceDeclaration(n)) return 'interface';
        if (ts.isTypeAliasDeclaration(n)) return 'type';
        if (ts.isEnumDeclaration(n)) return 'enum';
        return null;
      };
      const visit = n => {
        const kind = kindFor(n);
        if (kind && n.name && ts.isIdentifier(n.name)) {
          const line = lineOf(n.name.getStart(sf));
          if (changed.has(line)) symbols.push({ name: n.name.text, kind, line });
        }
        ts.forEachChild(n, visit);
      };
      visit(sf);
      process.stdout.write(JSON.stringify({ symbols }));
    } else if (req.mode === 'refs') {
      const refs = [];
      const seen = new Set();
      const visit = n => {
        if (ts.isIdentifier(n) && n.text === req.name) {
          const line = lineOf(n.getStart(sf));
          if (!seen.has(line)) {
            seen.add(line);
            let kind = 'ref';
            const p = n.parent;
            if (p && ts.isCallExpression(p) && p.expression === n) kind = 'call';
            else if (p && (ts.isImportSpecifier(p) || ts.isImportClause(p))) kind = 'import';
            else if (p && ts.isTypeReferenceNode(p)) kind = 'type-use';
            refs.push({ line, kind });
          }
        }
        ts.forEachChild(n, visit);
      };
      visit(sf);
      process.stdout.write(JSON.stringify({ refs }));
    } else {
      process.stdout.write(JSON.stringify({ error: 'unknown mode' }));
    }
  } catch (e) {
    process.stdout.write(JSON.stringify({ error: String(e && e.message || e) }));
  }
});
```

- [ ] **Step 2: Write the failing test**

```go
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
	if !nodeHasTypeScript() {
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
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/impact/ -run TestTSAnalyzer -v`
Expected: FAIL — `tsAnalyzer`/`nodeHasTypeScript` undefined.

- [ ] **Step 4: Write minimal implementation**

```go
// internal/impact/ts_analyzer.go
package impact

import (
	_ "embed"
	"encoding/json"
	"os/exec"
	"strings"
)

//go:embed ts_refs.js
var tsRefsScript []byte

type tsAnalyzer struct{}

func (tsAnalyzer) Supports(path string) bool {
	return strings.HasSuffix(path, ".ts") || strings.HasSuffix(path, ".tsx")
}

type tsRequest struct {
	Mode    string `json:"mode"`
	Content string `json:"content"`
	Changed []int  `json:"changed,omitempty"`
	Name    string `json:"name,omitempty"`
}

type tsResponse struct {
	Symbols []struct {
		Name string `json:"name"`
		Kind string `json:"kind"`
		Line int    `json:"line"`
	} `json:"symbols"`
	Refs []struct {
		Line int    `json:"line"`
		Kind string `json:"kind"`
	} `json:"refs"`
	Error string `json:"error"`
}

func runTSHelper(req tsRequest) (tsResponse, error) {
	var resp tsResponse
	in, err := json.Marshal(req)
	if err != nil {
		return resp, err
	}
	cmd := exec.Command("node", "-e", string(tsRefsScript))
	cmd.Stdin = strings.NewReader(string(in))
	out, err := cmd.Output()
	if err != nil {
		return resp, err
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return resp, err
	}
	if resp.Error != "" {
		return resp, &helperError{resp.Error}
	}
	return resp, nil
}

type helperError struct{ msg string }

func (e *helperError) Error() string { return "ts helper: " + e.msg }

// nodeHasTypeScript reports whether node can require('typescript') from CWD.
func nodeHasTypeScript() bool {
	cmd := exec.Command("node", "-e", "require.resolve('typescript')")
	return cmd.Run() == nil
}

func (tsAnalyzer) ChangedSymbols(content string, changed map[int]bool) ([]Symbol, error) {
	lines := make([]int, 0, len(changed))
	for ln := range changed {
		lines = append(lines, ln)
	}
	resp, err := runTSHelper(tsRequest{Mode: "symbols", Content: content, Changed: lines})
	if err != nil {
		return nil, err
	}
	out := make([]Symbol, 0, len(resp.Symbols))
	for _, s := range resp.Symbols {
		out = append(out, Symbol{Name: s.Name, Kind: s.Kind, DefLine: s.Line})
	}
	return out, nil
}

func (tsAnalyzer) References(path, content, name string) ([]Reference, error) {
	resp, err := runTSHelper(tsRequest{Mode: "refs", Content: content, Name: name})
	if err != nil {
		return nil, err
	}
	out := make([]Reference, 0, len(resp.Refs))
	for _, r := range resp.Refs {
		out = append(out, Reference{File: path, Line: r.Line, Kind: r.Kind})
	}
	return out, nil
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/impact/ -run TestTSAnalyzer -v`
Expected: PASS, or SKIP if node/typescript absent. (CI on TLP's runner has both.)

- [ ] **Step 6: Commit**

```bash
git add internal/impact/ts_refs.js internal/impact/ts_analyzer.go internal/impact/ts_analyzer_test.go
git commit -m "feat(impact): TypeScript analyzer via embedded Node helper (no CGO)"
```

---

### Task 5: CrossRefProvider (config, grep, confirm, summary)

**Files:**
- Create: `internal/impact/crossref.go`
- Test: `internal/impact/crossref_test.go`

**Interfaces:**
- Consumes: `Symbol`, `Reference`, `LangAnalyzer`, `ChangedNewLines` (Tasks 2-4); `reviewctx.FileReviewInput`, `reviewctx.ContextProvider` (Task 1).
- Produces: `impact.NewCrossRefProvider() *CrossRefProvider` (implements `reviewctx.ContextProvider`); env config via `OCR_IMPACT_CONTEXT` / `OCR_IMPACT_MAX_REFS`.

- [ ] **Step 1: Write the failing test**

```go
// internal/impact/crossref_test.go
package impact

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-code-review/open-code-review/internal/reviewctx"
)

func gitInit(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	for name, body := range files {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("add", "-A")
	run("-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "init")
}

func TestCrossRefProviderGoImpact(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir, map[string]string{
		"def.go":    "package p\nfunc Foo() {}\n",
		"caller.go": "package p\nfunc bar() { Foo() }\n",
	})
	p := NewCrossRefProvider()
	out, err := p.Context(context.Background(), reviewctx.FileReviewInput{
		RepoDir:    dir,
		Path:       "def.go",
		NewContent: "package p\nfunc Foo() {}\n",
		Diff:       "@@ -0,0 +1,2 @@\n+package p\n+func Foo() {}\n",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(out, "Foo") || !strings.Contains(out, "caller.go") {
		t.Fatalf("expected impact mentioning Foo in caller.go, got:\n%s", out)
	}
}

func TestCrossRefProviderDisabled(t *testing.T) {
	t.Setenv("OCR_IMPACT_CONTEXT", "off")
	p := NewCrossRefProvider()
	out, err := p.Context(context.Background(), reviewctx.FileReviewInput{Path: "x.go"})
	if err != nil || out != "" {
		t.Fatalf("disabled provider should return empty, got %q err %v", out, err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/impact/ -run TestCrossRefProvider -v`
Expected: FAIL — `NewCrossRefProvider` undefined.

- [ ] **Step 3: Write minimal implementation**

```go
// internal/impact/crossref.go
package impact

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	"github.com/open-code-review/open-code-review/internal/reviewctx"
)

const (
	defaultMaxRefs      = 20
	defaultPerSymbolCap = 8
)

// CrossRefProvider injects a cross-reference impact summary for the file's
// changed symbols. Implements reviewctx.ContextProvider.
type CrossRefProvider struct {
	enabled   bool
	maxRefs   int
	analyzers []LangAnalyzer
}

func NewCrossRefProvider() *CrossRefProvider {
	p := &CrossRefProvider{
		enabled:   true,
		maxRefs:   defaultMaxRefs,
		analyzers: []LangAnalyzer{goAnalyzer{}, tsAnalyzer{}},
	}
	if strings.EqualFold(strings.TrimSpace(os.Getenv("OCR_IMPACT_CONTEXT")), "off") {
		p.enabled = false
	}
	if v := strings.TrimSpace(os.Getenv("OCR_IMPACT_MAX_REFS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			p.maxRefs = n
		}
	}
	return p
}

func (p *CrossRefProvider) Name() string { return "crossref-impact" }

func (p *CrossRefProvider) analyzerFor(path string) LangAnalyzer {
	for _, a := range p.analyzers {
		if a.Supports(path) {
			return a
		}
	}
	return nil
}

func (p *CrossRefProvider) Context(ctx context.Context, in reviewctx.FileReviewInput) (string, error) {
	if !p.enabled || p.maxRefs == 0 {
		return "", nil
	}
	a := p.analyzerFor(in.Path)
	if a == nil {
		return "", nil // unsupported language: skip
	}
	changed := in.ChangedLines
	if changed == nil {
		changed = ChangedNewLines(in.Diff)
	}
	symbols, err := a.ChangedSymbols(in.NewContent, changed)
	if err != nil || len(symbols) == 0 {
		return "", err // parse error or nothing changed: skip (caller logs err)
	}

	type symRefs struct {
		sym  Symbol
		refs []Reference
	}
	var results []symRefs
	total := 0
	truncated := false
	for _, sym := range symbols {
		refs := p.findRefs(ctx, in.RepoDir, in.Path, sym.Name, a)
		if len(refs) == 0 {
			continue
		}
		if len(refs) > defaultPerSymbolCap {
			refs = refs[:defaultPerSymbolCap]
			truncated = true
		}
		if total+len(refs) > p.maxRefs {
			refs = refs[:p.maxRefs-total]
			truncated = true
		}
		total += len(refs)
		results = append(results, symRefs{sym, refs})
		if total >= p.maxRefs {
			truncated = true
			break
		}
	}
	if len(results) == 0 {
		return "", nil
	}
	return renderSummary(results, truncated), nil
}

// findRefs greps for candidate files then confirms via the analyzer.
func (p *CrossRefProvider) findRefs(ctx context.Context, repoDir, defPath, name string, a LangAnalyzer) []Reference {
	cmd := exec.CommandContext(ctx, "git", "grep", "-l", "-w", "-e", name)
	cmd.Dir = repoDir
	out, err := cmd.Output()
	if err != nil {
		return nil // no matches or grep error
	}
	var refs []Reference
	for _, cand := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if cand == "" || cand == defPath || !a.Supports(cand) {
			continue
		}
		body, err := os.ReadFile(repoDir + string(os.PathSeparator) + cand)
		if err != nil {
			continue
		}
		found, err := a.References(cand, string(body), name)
		if err != nil {
			continue
		}
		refs = append(refs, found...)
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].File != refs[j].File {
			return refs[i].File < refs[j].File
		}
		return refs[i].Line < refs[j].Line
	})
	return refs
}

func renderSummary(results []struct {
	sym  Symbol
	refs []Reference
}, truncated bool) string {
	var b strings.Builder
	b.WriteString("## Cross-reference impact (auto-computed, structural)\n")
	b.WriteString("Symbols changed in this file and where they are used elsewhere — verify these references are not broken by the change:\n")
	shown, totalKnown := 0, 0
	for _, r := range results {
		parts := make([]string, 0, len(r.refs))
		for _, ref := range r.refs {
			parts = append(parts, fmt.Sprintf("%s:%d (%s)", ref.File, ref.Line, ref.Kind))
		}
		shown += len(r.refs)
		totalKnown += len(r.refs)
		b.WriteString(fmt.Sprintf("- `%s` (%s): %s\n", r.sym.Name, r.sym.Kind, strings.Join(parts, ", ")))
	}
	if truncated {
		b.WriteString(fmt.Sprintf("(showing %d references, capped; dynamic or indirect uses may be missed)\n", shown))
	}
	return b.String()
}
```

> Note: `renderSummary`'s anonymous-struct parameter must match the `symRefs`
> shape used in `Context`. If the compiler complains about the unexported
> `symRefs` type crossing the function boundary, promote `symRefs` to a package
> type `type symRefs struct { sym Symbol; refs []Reference }` and use it in both.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/impact/ -run TestCrossRefProvider -v`
Expected: PASS (the Go impact test needs only `git`, always present in this repo).

- [ ] **Step 5: Commit**

```bash
git add internal/impact/crossref.go internal/impact/crossref_test.go
git commit -m "feat(impact): CrossRefProvider — grep + confirm + capped summary"
```

---

### Task 6: Wire into the agent + prompt

**Files:**
- Modify: `internal/agent/agent.go` (executeSubtask render loop ~548-566; add provider field + construction)
- Modify: `internal/config/template/task_template.json` (MAIN_TASK user message + system instruction)
- Test: `internal/agent/agent_extra_context_test.go`

**Interfaces:**
- Consumes: `reviewctx.Aggregate`, `reviewctx.FileReviewInput`, `reviewctx.ContextProvider` (Task 1); `impact.NewCrossRefProvider` (Task 5).

- [ ] **Step 1: Add the template variable to MAIN_TASK**

In `internal/config/template/task_template.json`, MAIN_TASK **user** message: insert before the `<user_task>` line (keep it one JSON string; use `\n`):

```
\n<cross_reference_impact>\n{{extra_context}}\n</cross_reference_impact>\n
```

And in MAIN_TASK **system** message "Reply limit" area add one line:

```
\n- When a <cross_reference_impact> section is provided, check whether the change breaks any listed reference before concluding.
```

- [ ] **Step 2: Write the failing test**

```go
// internal/agent/agent_extra_context_test.go
package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/open-code-review/open-code-review/internal/reviewctx"
)

type fakeProvider struct{ out string }

func (fakeProvider) Name() string { return "fake" }
func (f fakeProvider) Context(context.Context, reviewctx.FileReviewInput) (string, error) {
	return f.out, nil
}

func TestRenderExtraContextSubstitutes(t *testing.T) {
	a := &Agent{ctxProviders: []reviewctx.ContextProvider{fakeProvider{out: "IMPACT-BLOCK"}}}
	got := a.renderExtraContext(context.Background(), "x.go", "diff", "content")
	if !strings.Contains(got, "IMPACT-BLOCK") {
		t.Fatalf("extra context = %q, want it to contain IMPACT-BLOCK", got)
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/agent/ -run TestRenderExtraContext -v`
Expected: FAIL — `ctxProviders` field / `renderExtraContext` undefined.

- [ ] **Step 4: Add the field, constructor wiring, and helper**

In `internal/agent/agent.go`, add to the `Agent` struct an unexported field:

```go
	ctxProviders []reviewctx.ContextProvider
```

In `New(args Args)`, after the agent is constructed, default the providers when unset:

```go
	if a.ctxProviders == nil {
		a.ctxProviders = []reviewctx.ContextProvider{impact.NewCrossRefProvider()}
	}
```

Add the helper (near `executeSubtask`):

```go
func (a *Agent) renderExtraContext(ctx context.Context, path, diff, newContent string) string {
	if len(a.ctxProviders) == 0 {
		return ""
	}
	return reviewctx.Aggregate(ctx, a.ctxProviders, reviewctx.FileReviewInput{
		RepoDir:    a.args.RepoDir,
		Path:       path,
		NewContent: newContent,
		Diff:       diff,
	}, func(p string, err error) {
		a.recordWarning("context_provider_error", path, p+": "+err.Error())
	})
}
```

Add imports `"github.com/open-code-review/open-code-review/internal/impact"` and `"github.com/open-code-review/open-code-review/internal/reviewctx"`.

In `executeSubtask`, inside the render loop (after the `{{diff}}` replace at ~line 553), add:

```go
		content = strings.ReplaceAll(content, "{{extra_context}}", a.renderExtraContext(ctx, newPath, d.Diff, d.NewFileContent))
```

> Compute it once before the loop if preferred (it does not depend on `m`):
> `extra := a.renderExtraContext(ctx, newPath, d.Diff, d.NewFileContent)` then
> `strings.ReplaceAll(content, "{{extra_context}}", extra)` inside the loop.

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./internal/agent/ -run TestRenderExtraContext -v`
Expected: PASS.

- [ ] **Step 6: Full build, vet, and test**

Run:
```bash
go build ./... && go vet ./... && go test ./...
```
Expected: build ok, vet clean, all packages `ok` (TS analyzer tests SKIP if node/typescript absent).

- [ ] **Step 7: Rebuild the deployed binary and commit**

```bash
go build -o "$HOME/.local/bin/ocr" ./cmd/opencodereview
git add internal/agent/agent.go internal/config/template/task_template.json internal/agent/agent_extra_context_test.go
git commit -m "feat(review): inject cross-reference impact context into MAIN_TASK"
```

---

## Self-Review

**Spec coverage:**
- Approach (extract symbols → grep → confirm → inject): Tasks 2-6. ✓
- Native per-language, no CGO (go/parser + Node TS): Tasks 3-4. ✓
- ContextProvider abstraction + {{extra_context}}: Tasks 1, 6. ✓
- Config (OCR_IMPACT_CONTEXT / OCR_IMPACT_MAX_REFS) + no silent truncation: Task 5. ✓
- Graceful degradation (unsupported lang, missing node, parse/grep error): Tasks 4-6 (skip paths + warn). ✓
- Testing (fixtures, temp git repo, skip-on-no-node): every task. ✓
- Out of scope (learnings/LSP/graph): not present. ✓

**Type consistency:** `LangAnalyzer` signature (`ChangedSymbols(content, map[int]bool)`, `References(path, content, name)`) identical across Tasks 2/3/4/5. `reviewctx.FileReviewInput` fields identical across Tasks 1/5/6. `NewCrossRefProvider()` return used in Task 6 matches Task 5. The `symRefs`/`renderSummary` note flags the one place to promote a named type if the compiler objects.

**Placeholder scan:** no TBD/TODO; all code blocks complete.
