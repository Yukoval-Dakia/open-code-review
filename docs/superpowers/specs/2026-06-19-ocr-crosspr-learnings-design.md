# OCR Cross-PR Learnings — Design

**Date:** 2026-06-19
**Status:** Approved (design); implementation pending
**Repo / branch:** Yukoval-Dakia/open-code-review fork, `codex/claude-cli-provider`
**Builds on:** the `reviewctx.ContextProvider` hook added by the cross-reference impact feature (2026-06-19).

## Problem

OCR reviews each PR in isolation and forgets everything afterward. It cannot tell
that a kind of comment it keeps making is consistently dismissed by the team, nor
that a past suggestion was accepted. We want OCR to **learn from historical
feedback** — make fewer repeat mistakes, align with team preferences — without
migrating to a paid tool and while keeping data on our own side.

OCR runs in CI and exits when the review ends, so it cannot observe "how people
reacted after the comment was posted." Feedback must be **collected retroactively
on a later review**, from signals GitHub already records.

## Approach (chosen)

A per-review pipeline:

1. **Collect** (workflow layer): before OCR runs, a `github-script` step queries
   GitHub **GraphQL** for the resolve/reply state of OCR's prior inline review
   comments on the current PR, and writes the result to a JSON file OCR reads.
2. **Distill + store** (OCR binary): for each prior comment with a verdict, OCR
   forms a `Learning {comment text, file, symbol, verdict, embedding}` and appends
   it to a persistent local store. Embeddings come from BigModel.
3. **Retrieve** (OCR binary): for the file/changes under review, OCR embeds the
   change context and does local cosine similarity against the store to recall the
   most relevant past learnings.
4. **Inject** (OCR binary): a `LearningsProvider` (a `reviewctx.ContextProvider`)
   adds a "past review feedback" block to the MAIN_TASK prompt via the existing
   `{{extra_context}}` plumbing — **no new injection wiring**.

### Feedback signal interpretation (resolve/reply is a weak signal)
- `resolved` thread → **accepted** (developer dealt with it).
- `unresolved` and the comment is old (still unresolved on a later review) →
  **rejected (weak)** — likely ignored.
- a human (non-bot) `reply` containing disagreement → **rejected** (MVP: a small
  keyword check; richer reply parsing is a follow-up).
- Anything ambiguous → **skip** (do not store a noisy learning).

## Why not the alternatives
- **Aggregate-preference distillation** (LLM summarizes "rejected patterns" into a
  dynamic best_practices blob): cheaper to inject but loses the per-finding
  precision the user wants. Rejected.
- **Count/rule suppression**: lightest, but can only say "say less," not "say the
  right thing." Rejected.
- **Path/symbol-only retrieval** (no embeddings): cheaper and fully offline, but
  misses semantically-similar findings phrased differently. The user chose
  embedding retrieval for recall quality. Rejected for MVP (could be a fallback).
- **Local embedding model**: true air-gap, but deployment + Go bridging overhead on
  a Mac runner. Rejected: BigModel embedding adds no new data-egress surface
  because review already sends code to BigModel under the same key.

## Architecture

```
workflow (github-script, GraphQL)                OCR binary (Go)
─────────────────────────────────               ───────────────────────────────
review start
  │ query prior OCR inline comments'
  │ thread state on this PR
  ▼
  feedback.json  ───────────────────────────►  feedbackingest:
  [{comment_id, body, path, line,                read feedback.json → Learning{}
    verdict, ...}]                               embed new learnings (BigModel)
                                                 append to learningstore (local)
                                                       │
                                                       ▼
                                                 LearningsProvider (ContextProvider):
                                                   embed current change context
                                                   cosine top-k from store
                                                   render "past feedback" block
                                                       │
                                                       ▼
                                                 {{extra_context}} → MAIN_TASK prompt
```

## Components (new)

All Go, in the OCR binary, except the collector (workflow):

- `internal/learn/store.go` — `LearningStore`: persistent store + cosine top-k.
  - Storage: **JSON-lines file** under `~/.opencodereview/learnings/<repo-id>.jsonl`
    (zero deps, pure Go, data stays local, survives across runs, doesn't pollute
    the repo). Each line = one `Learning` with its embedding vector. Loaded into
    memory for cosine search. A soft cap (e.g. 5000 entries) with oldest-eviction
    keeps it bounded; eviction is logged, never silent.
- `internal/learn/embedder.go` — `Embedder`: BigModel embedding API client,
  reusing the resolved endpoint's **credentials (key)**. Note the embedding
  endpoint likely differs from the chat path (chat is `.../api/anthropic/v1/messages`;
  BigModel embeddings are OpenAI-style, probably `.../api/paas/v4/embeddings`), so
  the base URL/path is configured separately, not assumed equal to the chat URL.
  (Planning: confirm BigModel's embedding endpoint path/model id and I/O shape.)
- `internal/learn/ingest.go` — reads the workflow's `feedback.json`, turns
  verdicted comments into `Learning`s, embeds the new ones, appends to the store.
  Idempotent by `comment_id` (re-ingesting the same feedback is a no-op).
- `internal/learn/provider.go` — `LearningsProvider` implements
  `reviewctx.ContextProvider`: embeds the `FileReviewInput` change context,
  retrieves top-k similar learnings above a similarity threshold, renders the block.
- Collector (workflow): a `github-script` step in `ocr-codex-review.yml` that runs
  the GraphQL query and writes `feedback.json`. Lives in the-learning-project's
  workflow, not the OCR repo.

### Data shapes

```go
// internal/learn
type Verdict string // "accepted" | "rejected"

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

`feedback.json` (workflow → OCR):
```json
[{ "comment_id": "...", "body": "...", "path": "src/x.ts", "line": 42,
   "verdict": "accepted" }]
```
The workflow computes `verdict` from thread state per the rules above; OCR trusts
it (the GraphQL/state logic lives in one place).

## Configuration (env, OCR_* style)

- `OCR_LEARNINGS` = `on` (default) | `off`.
- `OCR_LEARNINGS_FEEDBACK` = path to `feedback.json` (set by the workflow); absent →
  skip ingestion, retrieval still runs against the existing store.
- `OCR_LEARNINGS_TOPK` (default 5), `OCR_LEARNINGS_MIN_SIM` (default 0.75).
- Embedding model/config read from the existing LLM endpoint config.

## Graceful degradation (must hold)
- No `feedback.json` / unreadable → skip ingestion; retrieval proceeds.
- Embedding API error (ingest or query) → skip that step; **review proceeds**,
  warning to stderr. Never fatal.
- Empty store / no match above threshold → provider returns "" (no block; the
  cross-ref empty-wrapper fix already makes "" leave no dangling tags).
- `OCR_LEARNINGS=off` → provider returns "".

## Phasing (two independently-shippable stages)

This is larger than cross-ref impact; split so each stage is verifiable alone.

- **Phase 1 — Collect + Store.** Workflow collector writes `feedback.json`; OCR
  ingests → embeds → persists to `LearningStore`. **No injection yet.** Verifiable:
  after a few PRs, the store fills with correctly-verdicted, embedded learnings.
- **Phase 2 — Retrieve + Inject.** `LearningsProvider` retrieves top-k and injects
  via `{{extra_context}}`. Verifiable: a review surfaces a relevant past learning.

Each phase gets its own implementation plan.

## Testing
- `LearningStore`: append/load round-trip; cosine ranking correctness (known
  vectors); soft-cap eviction (oldest dropped, logged); dedupe by `comment_id`.
- `embedder`: request/response mapping against a stubbed HTTP server; error → error
  (caller skips).
- `ingest`: `feedback.json` → store, idempotency, malformed entries skipped.
- `LearningsProvider`: with a seeded store + stub embedder, asserts top-k block
  rendering, the min-similarity gate, and `""` on no match / disabled / embed error.
- Collector (workflow github-script): the GraphQL query + verdict mapping checked
  with a JS syntax check + node-wrapped async check (as done for the inline change);
  full behavior is validated on a real PR.

## Scope (YAGNI)

**In scope:** resolve/reply signal (resolved/long-unresolved/reply-keyword);
per-comment learnings; BigModel embeddings; local JSON-lines store with cosine
top-k; `LearningsProvider` injection; env config; graceful degradation; two phases.

**Explicitly out of scope:**
- Aggregate/LLM-distilled preference summaries.
- "Was the suggested code actually applied" (diff-matching suggestion vs later
  commits) — a different, noisier signal source; not now.
- Rich NLP reply parsing (beyond a keyword check).
- Cross-runner shared store / vector DB / sqlite — local JSON-lines until scale
  demands otherwise.
- Local embedding model (the embedder interface leaves room to add it later).

## Open questions (resolve during planning)
- BigModel embedding endpoint: exact path, model id, request/response schema, and
  vector dimensionality.
- The GraphQL query for OCR's own inline comments + their thread resolve state, and
  the precise "long-unresolved → rejected" age threshold.
- Per-repo store id derivation (remote URL hash vs repo path).
