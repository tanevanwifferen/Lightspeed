package index

// Ranked symbol search.
//
// Ranking, in the order it is applied:
//
//  1. Filters (Kinds, Languages, Globs, Path) drop symbols before anything is
//     scored. Candidates counts what passed.
//  2. Each symbol is a document of four weighted fields — its name's tokens
//     (weight 3), its qualified name's tokens (1.5), its signature's (1) and its
//     doc sentence's (1) — and is scored with BM25 (k1 = 1.2, b = 0.75) over
//     the weighted term frequencies (BM25F-style), the average length being the
//     average over every symbol of the index. The query is tokenised the same
//     way (Tokenize: camelCase and snake_case split). A candidate must match at
//     least one query token; its score is multiplied by 0.5 + 0.5 × coverage,
//     coverage being the share of distinct query tokens it matched.
//  3. Tiers make the order exact > prefix > token > fuzzy a guarantee and not
//     a likelihood. Tier 4 is an exact name match: the normalised query (case
//     and every separator removed) equals the normalised name, qualified name
//     or last segment of it; a case-sensitive equality scores higher inside the
//     tier. Tier 3 is a prefix: the normalised name (or its last segment) starts
//     with the normalised query. Tier 2 is a token match by BM25. Tier 1 is
//     fuzzy and exists only when the caller asked for it and nothing else
//     matched. The composite Score is tier + s/(1+s), s being the in-tier score,
//     so it is below the next tier's however large s is.
//  4. Within a tier: score descending, then shallower nesting, then path, line
//     and id, so the order does not depend on the order of the input files.

import (
	"math"
	"sort"
	"strings"
)

const (
	bm25K1 = 1.2
	bm25B  = 0.75

	weightName      = 3.0
	weightQualified = 1.5
	weightSignature = 1.0
	weightDoc       = 1.0

	// maxSignatureTokens bounds how much of a long signature is indexed.
	maxSignatureTokens = 40
)

// Match kinds of a Hit.
const (
	MatchExact  = "exact"
	MatchPrefix = "prefix"
	MatchToken  = "token"
	MatchFuzzy  = "fuzzy"
)

const (
	tierFuzzy  = 1
	tierToken  = 2
	tierPrefix = 3
	tierExact  = 4
)

// A SearchQuery is one symbol search.
type SearchQuery struct {
	Text string
	// Kinds, Languages, Globs and Path narrow the search before ranking. Path is
	// a workspace-relative directory or file.
	Kinds     []string
	Languages []string
	Globs     []string
	Path      string
	// Limit caps Hits; 0 is no limit.
	Limit int
	// Fuzzy allows a trigram / edit-distance fallback, used only when nothing
	// matched.
	Fuzzy bool
}

// A Hit is one matching symbol.
type Hit struct {
	File     string  `json:"file"`
	Language string  `json:"language,omitempty"`
	Symbol   Symbol  `json:"symbol"`
	Score    float64 `json:"score"`
	// Match is "exact", "prefix", "token" or "fuzzy".
	Match string `json:"match"`
}

// A SearchResult is the answer to a search.
type SearchResult struct {
	Hits []Hit `json:"hits"`
	// Total is how many symbols matched before Limit.
	Total     int  `json:"total"`
	Truncated bool `json:"truncated"`
	// UsedFuzzy is set when the hits are fuzzy matches.
	UsedFuzzy bool `json:"used_fuzzy,omitempty"`
	// Nearest are the closest symbol names when there are no hits.
	Nearest []string `json:"nearest,omitempty"`
	// Candidates is how many symbols passed the filters.
	Candidates int `json:"candidates"`
}

// doc is one symbol as the search sees it.
type doc struct {
	file  int // index into SearchIndex.files
	sym   int // index into that file's Symbols
	depth int

	nameNorm string
	lastNorm string
	qualNorm string
	name     string // display name: last segment of the qualified name
	length   float64
}

type posting struct {
	doc int32
	tf  float32
}

// A SearchIndex is a ranked-search structure over a set of files. It is
// immutable: build a new one when the files change.
type SearchIndex struct {
	files    []*File
	docs     []doc
	postings map[string][]posting
	avgLen   float64
	// byName lists doc ids sorted by nameNorm, and byLast by lastNorm, for
	// exact and prefix lookups.
	byName []int32
	byLast []int32
}

// NewSearchIndex builds the search structure over files. Files without an
// outline contribute nothing.
func NewSearchIndex(files []*File) *SearchIndex {
	s := &SearchIndex{files: files, postings: map[string][]posting{}}
	var totalLen float64
	tf := map[string]float64{}
	for fi, f := range files {
		for si := range f.Symbols {
			sym := &f.Symbols[si]
			d := doc{
				file:     fi,
				sym:      si,
				depth:    symbolDepth(f.Symbols, si),
				nameNorm: normalise(sym.Name),
				qualNorm: normalise(sym.Qualified),
				name:     displayName(sym),
			}
			d.lastNorm = normalise(d.name)
			clear(tf)
			var length float64
			add := func(toks []string, w float64) {
				for _, t := range toks {
					tf[t] += w
					length += w
				}
			}
			add(Tokenize(sym.Name), weightName)
			add(Tokenize(sym.Qualified), weightQualified)
			sig := Tokenize(sym.Signature)
			if len(sig) > maxSignatureTokens {
				sig = sig[:maxSignatureTokens]
			}
			add(sig, weightSignature)
			add(Tokenize(sym.Doc), weightDoc)
			d.length = length
			totalLen += length
			id := int32(len(s.docs))
			s.docs = append(s.docs, d)
			for t, w := range tf {
				s.postings[t] = append(s.postings[t], posting{doc: id, tf: float32(w)})
			}
		}
	}
	if len(s.docs) > 0 {
		s.avgLen = totalLen / float64(len(s.docs))
	}
	s.byName = make([]int32, len(s.docs))
	s.byLast = make([]int32, len(s.docs))
	for i := range s.docs {
		s.byName[i] = int32(i)
		s.byLast[i] = int32(i)
	}
	sort.Slice(s.byName, func(i, j int) bool {
		return s.docs[s.byName[i]].nameNorm < s.docs[s.byName[j]].nameNorm
	})
	sort.Slice(s.byLast, func(i, j int) bool {
		return s.docs[s.byLast[i]].lastNorm < s.docs[s.byLast[j]].lastNorm
	})
	return s
}

// symbolDepth is how many enclosing symbols a symbol has.
func symbolDepth(syms []Symbol, i int) int {
	depth := 0
	for p := syms[i].Parent; p > 0 && p <= len(syms) && depth < 64; p = syms[p-1].Parent {
		depth++
	}
	return depth
}

// displayName is the symbol's own name without its container: the last
// segment of the qualified name, or Name when there is none. A Go method's
// server name `(*Server).Handle` is `Handle`.
func displayName(sym *Symbol) string {
	q := sym.Qualified
	if q == "" {
		q = sym.Name
	}
	if i := strings.LastIndex(q, "."); i >= 0 && i+1 < len(q) {
		return q[i+1:]
	}
	return q
}

// prefixRange is the run of ids in order whose key has the prefix.
func (s *SearchIndex) prefixRange(order []int32, key func(*doc) string, prefix string) []int32 {
	lo := sort.Search(len(order), func(i int) bool { return key(&s.docs[order[i]]) >= prefix })
	hi := lo
	for hi < len(order) && strings.HasPrefix(key(&s.docs[order[hi]]), prefix) {
		hi++
	}
	return order[lo:hi]
}

// scoped is a query's filters compiled once.
type scoped struct {
	kinds, langs map[string]bool
	globs        *GlobSet
	path         string
	fileOK       []int8
	s            *SearchIndex
}

func newScoped(s *SearchIndex, q SearchQuery) (*scoped, error) {
	sc := &scoped{s: s, fileOK: make([]int8, len(s.files))}
	if len(q.Kinds) > 0 {
		sc.kinds = map[string]bool{}
		for _, k := range q.Kinds {
			sc.kinds[strings.ToLower(k)] = true
		}
	}
	if len(q.Languages) > 0 {
		sc.langs = map[string]bool{}
		for _, l := range q.Languages {
			sc.langs[strings.ToLower(l)] = true
		}
	}
	if len(q.Globs) > 0 {
		g, err := CompileGlobs("", q.Globs)
		if err != nil {
			return nil, err
		}
		sc.globs = g
	}
	sc.path = strings.Trim(strings.ReplaceAll(q.Path, "\\", "/"), "/")
	if sc.path == "." {
		sc.path = ""
	}
	return sc, nil
}

func (sc *scoped) file(fi int) bool {
	if v := sc.fileOK[fi]; v != 0 {
		return v > 0
	}
	f := sc.s.files[fi]
	ok := true
	switch {
	case sc.langs != nil && !sc.langs[strings.ToLower(f.Language)]:
		ok = false
	case sc.path != "" && f.Path != sc.path && !strings.HasPrefix(f.Path, sc.path+"/"):
		ok = false
	case !sc.globs.Allows(f.Path):
		ok = false
	}
	if ok {
		sc.fileOK[fi] = 1
	} else {
		sc.fileOK[fi] = -1
	}
	return ok
}

func (sc *scoped) doc(id int32) bool {
	d := &sc.s.docs[id]
	if !sc.file(d.file) {
		return false
	}
	if sc.kinds != nil && !sc.kinds[strings.ToLower(sc.s.files[d.file].Symbols[d.sym].Kind)] {
		return false
	}
	return true
}

// candidate is a scored document.
type candidate struct {
	id    int32
	tier  int
	score float64
}

// Search runs a query. The only error is a malformed glob.
func (s *SearchIndex) Search(q SearchQuery) (SearchResult, error) {
	sc, err := newScoped(s, q)
	if err != nil {
		return SearchResult{}, err
	}
	res := SearchResult{Hits: []Hit{}}
	for id := range s.docs {
		if sc.doc(int32(id)) {
			res.Candidates++
		}
	}
	qnorm := normalise(q.Text)
	qtoks := distinct(Tokenize(q.Text))
	if qnorm == "" || len(qtoks) == 0 {
		return res, nil
	}

	cands := s.rank(sc, q.Text, qnorm, qtoks)
	if len(cands) == 0 && q.Fuzzy {
		cands = s.fuzzy(sc, qnorm)
		res.UsedFuzzy = len(cands) > 0
	}
	if len(cands) == 0 {
		res.Nearest = s.nearest(sc, qnorm)
		return res, nil
	}

	sort.Slice(cands, func(i, j int) bool { return s.less(cands[i], cands[j]) })
	res.Total = len(cands)
	if q.Limit > 0 && len(cands) > q.Limit {
		cands = cands[:q.Limit]
		res.Truncated = true
	}
	for _, c := range cands {
		d := &s.docs[c.id]
		f := s.files[d.file]
		res.Hits = append(res.Hits, Hit{
			File:     f.Path,
			Language: f.Language,
			Symbol:   f.Symbols[d.sym],
			Score:    float64(c.tier) + squash(c.score),
			Match:    matchName(c.tier),
		})
	}
	return res, nil
}

func matchName(tier int) string {
	switch tier {
	case tierExact:
		return MatchExact
	case tierPrefix:
		return MatchPrefix
	case tierToken:
		return MatchToken
	}
	return MatchFuzzy
}

// squash maps a non-negative score into [0, 1), monotonically.
func squash(x float64) float64 {
	if x <= 0 {
		return 0
	}
	return math.Min(x/(1+x), 0.999999)
}

func distinct(toks []string) []string {
	seen := map[string]bool{}
	out := toks[:0:0]
	for _, t := range toks {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out
}

// less is the total order of results: tier, score, nesting, path, line, id.
func (s *SearchIndex) less(a, b candidate) bool {
	if a.tier != b.tier {
		return a.tier > b.tier
	}
	if a.score != b.score {
		return a.score > b.score
	}
	da, db := &s.docs[a.id], &s.docs[b.id]
	if da.depth != db.depth {
		return da.depth < db.depth
	}
	fa, fb := s.files[da.file], s.files[db.file]
	if fa.Path != fb.Path {
		return fa.Path < fb.Path
	}
	sa, sb := &fa.Symbols[da.sym], &fb.Symbols[db.sym]
	if sa.Line != sb.Line {
		return sa.Line < sb.Line
	}
	return sa.ID < sb.ID
}

// rank finds every document that matches by name or by token and scores it.
func (s *SearchIndex) rank(sc *scoped, text, qnorm string, qtoks []string) []candidate {
	n := float64(len(s.docs))
	bm := map[int32]float64{}
	matched := map[int32]int{}
	for _, t := range qtoks {
		post := s.postings[t]
		if len(post) == 0 {
			continue
		}
		df := float64(len(post))
		idf := math.Log(1 + (n-df+0.5)/(df+0.5))
		for _, p := range post {
			d := &s.docs[p.doc]
			tf := float64(p.tf)
			norm := 1 - bm25B + bm25B*d.length/math.Max(s.avgLen, 1e-9)
			bm[p.doc] += idf * tf * (bm25K1 + 1) / (tf + bm25K1*norm)
			matched[p.doc]++
		}
	}

	ids := map[int32]bool{}
	for id := range bm {
		ids[id] = true
	}
	for _, id := range s.prefixRange(s.byName, func(d *doc) string { return d.nameNorm }, qnorm) {
		ids[id] = true
	}
	for _, id := range s.prefixRange(s.byLast, func(d *doc) string { return d.lastNorm }, qnorm) {
		ids[id] = true
	}

	var out []candidate
	for id := range ids {
		if !sc.doc(id) {
			continue
		}
		d := &s.docs[id]
		sym := &s.files[d.file].Symbols[d.sym]
		coverage := float64(matched[id]) / float64(len(qtoks))
		score := bm[id] * (0.5 + 0.5*coverage)

		tier := tierToken
		switch {
		case qnorm == d.nameNorm || qnorm == d.lastNorm || qnorm == d.qualNorm:
			tier = tierExact
			if text == sym.Name || text == d.name || text == sym.Qualified {
				score += 1000 // case-sensitive equality wins inside the tier
			}
		case strings.HasPrefix(d.nameNorm, qnorm) || strings.HasPrefix(d.lastNorm, qnorm):
			tier = tierPrefix
			shortest := len(d.nameNorm)
			if l := len(d.lastNorm); l < shortest {
				shortest = l
			}
			score += float64(len(qnorm)) / float64(max(shortest, 1))
		}
		if tier == tierToken && matched[id] == 0 {
			continue
		}
		out = append(out, candidate{id: id, tier: tier, score: score})
	}
	return out
}

// fuzzy scores every filtered document's name against the query.
func (s *SearchIndex) fuzzy(sc *scoped, qnorm string) []candidate {
	tri := trigrams(qnorm)
	var out []candidate
	for id := range s.docs {
		if !sc.doc(int32(id)) {
			continue
		}
		d := &s.docs[id]
		best := fuzzyScore(qnorm, tri, d.lastNorm)
		if d.nameNorm != d.lastNorm {
			best = math.Max(best, fuzzyScore(qnorm, tri, d.nameNorm))
		}
		if best > 0 {
			out = append(out, candidate{id: int32(id), tier: tierFuzzy, score: best})
		}
	}
	return out
}

// nearest is the closest names among the filtered symbols.
func (s *SearchIndex) nearest(sc *scoped, qnorm string) []string {
	seen := map[string]bool{}
	var names []string
	for id := range s.docs {
		if !sc.doc(int32(id)) {
			continue
		}
		if n := s.docs[id].name; !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return nearestNames(qnorm, names)
}
