package index

// The repository map: a token-budgeted, signature-level overview of the
// workspace, files ranked by how much the rest of the repository depends on
// them and each with its most telling symbols, so that an agent learns the
// shape of a codebase without reading it.
//
// The budget is a rule and not a hint: the rendered body (every `path:` header
// and every symbol line, newlines included) is at most 4 × BudgetTokens bytes,
// which is EstimateTokens(body) ≤ BudgetTokens. Files are taken in rank order
// until the next line would not fit, and then the map stops and says so.

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	defaultRepoMapBudget  = 1500
	defaultRepoMapPerFile = 8
	// repoMapTopFiles is how many of the highest-ranked files get PerFile
	// symbols; the rest get repoMapTailSymbols at most.
	repoMapTopFiles    = 20
	repoMapTailSymbols = 3
	// repoMapTextBytes bounds one symbol line's text.
	repoMapTextBytes = 160
)

// RepoMapOptions are the knobs of RepoMap.
type RepoMapOptions struct {
	// BudgetTokens caps the rendered map; 0 is 1500.
	BudgetTokens int
	// PerFile is the most symbols listed for a top-ranked file; 0 is 8.
	PerFile int
	// Path, Globs and Languages scope which files are considered.
	Path      string
	Globs     []string
	Languages []string
}

// A RepoMapSymbol is one listed symbol.
type RepoMapSymbol struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Line int    `json:"line"`
	// Text is one signature-level line: `kind Name  signature`.
	Text string `json:"text"`
}

// A RepoMapFile is one file of the map.
type RepoMapFile struct {
	Path     string          `json:"path"`
	Language string          `json:"language,omitempty"`
	Rank     float64         `json:"rank"`
	Symbols  []RepoMapSymbol `json:"symbols"`
	// SymbolTotal is how many symbols the file has, listed or not.
	SymbolTotal int `json:"symbol_total"`
}

// A RepoMapResult is the map.
type RepoMapResult struct {
	Files       []RepoMapFile `json:"files"`
	FilesTotal  int           `json:"files_total"`
	FilesListed int           `json:"files_listed"`
	Truncated   bool          `json:"truncated"`
	// TokensEstimated is EstimateTokens of the rendered body.
	TokensEstimated int `json:"tokens_estimated"`
	Budget          int `json:"budget"`
	// Ranked is false when no file had a rank and the files are ordered by
	// symbol count.
	Ranked bool `json:"ranked"`
}

// EstimateTokens is the token estimate the budget is measured in: one token
// per four bytes, rounded up.
func EstimateTokens(s string) int { return (len(s) + 3) / 4 }

// RepoMap builds the map over files. rank is a per-file centrality (missing =
// 0); the only error is a malformed glob.
func RepoMap(files []*File, rank map[string]float64, opts RepoMapOptions) (RepoMapResult, error) {
	budget := opts.BudgetTokens
	if budget <= 0 {
		budget = defaultRepoMapBudget
	}
	perFile := opts.PerFile
	if perFile <= 0 {
		perFile = defaultRepoMapPerFile
	}
	globs, err := CompileGlobs("", opts.Globs)
	if err != nil {
		return RepoMapResult{}, err
	}
	langs := map[string]bool{}
	for _, l := range opts.Languages {
		langs[strings.ToLower(l)] = true
	}
	path := strings.Trim(strings.ReplaceAll(opts.Path, "\\", "/"), "/")
	if path == "." {
		path = ""
	}

	var cands []*File
	ranked := false
	for _, f := range files {
		switch {
		case !f.HasOutline || len(f.Symbols) == 0:
			continue
		case len(langs) > 0 && !langs[strings.ToLower(f.Language)]:
			continue
		case path != "" && f.Path != path && !strings.HasPrefix(f.Path, path+"/"):
			continue
		case !globs.Allows(f.Path):
			continue
		}
		if rank[f.Path] > 0 {
			ranked = true
		}
		cands = append(cands, f)
	}
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if ra, rb := rank[a.Path], rank[b.Path]; ra != rb {
			return ra > rb
		}
		if len(a.Symbols) != len(b.Symbols) {
			return len(a.Symbols) > len(b.Symbols)
		}
		return a.Path < b.Path
	})

	res := RepoMapResult{Files: []RepoMapFile{}, FilesTotal: len(cands), Budget: budget, Ranked: ranked}
	limit := 4 * budget // bytes
	used := 0
	stopped := false
	for i, f := range cands {
		header := len(f.Path) + len(":\n")
		if used+header > limit {
			stopped = true
			break
		}
		used += header
		want := perFile
		if i >= repoMapTopFiles && want > repoMapTailSymbols {
			want = repoMapTailSymbols
		}
		out := RepoMapFile{Path: f.Path, Language: f.Language, Rank: rank[f.Path],
			Symbols: []RepoMapSymbol{}, SymbolTotal: len(f.Symbols)}
		chosen := chooseSymbols(f, want)
		for _, ci := range chosen {
			sym := repoMapSymbol(&f.Symbols[ci])
			size := len("  ") + len(sym.Text) + len("\n")
			if used+size > limit {
				stopped = true
				break
			}
			used += size
			out.Symbols = append(out.Symbols, sym)
		}
		res.Files = append(res.Files, out)
		if stopped {
			break
		}
	}
	res.FilesListed = len(res.Files)
	res.Truncated = stopped || res.FilesListed < res.FilesTotal
	res.TokensEstimated = EstimateTokens(renderRepoMapBody(res))
	return res, nil
}

// typeLike kinds are the ones whose members are worth listing.
func typeLike(kind string) bool {
	switch kind {
	case "class", "struct", "interface", "type", "enum", "module", "namespace", "package":
		return true
	}
	return false
}

func kindRank(kind string) int {
	switch {
	case typeLike(kind):
		return 0
	case kind == "function" || kind == "constructor":
		return 1
	case kind == "method":
		return 2
	}
	return 3
}

func exported(f *File, sym *Symbol) bool {
	name := displayName(sym)
	if name == "" {
		return false
	}
	r, _ := utf8.DecodeRuneInString(name)
	if f.Language == "go" {
		return unicode.IsUpper(r)
	}
	return r != '_'
}

// chooseSymbols picks up to n symbols of a file, best first by (exported,
// kind, nesting, line), and returns them in document order. Candidates are the
// top-level symbols and the direct members of type-like ones.
func chooseSymbols(f *File, n int) []int {
	type cand struct{ i, depth int }
	var cands []cand
	for i := range f.Symbols {
		depth := symbolDepth(f.Symbols, i)
		switch {
		case depth == 0:
			cands = append(cands, cand{i, 0})
		case depth == 1:
			if p := f.Symbols[i].Parent; p > 0 && p <= len(f.Symbols) && typeLike(f.Symbols[p-1].Kind) {
				cands = append(cands, cand{i, 1})
			}
		}
	}
	sort.SliceStable(cands, func(a, b int) bool {
		x, y := &f.Symbols[cands[a].i], &f.Symbols[cands[b].i]
		if ex, ey := exported(f, x), exported(f, y); ex != ey {
			return ex
		}
		if kx, ky := kindRank(x.Kind), kindRank(y.Kind); kx != ky {
			return kx < ky
		}
		if cands[a].depth != cands[b].depth {
			return cands[a].depth < cands[b].depth
		}
		return x.Line < y.Line
	})
	if len(cands) > n {
		cands = cands[:n]
	}
	idx := make([]int, len(cands))
	for i, c := range cands {
		idx[i] = c.i
	}
	sort.Slice(idx, func(a, b int) bool {
		x, y := &f.Symbols[idx[a]], &f.Symbols[idx[b]]
		if x.Line != y.Line {
			return x.Line < y.Line
		}
		return x.ID < y.ID
	})
	return idx
}

// repoMapSymbol is a symbol's line: `kind Name  signature`, one line, at most
// repoMapTextBytes.
func repoMapSymbol(sym *Symbol) RepoMapSymbol {
	text := strings.Join(strings.Fields(sym.Kind+" "+displayName(sym)), " ")
	if sig := strings.Join(strings.Fields(sym.Signature), " "); sig != "" {
		text += "  " + sig
	}
	return RepoMapSymbol{ID: sym.ID, Kind: sym.Kind, Line: sym.Line, Text: clipBytes(text, repoMapTextBytes)}
}

// clipBytes cuts s to at most n bytes on a rune boundary, marking the cut
// with "..." inside the limit.
func clipBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n - 3
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}

func renderRepoMapBody(r RepoMapResult) string {
	var b strings.Builder
	for _, f := range r.Files {
		b.WriteString(f.Path)
		b.WriteString(":\n")
		for _, s := range f.Symbols {
			b.WriteString("  ")
			b.WriteString(s.Text)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// RenderRepoMapText writes the map as plain text: a `path:` line per file and
// an indented line per symbol, then a `# ` notice when it was cut short.
func RenderRepoMapText(w io.Writer, r RepoMapResult) {
	io.WriteString(w, renderRepoMapBody(r))
	if r.Truncated {
		fmt.Fprintf(w, "# listed %d of %d files within a budget of %d tokens; raise --budget or scope with --path\n",
			r.FilesListed, r.FilesTotal, r.Budget)
	}
}
