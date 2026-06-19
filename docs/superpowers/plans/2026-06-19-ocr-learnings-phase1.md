# OCR Cross-PR Learnings — Phase 1 (Collect + Store) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Persist OCR's past review comments and their accepted/rejected verdicts as embedded "learnings" in a local store, fed by a workflow collector. No prompt injection yet (that is Phase 2).

**Architecture:** A `github-script` step in the-learning-project's workflow queries GraphQL for the resolve/reply state of OCR's prior inline comments on the current PR and writes `feedback.json`. The OCR binary, on review start, reads that file, embeds each new comment via BigModel, and appends it to a per-repo JSON-lines store under `~/.opencodereview/learnings/`. Ingestion is idempotent (dedupe by GitHub comment id) and fully best-effort (any failure skips ingestion; the review proceeds).

**Tech Stack:** Go (stdlib only, no CGO), BigModel embedding API (`embedding-3`, 2048-dim, OpenAI-style), GitHub Actions `github-script` + GraphQL.

## Global Constraints

- **No CGO.** Pure Go stdlib only (`net/http`, `encoding/json`, `os`, `bufio`, `crypto/sha256`). Verify with `CGO_ENABLED=0 go build ./...`.
- **Graceful degradation, never fatal.** Missing/unreadable `feedback.json`, embedding API errors, or store I/O errors must skip ingestion and let the review proceed; emit a `[ocr]` warning to stderr.
- **No silent truncation.** Soft-cap eviction in the store must log how many entries were dropped.
- **Idempotent ingestion.** Re-ingesting the same `feedback.json` (same comment ids) must be a no-op.
- **Module path:** `github.com/open-code-review/open-code-review`. **Branch:** `codex/claude-cli-provider`.
- **Test command:** `go test ./internal/learn/...` per task; `go test ./...` + `CGO_ENABLED=0 go build ./...` before final commit.
- **BigModel embedding (probed, confirmed):** `POST https://open.bigmodel.cn/api/paas/v4/embeddings`, header `Authorization: Bearer <token>`, request `{"model":"embedding-3","input":"<text>"}`, response `{"data":[{"embedding":[<float>...]}],"model":"...","usage":{...}}`, vector dim 2048. The embedding endpoint differs from the chat endpoint (`.../api/anthropic/v1/messages`) — configure its URL separately.

---

## File Structure

- `internal/learn/types.go` — `Verdict`, `Learning` (shared types; no logic).
- `internal/learn/store.go` — `LearningStore`: JSON-lines persistence, dedupe, soft-cap eviction. (Cosine TopK retrieval is **Phase 2** — not in this plan.)
- `internal/learn/embedder.go` — `Embedder` interface + `BigModelEmbedder` HTTP client.
- `internal/learn/ingest.go` — `Ingest`: read `feedback.json` → embed new → append to store.
- `internal/learn/config.go` — `LearningsConfig` from env (`OCR_LEARNINGS`, `OCR_LEARNINGS_FEEDBACK`, `OCR_EMBED_URL`, `OCR_EMBED_MODEL`), `RepoStorePath` helper.
- `cmd/opencodereview/review_cmd.go` — wire a best-effort ingest call before `agent.New(...)`.
- `the-learning-project/.github/workflows/ocr-codex-review.yml` — add a `github-script` collector step (separate repo).

---

## Task 1: Learning types + JSON-lines store

**Files:**
- Create: `internal/learn/types.go`
- Create: `internal/learn/store.go`
- Test: `internal/learn/store_test.go`

**Interfaces:**
- Produces:
  - `type Verdict string` with consts `VerdictAccepted Verdict = "accepted"`, `VerdictRejected Verdict = "rejected"`.
  - `type Learning struct { CommentID, Body, Path, Symbol string; Verdict Verdict; Embedding []float32; CreatedAt string }` (JSON tags as in spec).
  - `type LearningStore struct { path string; entries []Learning; index map[string]int; cap int }`
  - `func OpenStore(path string, softCap int) (*LearningStore, error)` — loads existing JSON-lines (missing file is OK → empty store).
  - `func (s *LearningStore) Has(commentID string) bool`
  - `func (s *LearningStore) Append(l Learning) (added bool, err error)` — dedupe by CommentID (no-op if present); persist; evict oldest beyond cap (logged to stderr).
  - `func (s *LearningStore) Len() int`

- [ ] **Step 1: Write the failing test**

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/learn/ -run TestStore -v`
Expected: FAIL — `undefined: OpenStore` / `undefined: Learning`.

- [ ] **Step 3: Write `internal/learn/types.go`**

```go
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
```

- [ ] **Step 4: Write `internal/learn/store.go`**

```go
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

// Append adds a learning (no-op if its CommentID already exists), evicts the
// oldest entries beyond the soft cap, and rewrites the file. Returns whether a
// new entry was added.
func (s *LearningStore) Append(l Learning) (bool, error) {
	if l.CommentID != "" && s.Has(l.CommentID) {
		return false, nil
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
	if err := s.flush(); err != nil {
		return true, err
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
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/learn/ -run TestStore -v && go test ./internal/learn/ -run TestOpenStore -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/learn/types.go internal/learn/store.go internal/learn/store_test.go
git commit -m "feat(learn): Learning types + JSON-lines store (dedupe, soft-cap)"
```

---

## Task 2: BigModel embedder

**Files:**
- Create: `internal/learn/embedder.go`
- Test: `internal/learn/embedder_test.go`

**Interfaces:**
- Produces:
  - `type Embedder interface { Embed(ctx context.Context, text string) ([]float32, error) }`
  - `type BigModelEmbedder struct { URL, Token, Model string; HTTP *http.Client }`
  - `func NewBigModelEmbedder(url, token, model string) *BigModelEmbedder`
  - `func (e *BigModelEmbedder) Embed(ctx context.Context, text string) ([]float32, error)`

- [ ] **Step 1: Write the failing test**

```go
package learn

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBigModelEmbedderEmbed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tok123" {
			t.Errorf("Authorization = %q, want Bearer tok123", got)
		}
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		if req["model"] != "embedding-3" {
			t.Errorf("model = %v, want embedding-3", req["model"])
		}
		if req["input"] != "hello" {
			t.Errorf("input = %v, want hello", req["input"])
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"data":[{"embedding":[0.1,0.2,0.3]}],"model":"embedding-3"}`)
	}))
	defer srv.Close()

	e := NewBigModelEmbedder(srv.URL, "tok123", "embedding-3")
	got, err := e.Embed(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(got) != 3 || got[0] != 0.1 || got[2] != 0.3 {
		t.Fatalf("embedding = %v, want [0.1 0.2 0.3]", got)
	}
}

func TestBigModelEmbedderHTTPErrorIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `{"error":{"message":"boom"}}`)
	}))
	defer srv.Close()
	e := NewBigModelEmbedder(srv.URL, "t", "embedding-3")
	if _, err := e.Embed(context.Background(), "x"); err == nil {
		t.Fatal("expected error on 500")
	} else if !strings.Contains(err.Error(), "500") {
		t.Fatalf("error should mention status: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/learn/ -run TestBigModelEmbedder -v`
Expected: FAIL — `undefined: NewBigModelEmbedder`.

- [ ] **Step 3: Write `internal/learn/embedder.go`**

```go
package learn

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Embedder turns text into a vector. Implemented by BigModelEmbedder; stubbed in
// tests and (Phase 2) in the provider.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// BigModelEmbedder calls BigModel's OpenAI-style embeddings endpoint.
type BigModelEmbedder struct {
	URL   string
	Token string
	Model string
	HTTP  *http.Client
}

// NewBigModelEmbedder builds an embedder. url is the full embeddings endpoint
// (e.g. https://open.bigmodel.cn/api/paas/v4/embeddings).
func NewBigModelEmbedder(url, token, model string) *BigModelEmbedder {
	return &BigModelEmbedder{
		URL:   url,
		Token: token,
		Model: model,
		HTTP:  &http.Client{Timeout: 30 * time.Second},
	}
}

type embedRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

type embedResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// Embed returns the embedding vector for text. Any non-2xx status or transport
// error is returned as an error so callers can skip gracefully.
func (e *BigModelEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	body, err := json.Marshal(embedRequest{Model: e.Model, Input: text})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+e.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("embedding API status %d: %s", resp.StatusCode, string(raw))
	}
	var parsed embedResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("decode embedding response: %w", err)
	}
	if len(parsed.Data) == 0 || len(parsed.Data[0].Embedding) == 0 {
		return nil, fmt.Errorf("embedding response had no vector")
	}
	return parsed.Data[0].Embedding, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/learn/ -run TestBigModelEmbedder -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/learn/embedder.go internal/learn/embedder_test.go
git commit -m "feat(learn): BigModel embeddings client"
```

---

## Task 3: Ingest feedback.json into the store

**Files:**
- Create: `internal/learn/ingest.go`
- Test: `internal/learn/ingest_test.go`

**Interfaces:**
- Consumes: `LearningStore` (Task 1), `Embedder` (Task 2).
- Produces:
  - `type FeedbackItem struct { CommentID, Body, Path, Symbol string; Verdict Verdict }` (JSON tags: `comment_id,body,path,symbol,verdict`).
  - `func Ingest(ctx context.Context, store *LearningStore, emb Embedder, feedbackPath, now string) (added int, err error)` — reads the JSON array at feedbackPath; for each item not already in the store and with a valid verdict, embeds Body and appends. Malformed/invalid items are skipped (not fatal). `now` is the CreatedAt stamp (caller supplies; keeps the func deterministic for tests).

- [ ] **Step 1: Write the failing test**

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/learn/ -run TestIngest -v`
Expected: FAIL — `undefined: Ingest`.

- [ ] **Step 3: Write `internal/learn/ingest.go`**

```go
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/learn/ -run TestIngest -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/learn/ingest.go internal/learn/ingest_test.go
git commit -m "feat(learn): ingest feedback.json into the store (idempotent, best-effort)"
```

---

## Task 4: Config + wiring into review_cmd

**Files:**
- Create: `internal/learn/config.go`
- Test: `internal/learn/config_test.go`
- Modify: `cmd/opencodereview/review_cmd.go` (add a best-effort ingest call before `agent.New(...)`, ~line 120)

**Interfaces:**
- Consumes: `OpenStore`, `NewBigModelEmbedder`, `Ingest`.
- Produces:
  - `type LearningsConfig struct { Enabled bool; FeedbackPath, EmbedURL, EmbedModel string }`
  - `func LoadConfig() LearningsConfig` — from env. Defaults: `Enabled` true unless `OCR_LEARNINGS=off`; `FeedbackPath`=`OCR_LEARNINGS_FEEDBACK`; `EmbedURL`=`OCR_EMBED_URL` or `https://open.bigmodel.cn/api/paas/v4/embeddings`; `EmbedModel`=`OCR_EMBED_MODEL` or `embedding-3`.
  - `func RepoStorePath(remoteURL string) (string, error)` — `~/.opencodereview/learnings/<sha256(remoteURL)[:16]>.jsonl`.
  - `const DefaultSoftCap = 5000`.

- [ ] **Step 1: Write the failing test**

```go
package learn

import (
	"strings"
	"testing"
)

func TestLoadConfigDefaults(t *testing.T) {
	t.Setenv("OCR_LEARNINGS", "")
	t.Setenv("OCR_LEARNINGS_FEEDBACK", "")
	t.Setenv("OCR_EMBED_URL", "")
	t.Setenv("OCR_EMBED_MODEL", "")
	c := LoadConfig()
	if !c.Enabled {
		t.Fatal("Enabled should default true")
	}
	if c.EmbedURL != "https://open.bigmodel.cn/api/paas/v4/embeddings" {
		t.Fatalf("EmbedURL default wrong: %s", c.EmbedURL)
	}
	if c.EmbedModel != "embedding-3" {
		t.Fatalf("EmbedModel default wrong: %s", c.EmbedModel)
	}
}

func TestLoadConfigOffAndOverrides(t *testing.T) {
	t.Setenv("OCR_LEARNINGS", "off")
	t.Setenv("OCR_EMBED_MODEL", "embedding-2")
	c := LoadConfig()
	if c.Enabled {
		t.Fatal("OCR_LEARNINGS=off should disable")
	}
	if c.EmbedModel != "embedding-2" {
		t.Fatalf("override ignored: %s", c.EmbedModel)
	}
}

func TestRepoStorePathStableAndScoped(t *testing.T) {
	a, err := RepoStorePath("https://github.com/me/repo.git")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := RepoStorePath("https://github.com/me/repo.git")
	if a != b {
		t.Fatal("same remote must map to same path")
	}
	c, _ := RepoStorePath("https://github.com/me/other.git")
	if a == c {
		t.Fatal("different remotes must map to different paths")
	}
	if !strings.HasSuffix(a, ".jsonl") || !strings.Contains(a, "learnings") {
		t.Fatalf("unexpected path: %s", a)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/learn/ -run "TestLoadConfig|TestRepoStorePath" -v`
Expected: FAIL — `undefined: LoadConfig`.

- [ ] **Step 3: Write `internal/learn/config.go`**

```go
package learn

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
)

const DefaultSoftCap = 5000

// LearningsConfig is the env-derived configuration for the learnings subsystem.
type LearningsConfig struct {
	Enabled      bool
	FeedbackPath string
	EmbedURL     string
	EmbedModel   string
}

// LoadConfig reads OCR_LEARNINGS* / OCR_EMBED_* env vars.
func LoadConfig() LearningsConfig {
	c := LearningsConfig{
		Enabled:      !strings.EqualFold(strings.TrimSpace(os.Getenv("OCR_LEARNINGS")), "off"),
		FeedbackPath: os.Getenv("OCR_LEARNINGS_FEEDBACK"),
		EmbedURL:     os.Getenv("OCR_EMBED_URL"),
		EmbedModel:   os.Getenv("OCR_EMBED_MODEL"),
	}
	if c.EmbedURL == "" {
		c.EmbedURL = "https://open.bigmodel.cn/api/paas/v4/embeddings"
	}
	if c.EmbedModel == "" {
		c.EmbedModel = "embedding-3"
	}
	return c
}

// RepoStorePath maps a repo (by its remote URL) to a stable per-repo store file
// under ~/.opencodereview/learnings/. Falls back to a literal key if the URL is
// empty (caller should pass repoDir in that case).
func RepoStorePath(remoteURL string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(remoteURL)))
	id := hex.EncodeToString(sum[:])[:16]
	return filepath.Join(home, ".opencodereview", "learnings", id+".jsonl"), nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/learn/ -run "TestLoadConfig|TestRepoStorePath" -v`
Expected: PASS.

- [ ] **Step 5: Wire into `cmd/opencodereview/review_cmd.go`**

Add this helper function at the end of the file (it owns all error handling so the call site stays one line):

```go
// runLearningsIngest ingests PR feedback (if configured) into the local store.
// Best-effort: every failure path warns and returns without affecting the review.
func runLearningsIngest(ctx context.Context, repoDir, token string, gitRunner *gitcmd.Runner) {
	cfg := learn.LoadConfig()
	if !cfg.Enabled || cfg.FeedbackPath == "" {
		return // disabled, or no feedback file supplied by the workflow
	}
	if token == "" {
		fmt.Fprintln(os.Stderr, "[ocr] learnings: no LLM token; skipping ingestion")
		return
	}
	remote, _ := gitRunner.Run(ctx, repoDir, "remote", "get-url", "origin")
	remote = strings.TrimSpace(remote)
	if remote == "" {
		remote = repoDir // fall back to repo path as the store key
	}
	storePath, err := learn.RepoStorePath(remote)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[ocr] learnings: store path: %v (skipped)\n", err)
		return
	}
	store, err := learn.OpenStore(storePath, learn.DefaultSoftCap)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[ocr] learnings: open store: %v (skipped)\n", err)
		return
	}
	emb := learn.NewBigModelEmbedder(cfg.EmbedURL, token, cfg.EmbedModel)
	added, err := learn.Ingest(ctx, store, emb, cfg.FeedbackPath, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		fmt.Fprintf(os.Stderr, "[ocr] learnings: ingest: %v\n", err)
		return
	}
	fmt.Fprintf(os.Stderr, "[ocr] learnings: ingested %d new feedback item(s); store now has %d\n", added, store.Len())
}
```

Add the import for the learn package to `review_cmd.go`'s import block:

```go
	"github.com/open-code-review/open-code-review/internal/learn"
```

Insert the call just before `ag := agent.New(agent.Args{` (so `repoDir`, `ep`, and `gitRunner` are all in scope). Use a fresh background context for ingestion (it is independent of the review span):

```go
	runLearningsIngest(context.Background(), repoDir, ep.Token, gitRunner)

	ag := agent.New(agent.Args{
```

(Note: `context`, `fmt`, `os`, `strings`, `time`, and `gitcmd` are already imported in `review_cmd.go`.)

- [ ] **Step 6: Build, vet, full test**

Run:
```bash
go build ./... && go vet ./internal/learn/... ./cmd/opencodereview/... && go test ./internal/learn/... ./cmd/opencodereview/...
CGO_ENABLED=0 go build ./...
```
Expected: all pass; CGO build clean.

- [ ] **Step 7: Commit**

```bash
git add internal/learn/config.go internal/learn/config_test.go cmd/opencodereview/review_cmd.go
git commit -m "feat(learn): env config + best-effort ingest wired into review"
```

---

## Task 5: Workflow collector (the-learning-project)

**Files:**
- Modify: `/Users/yukoval/yukoval-projects/the-learning-project/.github/workflows/ocr-codex-review.yml`

This task is in a **different repo** (the-learning-project). It adds a `github-script` step that runs BEFORE the "Run OCR review" step, queries GraphQL for the resolve/reply state of OCR's own prior inline comments on this PR, writes `feedback.json`, and exports `OCR_LEARNINGS_FEEDBACK`.

**Verdict rules (must match the spec):**
- thread `isResolved == true` → `accepted`.
- thread unresolved AND the comment's `createdAt` is older than 7 days → `rejected` (likely ignored).
- a reply authored by a non-bot whose body matches a disagreement keyword (`/\b(no|wrong|disagree|incorrect|not (right|true)|nah|invalid)\b/i`) → `rejected`.
- otherwise → skip (omit from feedback.json).

"OCR's own comments" are identified by the bot author login of the GITHUB_TOKEN used to post them (the workflow's actor) AND the body marker prefix `**OCR**` used by the inline renderer.

- [ ] **Step 1: Add the collector step**

Insert this step immediately before the `- name: Run OCR review` step:

```yaml
      - name: Collect prior-comment feedback (learnings)
        if: ${{ vars.OCR_LEARNINGS != 'off' }}
        uses: actions/github-script@v7
        env:
          PR_NUMBER: ${{ github.event.pull_request.number }}
        with:
          script: |
            const fs = require('fs');
            const prNumber = Number(process.env.PR_NUMBER);
            const DISAGREE = /\b(no|wrong|disagree|incorrect|not (right|true)|nah|invalid)\b/i;
            const STALE_DAYS = 7;
            const now = Date.now();

            // Page through this PR's review threads via GraphQL (isResolved lives here).
            const query = `query($owner:String!,$repo:String!,$pr:Int!,$cursor:String){
              repository(owner:$owner,name:$repo){
                pullRequest(number:$pr){
                  reviewThreads(first:50, after:$cursor){
                    pageInfo{ hasNextPage endCursor }
                    nodes{
                      isResolved
                      comments(first:50){
                        nodes{
                          id body path createdAt
                          author{ login __typename }
                        }
                      }
                    }
                  }
                }
              }
            }`;

            const out = [];
            let cursor = null;
            do {
              const data = await github.graphql(query, {
                owner: context.repo.owner, repo: context.repo.repo, pr: prNumber, cursor,
              });
              const threads = data.repository.pullRequest.reviewThreads;
              for (const th of threads.nodes) {
                const cs = th.comments.nodes;
                if (cs.length === 0) continue;
                // The first comment in a thread is the original review comment.
                const head = cs[0];
                const isOCR = head.body && head.body.startsWith('**OCR**');
                if (!isOCR) continue;

                let verdict = null;
                if (th.isResolved) {
                  verdict = 'accepted';
                } else {
                  // Any human reply expressing disagreement => rejected.
                  const humanDisagree = cs.slice(1).some(c =>
                    c.author && c.author.__typename === 'User' && DISAGREE.test(c.body || ''));
                  if (humanDisagree) {
                    verdict = 'rejected';
                  } else {
                    const ageDays = (now - Date.parse(head.createdAt)) / 86400000;
                    if (ageDays > STALE_DAYS) verdict = 'rejected';
                  }
                }
                if (!verdict) continue; // ambiguous -> skip

                out.push({
                  comment_id: head.id,
                  body: head.body,
                  path: head.path || '',
                  verdict,
                });
              }
              cursor = threads.pageInfo.hasNextPage ? threads.pageInfo.endCursor : null;
            } while (cursor);

            const path = `${process.env.RUNNER_TEMP || '.'}/ocr-feedback.json`;
            fs.writeFileSync(path, JSON.stringify(out));
            core.exportVariable('OCR_LEARNINGS_FEEDBACK', path);
            core.info(`OCR learnings: wrote ${out.length} verdicted feedback item(s) to ${path}`);
```

- [ ] **Step 2: Validate YAML + JS syntax**

Run:
```bash
cd /Users/yukoval/yukoval-projects/the-learning-project
ruby -ryaml -e "YAML.load_file('.github/workflows/ocr-codex-review.yml'); puts 'YAML ok'"
ruby -ryaml -e "y=YAML.load_file('.github/workflows/ocr-codex-review.yml'); s=y['jobs']['review']['steps'].find{|x| x['name']=='Collect prior-comment feedback (learnings)'}; File.write('/tmp/collect.js', s['with']['script'])"
{ echo 'async function __w(github, context, core, require){'; cat /tmp/collect.js; echo '}'; } > /tmp/collect_wrapped.js
node --check /tmp/collect_wrapped.js && echo 'JS ok'
```
Expected: `YAML ok` and `JS ok`.

- [ ] **Step 3: Commit (the-learning-project)**

```bash
cd /Users/yukoval/yukoval-projects/the-learning-project
git add .github/workflows/ocr-codex-review.yml
git commit -m "ci(ocr): collect prior-comment resolve/reply feedback for learnings (phase 1)"
```

(Push handled at execution time per the user's branch workflow. The OCR binary side must also be rebuilt — `go build -o ~/.local/bin/ocr ./cmd/opencodereview` — so the runner picks up the ingest wiring.)

---

## Final verification (after all tasks)

- [ ] `go test ./...` — all green.
- [ ] `CGO_ENABLED=0 go build ./...` — no CGO.
- [ ] `go build -o ~/.local/bin/ocr ./cmd/opencodereview` — rebuild the binary the runner uses.
- [ ] Whole-branch review (per subagent-driven-development): focus on graceful degradation (no path makes a review fail), idempotency, and that nothing is injected into the prompt yet (Phase 1 is collect+store only).
- [ ] Phase-1 acceptance: after a few real PRs, `~/.opencodereview/learnings/<id>.jsonl` accumulates entries with correct `verdict` and 2048-dim embeddings; the stderr line `[ocr] learnings: ingested N ...` appears in the run log.

## Phase 2 preview (NOT this plan)
`internal/learn/store.go` gains cosine `TopK`; `internal/learn/provider.go` implements `reviewctx.ContextProvider` (embeds the change context, retrieves top-k above `OCR_LEARNINGS_MIN_SIM`, renders a "past review feedback" block); registered alongside the cross-ref provider so it flows through `{{extra_context}}`.
