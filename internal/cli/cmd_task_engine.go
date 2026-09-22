package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/tanevanwifferen/Lightspeed/internal/daemon"
	"github.com/tanevanwifferen/Lightspeed/internal/index"
)

// The engine of `task_context` (docs/DECISIONS.md D41): rank symbols against
// the extracted terms, judge how far to believe the ranking, and expand the best
// few into the neighbourhood an agent would otherwise read three files to find.

// Match quality of a hit, from the index's tiers: the order exact > prefix >
// token is a guarantee of the search, the numbers here are how much each tier
// is worth against the weight of the term that produced it.
//
// Two more are judged here rather than by the search, because its token tier
// does not say WHERE the token was found: matchNameToken is a task word that is
// one of the words of the symbol's own name (idle in Pool.ReapIdle), which says
// nearly as much as a prefix does, and matchPath a word of the path of the
// symbol's file (daemon in internal/daemon/pool.go), which says as little as a
// word of its doc comment.
var matchQuality = map[string]float64{
	index.MatchExact:  1.0,
	index.MatchPrefix: 0.6,
	matchNameToken:    0.6,
	index.MatchToken:  0.3,
	matchPath:         0.3,
}

const (
	matchNameToken = "name_token"
	matchPath      = "path"
)

const (
	// perTermHits is how many hits of each term's search are considered.
	perTermHits = 15
	// pathBoost multiplies the score of a symbol in a file the task names.
	pathBoost = 1.5
	// pathFileScore is what a symbol of a named file scores with no other
	// evidence: enough to be listed, well below any name match.
	pathFileScore = 0.5
	// pathFileSymbols is how many top-level symbols of a named file are added.
	pathFileSymbols = 6
	// testPenalty multiplies the score of a symbol in a test file, unless the
	// task is about tests. A test's name is a sentence about the feature
	// (TestPoolReapIdleLeavesOtherServersAlone), so it collects more of a task's
	// words than the code it tests does; halved, it ranks below that code.
	testPenalty = 0.5
	// relevanceFloor drops a candidate scoring under this fraction of the best
	// one: a weak match next to a strong one is padding, not context.
	relevanceFloor = 0.15
	// clearMargin is how many times the score of any other exactly matching
	// symbol the best one must have, and leadMargin how many times the
	// runner-up's, for high confidence: a symbol named like the task's
	// identifier is THE match unless something else is named just as well or
	// scores nearly as much on other evidence.
	clearMargin = 1.5
	leadMargin  = 1.25
)

// A taskCand is one candidate symbol and the evidence for it.
type taskCand struct {
	file, lang string
	sym        index.Symbol
	score      float64
	// raw is the score from name evidence alone, before the file and test
	// adjustments: raw / the weight of all terms is how much of the task the
	// symbol accounts for.
	raw      float64
	bm25     float64
	evidence []string
	// strongExact / strongPrefix: an identifier-like term matched the name
	// exactly / as a prefix. exactWord: a plain word matched a name exactly.
	strongExact, strongPrefix, exactWord bool
	// matched is the quality each matching term matched at, and named the terms
	// among them that match the symbol's own name (exactly, as a prefix or as one
	// of its words); matchEv says there was any such evidence at all, as opposed
	// to a file the task named.
	matched map[string]float64
	named   map[string]bool
	matchEv bool
	inFile  bool
	// demoted marks a symbol of a test file in a task that is not about tests.
	demoted bool
}

// credit records that term t matches the candidate as how, keeping the best
// reading of a term, and reports whether the term is new to the candidate.
func (c *taskCand) credit(t *taskTerm, how string) (fresh bool) {
	q := matchQuality[how]
	old, seen := c.matched[t.Term]
	if seen && old >= q {
		return false
	}
	c.matched[t.Term], c.matchEv = q, true
	c.score += (q - old) * t.Weight
	c.raw += (q - old) * t.Weight
	if how != index.MatchToken && how != matchPath {
		c.named[t.Term] = true
	}
	switch {
	case how == index.MatchExact && t.identifierLike():
		c.strongExact = true
	case how == index.MatchPrefix && t.identifierLike():
		c.strongPrefix = true
	case how == index.MatchExact:
		c.exactWord = true
	}
	// One note per term: the better reading replaces the weaker one.
	c.evidence = slices.DeleteFunc(c.evidence, func(ev string) bool {
		return strings.HasPrefix(ev, "match:") && strings.HasSuffix(ev, ":"+t.Term)
	})
	c.note(fmt.Sprintf("match:%s:%s", how, t.Term))
	return !seen
}

func (c *taskCand) note(ev string) {
	if len(c.evidence) < 4 && !slices.Contains(c.evidence, ev) {
		c.evidence = append(c.evidence, ev)
	}
}

// A pathHit is a path term resolved against the workspace.
type pathHit struct {
	term  string
	files []string // workspace-relative files
	dir   string   // workspace-relative directory, when the term named one
}

// resolvePathTerm finds what a path term names: a file or directory under the
// workspace, or files whose path ends with the term (a bare `index.go`). More
// than three suffix matches is ambiguous and is reported as such rather than
// guessed at.
func resolvePathTerm(root string, files []string, term string) (pathHit, string) {
	hit := pathHit{term: term}
	if st, err := os.Stat(filepath.Join(root, filepath.FromSlash(term))); err == nil {
		if st.IsDir() {
			hit.dir = strings.Trim(term, "/")
			return hit, ""
		}
		hit.files = []string{term}
		return hit, ""
	}
	var suffix []string
	for _, f := range files {
		if f == term || strings.HasSuffix(f, "/"+term) {
			suffix = append(suffix, f)
		}
	}
	switch {
	case len(suffix) == 0:
		return hit, fmt.Sprintf("path %q is not in the workspace", term)
	case len(suffix) > 3:
		return hit, fmt.Sprintf("path %q matches %d files (%s, …); name more of the path to use it", term, len(suffix), strings.Join(suffix[:3], ", "))
	}
	hit.files = suffix
	return hit, ""
}

// rankTask searches every symbol-search term, merges the hits into candidates
// and scores them. It fills each term's Matches and Best, and returns the
// candidates best first together with the content hashes the files were indexed
// at and the first sync report.
func rankTask(run *indexRun, root, text string, terms []taskTerm) (cands []*taskCand, hashes map[string]string, report *index.SyncReport, warnings []string, err error) {
	byID := map[string]*taskCand{}
	hashes = map[string]string{}
	get := func(file, lang string, sym index.Symbol) *taskCand {
		c := byID[sym.ID]
		if c == nil {
			c = &taskCand{file: file, lang: lang, sym: sym, matched: map[string]float64{}, named: map[string]bool{}}
			byID[sym.ID] = c
		}
		return c
	}

	for i := range terms {
		t := &terms[i]
		if t.Kind == termPath {
			continue
		}
		var out index.SearchOutcome
		if err := run.do(daemon.IndexRequest{Op: daemon.IndexOpSearch, Search: &index.SearchQuery{Text: t.Term, Limit: perTermHits}}, &out); err != nil {
			return nil, nil, nil, nil, err
		}
		if report == nil {
			report = out.Report
		}
		for f, h := range out.FileHashes {
			hashes[f] = h
		}
		for _, h := range out.Hits {
			if _, ok := matchQuality[h.Match]; !ok {
				continue
			}
			if get(h.File, h.Language, h.Symbol).credit(t, h.Match) {
				t.Matches++
			}
			c := byID[h.Symbol.ID]
			c.bm25 = max(c.bm25, h.Score)
			if better(h.Match, t.Best) {
				t.Best = h.Match
			}
		}
	}

	// A compound name is found by the words together, not by any one of them:
	// fifteen hits for "idle" need not include Pool.ReapIdle, the one symbol that
	// is also about "reap". So the words are searched once more as one query,
	// whose ranking favours a symbol holding several of them.
	var words []string
	for _, t := range terms {
		if t.Kind == termWord {
			words = append(words, t.Term)
		}
	}
	if len(words) >= 2 {
		var out index.SearchOutcome
		if err := run.do(daemon.IndexRequest{Op: daemon.IndexOpSearch, Search: &index.SearchQuery{Text: strings.Join(words, " "), Limit: perTermHits}}, &out); err != nil {
			return nil, nil, nil, nil, err
		}
		for f, h := range out.FileHashes {
			hashes[f] = h
		}
		for _, h := range out.Hits {
			c := get(h.File, h.Language, h.Symbol)
			c.bm25 = max(c.bm25, h.Score)
		}
	}

	// What each candidate says about every word, read off the candidate itself:
	// the search's tiers are about the whole name and its token tier does not say
	// whether the word was in the name or in the doc comment.
	for _, c := range byID {
		name := c.sym.Qualified
		if name == "" {
			name = c.sym.Name
		}
		nameToks := index.Tokenize(name)
		textToks := index.Tokenize(c.sym.Signature + " " + c.sym.Doc)
		pathToks := index.Tokenize(strings.TrimSuffix(c.file, filepath.Ext(c.file)))
		for i := range terms {
			t := &terms[i]
			if t.Kind != termWord {
				continue
			}
			how := ""
			switch {
			case hasWord(nameToks, t.Term):
				how = matchNameToken
			case hasWord(textToks, t.Term):
				how = index.MatchToken
			case hasWord(pathToks, t.Term):
				how = matchPath
			}
			if how == "" {
				continue
			}
			if c.credit(t, how) {
				t.Matches++
			}
			if better(how, t.Best) {
				t.Best = how
			}
		}
	}
	for _, c := range byID {
		if !c.matchEv {
			delete(byID, c.sym.ID) // found by the combined query on a fuzzy or partial token only
		}
	}

	// File terms: boost what is in the named files, and list a named file's own
	// top-level symbols when nothing else speaks for them.
	var pathTerms []pathHit
	if slices.ContainsFunc(terms, func(t taskTerm) bool { return t.Kind == termPath }) {
		listing, lerr := index.ListFiles(run.e.base(), root)
		if lerr != nil {
			warnings = append(warnings, "could not list the workspace to resolve file names in the task: "+lerr.Error())
		}
		for i := range terms {
			if terms[i].Kind != termPath {
				continue
			}
			ph, why := resolvePathTerm(root, listing.Files, terms[i].Term)
			if why != "" {
				warnings = append(warnings, why)
				continue
			}
			pathTerms = append(pathTerms, ph)
			terms[i].Matches = max(len(ph.files), 1)
			terms[i].Best = "file"
		}
	}
	for _, ph := range pathTerms {
		for _, c := range byID {
			if c.file == ph.dir || (ph.dir != "" && strings.HasPrefix(c.file, ph.dir+"/")) || slices.Contains(ph.files, c.file) {
				c.score *= pathBoost
				c.note("in_file:" + ph.term)
			}
		}
		if len(ph.files) == 0 {
			continue
		}
		var sym index.SymbolsOutcome
		if err := run.do(daemon.IndexRequest{Op: daemon.IndexOpSymbols, Symbols: &index.SymbolsQuery{Files: ph.files}}, &sym); err != nil {
			return nil, nil, nil, nil, err
		}
		if report == nil {
			report = sym.Report
		}
		for _, fs := range sym.Files {
			hashes[fs.File] = fs.Hash
			added := 0
			for _, s := range fs.Symbols {
				if s.Parent != 0 || added >= pathFileSymbols {
					continue
				}
				added++
				c := get(fs.File, fs.Language, s)
				c.inFile = true
				if !c.matchEv {
					c.score = max(c.score, pathFileScore)
				}
				c.note("in_file:" + ph.term)
			}
		}
	}

	testsMentioned := regexp.MustCompile(`(?i)\btests?\b`).MatchString(text)
	for _, c := range byID {
		if !testsMentioned && isTestPath(c.file) {
			c.score *= testPenalty
			c.demoted = true
		}
		cands = append(cands, c)
	}
	slices.SortFunc(cands, func(a, b *taskCand) int {
		switch {
		case a.score != b.score:
			if a.score > b.score {
				return -1
			}
			return 1
		case len(a.named) != len(b.named):
			// Equal evidence: the one with more of the task in its own name.
			return len(b.named) - len(a.named)
		case isMember(a.sym) != isMember(b.sym):
			// Equal evidence: the declaration ahead of a member that shares its
			// name (the type Gate before the accessor Session.Gate).
			if !isMember(a.sym) {
				return -1
			}
			return 1
		case a.bm25 != b.bm25:
			if a.bm25 > b.bm25 {
				return -1
			}
			return 1
		case a.file != b.file:
			return strings.Compare(a.file, b.file)
		}
		return a.sym.Line - b.sym.Line
	})
	return cands, hashes, report, warnings, nil
}

// hasWord reports whether a task word is one of the tokens, allowing for the
// plural: "servers" is the word of Server and "importer" that of Importers.
func hasWord(toks []string, word string) bool {
	word = strings.ToLower(word)
	for _, t := range toks {
		if t == word || t+"s" == word || t == word+"s" || t+"es" == word || t == word+"es" {
			return true
		}
	}
	return false
}

// better reports whether match quality a beats b ("" is the worst).
func better(a, b string) bool { return matchQuality[a] > matchQuality[b] }

// The confidence levels.
const (
	confHigh   = "high"
	confMedium = "medium"
	confLow    = "low"
)

// confidenceRuleShort is the rule in a line, carried by every text answer;
// the per-answer evidence is in its reason. The full rule (`--rule`) is
// confidenceRule: about 300 tokens, which is too much to repeat every time.
const confidenceRuleShort = "high: one symbol matches every term (one exactly) and clearly leads; medium: something matches, not decisively; low: no symbol is named like half the task and none matches two of its terms with one in its name, so probably not implemented here. --rule prints the full rule"

// confidenceRuleName is the JSON default for confidence_rule (D46): a bare
// identifier for the rule that ran, not its text — the reason already carries
// the per-answer evidence, so repeating even the one-line rule in every
// answer was the last thing standing between task_context and its 700-token
// budget for the JSON default. --rule restores confidenceRule in full.
const confidenceRuleName = "term-coverage-v1"

// confidenceRule is the rule that produced the confidence in full, printed on
// request (`--rule`) so that it can be checked instead of trusted.
const confidenceRule = "high: (a) a name-shaped term of the task (camelCase, snake_case, dotted, quoted, --flag) matches one symbol's name exactly, no other symbol named exactly so scores within 1.5x of it, and it scores at least 1.25x the runner-up; or (b) in a task of plain words, one symbol matches every term, at least one of them as its exact name, accounts for at least 60% of the terms' weight (an exact match counts 1, a prefix or a word of the symbol's own name 0.6, a word of its signature, doc comment or file path 0.3), and scores at least 1.25x the next symbol (a member with the same name as the declaration is not a rival). " +
	"medium: something matches, but not that decisively: an exact match that is not clearly ahead, a name-shaped term matching as a prefix, one symbol accounting for half or more of a plain-word task's weight, a compound hit (one symbol matching two or more terms, at least one of them in its own name: Pool.ReapIdle for 'reap idle language servers') without meeting (b), a single plain word matching a name exactly, or only a file the task names; the terms that match nothing are named. " +
	"low: in a task of two or more plain-word terms, no symbol accounts for half of the terms' weight and there is no compound hit (a generic word matching an unrelated symbol does not make up for a term that matches nothing), a single word matching only loosely, or nothing at all; the answer is then that the task is probably not implemented here, and the weak matches are withheld. Symbols of test files count half unless the task mentions tests."

const (
	// lowCoverage: under this share of the terms' weight accounted for by the
	// best symbol, with no compound hit, is low confidence in a task of two or
	// more terms. Exactly half is not low: one of two terms naming a symbol
	// ("test-only importers" against Manager.Importers) is something to read.
	lowCoverage = 0.5
	// highCoverage: the share the top symbol must account for, with every term
	// matched, for high confidence in a plain-word task.
	highCoverage = 0.6
)

// judgeTask decides the confidence of a ranking and says why. cands are the
// ranked candidates, best first; terms carry what the search found for each.
func judgeTask(cands []*taskCand, terms []taskTerm) (level, reason string) {
	if len(cands) == 0 {
		return confLow, "no symbol name matches any term of the task"
	}
	top := cands[0]
	if top.strongExact {
		rival := 0.0 // the best other exact match
		for _, c := range cands[1:] {
			if c.strongExact {
				rival = c.score
				break
			}
		}
		clear := rival == 0 || top.score >= clearMargin*rival
		switch {
		case clear && len(cands) == 1:
			return confHigh, fmt.Sprintf("%s matches a name in the task exactly and nothing else scored", top.sym.Qualified)
		case clear && top.score >= leadMargin*cands[1].score:
			return confHigh, fmt.Sprintf("%s matches a name in the task exactly and scores %.1f against %.1f for the next candidate", top.sym.Qualified, top.score, cands[1].score)
		}
	}
	for _, c := range cands {
		switch {
		case c.strongExact:
			return confMedium, fmt.Sprintf("%s matches a name in the task exactly but is not clearly ahead of the other candidates (%.1f against %.1f)", c.sym.Qualified, c.score, cands[0].score)
		case c.strongPrefix:
			return confMedium, fmt.Sprintf("%s starts with a name in the task; no symbol is named exactly that", c.sym.Qualified)
		}
	}
	for _, c := range cands {
		switch {
		case c.inFile && c.matchEv:
			return confMedium, fmt.Sprintf("%s is in a file the task names and matches a term of it, but not as a name", c.sym.Qualified)
		case c.inFile:
			return confMedium, "the task names a file; its symbols are listed, but nothing in the task names a symbol"
		}
	}

	// Plain words: how much of the task does one symbol account for?
	n, total := 0, 0.0
	var missing []string
	for _, t := range terms {
		if t.Kind == termPath {
			continue
		}
		n++
		total += t.Weight
		if t.Matches == 0 {
			missing = append(missing, t.Term)
		}
	}
	if n == 0 || total == 0 {
		return confLow, "the task has no term to search for"
	}
	best := cands[0]
	for _, c := range cands {
		if c.raw > best.raw {
			best = c
		}
	}
	cover := best.raw / total
	if n == 1 {
		for _, c := range cands {
			if c.exactWord {
				return confMedium, fmt.Sprintf("%s is named exactly like the one word of the task; a single word is weak evidence on its own", c.sym.Qualified)
			}
		}
		return confLow, fmt.Sprintf("only single loose token matches (best: %s, from one term)", best.sym.Qualified)
	}
	// Low is the claim that the task is not implemented here, and an agent acts
	// on it by writing the code anew, so it needs the absence of BOTH kinds of
	// evidence: no symbol named like half of the task, and no compound hit, a
	// symbol whose own name holds one of the task's words and which speaks for a
	// second one as well (Pool.ReapIdle for "reap idle language servers"). The
	// share alone is diluted by every generic word of a short task ("language",
	// "servers", "code"), which is how a feature that exists used to come out low.
	var compound *taskCand
	for _, c := range cands {
		if len(c.named) == 0 || len(c.matched) < 2 {
			continue
		}
		// The code ahead of its tests, then the most of the task in the name.
		if compound == nil || compound.demoted && !c.demoted ||
			compound.demoted == c.demoted && (len(c.named) > len(compound.named) ||
				len(c.named) == len(compound.named) && c.raw > compound.raw) {
			compound = c
		}
	}
	if compound == nil && cover < lowCoverage {
		why := fmt.Sprintf("no symbol matches two of the task's %d terms with one of them in its name, and the best symbol (%s) accounts for %.0f%% of their weight", n, best.sym.Qualified, cover*100)
		if len(missing) > 0 {
			why += fmt.Sprintf("; %s matches no symbol", strings.Join(missing, ", "))
		}
		return confLow, why
	}
	rival := 0.0 // the best other symbol; a member sharing the top's name is its accessor, not a rival
	for _, c := range cands[1:] {
		if !isMember(top.sym) && isMember(c.sym) && c.sym.Name == top.sym.Name {
			continue
		}
		rival = c.score
		break
	}
	if len(top.matched) == n && top.exactWord && top.raw/total >= highCoverage && top.score >= leadMargin*rival {
		return confHigh, fmt.Sprintf("%s matches all %d terms of the task, one of them exactly, and scores %.1f against %.1f for the next candidate", top.sym.Qualified, n, top.score, rival)
	}
	why := fmt.Sprintf("%s accounts for %.0f%% of the task's terms (%d of %d matched), not decisively enough to be the answer", best.sym.Qualified, cover*100, len(best.matched), n)
	if compound != nil {
		why = fmt.Sprintf("%s matches %d of the task's %d terms (%s), %d of them in its name, not decisively enough to be the answer", compound.sym.Qualified, len(compound.matched), n, strings.Join(sortedKeys(compound.matched), ", "), len(compound.named))
	}
	if len(missing) > 0 {
		why += fmt.Sprintf("; %s matches no symbol, so that part may not exist yet", strings.Join(missing, ", "))
	}
	return confMedium, why
}

// wholeWord reports whether s[i:j] is a whole word in line.
func wholeWord(line string, i, j int) bool {
	isW := func(b byte) bool {
		return b == '_' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= 0x80
	}
	return (i == 0 || !isW(line[i-1])) && (j >= len(line) || !isW(line[j]))
}

// nameColumn finds where a symbol's name is inside its declaration, so that a
// position query lands on the name and not on the `func` keyword: the first
// whole-word occurrence of the last segment of the name in the declaration's
// first lines. It returns the 1-based line and byte column, or the declaration
// line and column 1 when the name is not there.
func nameColumn(root string, sym index.Symbol, file string) (line, col int) {
	line, col = sym.Line, 1
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(file)))
	if err != nil {
		return
	}
	name := sym.Name
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Trim(name, "()*")
	if name == "" {
		return
	}
	lines := strings.Split(string(data), "\n")
	for l := sym.Line; l <= min(sym.Line+3, max(sym.EndLine, sym.Line)) && l <= len(lines); l++ {
		text := lines[l-1]
		for from := 0; from < len(text); {
			i := strings.Index(text[from:], name)
			if i < 0 {
				break
			}
			i += from
			if wholeWord(text, i, i+len(name)) {
				return l, i + 1
			}
			from = i + 1
		}
	}
	return
}

// relPathSort orders related rows deterministically.
func relPathSort(a, b taskRelated) int {
	if c := strings.Compare(a.File, b.File); c != 0 {
		return c
	}
	return a.Line - b.Line
}

// isMember reports whether a symbol is qualified by another (Session.Gate as
// against Gate). The server's container is empty for a Go method, so the id's
// qualified name is what says so.
func isMember(s index.Symbol) bool {
	return s.Container != "" || s.Qualified != "" && s.Qualified != s.Name
}
