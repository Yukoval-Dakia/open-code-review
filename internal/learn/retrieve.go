package learn

import (
	"math"
	"sort"
)

// Cosine returns the cosine similarity of a and b in [-1,1]. Mismatched or
// zero-magnitude vectors yield 0 (treated as "no signal" rather than an error,
// so a malformed stored embedding can never spuriously suppress a finding).
func Cosine(a, b []float32) float32 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		av, bv := float64(a[i]), float64(b[i])
		dot += av * bv
		na += av * av
		nb += bv * bv
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return float32(dot / (math.Sqrt(na) * math.Sqrt(nb)))
}

// Match is a stored learning paired with its similarity to a query vector.
type Match struct {
	Learning Learning
	Score    float32
}

// TopRejected ranks stored learnings whose Verdict is Rejected by cosine
// similarity to vec and returns the top k (k<=0 returns all). Only rejected
// learnings are considered: the goal is to suppress findings a human already
// dismissed, never to suppress ones they accepted.
func (s *LearningStore) TopRejected(vec []float32, k int) []Match {
	matches := make([]Match, 0, len(s.entries))
	for _, e := range s.entries {
		if e.Verdict != VerdictRejected {
			continue
		}
		matches = append(matches, Match{Learning: e, Score: Cosine(vec, e.Embedding)})
	}
	sort.SliceStable(matches, func(i, j int) bool {
		return matches[i].Score > matches[j].Score
	})
	if k > 0 && len(matches) > k {
		matches = matches[:k]
	}
	return matches
}

// HasRejected reports whether the store holds any rejected learning. Callers
// use it to skip embedding work entirely when there is nothing to suppress.
func (s *LearningStore) HasRejected() bool {
	for _, e := range s.entries {
		if e.Verdict == VerdictRejected {
			return true
		}
	}
	return false
}

// BestRejected returns the single highest-similarity rejected learning for vec.
// ok is false when the store holds no rejected learnings.
func (s *LearningStore) BestRejected(vec []float32) (Match, bool) {
	top := s.TopRejected(vec, 1)
	if len(top) == 0 {
		return Match{}, false
	}
	return top[0], true
}
