// internal/impact/crossref.go
package impact

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/open-code-review/open-code-review/internal/reviewctx"
)

const (
	defaultMaxRefs      = 20
	defaultPerSymbolCap = 8
)

// symRefs pairs a changed symbol with its confirmed cross-file references.
type symRefs struct {
	sym  Symbol
	refs []Reference
}

// CrossRefProvider injects a cross-reference impact summary for the file's
// changed symbols. Implements reviewctx.ContextProvider.
type CrossRefProvider struct {
	enabled bool
	maxRefs int
}

func NewCrossRefProvider() *CrossRefProvider {
	p := &CrossRefProvider{
		enabled: true,
		maxRefs: defaultMaxRefs,
	}
	if strings.EqualFold(strings.TrimSpace(os.Getenv("OCR_IMPACT_CONTEXT")), "off") {
		p.enabled = false
	}
	if v := strings.TrimSpace(os.Getenv("OCR_IMPACT_MAX_REFS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			p.maxRefs = n
		}
	}
	return p
}

func (p *CrossRefProvider) Name() string { return "crossref-impact" }

// analyzerForPath returns the first LangAnalyzer that supports the given path.
func analyzerForPath(analyzers []LangAnalyzer, path string) LangAnalyzer {
	for _, a := range analyzers {
		if a.Supports(path) {
			return a
		}
	}
	return nil
}

func (p *CrossRefProvider) Context(ctx context.Context, in reviewctx.FileReviewInput) (string, error) {
	if !p.enabled || p.maxRefs == 0 {
		return "", nil
	}
	analyzers := []LangAnalyzer{goAnalyzer{}, tsAnalyzer{repoDir: in.RepoDir}}
	a := analyzerForPath(analyzers, in.Path)
	if a == nil {
		return "", nil // unsupported language: skip
	}
	changed := in.ChangedLines
	if changed == nil {
		changed = ChangedNewLines(in.Diff)
	}
	symbols, err := a.ChangedSymbols(in.NewContent, changed)
	if err != nil || len(symbols) == 0 {
		return "", nil // parse error or nothing changed: skip silently
	}

	var results []symRefs
	total := 0
	truncated := false
	for _, sym := range symbols {
		if total >= p.maxRefs {
			truncated = true
			break
		}
		refs := p.findRefs(ctx, in.RepoDir, in.Ref, in.Path, sym.Name, a)
		if len(refs) == 0 {
			continue
		}
		if len(refs) > defaultPerSymbolCap {
			refs = refs[:defaultPerSymbolCap]
			truncated = true
		}
		if total+len(refs) > p.maxRefs {
			refs = refs[:p.maxRefs-total]
			truncated = true
		}
		total += len(refs)
		results = append(results, symRefs{sym, refs})
		if total >= p.maxRefs {
			truncated = true
			break
		}
	}
	if len(results) == 0 {
		return "", nil
	}
	return renderSummary(results, truncated), nil
}

// findRefs greps for candidate files then confirms via the analyzer.
func (p *CrossRefProvider) findRefs(ctx context.Context, repoDir, ref, defPath, name string, a LangAnalyzer) []Reference {
	var cmd *exec.Cmd
	if ref != "" {
		cmd = exec.CommandContext(ctx, "git", "grep", "-l", "-w", "-e", name, ref)
	} else {
		cmd = exec.CommandContext(ctx, "git", "grep", "-l", "-w", "-e", name)
	}
	cmd.Dir = repoDir
	out, err := cmd.Output()
	if err != nil {
		return nil // no matches or grep error
	}
	var refs []Reference
	for _, cand := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if cand == "" {
			continue
		}
		// When grepping at a ref, git prefixes matches as "<ref>:<path>". Strip it.
		path := cand
		if ref != "" {
			path = strings.TrimPrefix(cand, ref+":")
		}
		if filepath.Clean(path) == filepath.Clean(defPath) || !a.Supports(path) {
			continue
		}
		body, err := readCandidate(ctx, repoDir, ref, path)
		if err != nil {
			continue
		}
		found, err := a.References(path, body, name)
		if err != nil {
			continue
		}
		refs = append(refs, found...)
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].File != refs[j].File {
			return refs[i].File < refs[j].File
		}
		return refs[i].Line < refs[j].Line
	})
	return refs
}

// readCandidate reads a candidate file body at the reviewed ref (git show) or
// from the working tree when ref is empty.
func readCandidate(ctx context.Context, repoDir, ref, path string) (string, error) {
	if ref == "" {
		b, err := os.ReadFile(filepath.Join(repoDir, path))
		return string(b), err
	}
	cmd := exec.CommandContext(ctx, "git", "show", ref+":"+path)
	cmd.Dir = repoDir
	out, err := cmd.Output()
	return string(out), err
}

func renderSummary(results []symRefs, truncated bool) string {
	var b strings.Builder
	b.WriteString("## Cross-reference impact (auto-computed, structural)\n")
	b.WriteString("Symbols changed in this file and where they are used elsewhere — verify these references are not broken by the change:\n")
	shown := 0
	for _, r := range results {
		parts := make([]string, 0, len(r.refs))
		for _, ref := range r.refs {
			parts = append(parts, fmt.Sprintf("%s:%d (%s)", ref.File, ref.Line, ref.Kind))
		}
		shown += len(r.refs)
		b.WriteString(fmt.Sprintf("- `%s` (%s): %s\n", r.sym.Name, r.sym.Kind, strings.Join(parts, ", ")))
	}
	if truncated {
		b.WriteString(fmt.Sprintf("(showing %d references, capped; dynamic or indirect uses may be missed)\n", shown))
	}
	return b.String()
}
