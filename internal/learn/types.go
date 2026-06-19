// Package learn persists OCR's past review comments and their accepted/rejected
// verdicts ("learnings") so future reviews can be informed by them.
package learn

// Verdict is the outcome of a past review comment, derived from GitHub thread state.
type Verdict string

const (
	VerdictAccepted Verdict = "accepted"
	VerdictRejected Verdict = "rejected"
)

// Learning is one past review comment plus its outcome and embedding.
type Learning struct {
	CommentID string    `json:"comment_id"` // GitHub node id; dedupe key
	Body      string    `json:"body"`       // the OCR comment text
	Path      string    `json:"path"`
	Symbol    string    `json:"symbol,omitempty"`
	Verdict   Verdict   `json:"verdict"`
	Embedding []float32 `json:"embedding"`
	CreatedAt string    `json:"created_at"`
}
