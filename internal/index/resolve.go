package index

import (
	"bytes"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// A Workspace is the file set an import is resolved against. Paths are
// workspace-relative with forward slashes.
type Workspace interface {
	// Has reports whether the file exists in the workspace.
	Has(path string) bool
	// Files lists every workspace file, sorted.
	Files() []string
	// Read returns a file's bytes; the resolver reads only manifests (go.mod,
	// go.work).
	Read(path string) ([]byte, error)
}

// NewFSWorkspace is a Workspace over files of the directory root; the caller
// supplies the list (the same gitignore-aware enumeration `tree` uses).
func NewFSWorkspace(root string, files []string) Workspace {
	sorted := append([]string(nil), files...)
	sort.Strings(sorted)
	set := make(map[string]bool, len(sorted))
	for _, f := range sorted {
		set[f] = true
	}
	return &fsWorkspace{root: root, files: sorted, set: set}
}

type fsWorkspace struct {
	root  string
	files []string
	set   map[string]bool
}

func (w *fsWorkspace) Has(p string) bool { return w.set[p] }
func (w *fsWorkspace) Files() []string   { return w.files }
func (w *fsWorkspace) Read(p string) ([]byte, error) {
	return os.ReadFile(filepath.Join(w.root, filepath.FromSlash(p)))
}

// Resolution categories.
const (
	CategoryWorkspace  = "workspace"
	CategoryStdlib     = "stdlib"
	CategoryThirdParty = "third-party"
	CategorySystem     = "system"
	CategoryUnresolved = "unresolved"
	// CategorySelf is an import that resolves to the importing file itself: it
	// names an item of that module, so it is no edge.
	CategorySelf = "self"
)

// A Resolution is where an import points.
type Resolution struct {
	// Targets are node ids: a package directory for Go, a file for every other
	// language. Empty when the import is external or unresolved.
	Targets []string `json:"targets,omitempty"`
	// External is true for an import that leaves the workspace (the standard
	// library, a dependency, a system header).
	External bool `json:"external,omitempty"`
	// Category is one of the Category* constants.
	Category string `json:"category"`
}

// A Resolver maps imports onto workspace files. It is built once per file set;
// everything it knows about the workspace comes from the [Workspace].
type Resolver struct {
	ws     Workspace
	files  map[string]bool
	goDirs map[string][]string // directory -> .go files in it
	byBase map[string][]string // base name -> paths, for C include suffix search
	mods   []goModule          // longest module path first
}

type goModule struct {
	path string // module path (or replaced path)
	dir  string // workspace directory
}

// NewResolver indexes the workspace's file set and reads its go.mod and go.work
// manifests.
func NewResolver(ws Workspace) *Resolver {
	r := &Resolver{
		ws:     ws,
		files:  map[string]bool{},
		goDirs: map[string][]string{},
		byBase: map[string][]string{},
	}
	var manifests []string
	for _, f := range ws.Files() {
		r.files[f] = true
		r.byBase[path.Base(f)] = append(r.byBase[path.Base(f)], f)
		switch {
		case strings.HasSuffix(f, ".go"):
			d := path.Dir(f)
			r.goDirs[d] = append(r.goDirs[d], f)
		case path.Base(f) == "go.mod" || path.Base(f) == "go.work":
			manifests = append(manifests, f)
		}
	}
	for _, m := range manifests {
		data, err := ws.Read(m)
		if err != nil {
			continue
		}
		r.readManifest(m, data)
	}
	sort.SliceStable(r.mods, func(i, j int) bool {
		if len(r.mods[i].path) != len(r.mods[j].path) {
			return len(r.mods[i].path) > len(r.mods[j].path)
		}
		return r.mods[i].path < r.mods[j].path
	})
	return r
}

// goDirective is one line of a go.mod or go.work, with `verb ( ... )` blocks
// flattened.
type goDirective struct {
	verb string
	args []string
}

func parseGoDirectives(src []byte) []goDirective {
	var out []goDirective
	block := ""
	for _, raw := range strings.Split(string(bytes.ReplaceAll(src, []byte("\r"), nil)), "\n") {
		if i := strings.Index(raw, "//"); i >= 0 {
			raw = raw[:i]
		}
		fields := strings.Fields(raw)
		if len(fields) == 0 {
			continue
		}
		switch {
		case block != "":
			if fields[0] == ")" {
				block = ""
				continue
			}
			out = append(out, goDirective{verb: block, args: fields})
		case len(fields) >= 2 && fields[len(fields)-1] == "(":
			block = fields[0]
		default:
			out = append(out, goDirective{verb: fields[0], args: fields[1:]})
		}
	}
	return out
}

func unquoteGo(s string) string { return strings.Trim(s, "\"`") }

func (r *Resolver) readManifest(file string, data []byte) {
	base := path.Dir(file)
	isMod := path.Base(file) == "go.mod"
	for _, d := range parseGoDirectives(data) {
		switch {
		case d.verb == "module" && isMod && len(d.args) >= 1:
			r.mods = append(r.mods, goModule{path: unquoteGo(d.args[0]), dir: base})
		case d.verb == "replace":
			arrow := -1
			for i, a := range d.args {
				if a == "=>" {
					arrow = i
				}
			}
			if arrow < 1 || arrow+1 >= len(d.args) {
				continue
			}
			target := unquoteGo(d.args[arrow+1])
			if !strings.HasPrefix(target, "./") && !strings.HasPrefix(target, "../") && target != "." && target != ".." {
				continue // a versioned module, not a local directory
			}
			dir := path.Join(base, target)
			if dir == ".." || strings.HasPrefix(dir, "../") {
				continue // outside the workspace
			}
			r.mods = append(r.mods, goModule{path: unquoteGo(d.args[0]), dir: dir})
		}
	}
}

// Resolve says where one import of from points.
//
// A file never imports itself: `use self::Item`, `crate::Item` written in the
// crate root, or `from . import name` in a package's __init__.py name an item
// of the importing module, not another file. Such a target is dropped, and an
// import left with no target is CategorySelf, so it is neither an edge (a lone
// self-edge would read as an import cycle) nor "unresolved".
func (r *Resolver) Resolve(from *File, ref ImportRef) Resolution {
	res := r.resolve(from, ref)
	if from.Language == "go" || len(res.Targets) == 0 {
		return res
	}
	kept := res.Targets[:0:0]
	for _, t := range res.Targets {
		if t != from.Path {
			kept = append(kept, t)
		}
	}
	if len(kept) == len(res.Targets) {
		return res
	}
	if len(kept) == 0 {
		return Resolution{Category: CategorySelf}
	}
	res.Targets = kept
	return res
}

func (r *Resolver) resolve(from *File, ref ImportRef) Resolution {
	switch from.Language {
	case "go":
		return r.resolveGo(ref.Spec)
	case "python":
		return r.resolvePython(from.Path, ref)
	case "javascript", "javascriptreact", "typescript", "typescriptreact":
		return r.resolveJS(from.Path, ref)
	case "rust":
		return r.resolveRust(from.Path, ref)
	case "c", "cpp":
		return r.resolveC(from.Path, ref)
	case "lua":
		return r.resolveLua(from.Path, ref)
	}
	return Resolution{Category: CategoryUnresolved}
}

func workspaceHit(targets ...string) Resolution {
	return Resolution{Targets: targets, Category: CategoryWorkspace}
}

func external(cat string) Resolution { return Resolution{External: true, Category: cat} }

func unresolved() Resolution { return Resolution{Category: CategoryUnresolved} }

// --- Go ---

// goPackageDir maps a Go import path to the workspace directory of a module it
// belongs to, when there is one.
func (r *Resolver) goPackageDir(spec string) (dir string, matched bool) {
	for _, m := range r.mods {
		if spec == m.path {
			return m.dir, true
		}
		if strings.HasPrefix(spec, m.path+"/") {
			return path.Join(m.dir, strings.TrimPrefix(spec, m.path+"/")), true
		}
	}
	return "", false
}

func (r *Resolver) resolveGo(spec string) Resolution {
	if dir, ok := r.goPackageDir(spec); ok {
		if len(r.goDirs[dir]) > 0 {
			return workspaceHit(dir)
		}
		return unresolved()
	}
	first, _, _ := strings.Cut(spec, "/")
	if !strings.Contains(first, ".") {
		return external(CategoryStdlib)
	}
	return external(CategoryThirdParty)
}

// --- shared helpers ---

// ancestors are dir and each of its parents up to the workspace root ".".
func ancestors(dir string) []string {
	var out []string
	for {
		out = append(out, dir)
		if dir == "." || dir == "/" || dir == "" {
			return out
		}
		dir = path.Dir(dir)
	}
}

func fileBase(p string) string {
	b := path.Base(p)
	return strings.TrimSuffix(b, path.Ext(b))
}

func dedupeSorted(in []string) []string {
	sort.Strings(in)
	out := in[:0]
	for i, s := range in {
		if i == 0 || s != in[i-1] {
			out = append(out, s)
		}
	}
	return out
}

// --- Python ---

var pythonStdlib = map[string]bool{
	"__future__": true, "abc": true, "argparse": true, "asyncio": true, "base64": true,
	"collections": true, "contextlib": true, "copy": true, "csv": true, "dataclasses": true,
	"datetime": true, "enum": true, "functools": true, "hashlib": true, "http": true, "inspect": true,
	"io": true, "itertools": true, "json": true, "logging": true, "math": true, "operator": true,
	"os": true, "pathlib": true, "random": true, "re": true, "shutil": true, "socket": true,
	"sqlite3": true, "string": true, "struct": true, "subprocess": true, "sys": true,
	"tempfile": true, "textwrap": true, "threading": true, "time": true, "traceback": true,
	"typing": true, "unittest": true, "urllib": true, "uuid": true, "warnings": true,
}

// pyModule is the file of the module parts below dir: `dir/a/b.py` or
// `dir/a/b/__init__.py`; with no parts, dir's own `__init__.py`.
func (r *Resolver) pyModule(dir string, parts []string) (string, bool) {
	p := dir
	if len(parts) > 0 {
		p = path.Join(dir, path.Join(parts...))
		if f := p + ".py"; r.files[f] {
			return f, true
		}
	}
	if f := path.Join(p, "__init__.py"); r.files[f] {
		return f, true
	}
	return "", false
}

func (r *Resolver) resolvePython(from string, ref ImportRef) Resolution {
	spec := ref.Spec
	dots := len(spec) - len(strings.TrimLeft(spec, "."))
	rest := spec[dots:]
	var parts []string
	if rest != "" {
		parts = strings.Split(rest, ".")
	}
	submodules := func(root string, base []string) []string {
		var out []string
		for _, n := range ref.Names {
			if f, ok := r.pyModule(root, append(append([]string(nil), base...), n)); ok {
				out = append(out, f)
			}
		}
		return out
	}

	if dots > 0 {
		base := path.Dir(from)
		for k := 1; k < dots; k++ {
			if base == "." {
				return unresolved()
			}
			base = path.Dir(base)
		}
		var targets []string
		if f, ok := r.pyModule(base, parts); ok {
			targets = append(targets, f)
		}
		targets = append(targets, submodules(base, parts)...)
		if len(targets) == 0 {
			return unresolved()
		}
		return workspaceHit(dedupeSorted(targets)...)
	}

	if len(parts) == 0 {
		return unresolved()
	}
	roots := ancestors(path.Dir(from))
	if !contains(roots, "src") {
		roots = append(roots, "src")
	}
	for _, root := range roots {
		for k := len(parts); k >= 1; k-- {
			f, ok := r.pyModule(root, parts[:k])
			if !ok {
				continue
			}
			targets := []string{f}
			if k == len(parts) {
				targets = append(targets, submodules(root, parts)...)
			}
			return workspaceHit(dedupeSorted(targets)...)
		}
	}
	if pythonStdlib[parts[0]] {
		return external(CategoryStdlib)
	}
	return external(CategoryThirdParty)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// --- JavaScript and TypeScript ---

var jsExts = []string{".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".json", ".mts", ".cts"}

// A specifier written with a JS extension may name a TS source (TS ESM).
var jsToTS = map[string][]string{
	".js": {".ts", ".tsx"}, ".jsx": {".tsx"}, ".mjs": {".mts"}, ".cjs": {".cts"},
}

var nodeBuiltins = map[string]bool{
	"assert": true, "buffer": true, "child_process": true, "crypto": true, "events": true, "fs": true,
	"http": true, "https": true, "net": true, "os": true, "path": true, "process": true,
	"readline": true, "stream": true, "url": true, "util": true, "zlib": true, "worker_threads": true,
}

func (r *Resolver) resolveJS(from string, ref ImportRef) Resolution {
	spec := ref.Spec
	relative := spec == "." || spec == ".." || strings.HasPrefix(spec, "./") || strings.HasPrefix(spec, "../")
	if !relative {
		if strings.HasPrefix(spec, "node:") || nodeBuiltins[spec] {
			return external(CategoryStdlib)
		}
		return external(CategoryThirdParty)
	}
	base := path.Join(path.Dir(from), spec)
	if base == ".." || strings.HasPrefix(base, "../") {
		return unresolved()
	}
	if ext := path.Ext(base); contains(jsExts, ext) && r.files[base] {
		return workspaceHit(base)
	}
	if ext := path.Ext(base); ext != "" {
		stem := strings.TrimSuffix(base, ext)
		for _, alt := range jsToTS[ext] {
			if r.files[stem+alt] {
				return workspaceHit(stem + alt)
			}
		}
	}
	for _, e := range jsExts {
		if r.files[base+e] {
			return workspaceHit(base + e)
		}
	}
	for _, e := range jsExts {
		if f := path.Join(base, "index"+e); r.files[f] {
			return workspaceHit(f)
		}
	}
	return unresolved()
}

// --- Rust ---

var rustStd = map[string]bool{"std": true, "core": true, "alloc": true, "proc_macro": true, "test": true}

// rustSelf is the module a file defines: its own file and the directory its
// child modules live in.
func rustSelf(file string) (childDir string) {
	switch fileBase(file) {
	case "mod", "lib", "main":
		return path.Dir(file)
	}
	return path.Join(path.Dir(file), fileBase(file))
}

// rustModuleAt finds the module file whose children live in childDir.
func (r *Resolver) rustModuleAt(childDir string) (string, bool) {
	cands := []string{path.Join(childDir, "mod.rs")}
	if childDir != "." {
		cands = append(cands, path.Join(path.Dir(childDir), path.Base(childDir)+".rs"))
	}
	cands = append(cands, path.Join(childDir, "lib.rs"), path.Join(childDir, "main.rs"))
	for _, c := range cands {
		if r.files[c] {
			return c, true
		}
	}
	return "", false
}

func (r *Resolver) rustParent(file string) (parent, childDir string, ok bool) {
	switch fileBase(file) {
	case "lib", "main":
		return "", "", false
	case "mod":
		childDir = path.Dir(path.Dir(file))
	default:
		childDir = path.Dir(file)
	}
	if childDir == "" {
		childDir = "."
	}
	f, ok := r.rustModuleAt(childDir)
	return f, childDir, ok
}

func (r *Resolver) rustCrateRoot(from string) (file, childDir string, ok bool) {
	for _, d := range ancestors(path.Dir(from)) {
		if r.files[path.Join(d, "Cargo.toml")] {
			for _, c := range []string{"src/lib.rs", "src/main.rs"} {
				if f := path.Join(d, c); r.files[f] {
					return f, path.Join(d, "src"), true
				}
			}
		}
	}
	for _, d := range ancestors(path.Dir(from)) {
		for _, c := range []string{"lib.rs", "main.rs"} {
			if f := path.Join(d, c); r.files[f] {
				return f, d, true
			}
		}
	}
	return "", "", false
}

// rustWalk follows module segments from a module as deep as files exist.
func (r *Resolver) rustWalk(file, childDir string, segs []string) (string, int) {
	walked := 0
	for _, s := range segs {
		if f := path.Join(childDir, s+".rs"); r.files[f] {
			file, childDir = f, path.Join(childDir, s)
		} else if f := path.Join(childDir, s, "mod.rs"); r.files[f] {
			file, childDir = f, path.Join(childDir, s)
		} else {
			break
		}
		walked++
	}
	return file, walked
}

func (r *Resolver) resolveRust(from string, ref ImportRef) Resolution {
	switch ref.Kind {
	case "extern":
		if rustStd[ref.Spec] {
			return external(CategoryStdlib)
		}
		return external(CategoryThirdParty)
	case "mod":
		cd := rustSelf(from)
		for _, c := range []string{path.Join(cd, ref.Spec+".rs"), path.Join(cd, ref.Spec, "mod.rs")} {
			if r.files[c] {
				return workspaceHit(c)
			}
		}
		return unresolved()
	}

	segs := strings.Split(ref.Spec, "::")
	switch segs[0] {
	case "crate":
		file, cd, ok := r.rustCrateRoot(from)
		if !ok {
			return unresolved()
		}
		target, _ := r.rustWalk(file, cd, segs[1:])
		return workspaceHit(target)
	case "self":
		target, _ := r.rustWalk(from, rustSelf(from), segs[1:])
		return workspaceHit(target)
	case "super":
		file, cd := from, rustSelf(from)
		for len(segs) > 0 && segs[0] == "super" {
			p, pcd, ok := r.rustParent(file)
			if !ok {
				return unresolved()
			}
			file, cd = p, pcd
			segs = segs[1:]
		}
		target, _ := r.rustWalk(file, cd, segs)
		return workspaceHit(target)
	}
	if rustStd[segs[0]] {
		return external(CategoryStdlib)
	}
	// A child module of the current one (`mod foo;` then `use foo::x`).
	if target, walked := r.rustWalk(from, rustSelf(from), segs); walked > 0 {
		return workspaceHit(target)
	}
	// 2015 paths are relative to the crate root.
	if file, cd, ok := r.rustCrateRoot(from); ok {
		if target, walked := r.rustWalk(file, cd, segs); walked > 0 {
			return workspaceHit(target)
		}
	}
	return external(CategoryThirdParty)
}

// --- C and C++ ---

func (r *Resolver) resolveC(from string, ref ImportRef) Resolution {
	spec := ref.Spec
	if ref.Kind != "system" {
		for _, c := range []string{path.Join(path.Dir(from), spec), path.Clean(spec), path.Join("include", spec)} {
			if c != ".." && !strings.HasPrefix(c, "../") && r.files[c] {
				return workspaceHit(c)
			}
		}
	}
	var hits []string
	for _, f := range r.byBase[path.Base(spec)] {
		if f == spec || strings.HasSuffix(f, "/"+spec) {
			hits = append(hits, f)
		}
	}
	if len(hits) == 1 {
		return workspaceHit(hits[0])
	}
	if ref.Kind == "system" {
		return external(CategorySystem)
	}
	if len(hits) > 1 {
		return unresolved() // ambiguous: several files could be meant
	}
	return unresolved()
}

// --- Lua ---

var luaStdlib = map[string]bool{
	"string": true, "table": true, "os": true, "io": true, "math": true, "coroutine": true,
	"debug": true, "utf8": true, "package": true, "bit": true, "jit": true, "ffi": true,
}

func (r *Resolver) resolveLua(from string, ref ImportRef) Resolution {
	rel := strings.ReplaceAll(ref.Spec, ".", "/")
	for _, d := range ancestors(path.Dir(from)) {
		for _, c := range []string{path.Join(d, rel+".lua"), path.Join(d, rel, "init.lua")} {
			if r.files[c] {
				return workspaceHit(c)
			}
		}
	}
	first, _, _ := strings.Cut(ref.Spec, ".")
	if luaStdlib[first] {
		return external(CategoryStdlib)
	}
	return external(CategoryThirdParty)
}
