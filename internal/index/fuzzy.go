package index

import (
	"sort"
	"strings"
)

// Fuzzy matching is the last resort of a symbol search, used only when
// nothing matched at all and only when the caller asked for it: trigram
// Jaccard similarity and Damerau-Levenshtein distance over the normalised name.

// fuzzyTrigramMin is the Jaccard similarity at which a name is accepted as
// a fuzzy match on trigrams alone.
const fuzzyTrigramMin = 0.3

// nearestLimit is how many nearest names an empty answer lists.
const nearestLimit = 5

// editDistance is the Damerau-Levenshtein distance (optimal string alignment:
// an adjacent transposition costs one edit, the commonest typo) between two
// strings, in runes.
func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	if len(ra) == 0 {
		return len(rb)
	}
	if len(rb) == 0 {
		return len(ra)
	}
	d := make([][]int, len(ra)+1)
	for i := range d {
		d[i] = make([]int, len(rb)+1)
		d[i][0] = i
	}
	for j := 0; j <= len(rb); j++ {
		d[0][j] = j
	}
	for i := 1; i <= len(ra); i++ {
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			d[i][j] = min(d[i-1][j]+1, d[i][j-1]+1, d[i-1][j-1]+cost)
			if i > 1 && j > 1 && ra[i-1] == rb[j-2] && ra[i-2] == rb[j-1] {
				d[i][j] = min(d[i][j], d[i-2][j-2]+1)
			}
		}
	}
	return d[len(ra)][len(rb)]
}

// trigrams is the set of three-rune windows of s. A string shorter than
// three runes is its own single "trigram", so short names can still match.
func trigrams(s string) map[string]struct{} {
	r := []rune(s)
	out := map[string]struct{}{}
	if len(r) == 0 {
		return out
	}
	if len(r) < 3 {
		out[s] = struct{}{}
		return out
	}
	for i := 0; i+3 <= len(r); i++ {
		out[string(r[i:i+3])] = struct{}{}
	}
	return out
}

// jaccard is |a ∩ b| / |a ∪ b|.
func jaccard(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	small, large := a, b
	if len(small) > len(large) {
		small, large = large, small
	}
	shared := 0
	for k := range small {
		if _, ok := large[k]; ok {
			shared++
		}
	}
	return float64(shared) / float64(len(a)+len(b)-shared)
}

// fuzzyScore says whether want (normalised) is a fuzzy match of name
// (normalised) and how good: in (0, 1], 0 for no match.
func fuzzyScore(wantStr string, wantTri map[string]struct{}, name string) float64 {
	dist := editDistance(wantStr, name)
	longest := max(len([]rune(wantStr)), len([]rune(name)))
	limit := max(1, len([]rune(wantStr))/4)
	sim := jaccard(wantTri, trigrams(name))
	if dist > limit && sim < fuzzyTrigramMin {
		return 0
	}
	closeness := 1 - float64(dist)/float64(max(longest, 1))
	if closeness < 0 {
		closeness = 0
	}
	score := (closeness + sim) / 2
	if score <= 0 {
		score = 0.001
	}
	return score
}

// nearestNames lists up to nearestLimit distinct names closest to want, by
// edit distance and then trigram similarity, then alphabetically.
func nearestNames(wantStr string, names []string) []string {
	wantTri := trigrams(wantStr)
	type cand struct {
		name string
		dist int
		sim  float64
	}
	seen := map[string]bool{}
	var cands []cand
	for _, n := range names {
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		norm := normalise(n)
		c := cand{n, editDistance(wantStr, norm), jaccard(wantTri, trigrams(norm))}
		// "Nearest" must be near: a name that shares nothing with the query is
		// not a suggestion, it is noise, and an empty answer that lists it reads
		// as a guess. A name qualifies when a few edits away (up to half the
		// query, at least 2) or when it shares a real fraction of its trigrams,
		// or one contains the other.
		near := c.dist <= max(2, len([]rune(wantStr))/2) || c.sim >= fuzzyTrigramMin ||
			(len(wantStr) >= 3 && (strings.Contains(norm, wantStr) || strings.Contains(wantStr, norm)))
		if near {
			cands = append(cands, c)
		}
	}
	sort.Slice(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.dist != b.dist {
			return a.dist < b.dist
		}
		if a.sim != b.sim {
			return a.sim > b.sim
		}
		return a.name < b.name
	})
	if len(cands) > nearestLimit {
		cands = cands[:nearestLimit]
	}
	var out []string
	for _, c := range cands {
		out = append(out, c.name)
	}
	return out
}
