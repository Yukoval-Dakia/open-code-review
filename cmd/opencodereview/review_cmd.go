package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/open-code-review/open-code-review/internal/agent"
	"github.com/open-code-review/open-code-review/internal/config/rules"
	"github.com/open-code-review/open-code-review/internal/config/template"
	"github.com/open-code-review/open-code-review/internal/config/toolsconfig"
	"github.com/open-code-review/open-code-review/internal/diff"
	"github.com/open-code-review/open-code-review/internal/gitcmd"
	"github.com/open-code-review/open-code-review/internal/learn"
	"github.com/open-code-review/open-code-review/internal/llm"
	"github.com/open-code-review/open-code-review/internal/stdout"
	"github.com/open-code-review/open-code-review/internal/telemetry"
	"github.com/open-code-review/open-code-review/internal/tool"
)

func runReview(args []string) error {
	opts, err := parseReviewFlags(args)
	if err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}
	if opts.showHelp {
		printReviewUsage()
		return nil
	}

	if err := requireGitRepo(opts.repoDir); err != nil {
		return err
	}

	tpl, err := template.LoadDefault()
	if err != nil {
		return fmt.Errorf("load default template: %w", err)
	}
	if opts.maxTools > 0 {
		tpl.MaxToolRequestTimes = opts.maxTools
	}
	if err := tpl.Validate(); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}

	repoDir, err := resolveRepoDir(opts.repoDir)
	if err != nil {
		return fmt.Errorf("resolve repo: %w", err)
	}
	if err := validateReviewRefs(repoDir, opts); err != nil {
		return err
	}

	if opts.commit != "" && opts.background == "" {
		if msg, err := getCommitMessage(repoDir, opts.commit); err == nil && msg != "" {
			opts.background = msg
		}
	}

	resolver, fileFilter, err := rules.NewResolver(repoDir, opts.rulePath)
	if err != nil {
		return fmt.Errorf("load rules: %w", err)
	}

	if opts.preview {
		return runPreview(repoDir, opts, fileFilter)
	}

	toolEntries, err := toolsconfig.Load(opts.toolConfigPath)
	if err != nil {
		return fmt.Errorf("load tools: %w", err)
	}
	planToolDefs := agent.BuildToolDefs(toolEntries, true)
	mainToolDefs := agent.BuildToolDefs(toolEntries, false)

	cfgPath, err := defaultConfigPath()
	if err != nil {
		return err
	}

	appCfg, err := LoadAppConfig(cfgPath)
	if err != nil {
		return fmt.Errorf("load app config: %w", err)
	}
	var lang string
	if appCfg != nil {
		lang = appCfg.Language
	}
	tpl.ApplyLanguage(lang)

	ep, err := llm.ResolveEndpointWithModelOverride(cfgPath, opts.model)
	if err != nil {
		return fmt.Errorf("resolve LLM endpoint: %w", err)
	}
	if ep.Protocol == "codex" {
		if ep.ExtraBody == nil {
			ep.ExtraBody = make(map[string]any)
		}
		ep.ExtraBody["repo_dir"] = repoDir
	}

	llmClient := llm.NewLLMClient(ep)
	model := ep.Model

	gitRunner := gitcmd.New(opts.maxGitProcs)

	collector := tool.NewCommentCollector()
	mode := tool.ParseReviewMode(opts.from, opts.to, opts.commit)
	ref, _ := mode.RefValue(opts.to, opts.commit)
	fileReader := &tool.FileReader{
		RepoDir: repoDir,
		Mode:    mode,
		Ref:     ref,
		Runner:  gitRunner,
	}
	tools := buildToolRegistry(collector, fileReader)

	suppressor := setupLearnings(context.Background(), repoDir, ep.Token, gitRunner)

	ag := agent.New(agent.Args{
		RepoDir:               repoDir,
		From:                  opts.from,
		To:                    opts.to,
		Commit:                opts.commit,
		Template:              *tpl,
		SystemRule:            resolver,
		FileFilter:            fileFilter,
		LLMClient:             llmClient,
		Tools:                 tools,
		PlanToolDefs:          planToolDefs,
		MainToolDefs:          mainToolDefs,
		CommentCollector:      collector,
		CommentWorkerPool:     agent.NewCommentWorkerPool(opts.concurrency),
		MaxConcurrency:        opts.concurrency,
		ConcurrentTaskTimeout: opts.perFileTimeout,
		Model:                 model,
		Background:            opts.background,
		GitRunner:             gitRunner,
	})

	// Silence progress output during execution; restore before Summary in agent mode.
	var unsilence func()
	if opts.outputFormat == "json" || opts.audience == "agent" {
		unsilence = stdout.Quiet()
		defer func() {
			if unsilence != nil {
				unsilence()
			}
		}()
	}

	ctx, span := telemetry.StartSpan(context.Background(), "review.run")
	defer span.End()
	startTime := time.Now()

	comments, err := ag.Run(ctx)
	if err != nil {
		telemetry.SetAttr(span, "error", err.Error())
		return fmt.Errorf("review failed: %w", err)
	}

	// Resolve line numbers by matching existing_code against diff hunks.
	comments = diff.ResolveLineNumbers(comments, ag.Diffs())

	// Suppress low-severity / low-confidence comments to improve signal-to-noise.
	// The drop count is reported (never silently truncated); tune or disable via
	// OCR_MIN_SEVERITY / OCR_MIN_CONFIDENCE / OCR_DISABLE_SEVERITY_FILTER.
	if cf := loadCommentFilter(); cf.enabled {
		var dropped int
		comments, dropped = cf.apply(comments)
		if dropped > 0 {
			fmt.Fprintf(os.Stderr, "[ocr] severity filter dropped %d comment(s) below min-severity=%s / min-confidence=%.2f (set OCR_DISABLE_SEVERITY_FILTER=1 to disable)\n",
				dropped, cf.minSeverityLabel, cf.minConfidence)
		}
	}

	// Suppress comments that repeat a previously human-rejected finding (the
	// multi-round re-flag problem). No-op unless cross-PR learnings are
	// configured and the store holds rejected verdicts. Never silently
	// truncated; set OCR_REFLAG_SUPPRESS=off to disable.
	if suppressor.enabled {
		var reflagged int
		comments, reflagged = suppressor.apply(ctx, comments)
		if reflagged > 0 {
			fmt.Fprintf(os.Stderr, "[ocr] reflag suppressor dropped %d comment(s) matching prior rejected findings (cosine>=%.2f; set OCR_REFLAG_SUPPRESS=off to disable)\n",
				reflagged, suppressor.threshold)
		}
	}

	// Record summary metrics (files_reviewed is refined by agent.Run).
	duration := time.Since(startTime)
	telemetry.RecordReviewDuration(ctx, duration)
	if len(comments) > 0 {
		telemetry.RecordCommentsGenerated(ctx, int64(len(comments)))
	}

	// If no files were reviewed (e.g. workspace has no changes), inform the caller in JSON mode.
	if opts.outputFormat == "json" && len(comments) == 0 && ag.FilesReviewed() == 0 {
		return outputJSONNoFiles()
	}

	// In agent mode (text output), restore stdout so Summary reaches the terminal.
	if opts.audience == "agent" && opts.outputFormat != "json" && unsilence != nil {
		unsilence()
		unsilence = nil
	}

	if opts.outputFormat != "json" {
		telemetry.PrintTraceSummary(ag.FilesReviewed(), int64(len(comments)), ag.TotalInputTokens(), ag.TotalOutputTokens(), ag.TotalTokensUsed(), ag.TotalCacheReadTokens(), ag.TotalCacheWriteTokens(), duration)
	}

	if opts.outputFormat == "json" {
		return outputJSONWithWarnings(comments, ag.Warnings(), ag.FilesReviewed(), ag.TotalInputTokens(), ag.TotalOutputTokens(), ag.TotalTokensUsed(), ag.TotalCacheReadTokens(), ag.TotalCacheWriteTokens(), duration)
	}
	if opts.audience == "agent" {
		outputTextWithWarnings(comments, ag.Warnings())
		return nil
	}
	outputTextWithWarnings(comments, ag.Warnings())

	return nil
}

func resolveRepoDir(input string) (string, error) {
	if input == "" {
		var err error
		input, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("get working directory: %w", err)
		}
	}
	absPath, err := filepath.Abs(input)
	if err != nil {
		return "", fmt.Errorf("resolve absolute path: %w", err)
	}
	out, err := runGitCmd(absPath, "rev-parse", "--git-dir")
	if err != nil || len(out) == 0 {
		return "", fmt.Errorf("%s is not a git repository", absPath)
	}
	return absPath, nil
}

// requireGitRepo validates that the given directory is part of a git repository.
func requireGitRepo(dir string) error {
	repoDir, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}
	out, err := runGitCmd(repoDir, "rev-parse", "--git-dir")
	if err != nil || len(out) == 0 {
		return fmt.Errorf("%s is not a git repository, code review requires a valid git repository", repoDir)
	}
	return nil
}

func validateReviewRefs(repoDir string, opts reviewOptions) error {
	refs := []struct {
		flag string
		ref  string
	}{
		{"--from", opts.from},
		{"--to", opts.to},
		{"--commit", opts.commit},
	}
	for _, item := range refs {
		if item.ref == "" {
			continue
		}
		if strings.HasPrefix(item.ref, "-") {
			return fmt.Errorf("%s value %q is not a valid git ref: refs must not start with '-'", item.flag, item.ref)
		}
		if out, err := runGitCmd(repoDir, "rev-parse", "--verify", "--end-of-options", item.ref+"^{commit}"); err != nil {
			msg := strings.TrimSpace(string(out))
			if msg != "" {
				return fmt.Errorf("%s value %q is not a valid commit ref: %s", item.flag, item.ref, msg)
			}
			return fmt.Errorf("%s value %q is not a valid commit ref", item.flag, item.ref)
		}
	}
	return nil
}

func runPreview(repoDir string, opts reviewOptions, fileFilter *rules.FileFilter) error {
	gitRunner := gitcmd.New(opts.maxGitProcs)
	ag := agent.New(agent.Args{
		RepoDir:    repoDir,
		From:       opts.from,
		To:         opts.to,
		Commit:     opts.commit,
		FileFilter: fileFilter,
		GitRunner:  gitRunner,
	})

	preview, err := ag.Preview(context.Background())
	if err != nil {
		return fmt.Errorf("preview failed: %w", err)
	}

	outputPreviewText(preview)
	return nil
}

func buildToolRegistry(collector *tool.CommentCollector, fr *tool.FileReader) *tool.Registry {
	reg := tool.NewRegistry()
	reg.Register(tool.NewFileRead(fr))
	reg.Register(tool.NewFileFind(fr))
	reg.Register(tool.NewFileReadDiff(tool.DiffMap{}))
	reg.Register(tool.NewCodeSearch(fr))
	reg.Register(&tool.CodeCommentProvider{Collector: collector})
	return reg
}

// setupLearnings ingests PR feedback (if configured) into the local store and
// returns a re-flag suppressor backed by that same store + embedder. Best-effort:
// every failure path warns and returns a disabled suppressor (zero value) so the
// review proceeds unaffected.
func setupLearnings(ctx context.Context, repoDir, token string, gitRunner *gitcmd.Runner) reflagSuppressor {
	cfg := learn.LoadConfig()
	if !cfg.Enabled {
		return reflagSuppressor{} // disabled
	}
	if token == "" {
		fmt.Fprintln(os.Stderr, "[ocr] learnings: no LLM token; skipping")
		return reflagSuppressor{}
	}
	remote, _ := gitRunner.Run(ctx, repoDir, "remote", "get-url", "origin")
	remote = strings.TrimSpace(remote)
	if remote == "" {
		remote = repoDir // fall back to repo path as the store key
	}
	storePath, err := learn.RepoStorePath(remote)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[ocr] learnings: store path: %v (skipped)\n", err)
		return reflagSuppressor{}
	}
	store, err := learn.OpenStore(storePath, learn.DefaultSoftCap)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[ocr] learnings: open store: %v (skipped)\n", err)
		return reflagSuppressor{}
	}
	emb := learn.NewBigModelEmbedder(cfg.EmbedURL, token, cfg.EmbedModel)

	// Ingestion only runs when the workflow supplied a feedback file; absent
	// one, we still build a suppressor from whatever the store already holds.
	if cfg.FeedbackPath != "" {
		added, err := learn.Ingest(ctx, store, emb, cfg.FeedbackPath, time.Now().UTC().Format(time.RFC3339))
		if err != nil {
			fmt.Fprintf(os.Stderr, "[ocr] learnings: ingest: %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "[ocr] learnings: ingested %d new feedback item(s); store now has %d\n", added, store.Len())
		}
	}
	return newReflagSuppressor(true, emb, store)
}
