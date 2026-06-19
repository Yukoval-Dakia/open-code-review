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
	if !strings.Contains(got, "<cross_reference_impact>") {
		t.Fatalf("non-empty extra context should be wrapped, got %q", got)
	}
}

// An empty provider output must yield "" — no dangling <cross_reference_impact>
// tags in the prompt for files with nothing to add.
func TestRenderExtraContextEmptyHasNoWrapper(t *testing.T) {
	a := &Agent{ctxProviders: []reviewctx.ContextProvider{fakeProvider{out: "  "}}}
	got := a.renderExtraContext(context.Background(), "x.md", "diff", "content")
	if got != "" {
		t.Fatalf("empty extra context should render to \"\", got %q", got)
	}
}
