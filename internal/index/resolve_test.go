package index

import (
	"reflect"
	"testing"
)

// impResolve resolves ref from a file of the given language in a workspace of
// the named (empty) files.
func impResolve(t *testing.T, from, lang string, ref ImportRef, files map[string]string) Resolution {
	t.Helper()
	if files[from] == "" {
		files[from] = " "
	}
	r := NewResolver(newImpWS(files))
	return r.Resolve(&File{Path: from, Language: lang}, ref)
}

func TestResolveGoModules(t *testing.T) {
	files := map[string]string{
		"go.mod":                  "module example.com/app\n\ngo 1.22\n\nreplace example.com/shared => ./third_party/shared\nreplace example.com/remote v1.0.0 => example.com/fork v1.0.1\n",
		"main.go":                 "package main",
		"lib/lib.go":              "package lib",
		"lib/lib_test.go":         "package lib",
		"empty/x.txt":             "",
		"tools/go.mod":            "module example.com/app/tools\n",
		"tools/t.go":              "package tools",
		"tools/sub/s.go":          "package sub",
		"third_party/shared/s.go": "package shared",
		"go.work":                 "go 1.22\nuse (\n\t./\n\t./tools\n)\nreplace example.com/workreplace => ./wr\n",
		"wr/w.go":                 "package wr",
	}
	cases := []struct {
		spec string
		want Resolution
	}{
		{"fmt", Resolution{External: true, Category: "stdlib"}},
		{"net/http", Resolution{External: true, Category: "stdlib"}},
		{"github.com/other/x", Resolution{External: true, Category: "third-party"}},
		{"example.com/app/lib", Resolution{Targets: []string{"lib"}, Category: "workspace"}},
		{"example.com/app", Resolution{Targets: []string{"."}, Category: "workspace"}},
		// The nested module's longer prefix wins over the root module's.
		{"example.com/app/tools", Resolution{Targets: []string{"tools"}, Category: "workspace"}},
		{"example.com/app/tools/sub", Resolution{Targets: []string{"tools/sub"}, Category: "workspace"}},
		{"example.com/shared", Resolution{Targets: []string{"third_party/shared"}, Category: "workspace"}},
		{"example.com/workreplace", Resolution{Targets: []string{"wr"}, Category: "workspace"}},
		// In a workspace module but no Go files there.
		{"example.com/app/empty", Resolution{Category: "unresolved"}},
		{"example.com/app/nothere", Resolution{Category: "unresolved"}},
		// A versioned replacement is not a local directory.
		{"example.com/remote", Resolution{External: true, Category: "third-party"}},
	}
	for _, c := range cases {
		got := impResolve(t, "main.go", "go", ImportRef{Spec: c.spec}, files)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %+v, want %+v", c.spec, got, c.want)
		}
	}
}

func TestResolvePython(t *testing.T) {
	files := map[string]string{
		"app/__init__.py":     "",
		"app/main.py":         "",
		"app/util.py":         "",
		"app/sub/__init__.py": "",
		"app/sub/deep.py":     "",
		"app/sub/mod.py":      "",
		"top.py":              "",
		"src/lib/__init__.py": "",
		"src/lib/thing.py":    "",
		"scripts/run.py":      "",
	}
	cases := []struct {
		name, from string
		ref        ImportRef
		want       Resolution
	}{
		{"absolute module", "app/main.py", ImportRef{Spec: "app.util"}, workspaceHit("app/util.py")},
		{"absolute package", "scripts/run.py", ImportRef{Spec: "app"}, workspaceHit("app/__init__.py")},
		{"dotted prefix", "scripts/run.py", ImportRef{Spec: "app.util.helper"}, workspaceHit("app/util.py")},
		{"from pkg import submodule", "scripts/run.py", ImportRef{Spec: "app.sub", Names: []string{"deep", "attr"}}, workspaceHit("app/sub/__init__.py", "app/sub/deep.py")},
		{"one dot is the same package", "app/main.py", ImportRef{Spec: ".util"}, workspaceHit("app/util.py")},
		{"one dot bare", "app/main.py", ImportRef{Spec: ".", Names: []string{"util"}}, workspaceHit("app/__init__.py", "app/util.py")},
		{"package __init__ imports an item of itself", "app/__init__.py", ImportRef{Spec: ".", Names: []string{"thing_name"}}, Resolution{Category: "self"}},
		{"package __init__ imports its submodule but not itself", "app/__init__.py", ImportRef{Spec: ".", Names: []string{"util", "thing_name"}}, workspaceHit("app/util.py")},
		{"two dots", "app/sub/deep.py", ImportRef{Spec: "..util"}, workspaceHit("app/util.py")},
		{"two dots and names", "app/sub/deep.py", ImportRef{Spec: "..", Names: []string{"util", "sub"}}, workspaceHit("app/__init__.py", "app/sub/__init__.py", "app/util.py")},
		{"sibling of a script", "app/sub/deep.py", ImportRef{Spec: "mod"}, workspaceHit("app/sub/mod.py")},
		{"src layout", "scripts/run.py", ImportRef{Spec: "lib.thing"}, workspaceHit("src/lib/thing.py")},
		{"relative but missing", "app/main.py", ImportRef{Spec: ".nothere"}, Resolution{Category: "unresolved"}},
		{"above the root", "top.py", ImportRef{Spec: "..x"}, Resolution{Category: "unresolved"}},
		{"stdlib", "app/main.py", ImportRef{Spec: "os.path"}, Resolution{External: true, Category: "stdlib"}},
		{"third party", "app/main.py", ImportRef{Spec: "requests"}, Resolution{External: true, Category: "third-party"}},
	}
	for _, c := range cases {
		got := impResolve(t, c.from, "python", c.ref, files)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestResolveJS(t *testing.T) {
	files := map[string]string{
		"src/a.ts":          "",
		"src/b.tsx":         "",
		"src/c.js":          "",
		"src/data.json":     "",
		"src/util/index.ts": "",
		"src/esm.ts":        "",
		"src/lib/x.ts":      "",
		"shared/s.ts":       "",
	}
	cases := []struct {
		spec string
		want Resolution
	}{
		{"./a", workspaceHit("src/a.ts")},
		{"./b", workspaceHit("src/b.tsx")},
		{"./c", workspaceHit("src/c.js")},
		{"./c.js", workspaceHit("src/c.js")},
		{"./data.json", workspaceHit("src/data.json")},
		{"./util", workspaceHit("src/util/index.ts")},
		{"./util/", workspaceHit("src/util/index.ts")},
		{"./esm.js", workspaceHit("src/esm.ts")}, // TS ESM: .js names the .ts source
		{"../shared/s", workspaceHit("shared/s.ts")},
		{"./lib/x", workspaceHit("src/lib/x.ts")},
		{"./missing", Resolution{Category: "unresolved"}},
		{"../../outside", Resolution{Category: "unresolved"}},
		{"react", Resolution{External: true, Category: "third-party"}},
		{"@scope/pkg/sub", Resolution{External: true, Category: "third-party"}},
		{"node:fs", Resolution{External: true, Category: "stdlib"}},
		{"path", Resolution{External: true, Category: "stdlib"}},
	}
	for _, c := range cases {
		got := impResolve(t, "src/main.ts", "typescript", ImportRef{Spec: c.spec}, files)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %+v, want %+v", c.spec, got, c.want)
		}
	}
}

func TestResolveRust(t *testing.T) {
	files := map[string]string{
		"Cargo.toml":        "",
		"src/lib.rs":        "",
		"src/a.rs":          "",
		"src/a/b.rs":        "",
		"src/a/c/mod.rs":    "",
		"src/a/c/d.rs":      "",
		"src/other/mod.rs":  "",
		"src/other/leaf.rs": "",
	}
	cases := []struct {
		name, from string
		ref        ImportRef
		want       Resolution
	}{
		{"mod from crate root", "src/lib.rs", ImportRef{Spec: "a", Kind: "mod"}, workspaceHit("src/a.rs")},
		{"mod dir form", "src/lib.rs", ImportRef{Spec: "other", Kind: "mod"}, workspaceHit("src/other/mod.rs")},
		{"mod from a plain file", "src/a.rs", ImportRef{Spec: "b", Kind: "mod"}, workspaceHit("src/a/b.rs")},
		{"mod from mod.rs", "src/a/c/mod.rs", ImportRef{Spec: "d", Kind: "mod"}, workspaceHit("src/a/c/d.rs")},
		{"mod missing", "src/lib.rs", ImportRef{Spec: "ghost", Kind: "mod"}, Resolution{Category: "unresolved"}},
		{"crate path", "src/a/b.rs", ImportRef{Spec: "crate::a::c::d::Thing", Kind: "use"}, workspaceHit("src/a/c/d.rs")},
		{"crate item in root", "src/a/b.rs", ImportRef{Spec: "crate::Thing", Kind: "use"}, workspaceHit("src/lib.rs")},
		{"crate to dir module", "src/a.rs", ImportRef{Spec: "crate::other::Thing", Kind: "use"}, workspaceHit("src/other/mod.rs")},
		{"self", "src/a.rs", ImportRef{Spec: "self::b::Item", Kind: "use"}, workspaceHit("src/a/b.rs")},
		{"self item, no module walked", "src/a.rs", ImportRef{Spec: "self::Item", Kind: "use"}, Resolution{Category: "self"}},
		{"self missing module", "src/a.rs", ImportRef{Spec: "self::nothere::x", Kind: "use"}, Resolution{Category: "self"}},
		{"self enum glob", "src/a.rs", ImportRef{Spec: "self::Enum::*", Kind: "use"}, Resolution{Category: "self"}},
		{"crate item in the crate root", "src/lib.rs", ImportRef{Spec: "crate::Thing", Kind: "use"}, Resolution{Category: "self"}},
		{"super from plain file", "src/a/b.rs", ImportRef{Spec: "super::Item", Kind: "use"}, workspaceHit("src/a.rs")},
		{"super twice", "src/a/c/d.rs", ImportRef{Spec: "super::super::Item", Kind: "use"}, workspaceHit("src/a.rs")},
		{"super from mod.rs", "src/a/c/mod.rs", ImportRef{Spec: "super::b::X", Kind: "use"}, workspaceHit("src/a/b.rs")},
		{"child module by bare path", "src/lib.rs", ImportRef{Spec: "a::b::X", Kind: "use"}, workspaceHit("src/a/b.rs")},
		{"super above the root", "src/lib.rs", ImportRef{Spec: "super::x", Kind: "use"}, Resolution{Category: "unresolved"}},
		{"std", "src/lib.rs", ImportRef{Spec: "std::fmt::Debug", Kind: "use"}, Resolution{External: true, Category: "stdlib"}},
		{"other crate", "src/lib.rs", ImportRef{Spec: "serde::Serialize", Kind: "use"}, Resolution{External: true, Category: "third-party"}},
		{"extern crate", "src/lib.rs", ImportRef{Spec: "serde", Kind: "extern"}, Resolution{External: true, Category: "third-party"}},
	}
	for _, c := range cases {
		got := impResolve(t, c.from, "rust", c.ref, files)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestResolveC(t *testing.T) {
	files := map[string]string{
		"src/a.c":              "",
		"src/a.h":              "",
		"src/util/u.h":         "",
		"include/proj/api.h":   "",
		"include/proj/other.h": "",
		"third/dup/x.h":        "",
		"third/dup2/x.h":       "",
		"vendor/lib/only.h":    "",
		"root.h":               "",
	}
	cases := []struct {
		name string
		ref  ImportRef
		want Resolution
	}{
		{"relative to the file", ImportRef{Spec: "a.h", Kind: "include"}, workspaceHit("src/a.h")},
		{"relative subdir", ImportRef{Spec: "util/u.h", Kind: "include"}, workspaceHit("src/util/u.h")},
		{"workspace root", ImportRef{Spec: "root.h", Kind: "include"}, workspaceHit("root.h")},
		{"include dir", ImportRef{Spec: "proj/api.h", Kind: "include"}, workspaceHit("include/proj/api.h")},
		{"unique suffix", ImportRef{Spec: "lib/only.h", Kind: "include"}, workspaceHit("vendor/lib/only.h")},
		{"system header found by suffix", ImportRef{Spec: "lib/only.h", Kind: "system"}, workspaceHit("vendor/lib/only.h")},
		{"system header, ambiguous suffix", ImportRef{Spec: "x.h", Kind: "system"}, Resolution{External: true, Category: "system"}},
		{"system header not in workspace", ImportRef{Spec: "stdio.h", Kind: "system"}, Resolution{External: true, Category: "system"}},
		{"quoted, ambiguous", ImportRef{Spec: "x.h", Kind: "include"}, Resolution{Category: "unresolved"}},
		{"quoted, missing", ImportRef{Spec: "ghost.h", Kind: "include"}, Resolution{Category: "unresolved"}},
	}
	for _, c := range cases {
		got := impResolve(t, "src/a.c", "c", c.ref, files)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestResolveLua(t *testing.T) {
	files := map[string]string{
		"lua/mod/a.lua":     "",
		"lua/mod/init.lua":  "",
		"lua/other/x.lua":   "",
		"lua/mod/local.lua": "",
		"top.lua":           "",
	}
	cases := []struct {
		name, from string
		spec       string
		want       Resolution
	}{
		{"from the root", "top.lua", "lua.mod.a", workspaceHit("lua/mod/a.lua")},
		{"init", "top.lua", "lua.mod", workspaceHit("lua/mod/init.lua")},
		{"relative to an ancestor", "lua/mod/a.lua", "other.x", workspaceHit("lua/other/x.lua")},
		{"sibling", "lua/mod/a.lua", "local", workspaceHit("lua/mod/local.lua")},
		{"stdlib", "top.lua", "string", Resolution{External: true, Category: "stdlib"}},
		{"external", "top.lua", "lpeg", Resolution{External: true, Category: "third-party"}},
	}
	for _, c := range cases {
		got := impResolve(t, c.from, "lua", ImportRef{Spec: c.spec, Kind: "require"}, files)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestResolveUnknownLanguage(t *testing.T) {
	got := impResolve(t, "a.zig", "zig", ImportRef{Spec: "std"}, map[string]string{})
	if got.Category != "unresolved" || got.External || len(got.Targets) != 0 {
		t.Errorf("got %+v", got)
	}
}
