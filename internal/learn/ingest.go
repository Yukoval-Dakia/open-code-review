package learn

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
)

// FeedbackItem is one entry in the workflow-produced feedback.json.
type FeedbackItem struct {
	CommentID string  `json:"comment_id"`
	Body      string  `json:"body"`
	Path      string  `json:"path"`
	Symbol    string  `json:"symbol"`
	Verdict   Verdict `json:"verdict"`
}

func validVerdict(v Verdict) bool {
	return v == VerdictAccepted || v == VerdictRejected
}

// Ingest reads feedbackPath (a JSON array of FeedbackItem), embeds each new,
// valid item's Body, and appends it to store. Returns how many new learnings
// were added. A missing file is a clean no-op. An embedding error for one item
// skips that item (warning to stderr) but does not fail the whole ingest.
func Ingest(ctx context.Context, store *LearningStore, emb Embedder, feedbackPath, now string) (int, error) {
	raw, err := os.ReadFile(feedbackPath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	var items []FeedbackItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return 0, fmt.Errorf("parse feedback.json: %w", err)
	}
	added := 0
	for _, it := range items {
		if it.CommentID == "" || it.Body == "" || !validVerdict(it.Verdict) {
			continue
		}
		if store.Has(it.CommentID) {
			continue
		}
		vec, err := emb.Embed(ctx, it.Body)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[ocr] learnings: embed failed for comment %s: %v (skipped)\n", it.CommentID, err)
			continue
		}
		ok, err := store.Append(Learning{
			CommentID: it.CommentID,
			Body:      it.Body,
			Path:      it.Path,
			Symbol:    it.Symbol,
			Verdict:   it.Verdict,
			Embedding: vec,
			CreatedAt: now,
		})
		if err != nil {
			return added, err
		}
		if ok {
			added++
		}
	}
	return added, nil
}
