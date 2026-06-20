package learn

import "sort"

// CalibrationStats summarizes the pairwise cosine similarity between distinct
// rejected learnings in a store. It answers the question the reflag threshold
// must balance: "how similar are genuinely different rejected findings to each
// other?" A threshold set above this distribution's high percentiles suppresses
// true repeats (cosine ~1.0) without collapsing distinct findings into one.
type CalibrationStats struct {
	Rejected  int     // number of rejected learnings considered
	Pairs     int     // number of distinct unordered pairs compared
	Min       float32 // lowest pairwise cosine
	Median    float32
	P90       float32
	P95       float32
	Max       float32 // highest pairwise cosine (near-duplicate rejected findings)
	Suggested float32 // recommended OCR_REFLAG_THRESHOLD
}

// Calibrate computes pairwise-cosine statistics over the store's rejected
// learnings. With fewer than two embedded rejected learnings there is nothing
// to compare, so ok is false. The suggested threshold sits a small margin above
// P95 (clamped to [0.80, 0.97]): high enough to clear almost all distinct-pair
// similarities, low enough to still catch paraphrased repeats.
func (s *LearningStore) Calibrate() (CalibrationStats, bool) {
	var vecs [][]float32
	for _, e := range s.entries {
		if e.Verdict == VerdictRejected && len(e.Embedding) > 0 {
			vecs = append(vecs, e.Embedding)
		}
	}
	if len(vecs) < 2 {
		return CalibrationStats{Rejected: len(vecs)}, false
	}
	var sims []float32
	for i := 0; i < len(vecs); i++ {
		for j := i + 1; j < len(vecs); j++ {
			sims = append(sims, Cosine(vecs[i], vecs[j]))
		}
	}
	sort.Slice(sims, func(a, b int) bool { return sims[a] < sims[b] })

	st := CalibrationStats{
		Rejected: len(vecs),
		Pairs:    len(sims),
		Min:      sims[0],
		Median:   percentile(sims, 0.50),
		P90:      percentile(sims, 0.90),
		P95:      percentile(sims, 0.95),
		Max:      sims[len(sims)-1],
	}
	st.Suggested = clamp(st.P95+0.02, 0.80, 0.97)
	return st, true
}

// percentile returns the p-quantile (0..1) of a sorted slice via nearest-rank.
func percentile(sorted []float32, p float64) float32 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(p * float64(len(sorted)-1))
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func clamp(v, lo, hi float32) float32 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
