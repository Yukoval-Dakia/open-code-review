package main

import (
	"testing"

	"github.com/open-code-review/open-code-review/internal/model"
)

func TestSeverityRankOrdering(t *testing.T) {
	if !(severityRank("blocker") > severityRank("major") &&
		severityRank("major") > severityRank("minor") &&
		severityRank("minor") > severityRank("nit") &&
		severityRank("nit") > severityRank("")) {
		t.Fatalf("severity ordering wrong: blocker=%d major=%d minor=%d nit=%d unknown=%d",
			severityRank("blocker"), severityRank("major"), severityRank("minor"),
			severityRank("nit"), severityRank(""))
	}
}

func TestCommentFilterApply(t *testing.T) {
	f := commentFilter{enabled: true, minSeverity: severityRank("major"), minSeverityLabel: "major", minConfidence: 0.7}
	in := []model.LlmComment{
		{Content: "blocker high conf", Severity: "blocker", Confidence: 0.9}, // keep
		{Content: "major at threshold", Severity: "major", Confidence: 0.7},  // keep (== threshold)
		{Content: "minor", Severity: "minor", Confidence: 0.9},               // drop: severity
		{Content: "nit", Severity: "nit", Confidence: 1.0},                   // drop: severity
		{Content: "major low conf", Severity: "major", Confidence: 0.5},      // drop: confidence
		{Content: "unlabeled", Severity: "", Confidence: 0},                  // keep: unknown->major, no conf gate
		{Content: "major no conf", Severity: "major", Confidence: 0},         // keep: conf gate skipped when 0
	}
	kept, dropped := f.apply(in)
	if dropped != 3 {
		t.Errorf("dropped = %d, want 3", dropped)
	}
	if len(kept) != 4 {
		t.Errorf("kept = %d, want 4", len(kept))
	}
}

func TestCommentFilterDisabledKeepsAll(t *testing.T) {
	f := commentFilter{enabled: false}
	in := []model.LlmComment{{Severity: "nit", Confidence: 0.1}}
	kept, dropped := f.apply(in)
	if dropped != 0 || len(kept) != 1 {
		t.Fatalf("disabled filter should keep all: kept=%d dropped=%d", len(kept), dropped)
	}
}

func TestLoadCommentFilterEnvOverrides(t *testing.T) {
	t.Setenv("OCR_MIN_SEVERITY", "minor")
	t.Setenv("OCR_MIN_CONFIDENCE", "0.5")
	f := loadCommentFilter()
	if f.minSeverity != severityRank("minor") {
		t.Errorf("minSeverity = %d, want %d", f.minSeverity, severityRank("minor"))
	}
	if f.minConfidence != 0.5 {
		t.Errorf("minConfidence = %v, want 0.5", f.minConfidence)
	}

	t.Setenv("OCR_DISABLE_SEVERITY_FILTER", "1")
	if loadCommentFilter().enabled {
		t.Error("OCR_DISABLE_SEVERITY_FILTER=1 should disable the filter")
	}
}
