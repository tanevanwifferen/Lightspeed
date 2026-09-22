package cli

import (
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// Term extraction for `task_context` (docs/DECISIONS.md D41): what in a
// free-text task could be the name of something in the code. There is no model
// here, only rules that can be read, tested and predicted: code-shaped tokens
// are identifiers and weigh most, file paths and flags say where to look or
// what to call it, and plain words are the weakest evidence and are dropped
// when they are only English.

// The kinds of a term.
const (
	termIdentifier = "identifier" // camelCase, snake_case, dotted path, quoted name, foo()
	termPath       = "path"       // a file or directory of the workspace
	termFlag       = "flag"       // --some-flag
	termWord       = "word"       // a plain word, or a word split out of an identifier
)

// Weights: how much a match on a term of that kind counts. The absolute values
// only matter relative to one another and to the match quality of a hit
// (exact 1, prefix 0.6, token 0.3), so a single exact identifier (3) outweighs
// any number of token matches on prose words.
const (
	weightIdentifier = 3.0
	weightQuoted     = 3.5 // a name the author put in backticks or quotes
	weightDotted     = 1.5 // the qualifier segments of pkg.Type.Method
	weightFlag       = 2.0
	weightPath       = 2.5
	weightPathStem   = 1.2 // the file's base name, as a name
	weightSplit      = 0.8 // a word split out of an identifier
	weightWord       = 1.0
)

// maxTaskTerms bounds how many terms are searched; the rest are reported as
// dropped, lowest weight first.
const maxTaskTerms = 12

// A taskTerm is one extracted term and what became of it.
type taskTerm struct {
	Term   string  `json:"term"`
	Kind   string  `json:"kind"`
	Weight float64 `json:"weight"`
	// From says which rule produced it: backticks, quotes, camelCase,
	// snake_case, dotted, call, path, path stem, flag, split or prose.
	From string `json:"from"`
	// Used says what the term was used for: "symbol search" or "file boost".
	Used string `json:"used"`
	// Matches is how many symbols matched it and Best the best match quality
	// among them (exact, prefix or token), filled in by the ranking.
	Matches int    `json:"matches"`
	Best    string `json:"best,omitempty"`

	// quoted marks a term the author put in backticks or quotes.
	quoted bool
	order  int
}

// identifierLike reports whether a term is a name-shaped one, the kind whose
// exact match justifies high confidence.
func (t taskTerm) identifierLike() bool {
	return t.Kind == termIdentifier || t.Kind == termFlag
}

// taskStopwords are the words that say nothing about which code a task is
// about: English function words, and the generic verbs and nouns of a request
// ("add a function that…"). The list is deliberately short and boring; a word
// that is also a plausible identifier (handle, parse, read, write, search,
// index, limit, path) is NOT here, because dropping it would make a task about
// that code unanswerable.
var taskStopwords = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`
		a an the this that these those it its is are was were be been being am
		and or but if then else so as at by for from in into of on onto to with without
		about after before over under again also just only very too not no nor
		do does did done doing have has had having can could should would will shall may might must
		i me my we our you your they them their he she his her
		what which who whom whose where when why how there here
		some any all each every both few more most other such same than
		please want wants need needs like get gets got set sets
		add adds adding fix fixes fixing implement implements implementing make makes making
		create creates creating update updates updating change changes changing modify modifies
		use uses using used support supports allow allows ensure ensures improve improves
		refactor refactors work works working try tries
		code function functions method methods class classes file files feature features
		bug bugs issue issues problem problems thing things stuff way ways
		one two another something anything everything nothing still now even really actually
		new old current existing`) {
		m[w] = true
	}
	return m
}()

var (
	backtickSpan = regexp.MustCompile("`([^`\n]+)`")
	doubleQuoted = regexp.MustCompile(`"([^"\n]+)"`)
	singleQuoted = regexp.MustCompile(`(^|[\s(\[])'([^'\s]+)'`)
	dottedName   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)+$`)
	pathExt      = regexp.MustCompile(`(?i)\.(go|py|pyi|ts|tsx|js|jsx|mjs|rs|c|h|cc|cpp|hpp|lua|java|kt|rb|php|cs|swift|md|toml|json|yaml|yml|sh)$`)
	wordChars    = regexp.MustCompile(`^[A-Za-z0-9_]+$`)
	wordRun      = regexp.MustCompile(`[A-Za-z0-9_]+`)
)

// extractTerms reads the terms of a task, heaviest first, at most maxTaskTerms;
// dropped is how many more there were. Two occurrences of one term keep the
// heavier reading.
func extractTerms(text string) (terms []taskTerm, dropped int) {
	best := map[string]*taskTerm{}
	order := 0
	add := func(term, kind, from string, weight float64, quoted bool) {
		if term == "" {
			return
		}
		key := kind + "\x00" + strings.ToLower(term)
		if kind != termPath {
			// One symbol name, however it was spelled: an identifier and a word
			// of the same letters are the same search.
			key = "name\x00" + strings.ToLower(strings.ReplaceAll(term, "_", ""))
		}
		if old, ok := best[key]; ok {
			if weight > old.Weight {
				old.Kind, old.From, old.Weight, old.quoted = kind, from, weight, quoted
			}
			return
		}
		order++
		best[key] = &taskTerm{Term: term, Kind: kind, Weight: weight, From: from, quoted: quoted, order: order}
	}

	// Quoted spans first, and cut out of the text so their words are not read
	// twice. A span with no spaces is a name the author chose to mark; one with
	// spaces (`go test ./...`) is prose or a command line, tokenised as such.
	var quotedTokens []string
	strip := func(re *regexp.Regexp, group int) {
		text = re.ReplaceAllStringFunc(text, func(m string) string {
			inner := re.FindStringSubmatch(m)[group]
			if strings.ContainsAny(inner, " \t") {
				return " " + inner + " "
			}
			quotedTokens = append(quotedTokens, inner)
			return " "
		})
	}
	strip(backtickSpan, 1)
	strip(doubleQuoted, 1)
	strip(singleQuoted, 2)
	for _, tok := range quotedTokens {
		classifyTerm(tok, true, add)
	}
	for _, tok := range strings.Fields(text) {
		classifyTerm(tok, false, add)
	}

	for _, t := range best {
		terms = append(terms, *t)
	}
	slices.SortFunc(terms, func(a, b taskTerm) int {
		switch {
		case a.Weight != b.Weight:
			if a.Weight > b.Weight {
				return -1
			}
			return 1
		default:
			return a.order - b.order
		}
	})
	if len(terms) > maxTaskTerms {
		dropped = len(terms) - maxTaskTerms
		terms = terms[:maxTaskTerms]
	}
	for i := range terms {
		terms[i].Used = "symbol search"
		if terms[i].Kind == termPath {
			terms[i].Used = "file boost"
		}
	}
	return terms, dropped
}

// classifyTerm turns one whitespace-separated token into terms.
func classifyTerm(tok string, quoted bool, add func(term, kind, from string, weight float64, quoted bool)) {
	call := strings.HasSuffix(strings.TrimRight(tok, ".,;:!?"), "()")
	tok = strings.Trim(tok, ".,;:!?()[]{}<>\"'`")
	if tok == "" {
		return
	}
	from := func(rule string) string {
		if quoted {
			return "backticks/quotes " + rule
		}
		return rule
	}
	weigh := func(w float64) float64 {
		if quoted {
			return w + 0.5
		}
		return w
	}

	switch {
	case strings.HasPrefix(tok, "--") && len(tok) > 2:
		name := strings.ReplaceAll(strings.TrimLeft(tok, "-"), "-", "_")
		name = strings.SplitN(name, "=", 2)[0]
		if len(name) >= 2 {
			add(name, termFlag, from("flag"), weightFlag, quoted)
		}
		return
	case strings.Contains(tok, "/") || (pathExt.MatchString(tok) && !strings.HasPrefix(tok, ".")):
		p := strings.TrimPrefix(path.Clean(tok), "./")
		if p == "." || p == "" {
			return
		}
		add(p, termPath, from("path"), weightPath, quoted)
		stem := strings.TrimSuffix(path.Base(p), path.Ext(p))
		if len(stem) >= 4 && wordChars.MatchString(stem) && !taskStopwords[strings.ToLower(stem)] {
			add(stem, termWord, "path stem", weightPathStem, false)
		}
		return
	case dottedName.MatchString(tok):
		segs := strings.Split(tok, ".")
		for i, s := range segs {
			w := weightDotted
			if i == len(segs)-1 {
				w = weightIdentifier
			}
			add(s, termIdentifier, from("dotted"), weigh(w), quoted)
		}
		return
	}

	if !wordChars.MatchString(tok) {
		// Punctuation inside a word we cannot search for (a URL, an operator).
		// Its alphanumeric runs are still words.
		for _, part := range wordRun.FindAllString(tok, -1) {
			classifyTerm(part, false, add)
		}
		return
	}
	switch rule := identifierRule(tok, call, quoted); {
	case rule != "":
		w := weightIdentifier
		if quoted {
			w = weightQuoted
		}
		add(tok, termIdentifier, from(rule), w, quoted)
		for _, part := range splitIdentifier(tok) {
			if lower := strings.ToLower(part); len(lower) >= 3 && !taskStopwords[lower] {
				add(lower, termWord, "split", weightSplit, false)
			}
		}
	default:
		lower := strings.ToLower(tok)
		if len(lower) < 3 || taskStopwords[lower] || isNumber(lower) {
			return
		}
		add(lower, termWord, "prose", weightWord, false)
	}
}

// identifierRule names why a token looks like a code name, or "" when it is a
// plain word: snake_case, camelCase or PascalCase with two humps, foo(), or
// anything the author quoted.
func identifierRule(tok string, call, quoted bool) string {
	switch {
	case strings.Contains(tok, "_") && strings.Trim(tok, "_") != "":
		return "snake_case"
	case humps(tok) >= 2 || (hasLowerThenUpper(tok)):
		return "camelCase"
	case call:
		return "call"
	case quoted:
		return "quoted"
	}
	return ""
}

// humps counts the capitals that start a word: the two of ParseConfig.
func humps(s string) int {
	n := 0
	rs := []rune(s)
	for i, r := range rs {
		if unicode.IsUpper(r) && (i == 0 || unicode.IsLower(rs[i-1]) || unicode.IsDigit(rs[i-1]) ||
			(i+1 < len(rs) && unicode.IsLower(rs[i+1]) && unicode.IsUpper(rs[i-1]))) {
			n++
		}
	}
	return n
}

// hasLowerThenUpper is the camelCase test: a lower-case letter directly
// followed by a capital.
func hasLowerThenUpper(s string) bool {
	rs := []rune(s)
	for i := 1; i < len(rs); i++ {
		if unicode.IsLower(rs[i-1]) && unicode.IsUpper(rs[i]) {
			return true
		}
	}
	return false
}

// splitIdentifier splits snake_case and camelCase into words.
func splitIdentifier(s string) []string {
	var out []string
	for _, chunk := range strings.Split(s, "_") {
		rs := []rune(chunk)
		start := 0
		for i := 1; i < len(rs); i++ {
			boundary := unicode.IsLower(rs[i-1]) && unicode.IsUpper(rs[i]) ||
				unicode.IsUpper(rs[i-1]) && unicode.IsUpper(rs[i]) && i+1 < len(rs) && unicode.IsLower(rs[i+1]) ||
				unicode.IsLetter(rs[i-1]) != unicode.IsLetter(rs[i]) && unicode.IsDigit(rs[i])
			if boundary {
				out = append(out, string(rs[start:i]))
				start = i
			}
		}
		if start < len(rs) {
			out = append(out, string(rs[start:]))
		}
	}
	return out
}

func isNumber(s string) bool {
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return s != ""
}
