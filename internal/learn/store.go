package learn

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// LearningStore is an append-only, deduplicated, soft-capped JSON-lines store.
// It loads fully into memory; Phase 2 adds cosine retrieval over s.entries.
type LearningStore struct {
	path    string
	entries []Learning
	index   map[string]int // CommentID -> position in entries
	cap     int
}

// OpenStore loads the JSON-lines file at path (a missing file yields an empty
// store). softCap bounds the number of retained entries (<=0 means unbounded).
func OpenStore(path string, softCap int) (*LearningStore, error) {
	s := &LearningStore{path: path, index: map[string]int{}, cap: softCap}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024) // embeddings make lines large
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var l Learning
		if err := json.Unmarshal(line, &l); err != nil {
			continue // skip malformed lines rather than failing the whole load
		}
		s.index[l.CommentID] = len(s.entries)
		s.entries = append(s.entries, l)
	}
	return s, sc.Err()
}

// Has reports whether a learning with the given CommentID is already stored.
func (s *LearningStore) Has(commentID string) bool {
	_, ok := s.index[commentID]
	return ok
}

// Len returns the number of stored learnings.
func (s *LearningStore) Len() int { return len(s.entries) }

// Append adds a learning (no-op if its CommentID already exists or is empty),
// evicts the oldest entries beyond the soft cap, and rewrites the file. Returns
// whether a new entry was added.
func (s *LearningStore) Append(l Learning) (bool, error) {
	// Fix 3: reject entries with no CommentID — they can't be deduped.
	if l.CommentID == "" {
		return false, nil
	}
	if s.Has(l.CommentID) {
		return false, nil
	}

	// Snapshot pre-mutation state for rollback on flush failure.
	prevEntries := make([]Learning, len(s.entries))
	copy(prevEntries, s.entries)
	prevIndex := make(map[string]int, len(s.index))
	for k, v := range s.index {
		prevIndex[k] = v
	}

	s.entries = append(s.entries, l)
	if s.cap > 0 && len(s.entries) > s.cap {
		drop := len(s.entries) - s.cap
		fmt.Fprintf(os.Stderr, "[ocr] learnings store at cap (%d); evicting %d oldest entr(ies)\n", s.cap, drop)
		s.entries = s.entries[drop:]
	}
	// Rebuild index after possible eviction.
	s.index = make(map[string]int, len(s.entries))
	for i, e := range s.entries {
		s.index[e.CommentID] = i
	}

	// Fix 1: roll back in-memory state if flush fails.
	if err := s.flush(); err != nil {
		s.entries = prevEntries
		s.index = prevIndex
		return false, err
	}
	return true, nil
}

// flush rewrites the whole store atomically (temp file + rename).
func (s *LearningStore) flush() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	// Fix 2: clean up tmp file on any error path; harmless no-op after rename.
	defer os.Remove(tmp)

	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	for _, e := range s.entries {
		if err := enc.Encode(e); err != nil {
			f.Close()
			return err
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
