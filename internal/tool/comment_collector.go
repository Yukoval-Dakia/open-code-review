package tool

import (
	"sync"

	"github.com/open-code-review/open-code-review/internal/model"
)

// CommentCollector is a thread-safe, per-Agent comment store.
// Each Agent instance owns its own collector so reviews across different repos do not interfere.
type CommentCollector struct {
	mu       sync.Mutex
	comments []model.LlmComment
}

// NewCommentCollector creates an empty collector.
func NewCommentCollector() *CommentCollector {
	return &CommentCollector{}
}

// Add appends a comment to the collector.
func (c *CommentCollector) Add(cm model.LlmComment) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.comments = append(c.comments, cm)
}

// Comments returns all collected comments.
func (c *CommentCollector) Comments() []model.LlmComment {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]model.LlmComment, len(c.comments))
	copy(out, c.comments)
	return out
}

// CommentsForPath returns a copy of comments whose Path matches the given path.
func (c *CommentCollector) CommentsForPath(path string) []model.LlmComment {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []model.LlmComment
	for _, cm := range c.comments {
		if cm.Path == path {
			out = append(out, cm)
		}
	}
	return out
}

// RemoveByPathAndIndices removes comments for a given path whose per-path index
// (0-based position among all comments with that path) is in the indices set.
func (c *CommentCollector) RemoveByPathAndIndices(path string, indices map[int]struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	kept := c.comments[:0]
	pathIdx := 0
	for _, cm := range c.comments {
		if cm.Path == path {
			if _, remove := indices[pathIdx]; remove {
				pathIdx++
				continue
			}
			pathIdx++
		}
		kept = append(kept, cm)
	}
	tail := c.comments[len(kept):]
	for i := range tail {
		tail[i] = model.LlmComment{}
	}
	c.comments = kept
}
