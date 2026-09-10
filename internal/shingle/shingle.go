// Package shingle turns text into a set of 64-bit fingerprints so that two
// requests can be compared for content overlap without retaining either.
//
// Method: whitespace-tokenize, hash overlapping k-grams (k=8) with FNV-1a,
// then keep only hashes that pass a content-defined sampling gate
// (h % mod == 0). Sampling is content-defined rather than positional, so an
// insertion at the start of a prompt does not shift every fingerprint —
// which is what makes "same context, one more turn appended" show up as
// ~90% overlap rather than ~0%.
//
// A Set is the only thing that persists. It cannot be inverted to text.
package shingle

import (
	"hash/fnv"
	"strings"
	"unicode"
)

const (
	K   = 8  // tokens per shingle
	Mod = 4  // keep 1 in Mod shingles (content-defined)
	Min = 12 // texts shorter than this many tokens yield a dense set instead
)

type Set map[uint64]struct{}

func tokenize(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return unicode.IsSpace(r)
	})
}

// Of fingerprints a text.
func Of(text string) Set {
	toks := tokenize(text)
	out := Set{}
	if len(toks) == 0 {
		return out
	}
	if len(toks) < K {
		h := fnv.New64a()
		for _, t := range toks {
			h.Write([]byte(t))
			h.Write([]byte{0})
		}
		out[h.Sum64()] = struct{}{}
		return out
	}
	dense := len(toks) < Min*K
	for i := 0; i+K <= len(toks); i++ {
		h := fnv.New64a()
		for _, t := range toks[i : i+K] {
			h.Write([]byte(t))
			h.Write([]byte{0})
		}
		v := h.Sum64()
		if dense || v%Mod == 0 {
			out[v] = struct{}{}
		}
	}
	return out
}

// Overlap is |a ∩ b| / |a|: the fraction of a already present in b.
func Overlap(a, b Set) float64 {
	if len(a) == 0 {
		return 0
	}
	n := 0
	for k := range a {
		if _, ok := b[k]; ok {
			n++
		}
	}
	return float64(n) / float64(len(a))
}

// Merge adds b into a.
func (a Set) Merge(b Set) {
	for k := range b {
		a[k] = struct{}{}
	}
}

// Jaccard similarity.
func Jaccard(a, b Set) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1
	}
	inter := 0
	for k := range a {
		if _, ok := b[k]; ok {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}
