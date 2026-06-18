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
