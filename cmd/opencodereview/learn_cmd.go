package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/open-code-review/open-code-review/internal/gitcmd"
	"github.com/open-code-review/open-code-review/internal/learn"
	"github.com/open-code-review/open-code-review/internal/llm"
)

// runLearn dispatches `ocr learn <subcommand>`.
func runLearn(args []string) error {
	if len(args) == 0 {
		printLearnUsage()
		return nil
	}
	switch args[0] {
	case "ingest":
		return runLearnIngest(args[1:])
	case "calibrate":
		return runLearnCalibrate(args[1:])
	case "-h", "--help":
		printLearnUsage()
		return nil
	default:
		return fmt.Errorf("unknown learn command: %s\nRun 'ocr learn -h' for usage", args[0])
	}
}

// runLearnIngest collects feedback into the learnings store WITHOUT running a
// review. It is the standalone counterpart to the best-effort ingest that
// `ocr review` performs, intended for a lightweight "PR closed" workflow job
// that captures final thread verdicts (resolved/unresolved) at merge time —
// the reliable capture point that a review-time collector misses.
func runLearnIngest(args []string) error {
	fs := flag.NewFlagSet("learn ingest", flag.ContinueOnError)
	repoDir := fs.String("repo", "", "repository directory (default: current directory)")
	feedback := fs.String("feedback", "", "path to feedback.json (overrides OCR_LEARNINGS_FEEDBACK)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// A --feedback flag takes precedence; otherwise runLearningsIngest reads
	// OCR_LEARNINGS_FEEDBACK from the environment (same as the review path).
	if *feedback != "" {
		if err := os.Setenv("OCR_LEARNINGS_FEEDBACK", *feedback); err != nil {
			return fmt.Errorf("set feedback path: %w", err)
		}
	}

	if err := requireGitRepo(*repoDir); err != nil {
		return err
	}
	resolved, err := resolveRepoDir(*repoDir)
	if err != nil {
		return fmt.Errorf("resolve repo: %w", err)
	}

	cfgPath, err := defaultConfigPath()
	if err != nil {
		return err
	}
	ep, err := llm.ResolveEndpointWithModelOverride(cfgPath, "")
	if err != nil {
		return fmt.Errorf("resolve LLM endpoint: %w", err)
	}

	gitRunner := gitcmd.New(4)
	// Ingest-only entry point: setupLearnings performs the ingest as a side
	// effect; the returned suppressor is irrelevant here and discarded.
	_ = setupLearnings(context.Background(), resolved, ep.Token, gitRunner)
	return nil
}

// runLearnCalibrate reports the pairwise-cosine distribution of the repo's
// rejected learnings and suggests an OCR_REFLAG_THRESHOLD. It reads only the
// local store (no embedding/LLM calls), so it is cheap and offline.
func runLearnCalibrate(args []string) error {
	fs := flag.NewFlagSet("learn calibrate", flag.ContinueOnError)
	repoDir := fs.String("repo", "", "repository directory (default: current directory)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := requireGitRepo(*repoDir); err != nil {
		return err
	}
	resolved, err := resolveRepoDir(*repoDir)
	if err != nil {
		return fmt.Errorf("resolve repo: %w", err)
	}

	remote, _ := gitcmd.New(4).Run(context.Background(), resolved, "remote", "get-url", "origin")
	remote = strings.TrimSpace(remote)
	if remote == "" {
		remote = resolved
	}
	storePath, err := learn.RepoStorePath(remote)
	if err != nil {
		return fmt.Errorf("store path: %w", err)
	}
	store, err := learn.OpenStore(storePath, learn.DefaultSoftCap)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}

	st, ok := store.Calibrate()
	if !ok {
		fmt.Printf("Not enough data to calibrate: %d rejected learning(s) with embeddings (need >= 2).\nStore: %s\n", st.Rejected, storePath)
		return nil
	}
	fmt.Printf(`Reflag threshold calibration
  store:            %s
  rejected:         %d  (pairs compared: %d)
  pairwise cosine:  min=%.3f  median=%.3f  p90=%.3f  p95=%.3f  max=%.3f
  suggested OCR_REFLAG_THRESHOLD = %.2f

Distinct rejected findings cluster at/below p95 (%.3f); set the threshold above
it so true repeats (cosine ~1.0) are suppressed without collapsing distinct ones.
`, storePath, st.Rejected, st.Pairs, st.Min, st.Median, st.P90, st.P95, st.Max, st.Suggested, st.P95)
	return nil
}

func printLearnUsage() {
	fmt.Println(`Usage:
  ocr learn <command>

Commands:
  ingest      Collect feedback.json into the local learnings store (no review)
  calibrate   Suggest a reflag threshold from the local store (offline)

Flags (ingest):
  --repo <dir>        Repository directory (default: current directory)
  --feedback <path>   Path to feedback.json (overrides OCR_LEARNINGS_FEEDBACK)

Flags (calibrate):
  --repo <dir>        Repository directory (default: current directory)

Examples:
  ocr learn ingest --feedback /tmp/ocr-feedback.json
  ocr learn calibrate`)
}
