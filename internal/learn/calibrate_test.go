package learn

import (
	"math"
	"path/filepath"
	"testing"
)

func TestCalibrateNeedsTwoRejected(t *testing.T) {
	s, _ := OpenStore(filepath.Join(t.TempDir(), "s.jsonl"), 100)
	if _, ok := s.Calibrate(); ok {
		t.Fatal("empty store should not calibrate")
	}
	mustAppend(t, s, Learning{CommentID: "r1", Verdict: VerdictRejected, Embedding: []float32{1, 0}})
	// Accepted entries are ignored, so still only one rejected → not enough.
	mustAppend(t, s, Learning{CommentID: "a1", Verdict: VerdictAccepted, Embedding: []float32{0, 1}})
	if _, ok := s.Calibrate(); ok {
		t.Fatal("one rejected learning should not calibrate")
	}
}

func TestCalibrateStats(t *testing.T) {
	s, _ := OpenStore(filepath.Join(t.TempDir(), "s.jsonl"), 100)
	// Three orthogonal-ish rejected vectors → all pairwise cosines 0.
	mustAppend(t, s, Learning{CommentID: "r1", Verdict: VerdictRejected, Embedding: []float32{1, 0}})
	mustAppend(t, s, Learning{CommentID: "r2", Verdict: VerdictRejected, Embedding: []float32{0, 1}})
	mustAppend(t, s, Learning{CommentID: "r3", Verdict: VerdictRejected, Embedding: []float32{0, 1}})

	st, ok := s.Calibrate()
	if !ok {
		t.Fatal("expected calibration with 3 rejected")
	}
	if st.Rejected != 3 || st.Pairs != 3 {
		t.Fatalf("Rejected=%d Pairs=%d want 3,3", st.Rejected, st.Pairs)
	}
	// r2==r3 → max cosine 1; r1 vs others → 0.
	if math.Abs(float64(st.Max-1)) > 1e-6 {
		t.Fatalf("Max=%v want ~1", st.Max)
	}
	if math.Abs(float64(st.Min)) > 1e-6 {
		t.Fatalf("Min=%v want ~0", st.Min)
	}
	// Suggested is clamped into [0.80, 0.97].
	if st.Suggested < 0.80 || st.Suggested > 0.97 {
		t.Fatalf("Suggested=%v out of clamp range", st.Suggested)
	}
}
