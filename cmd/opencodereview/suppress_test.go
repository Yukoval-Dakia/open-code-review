package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/open-code-review/open-code-review/internal/learn"
	"github.com/open-code-review/open-code-review/internal/model"
)

// fakeEmbedder returns a canned vector per text; unknown text errors.
type fakeEmbedder struct {
	vecs map[string][]float32
	err  error
}

func (f fakeEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	if f.err != nil {
		return nil, f.err
	}
	v, ok := f.vecs[text]
	if !ok {
		return nil, errors.New("no vec")
	}
	return v, nil
}

func newStore(t *testing.T, ls ...learn.Learning) *learn.LearningStore {
	t.Helper()
	s, err := learn.OpenStore(filepath.Join(t.TempDir(), "s.jsonl"), 100)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	for _, l := range ls {
		if _, err := s.Append(l); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	return s
}

func TestReflagSuppressorDropsRepeat(t *testing.T) {
	store := newStore(t, learn.Learning{
		CommentID: "r1", Body: "nil deref", Path: "a.go",
		Verdict: learn.VerdictRejected, Embedding: []float32{1, 0},
	})
	emb := fakeEmbedder{vecs: map[string][]float32{
		"repeat of nil deref": {1, 0}, // identical → cosine 1, suppressed
		"a brand new finding": {0, 1}, // orthogonal → kept
	}}
	s := newReflagSuppressor(true, emb, store)
	if !s.enabled {
		t.Fatal("expected suppressor enabled (store has rejected)")
	}
	comments := []model.LlmComment{
		{Path: "a.go", Content: "repeat of nil deref", StartLine: 1},
		{Path: "a.go", Content: "a brand new finding", StartLine: 2},
	}
	kept, dropped := s.apply(context.Background(), comments)
	if dropped != 1 || len(kept) != 1 || kept[0].Content != "a brand new finding" {
		t.Fatalf("dropped=%d kept=%v want 1 dropped, new finding kept", dropped, kept)
	}
}

func TestReflagSuppressorPathGated(t *testing.T) {
	store := newStore(t, learn.Learning{
		CommentID: "r1", Body: "x", Path: "a.go",
		Verdict: learn.VerdictRejected, Embedding: []float32{1, 0},
	})
	emb := fakeEmbedder{vecs: map[string][]float32{"same text": {1, 0}}}
	s := newReflagSuppressor(true, emb, store)
	// Identical embedding but on a different file → must be kept.
	kept, dropped := s.apply(context.Background(), []model.LlmComment{
		{Path: "b.go", Content: "same text"},
	})
	if dropped != 0 || len(kept) != 1 {
		t.Fatalf("cross-file should not suppress: dropped=%d", dropped)
	}
}

func TestReflagSuppressorFailsOpen(t *testing.T) {
	store := newStore(t, learn.Learning{
		CommentID: "r1", Body: "x", Path: "a.go",
		Verdict: learn.VerdictRejected, Embedding: []float32{1, 0},
	})
	s := newReflagSuppressor(true, fakeEmbedder{err: errors.New("boom")}, store)
	kept, dropped := s.apply(context.Background(), []model.LlmComment{{Path: "a.go", Content: "y"}})
	if dropped != 0 || len(kept) != 1 {
		t.Fatalf("embed error must keep the comment: dropped=%d", dropped)
	}
}

func TestReflagSuppressorDisabledWhenNoRejected(t *testing.T) {
	store := newStore(t, learn.Learning{
		CommentID: "a1", Body: "x", Verdict: learn.VerdictAccepted, Embedding: []float32{1, 0},
	})
	s := newReflagSuppressor(true, fakeEmbedder{}, store)
	if s.enabled {
		t.Fatal("no rejected learnings → suppressor should be disabled")
	}
	comments := []model.LlmComment{{Content: "anything"}}
	kept, dropped := s.apply(context.Background(), comments)
	if dropped != 0 || len(kept) != 1 {
		t.Fatalf("disabled suppressor must pass through: dropped=%d", dropped)
	}
}

func TestReflagSuppressorEnvOff(t *testing.T) {
	store := newStore(t, learn.Learning{
		CommentID: "r1", Body: "x", Verdict: learn.VerdictRejected, Embedding: []float32{1, 0},
	})
	t.Setenv("OCR_REFLAG_SUPPRESS", "off")
	s := newReflagSuppressor(true, fakeEmbedder{}, store)
	if s.enabled {
		t.Fatal("OCR_REFLAG_SUPPRESS=off must disable")
	}
}
