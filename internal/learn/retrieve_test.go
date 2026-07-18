package learn

import (
	"math"
	"path/filepath"
	"testing"
)

func TestCosine(t *testing.T) {
	cases := []struct {
		name string
		a, b []float32
		want float32
	}{
		{"identical", []float32{1, 0, 0}, []float32{1, 0, 0}, 1},
		{"orthogonal", []float32{1, 0}, []float32{0, 1}, 0},
		{"opposite", []float32{1, 1}, []float32{-1, -1}, -1},
		{"len-mismatch", []float32{1, 0}, []float32{1, 0, 0}, 0},
		{"zero-vector", []float32{0, 0}, []float32{1, 1}, 0},
		{"empty", nil, nil, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Cosine(c.a, c.b)
			if math.Abs(float64(got-c.want)) > 1e-6 {
				t.Fatalf("Cosine(%v,%v)=%v want %v", c.a, c.b, got, c.want)
			}
		})
	}
}

func TestTopRejectedAndBest(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.jsonl")
	s, err := OpenStore(p, 100)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	// Two rejected, one accepted. Accepted must never be returned.
	mustAppend(t, s, Learning{CommentID: "r1", Body: "near", Verdict: VerdictRejected, Embedding: []float32{1, 0}})
	mustAppend(t, s, Learning{CommentID: "r2", Body: "far", Verdict: VerdictRejected, Embedding: []float32{0, 1}})
	mustAppend(t, s, Learning{CommentID: "a1", Body: "accepted-near", Verdict: VerdictAccepted, Embedding: []float32{1, 0}})

	q := []float32{1, 0}
	top := s.TopRejected(q, 10)
	if len(top) != 2 {
		t.Fatalf("TopRejected len=%d want 2 (accepted excluded)", len(top))
	}
	if top[0].Learning.CommentID != "r1" {
		t.Fatalf("top[0]=%s want r1 (highest cosine)", top[0].Learning.CommentID)
	}
	if top[0].Score < top[1].Score {
		t.Fatalf("results not sorted desc: %v < %v", top[0].Score, top[1].Score)
	}

	best, ok := s.BestRejected(q)
	if !ok || best.Learning.CommentID != "r1" {
		t.Fatalf("BestRejected ok=%v id=%s want r1", ok, best.Learning.CommentID)
	}

	// Empty store: no rejected match.
	empty, _ := OpenStore(filepath.Join(t.TempDir(), "e.jsonl"), 100)
	if _, ok := empty.BestRejected(q); ok {
		t.Fatalf("BestRejected on empty store should be ok=false")
	}
}

func mustAppend(t *testing.T, s *LearningStore, l Learning) {
	t.Helper()
	if _, err := s.Append(l); err != nil {
		t.Fatalf("Append %s: %v", l.CommentID, err)
	}
}
