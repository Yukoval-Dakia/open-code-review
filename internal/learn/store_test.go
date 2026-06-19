package learn

import (
	"path/filepath"
	"testing"
)

func tmpStorePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "store.jsonl")
}

func TestStoreAppendLoadRoundTripAndDedupe(t *testing.T) {
	p := tmpStorePath(t)
	s, err := OpenStore(p, 100)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	added, err := s.Append(Learning{CommentID: "c1", Body: "b1", Path: "a.go", Verdict: VerdictAccepted, Embedding: []float32{0.1, 0.2}, CreatedAt: "t1"})
	if err != nil || !added {
		t.Fatalf("first append: added=%v err=%v", added, err)
	}
	// Dedupe: same CommentID is a no-op.
	added, err = s.Append(Learning{CommentID: "c1", Body: "dup", Verdict: VerdictRejected})
	if err != nil || added {
		t.Fatalf("dup append should be no-op: added=%v err=%v", added, err)
	}
	if s.Len() != 1 {
		t.Fatalf("Len = %d, want 1", s.Len())
	}
	// Reload from disk: entry survives, Has works.
	s2, err := OpenStore(p, 100)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if !s2.Has("c1") {
		t.Fatalf("reloaded store missing c1")
	}
	if got := s2.entries[0]; got.Body != "b1" || got.Verdict != VerdictAccepted || len(got.Embedding) != 2 {
		t.Fatalf("reloaded entry mismatch: %+v", got)
	}
}

func TestStoreSoftCapEvictsOldest(t *testing.T) {
	p := tmpStorePath(t)
	s, _ := OpenStore(p, 2)
	for _, id := range []string{"c1", "c2", "c3"} {
		if _, err := s.Append(Learning{CommentID: id, Body: id}); err != nil {
			t.Fatalf("append %s: %v", id, err)
		}
	}
	if s.Len() != 2 {
		t.Fatalf("Len = %d, want 2 (cap)", s.Len())
	}
	if s.Has("c1") {
		t.Fatalf("oldest c1 should have been evicted")
	}
	if !s.Has("c2") || !s.Has("c3") {
		t.Fatalf("c2/c3 should remain")
	}
	// Eviction must survive a reload (file rewritten).
	s2, _ := OpenStore(p, 2)
	if s2.Has("c1") || !s2.Has("c3") {
		t.Fatalf("reloaded store should reflect eviction")
	}
}

func TestOpenStoreMissingFileIsEmpty(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "nope.jsonl"), 10)
	if err != nil {
		t.Fatalf("missing file should be OK: %v", err)
	}
	if s.Len() != 0 {
		t.Fatalf("Len = %d, want 0", s.Len())
	}
}
