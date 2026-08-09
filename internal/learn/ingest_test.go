// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package learn

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

type stubEmbedder struct {
	calls int
	vec   []float32
	err   error
}

func (s *stubEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	s.calls++
	return s.vec, s.err
}

func writeFeedback(t *testing.T, dir, content string) string {
	t.Helper()
	p := filepath.Join(dir, "feedback.json")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestIngestEmbedsAndStoresNewItemsThenIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	store, _ := OpenStore(filepath.Join(dir, "s.jsonl"), 100)
	emb := &stubEmbedder{vec: []float32{1, 0}}
	fp := writeFeedback(t, dir, `[
	  {"comment_id":"c1","body":"avoid X","path":"a.go","verdict":"rejected"},
	  {"comment_id":"c2","body":"good catch","path":"b.go","verdict":"accepted"}
	]`)

	added, err := Ingest(context.Background(), store, emb, fp, "t0")
	if err != nil || added != 2 {
		t.Fatalf("first ingest: added=%d err=%v", added, err)
	}
	if store.Len() != 2 || emb.calls != 2 {
		t.Fatalf("store.Len=%d emb.calls=%d, want 2/2", store.Len(), emb.calls)
	}
	if !store.Has("c1") || store.entries[0].Verdict != VerdictRejected || len(store.entries[0].Embedding) != 2 {
		t.Fatalf("stored entry wrong: %+v", store.entries[0])
	}

	// Re-ingest the same file: idempotent, no new embeds.
	added, err = Ingest(context.Background(), store, emb, fp, "t1")
	if err != nil || added != 0 {
		t.Fatalf("re-ingest: added=%d err=%v, want 0", added, err)
	}
	if emb.calls != 2 {
		t.Fatalf("idempotent ingest must not re-embed: calls=%d", emb.calls)
	}
}

func TestIngestSkipsMalformedAndInvalidVerdict(t *testing.T) {
	dir := t.TempDir()
	store, _ := OpenStore(filepath.Join(dir, "s.jsonl"), 100)
	emb := &stubEmbedder{vec: []float32{1}}
	fp := writeFeedback(t, dir, `[
	  {"comment_id":"ok","body":"b","path":"a.go","verdict":"accepted"},
	  {"comment_id":"noverdict","body":"b","path":"a.go","verdict":"maybe"},
	  {"comment_id":"nobody","path":"a.go","verdict":"accepted"}
	]`)
	added, err := Ingest(context.Background(), store, emb, fp, "t0")
	if err != nil {
		t.Fatalf("ingest err: %v", err)
	}
	if added != 1 || !store.Has("ok") {
		t.Fatalf("only the valid item should ingest: added=%d", added)
	}
}

func TestIngestMissingFileIsNoError(t *testing.T) {
	store, _ := OpenStore(filepath.Join(t.TempDir(), "s.jsonl"), 100)
	added, err := Ingest(context.Background(), store, &stubEmbedder{}, filepath.Join(t.TempDir(), "nope.json"), "t0")
	if err != nil || added != 0 {
		t.Fatalf("missing feedback should be a clean no-op: added=%d err=%v", added, err)
	}
}

func TestIngestUpdatesFinalVerdictWithoutReembeddingUnchangedBody(t *testing.T) {
	dir := t.TempDir()
	store, _ := OpenStore(filepath.Join(dir, "s.jsonl"), 100)
	emb := &stubEmbedder{vec: []float32{1, 0}}
	fp := writeFeedback(t, dir, `[{"comment_id":"c1","body":"possible race","path":"a.go","verdict":"rejected"}]`)
	if changed, err := Ingest(context.Background(), store, emb, fp, "t0"); err != nil || changed != 1 {
		t.Fatalf("initial ingest: changed=%d err=%v", changed, err)
	}
	fp = writeFeedback(t, dir, `[{"comment_id":"c1","body":"possible race","path":"a.go","verdict":"accepted"}]`)
	if changed, err := Ingest(context.Background(), store, emb, fp, "t1"); err != nil || changed != 1 {
		t.Fatalf("final ingest: changed=%d err=%v", changed, err)
	}
	got, ok := store.Get("c1")
	if !ok || got.Verdict != VerdictAccepted || got.CreatedAt != "t0" {
		t.Fatalf("updated learning = %+v, want accepted with original timestamp", got)
	}
	if emb.calls != 1 {
		t.Fatalf("verdict-only update re-embedded body: calls=%d", emb.calls)
	}
}
