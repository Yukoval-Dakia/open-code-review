package main

import (
	"os"
	"strconv"
	"strings"

	"github.com/open-code-review/open-code-review/internal/model"
)

// severityRank maps a self-assessed severity label to an ordinal for threshold
// comparison. Unknown/empty severity is rank 0.
func severityRank(s string) int {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "blocker":
		return 4
	case "major":
		return 3
	case "minor":
		return 2
	case "nit":
		return 1
	default:
		return 0
	}
}

// commentFilter suppresses low-severity / low-confidence comments before output
// to improve the signal-to-noise ratio. Configured via environment variables so
// it fits the existing OCR_* / CI configuration style:
//
//	OCR_DISABLE_SEVERITY_FILTER=1   turn the filter off entirely
//	OCR_MIN_SEVERITY=minor          minimum severity kept (blocker|major|minor|nit)
//	OCR_MIN_CONFIDENCE=0.5          minimum self-assessed confidence kept (0.0-1.0)
type commentFilter struct {
	enabled          bool
	minSeverity      int
	minSeverityLabel string
	minConfidence    float64
}

func loadCommentFilter() commentFilter {
	f := commentFilter{
		enabled:          true,
		minSeverity:      severityRank("minor"),
		minSeverityLabel: "minor",
		minConfidence:    0.5,
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("OCR_DISABLE_SEVERITY_FILTER"))) {
	case "1", "true", "yes":
		f.enabled = false
	}
	if v := strings.TrimSpace(os.Getenv("OCR_MIN_SEVERITY")); v != "" {
		if r := severityRank(v); r > 0 {
			f.minSeverity = r
			f.minSeverityLabel = strings.ToLower(v)
		}
	}
	if v := strings.TrimSpace(os.Getenv("OCR_MIN_CONFIDENCE")); v != "" {
		if c, err := strconv.ParseFloat(v, 64); err == nil && c >= 0 && c <= 1 {
			f.minConfidence = c
		}
	}
	return f
}

// apply returns the kept comments and the number dropped. A comment with no
// severity (the model failed to classify it) is treated as "major" so a real
// finding is never silently dropped just because it lacks a label; the
// confidence gate only applies when the model supplied a confidence.
func (f commentFilter) apply(comments []model.LlmComment) (kept []model.LlmComment, dropped int) {
	if !f.enabled {
		return comments, 0
	}
	kept = make([]model.LlmComment, 0, len(comments))
	for _, c := range comments {
		sev := severityRank(c.Severity)
		if sev == 0 {
			sev = severityRank("major")
		}
		if sev < f.minSeverity {
			dropped++
			continue
		}
		if c.Confidence > 0 && c.Confidence < f.minConfidence {
			dropped++
			continue
		}
		kept = append(kept, c)
	}
	return kept, dropped
}
