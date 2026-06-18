# OCR Cross-Reference Impact Context — Design

**Date:** 2026-06-19
**Status:** Approved (design); implementation pending
**Repo / branch:** Yukoval-Dakia/open-code-review fork, `codex/claude-cli-provider`

## Problem

OCR is an agentic reviewer: the model can call `file_read` / `code_search` /
`file_read_diff` to pull cross-file context on demand. But this is *pull-based* —
the model often does not realize it should look at a changed symbol's callers, so
it misses cross-file breakage (a changed function signature breaking its callers,
a changed exported type breaking dependents). Cross-file *impact* is under-covered.

**Goal:** give OCR reliable cross-file impact awareness. For the symbols changed
in a file, automatically tell the model where those symbols are used elsewhere, so
it can check those references for breakage — without migrating to a paid tool
(Greptile / CodeRabbit) and without a heavy whole-repo semantic index.

## Approach (chosen)

A deterministic, per-file pre-pass that runs **before** the model reviews a file:

1. Parse the changed file to find the **definitions** (functions, methods,
   classes, interfaces, types, enums, exports) whose line range overlaps the
   diff's changed lines → the file's **changed symbols**.
2. For each changed symbol, find **references across the repo**: `git grep`
   candidates, then a language-aware parse of each candidate file to **confirm**
   the occurrence is a real reference (call / import / type-use) and drop noise
   (comments, string literals, same-name-different-binding).
3. Assemble a compact, **capped** impact summary, grouped by symbol.
4. **Inject** the summary into the review context (a new template variable) plus a
   prompt instruction to check the listed references for breakage.

No LLM cost beyond the injected summary tokens. The model then uses its existing
`file_read` to investigate the flagged references.

### Why not the alternatives
- **LSP / tsserver (semantic, max precision):** too heavy for CI — language-server
  lifecycle, full project load per review, complex to drive from a Go binary, one
  server per language. Rejected.
- **A pull-based `find_references` tool:** reintroduces the recall problem (the
  model may not call it). Rejected as the primary mechanism. (Could be added later
  as a complement.)

## Parser technology: native per-language, no CGO

Behind a `LangAnalyzer` interface, one implementation per language. Native parsers
(not tree-sitter) so OCR stays a **pure-Go static binary** (no CGO, no change to
the prebuilt-binary distribution) and each language is parsed by its own parser:

- **Go** → stdlib `go/parser` + `go/ast` (pure Go, zero deps, in-process).
- **TypeScript / TSX** → a small embedded Node helper using the TypeScript
  compiler's `ts.createSourceFile` (per-file AST; no project load, no typecheck).
  The helper resolves `typescript` from the repo under review (`node_modules`). If
  Node or `typescript` is unavailable, the TS analyzer reports unsupported and
  impact is skipped for TS files.

Precision is **structural AST** (distinguishes call / import / definition from
comment / string; does not fully resolve which imported binding a name refers to).
Sufficient to surface candidate callers for the model to verify, and far better
than text grep.

## Context-provider abstraction (forward-compatible with learnings)

The injection is generalized so the future **cross-PR learnings** subsystem reuses
the same plumbing:

```go
// FileReviewInput is the per-file context handed to each provider.
type FileReviewInput struct {
    RepoDir      string
    Path         string // file under review (new path)
    NewContent   string // full new content of the file
    ChangedLines []int  // changed line numbers in the new file
    Diff         string // the file's unified diff
}

// ContextProvider supplies extra, injectable review context for one file.
type ContextProvider interface {
    Name() string
    // Context returns a compact text block to inject, or "" if nothing to add.
    // Must be deterministic and side-effect-free.
    Context(ctx context.Context, in FileReviewInput) (string, error)
}
```

The cross-ref impact analyzer is the **first** `ContextProvider`. The agent runs all
configured providers per file and concatenates non-empty outputs into a single
`{{extra_context}}` template variable. A future `LearningsProvider` plugs in the
same way — no further plumbing required.

## Components

`internal/impact/` (new package):
- `LangAnalyzer` interface + `goAnalyzer` (go/parser) + `tsAnalyzer` (Node helper).
- `crossRefFinder`: orchestrates changed-symbol extraction → git-grep candidates →
  reference confirmation → summary assembly. Implements `ContextProvider`.
- Embedded TS helper script `ts_refs.js` via `//go:embed`.

`internal/reviewctx/` (small): the `ContextProvider` interface, `FileReviewInput`,
and an aggregator that runs providers and joins their output. (May live in
`internal/impact` if that reads cleaner during planning.)

**Integration:** in the agent's per-file review setup (where MAIN_TASK template
variables are rendered), run the provider aggregator and populate
`{{extra_context}}`. Add one instruction line to the MAIN_TASK system prompt in
`task_template.json` referencing the cross-reference impact section.

## Data flow

```
per file under review:
  diff + new content + changed line numbers
        │
        ▼
  LangAnalyzer.ChangedSymbols      ──►  [changed symbols]
        │  (per symbol)
        ▼
  git grep -nw <name>  (file-filtered, exclude def file)  ──►  [candidate file:line]
        │  (per candidate file)
        ▼
  LangAnalyzer.References(content, name)  ──►  [confirmed references]
        │
        ▼
  assemble capped summary  ──►  {{extra_context}}  ──►  MAIN_TASK prompt
```

Example injected block:

```
## Cross-reference impact (auto-computed, structural)
Symbols changed in this file and where they are used elsewhere — verify these
references are not broken by the change:
- `parseConfig` (exported function): src/app.ts:42 (call), src/cli.ts:18 (call)
- `UserRole` (enum): src/auth/guard.ts:7 (import), src/auth/guard.ts:31 (type-use)
(showing 8 of 12 references; dynamic or indirect uses may be missed)
```

## Configuration (env, matching OCR_* style)

- `OCR_IMPACT_CONTEXT` = `on` (default) | `off`.
- `OCR_IMPACT_MAX_REFS` = total references cap (default 20).
- Per-symbol cap (default 8) and a total-character cap on the injected block.
- Truncation is always reported in the summary; never silently truncated.

## Graceful degradation

- File in an unsupported language → no impact context; review proceeds unchanged.
- Node / `typescript` missing → TS analyzer reports unsupported; skipped; no error.
- `git grep` / parse error for a symbol → that symbol contributes nothing; a
  warning goes to stderr; review proceeds.

## Testing

- `goAnalyzer`: fixtures — changed-symbol extraction given content + changed line
  ranges; reference confirmation **excludes** comment / string / shadowed
  same-name occurrences.
- `tsAnalyzer`: fixtures, `t.Skip` when Node is unavailable.
- `crossRefFinder`: a temp git repo with a known definition + references; assert the
  assembled summary, the caps + truncation note, and the graceful-skip paths.
- Aggregator: zero providers → empty `{{extra_context}}`; one provider → injected.

## Scope (YAGNI)

**In scope (MVP):** Go + TypeScript/TSX; structural references (call / import /
type-use); auto-injected; caps + env config + graceful degradation; the
`ContextProvider` abstraction.

**Explicitly out of scope (separate efforts):**
- Full semantic resolution / LSP / type-checking.
- Whole-repo dependency / call graph.
- **Cross-PR learnings** — its own spec next; depends on first resolving the
  *feedback-signal* question (how OCR learns whether a past comment was accepted or
  dismissed). This design only leaves the `ContextProvider` hook for it.
- Languages beyond Go / TS (the interface makes adding them incremental).

## Open questions (resolve during planning)

- The exact integration point in `internal/agent` for rendering `{{extra_context}}`.
- The embedded TS helper's invocation contract (stdin JSON in, JSON out) and how it
  locates `typescript` in the reviewed repo.
