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
