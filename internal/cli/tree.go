package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/tanevanwifferen/Lightspeed/internal/daemon"
	"github.com/tanevanwifferen/Lightspeed/internal/index"
	"github.com/tanevanwifferen/Lightspeed/internal/render"
	"github.com/tanevanwifferen/Lightspeed/internal/router"
	"github.com/tanevanwifferen/Lightspeed/internal/symbols"
)

// `tree` and `repo_outline`: what is in the workspace, and which language
// server would handle it. Neither starts a server or a daemon; they read the
// tree, the definitions of the workspace (D17) and, for `repo_outline`, look
// for the executables — the same things `servers` does. They are for an agent
// that has just been dropped into a repository and would otherwise `ls` its
// way around (docs/DECISIONS.md D23).

const (
	// defaultTreeFiles is how many files `tree` lists unless told otherwise.
	// An agent that wants more says so; one that does not gets an answer it
	// can afford to read.
	defaultTreeFiles = 500
	// walkFileLimit bounds the fallback walk, so that a directory that is
	// not a repository — a home directory, say — is not read to the end.
	walkFileLimit = index.WalkFileLimit
)

// A fileListing is the files under a directory, and how they were found.
type fileListing struct {
	// Files are the paths relative to the workspace root, with forward
	// slashes, sorted.
	Files []string
	// Source is "git" when git said which files there are, "walk" when the
	// directory was walked.
	Source   string
	Warnings []string
}

// listWorkspaceFiles lists the files under dir, as paths relative to root.
//
// Inside a git repository it is git's own answer — the tracked files and the
// untracked ones that are not ignored (`git ls-files --cached --others
// --exclude-standard`) — so .gitignore, .git/info/exclude and the user's global
// excludes all apply without lightspeed reimplementing them. A tracked file
// that has been deleted from the worktree is left out, since nothing can be
// read from it. Anywhere else, or if git will not answer, the directory is
// walked, skipping hidden and build directories (skipDir), which is a guess at
// what an ignore file would say and is reported as one.
func listWorkspaceFiles(ctx context.Context, dir, root string) (fileListing, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fileListing{}, render.Errorf(render.CodeUsage, "resolving %s: %v", dir, err)
	}
	// Confinement comes before anything is looked at, and follows symlinks
	// (relToRoot resolves them as far as the path exists): a directory that
	// leaves the workspace is refused whether or not it exists, so the answer
	// does not say what is out there, and a link inside the workspace that
	// points out of it is out of it. root is the workspace of --path or the
	// working directory, never derived from dir itself.
	if _, in := relToRoot(root, abs); !in {
		return fileListing{}, render.Errorf(render.CodeOutsideWorkspace,
			"%s is outside the workspace %s; only directories inside it can be listed", dir, root)
	}
	info, err := os.Stat(abs)
	switch {
	case err != nil:
		return fileListing{}, render.Errorf(render.CodeNoSuchFile, "%s: no such directory", dir)
	case !info.IsDir():
		return fileListing{}, render.Errorf(render.CodeUsage, "%s is a file, not a directory", dir)
	}
	abs = canonPath(abs)
	dirRel, _ := relToRoot(root, abs)

	var listing fileListing
	names, err := gitFiles(ctx, abs)
	if err == nil {
		listing.Source = "git"
	} else {
		listing.Source = "walk"
		if err != errNoGit && err != errNotARepo {
			listing.Warnings = append(listing.Warnings, "git would not list the files ("+err.Error()+"), so the directory was walked")
		} else {
			listing.Warnings = append(listing.Warnings,
				"not a git repository, so .gitignore was not applied; hidden and build directories were skipped instead")
		}
		var capped bool
		names, capped = walkFiles(ctx, abs)
		if capped {
			listing.Warnings = append(listing.Warnings, fmt.Sprintf("the walk stopped after %d files", walkFileLimit))
		}
	}

	seen := make(map[string]bool, len(names))
	for _, name := range names {
		full := filepath.Join(abs, filepath.FromSlash(name))
		if st, err := os.Stat(full); err != nil || st.IsDir() {
			continue
		}
		rel := name
		if dirRel != "." {
			rel = path.Join(dirRel, name)
		}
		if !seen[rel] {
			seen[rel] = true
			listing.Files = append(listing.Files, rel)
		}
	}
	sort.Strings(listing.Files)
	return listing, nil
}

// gitFiles is git's list of the files under dir, relative to dir. The
// enumeration itself is internal/index's, which the index revalidates with, so
// that a file is indexed exactly when `tree` lists it.
func gitFiles(ctx context.Context, dir string) ([]string, error) {
	names, err := index.GitFiles(ctx, dir)
	switch {
	case errors.Is(err, index.ErrNoGit):
		return nil, errNoGit
	case errors.Is(err, index.ErrNotARepo):
		return nil, errNotARepo
	}
	return names, err
}

// walkFiles is the fallback: every file under dir except in hidden and build
// directories, relative to dir with forward slashes.
func walkFiles(ctx context.Context, dir string) (names []string, capped bool) {
	return index.WalkFiles(ctx, dir)
}

// --- tree ---

// A treeFile is one file of `tree`.
type treeFile struct {
	Path string `json:"path"`
	// Language is the language id of the file, when it has one.
	Language string `json:"language,omitempty"`
	// Server is the server the definitions say would handle the file, when
	// one does. It is named whether or not it is installed: `repo_outline`
	// says that.
	Server string `json:"server,omitempty"`
}

// treeData is the payload of `tree`.
type treeData struct {
	// Root is the workspace root every path is relative to.
	Root string `json:"root"`
	// Dir is the listed directory relative to Root, when it is not the root.
	Dir    string     `json:"dir,omitempty"`
	Source string     `json:"source"`
	Prefix string     `json:"prefix,omitempty"`
	Files  []treeFile `json:"files"`
	// Count is how many files this output lists, Total how many matched
	// before --max-files. They differ exactly when Truncated is set.
	Count     int  `json:"count"`
	Total     int  `json:"total"`
	Truncated bool `json:"truncated"`
	MaxFiles  int  `json:"max_files,omitempty"`
}

// dirArgument reads the optional directory positional of `tree` and
// `repo_outline`, and the workspace it must be inside. The workspace is the
// one --path (default the working directory) belongs to, exactly as for
// `file`; it is not derived from the directory, which would make the
// confinement check compare a directory with itself. Without a positional the
// directory is --path itself.
func dirArgument(pathFlag string, positional []string) (dir, root string, err error) {
	if root, err = commandWorkspace(pathFlag); err != nil {
		return "", "", err
	}
	dir = pathFlag
	if len(positional) == 1 {
		dir = positional[0]
	}
	return dir, root, nil
}

// pathFlagUsage is the help of --path on `tree` and `repo_outline`.
const pathFlagUsage = "any directory inside the workspace; the listed directory must be inside it too"

// limitOverride is the bound a command's own flag (--max-files, --max-dirs)
// sets, unless the caller also gave --limit: the common flag every list command
// answers to wins, and 0 lifts the bound (D42).
func limitOverride(fset *flag.FlagSet, common *commonFlags, own int) int {
	given := false
	if fset != nil {
		fset.Visit(func(f *flag.Flag) { given = given || f.Name == "limit" })
	}
	switch {
	case !given:
		return own
	case common.limit == 0:
		return math.MaxInt
	}
	return common.limit
}

// treeCommand implements `lightspeed tree [dir]`: the files of the workspace,
// gitignore-aware, with the language and server of each.
func treeCommand(e *env, c *command, args []string) int {
	var (
		prefix   string
		maxFiles int
		pathFlag string
		fset     *flag.FlagSet
	)
	common, positional, err := parseFlagsRange(e, c, args, 0, 1, func(fs *flag.FlagSet) {
		fset = fs
		fs.StringVar(&pathFlag, "path", ".", pathFlagUsage)
		fs.StringVar(&prefix, "prefix", "", "only files whose workspace-relative path starts with this")
		fs.IntVar(&maxFiles, "max-files", defaultTreeFiles, "list at most this many files; truncation is always reported (--limit is the same bound, and wins when both are given; 0 lifts it)")
	})
	if err != nil {
		return e.flagError(err)
	}
	format, err := managementFormat(common, "tree", e.stdout)
	if err != nil {
		return e.fail(err)
	}
	if maxFiles < 1 {
		return e.usagef("tree: --max-files must be at least 1 (got %d)", maxFiles)
	}
	maxFiles = limitOverride(fset, common, maxFiles)
	dir, root, err := dirArgument(pathFlag, positional)
	if err != nil {
		return e.fail(err)
	}

	ctx, cancel := context.WithTimeout(e.base(), common.timeout)
	defer cancel()
	listing, err := listWorkspaceFiles(ctx, dir, root)
	if err != nil {
		return e.fail(err)
	}
	cfg, err := e.configForRoot(root)
	if err != nil {
		return e.fail(err)
	}

	var matched []string
	for _, f := range listing.Files {
		if strings.HasPrefix(f, prefix) {
			matched = append(matched, f)
		}
	}
	data := treeData{Root: root, Source: listing.Source, Prefix: prefix, Total: len(matched), MaxFiles: maxFiles}
	if maxFiles == math.MaxInt {
		data.MaxFiles = 0
	}
	if abs, err := filepath.Abs(dir); err == nil {
		if rel, in := relToRoot(root, abs); in && rel != "." {
			data.Dir = rel
		}
	}
	warnings := slices.Clone(listing.Warnings)
	if len(matched) > maxFiles {
		matched = matched[:maxFiles]
		data.Truncated = true
		warnings = append(warnings, fmt.Sprintf(
			"listed %d of %d files; narrow it with --prefix or a directory, or raise --max-files", maxFiles, data.Total))
	}
	data.Files = make([]treeFile, 0, len(matched))
	for _, f := range matched {
		data.Files = append(data.Files, describeFile(cfg.router, root, f))
	}
	data.Count = len(data.Files)

	if format == render.FormatText {
		writeTreeText(e.stdout, data)
		return ExitOK
	}
	return e.writeData(common, data, warnings, ExitOK)
}

// describeFile names a file's language and the server that would handle it.
func describeFile(r *router.Router, root, rel string) treeFile {
	abs := filepath.Join(root, filepath.FromSlash(rel))
	out := treeFile{Path: rel, Language: router.LanguageID(abs)}
	if matches, err := r.Resolve(abs); err == nil && len(matches) > 0 {
		out.Server = matches[0].Server.Name
		if out.Language == "" {
			out.Language = matches[0].LanguageID
		}
	}
	return out
}

// writeTreeText is one file per line: the path, then what handles it.
func writeTreeText(w io.Writer, d treeData) {
	for _, f := range d.Files {
		fmt.Fprint(w, f.Path)
		if f.Language != "" || f.Server != "" {
			fmt.Fprintf(w, "  (%s", f.Language)
			if f.Server != "" {
				fmt.Fprintf(w, ", %s", f.Server)
			}
			fmt.Fprint(w, ")")
		}
		fmt.Fprintln(w)
	}
	if d.Truncated {
		fmt.Fprintf(w, "# %d of %d files; --max-files raises the limit\n", d.Count, d.Total)
	}
}

// --- repo_outline ---

const (
	defaultOutlineDepth = 2
	defaultOutlineDirs  = 200
)

// A repoDir is one directory of `repo_outline`, with the files under it.
type repoDir struct {
	Path string `json:"path"`
	// Files counts every file at or below the directory, so a parent's
	// count includes its children's.
	Files     int            `json:"files"`
	Languages map[string]int `json:"languages,omitempty"`
	// Symbols counts the symbols of the files at or below the directory that
	// the index holds current outlines of; absent when the index is cold.
	Symbols int `json:"symbols,omitempty"`
}

// A repoLanguage is one language of the repository.
type repoLanguage struct {
	Language string `json:"language"`
	Files    int    `json:"files"`
	// Server is the server that handles the language's files, when one does.
	Server string `json:"server,omitempty"`
}

// A repoServer is a server the repository's files resolve to, and whether it
// can be run.
type repoServer struct {
	Name      string   `json:"name"`
	Languages []string `json:"languages"`
	Files     int      `json:"files"`
	Installed bool     `json:"installed"`
	// Path is where the executable was found, when it was.
	Path string `json:"path,omitempty"`
	// Install is the command that installs it, when it is not installed and
	// the definition says how.
	Install string `json:"install,omitempty"`
}

// repoOutlineData is the payload of `repo_outline`.
type repoOutlineData struct {
	Root   string `json:"root"`
	Dir    string `json:"dir,omitempty"`
	Source string `json:"source"`
	Files  int    `json:"files"`
	// Directories are the directories down to Depth levels below the listed
	// one, each with its file count, in path order.
	Directories      []repoDir      `json:"directories"`
	TotalDirectories int            `json:"total_directories"`
	Truncated        bool           `json:"truncated"`
	Depth            int            `json:"depth"`
	Languages        []repoLanguage `json:"languages"`
	// Unrecognised counts the files with no language id.
	Unrecognised int          `json:"unrecognised,omitempty"`
	Servers      []repoServer `json:"servers"`
	// Symbols is what the workspace index knows, when it has anything current
	// (D32): this command reads it and never builds it.
	Symbols *repoSymbols `json:"symbols,omitempty"`
}

// A repoSymbols is the warm part of the workspace index for `repo_outline`.
type repoSymbols struct {
	// Files is how many files under the directory have a current outline and
	// Total how many symbols they hold, ByKind by kind.
	Files  int            `json:"files"`
	Total  int            `json:"total"`
	ByKind map[string]int `json:"by_kind"`
	// NotCurrent is how many files a server handles that have no current
	// outline — never indexed, or changed since — and so are not counted.
	NotCurrent int `json:"not_current"`
}

// repoOutlineCommand implements `lightspeed repo_outline [dir]`.
func repoOutlineCommand(e *env, c *command, args []string) int {
	var (
		depth, maxDirs int
		pathFlag       string
		fset           *flag.FlagSet
	)
	common, positional, err := parseFlagsRange(e, c, args, 0, 1, func(fs *flag.FlagSet) {
		fset = fs
		fs.StringVar(&pathFlag, "path", ".", pathFlagUsage)
		fs.IntVar(&depth, "depth", defaultOutlineDepth, "list directories down to this many levels below the directory")
		fs.IntVar(&maxDirs, "max-dirs", defaultOutlineDirs, "list at most this many directories; truncation is always reported (--limit is the same bound, and wins when both are given; 0 lifts it)")
	})
	if err != nil {
		return e.flagError(err)
	}
	format, err := managementFormat(common, "repo_outline", e.stdout)
	if err != nil {
		return e.fail(err)
	}
	if depth < 1 || maxDirs < 1 {
		return e.usagef("repo_outline: --depth and --max-dirs must be at least 1")
	}
	maxDirs = limitOverride(fset, common, maxDirs)
	dir, root, err := dirArgument(pathFlag, positional)
	if err != nil {
		return e.fail(err)
	}

	ctx, cancel := context.WithTimeout(e.base(), common.timeout)
	defer cancel()
	listing, err := listWorkspaceFiles(ctx, dir, root)
	if err != nil {
		return e.fail(err)
	}
	cfg, err := e.configForRoot(root)
	if err != nil {
		return e.fail(err)
	}

	abs := canonPath(dir)
	base, _ := relToRoot(root, abs)
	data := repoOutlineData{Root: root, Source: listing.Source, Files: len(listing.Files), Depth: depth}
	if base != "." {
		data.Dir = base
	}

	dirs := map[string]*repoDir{}
	touch := func(p string) *repoDir {
		d, ok := dirs[p]
		if !ok {
			d = &repoDir{Path: p, Languages: map[string]int{}}
			dirs[p] = d
		}
		return d
	}
	// The warm part of the index, read from what the daemon (or an earlier
	// command) persisted: never built for this command, so this stays as fast
	// and as server-free as it was.
	counts := warmIndexCounts(ctx, cfg, root)
	var sums *repoSymbols
	if counts != nil && counts.Warm {
		sums = &repoSymbols{ByKind: map[string]int{}}
	}
	langFiles := map[string]int{}
	firstOf := map[string]string{}
	for _, f := range listing.Files {
		lang := router.LanguageID(filepath.Join(root, filepath.FromSlash(f)))
		if lang == "" {
			data.Unrecognised++
		} else {
			langFiles[lang]++
			if _, ok := firstOf[lang]; !ok {
				firstOf[lang] = f
			}
		}
		// The directories of the file below dir, to depth levels.
		below := strings.TrimPrefix(strings.TrimPrefix(f, base), "/")
		if base == "." {
			below = f
		}
		parts := strings.Split(below, "/")
		fc, warmFile := counts.fileCount(f)
		touchDir := func(p string) {
			d := touch(p)
			d.Files++
			d.Symbols += fc.Symbols
			if lang != "" {
				d.Languages[lang]++
			}
		}
		if sums != nil && warmFile {
			sums.Files++
			sums.Total += fc.Symbols
			for k, n := range fc.Kinds {
				sums.ByKind[k] += n
			}
		}
		touchDir(".")
		for i := 1; i < len(parts) && i <= depth; i++ {
			touchDir(strings.Join(parts[:i], "/"))
		}
	}
	data.TotalDirectories = len(dirs)
	paths := make([]string, 0, len(dirs))
	for p := range dirs {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	if len(paths) > maxDirs {
		paths = paths[:maxDirs]
		data.Truncated = true
	}
	for _, p := range paths {
		d := dirs[p]
		if len(d.Languages) == 0 {
			d.Languages = nil
		}
		data.Directories = append(data.Directories, *d)
	}

	// Which server each language resolves to is decided by one
	// representative file: the definitions claim files by language and glob,
	// and resolving every file would cost a walk up the tree for each.
	warnings := slices.Clone(listing.Warnings)
	servers := map[string]*repoServer{}
	langs := make([]string, 0, len(langFiles))
	for lang := range langFiles {
		langs = append(langs, lang)
	}
	sort.Strings(langs)
	for _, lang := range langs {
		entry := repoLanguage{Language: lang, Files: langFiles[lang]}
		abs := filepath.Join(root, filepath.FromSlash(firstOf[lang]))
		if matches, err := cfg.router.Resolve(abs); err == nil && len(matches) > 0 {
			name := matches[0].Server.Name
			entry.Server = name
			srv, ok := servers[name]
			if !ok {
				srv = &repoServer{Name: name}
				servers[name] = srv
			}
			srv.Languages = append(srv.Languages, lang)
			srv.Files += entry.Files
		}
		data.Languages = append(data.Languages, entry)
	}
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		srv := servers[name]
		rs, err := cfg.probe(ctx, name)
		if err != nil {
			return e.fail(err)
		}
		srv.Installed = rs.Installed()
		if srv.Installed {
			srv.Path = rs.Binary.Path
		} else if cmd := rs.InstallCommand(); len(cmd) > 0 {
			srv.Install = strings.Join(cmd, " ")
		}
		data.Servers = append(data.Servers, *srv)
	}
	if data.Truncated {
		warnings = append(warnings, fmt.Sprintf("listed %d of %d directories; narrow it with a directory or a smaller --depth, or raise --max-dirs",
			maxDirs, data.TotalDirectories))
	}
	if sums != nil {
		inScope := map[string]bool{}
		for _, f := range listing.Files {
			inScope[f] = true
		}
		for _, p := range counts.NotCurrentPaths {
			if inScope[p] {
				sums.NotCurrent++
			}
		}
		data.Symbols = sums
		if sums.NotCurrent > 0 {
			warnings = append(warnings, fmt.Sprintf("%d files have no current outline in the index (new, or changed since it was built), so their symbols are not counted; `index build` refreshes it", sums.NotCurrent))
		}
	}

	if format == render.FormatText {
		writeRepoOutlineText(e.stdout, data)
		return ExitOK
	}
	return e.writeData(common, data, warnings, ExitOK)
}

// writeRepoOutlineText is the human reading of `repo_outline`.
func writeRepoOutlineText(w io.Writer, d repoOutlineData) {
	fmt.Fprintf(w, "%s: %d files", d.Root, d.Files)
	if d.Symbols != nil {
		fmt.Fprintf(w, ", %d symbols in %d indexed files", d.Symbols.Total, d.Symbols.Files)
	}
	fmt.Fprintln(w)
	for _, dir := range d.Directories {
		fmt.Fprintf(w, "  %-40s %6d", dir.Path+"/", dir.Files)
		if d.Symbols != nil {
			fmt.Fprintf(w, " %7s", fmt.Sprintf("%d sym", dir.Symbols))
		}
		langs := make([]string, 0, len(dir.Languages))
		for l := range dir.Languages {
			langs = append(langs, l)
		}
		sort.Strings(langs)
		for _, l := range langs {
			fmt.Fprintf(w, "  %s:%d", l, dir.Languages[l])
		}
		fmt.Fprintln(w)
	}
	if d.Truncated {
		fmt.Fprintf(w, "  # %d of %d directories\n", len(d.Directories), d.TotalDirectories)
	}
	for _, s := range d.Servers {
		state := "missing"
		if s.Installed {
			state = "installed"
		}
		fmt.Fprintf(w, "server %-16s %-9s %s (%d files)", s.Name, state, strings.Join(s.Languages, ","), s.Files)
		if s.Install != "" {
			fmt.Fprintf(w, "  install: %s", s.Install)
		}
		fmt.Fprintln(w)
	}
}

// staticBackend is the index's Backend for a command that only reads what is
// already persisted: it can say which server handles a file and what that
// server's executable is, and it cannot outline anything.
type staticBackend struct {
	r    *router.Router
	root string
}

var errStaticBackend = errors.New("this command reads the index and does not build it")

func (b staticBackend) Server(rel string) string {
	if def := b.r.Claim(b.root, rel); def != nil {
		return def.Name
	}
	return ""
}

func (b staticBackend) ServerKey(server string) string {
	for _, def := range b.r.Servers() {
		if def.Name == server {
			return daemon.ExecutableKey(def)
		}
	}
	return ""
}

func (staticBackend) Ready(context.Context, string, string) (string, error) {
	return "", errStaticBackend
}

func (staticBackend) Outline(context.Context, string, string, []byte) ([]symbols.Symbol, error) {
	return nil, errStaticBackend
}

// warmIndexCounts reads the symbol counts of the workspace's persisted index,
// or nil when there is none (or it is unusable). It touches no server and no
// daemon, and builds nothing.
func warmIndexCounts(ctx context.Context, cfg *workspaceConfig, root string) *warmCounts {
	dir, err := daemon.IndexCacheDir(root)
	if err != nil {
		return nil
	}
	if _, err := os.Stat(dir); err != nil {
		return nil
	}
	m := index.New(index.Options{Root: root, CacheDir: dir, Build: daemon.SelfBuild().ID(),
		Backend: staticBackend{r: cfg.router, root: root}})
	c, err := m.Counts(ctx)
	if err != nil {
		return nil
	}
	return &warmCounts{Counts: c}
}

// warmCounts is index.Counts with a nil-safe lookup.
type warmCounts struct{ *index.Counts }

func (w *warmCounts) fileCount(rel string) (index.FileCount, bool) {
	if w == nil || w.Counts == nil {
		return index.FileCount{}, false
	}
	fc, ok := w.Files[rel]
	return fc, ok
}
