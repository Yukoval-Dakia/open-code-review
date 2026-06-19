package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/open-code-review/open-code-review/internal/gitcmd"
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
	runLearningsIngest(context.Background(), resolved, ep.Token, gitRunner)
	return nil
}

func printLearnUsage() {
	fmt.Println(`Usage:
  ocr learn <command>

Commands:
  ingest    Collect feedback.json into the local learnings store (no review)

Flags (ingest):
  --repo <dir>        Repository directory (default: current directory)
  --feedback <path>   Path to feedback.json (overrides OCR_LEARNINGS_FEEDBACK)

Example:
  ocr learn ingest --feedback /tmp/ocr-feedback.json`)
}
