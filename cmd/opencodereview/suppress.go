package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/open-code-review/open-code-review/internal/learn"
	"github.com/open-code-review/open-code-review/internal/model"
)

// defaultReflagThreshold is the cosine similarity at or above which a freshly
// produced comment is considered a re-flag of a previously human-rejected
// finding. Tuned conservatively: embedding-3 puts genuine paraphrases of the
// same finding well above 0.85, while distinct issues on the same file sit
// lower, so the gate suppresses repeats without swallowing new findings.
const defaultReflagThreshold = 0.86

// reflagSuppressor drops comments that closely match a previously rejected
// learning, fixing the multi-round "re-flag" problem where each stateless
// review run re-raises a finding a human already dismissed. It complements the
// severity filter (filter.go) and runs as a separate output-stage pass.
type reflagSuppressor struct {
	enabled   bool
	threshold float32
	emb       learn.Embedder
	store     *learn.LearningStore
}

// newReflagSuppressor builds a suppressor from env config. It is a no-op (and
// performs no embedding) unless learnings are enabled, an embedder and store
// are available, and the store actually holds rejected learnings.
//
//	OCR_REFLAG_SUPPRESS=off    disable re-flag suppression entirely
//	OCR_REFLAG_THRESHOLD=0.86  min cosine similarity to treat as a repeat
func newReflagSuppressor(enabled bool, emb learn.Embedder, store *learn.LearningStore) reflagSuppressor {
	r := reflagSuppressor{threshold: defaultReflagThreshold, emb: emb, store: store}
	if strings.EqualFold(strings.TrimSpace(os.Getenv("OCR_REFLAG_SUPPRESS")), "off") {
		enabled = false
	}
	if v := strings.TrimSpace(os.Getenv("OCR_REFLAG_THRESHOLD")); v != "" {
		if f, err := strconv.ParseFloat(v, 32); err == nil && f > 0 && f <= 1 {
			r.threshold = float32(f)
		}
	}
	r.enabled = enabled && emb != nil && store != nil && store.HasRejected()
	return r
}

// apply returns the kept comments and the number suppressed as re-flags. Each
// kept comment's content is embedded once and compared against the most similar
// rejected learning; an embed failure for one comment keeps that comment (fail
// open — never silently drop a real finding because the embed API hiccupped).
// Path gating: a rejected learning only suppresses a comment on the same file
// (or one stored without a path), avoiding cross-file false positives.
func (r reflagSuppressor) apply(ctx context.Context, comments []model.LlmComment) (kept []model.LlmComment, dropped int) {
	if !r.enabled {
		return comments, 0
	}
	kept = make([]model.LlmComment, 0, len(comments))
	for _, c := range comments {
		vec, err := r.emb.Embed(ctx, c.Content)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[ocr] reflag: embed failed for %s:%d (kept): %v\n", c.Path, c.StartLine, err)
			kept = append(kept, c)
			continue
		}
		best, ok := r.store.BestRejected(vec)
		if ok && best.Score >= r.threshold && pathMatches(best.Learning.Path, c.Path) {
			dropped++
			continue
		}
		kept = append(kept, c)
	}
	return kept, dropped
}

// pathMatches gates suppression to the same file. An empty stored path matches
// anything (the feedback collector may not have recorded a path).
func pathMatches(stored, current string) bool {
	stored = strings.TrimSpace(stored)
	return stored == "" || stored == strings.TrimSpace(current)
}
