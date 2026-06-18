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

func TestCrossRefProviderDefFileNotReported(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir, map[string]string{
		"def.go":    "package p\nfunc Foo() {}\n",
		"caller.go": "package p\nfunc bar() { Foo() }\n",
	})
	p := NewCrossRefProvider()
	// Pass "./def.go" with a leading "./" prefix to exercise filepath.Clean normalization.
	out, err := p.Context(context.Background(), reviewctx.FileReviewInput{
		RepoDir:    dir,
		Path:       "./def.go",
		NewContent: "package p\nfunc Foo() {}\n",
		Diff:       "@@ -0,0 +1,2 @@\n+package p\n+func Foo() {}\n",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(out, "caller.go") {
		t.Fatalf("expected output to mention caller.go, got:\n%s", out)
	}
	if strings.Contains(out, "def.go") {
		t.Fatalf("definition file def.go must NOT appear as a reference, got:\n%s", out)
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

// TestCrossRefProviderRefMode verifies that when Ref is set the provider greps
// at the given ref (HEAD) rather than the working tree. The test commits
// caller.go with a Foo() call, then overwrites it in the working tree to
// remove the call. With Ref="HEAD" the cross-ref must still report caller.go.
func TestCrossRefProviderRefMode(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir, map[string]string{
		"def.go":    "package p\nfunc Foo() {}\n",
		"caller.go": "package p\nfunc bar() { Foo() }\n",
	})

	// Overwrite caller.go in the working tree so Foo() call is gone.
	callerPath := filepath.Join(dir, "caller.go")
	if err := os.WriteFile(callerPath, []byte("package p\nfunc bar() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := NewCrossRefProvider()
	out, err := p.Context(context.Background(), reviewctx.FileReviewInput{
		RepoDir:    dir,
		Path:       "def.go",
		NewContent: "package p\nfunc Foo() {}\n",
		Diff:       "@@ -0,0 +1,2 @@\n+package p\n+func Foo() {}\n",
		Ref:        "HEAD",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(out, "Foo") || !strings.Contains(out, "caller.go") {
		t.Fatalf("ref-mode should report caller.go (at HEAD), got:\n%s", out)
	}
}
