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
