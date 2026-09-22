package index

import "testing"

func TestExtractPython(t *testing.T) {
	src := `"""Module docstring.

import in_docstring
from also_docstring import x
"""
# import in_comment
import os, sys as system
import a.b.c as abc
from collections import (
    OrderedDict,   # a comment inside
    defaultdict as dd,
)
from . import sibling
from .. import up
from ..pkg.mod import thing, other
from .rel import *
from __future__ import annotations
x = "import in_string"
y = '''
import in_triple
'''
z = r"from raw import y"
def f():
    import lazy
    if x: pass
    from inner.mod import (a,
        b)
a = 1; import semi
`
	refs := impExtract(t, "python", "p.py", src)
	impEqual(t, "python", impSpecs(refs), []string{
		"os", "sys", "a.b.c", "collections", ".", "..", "..pkg.mod", ".rel", "__future__", "lazy", "inner.mod", "semi",
	})
	byNames := map[string][]string{}
	for _, r := range refs {
		byNames[r.Spec] = r.Names
	}
	impEqual(t, "collections names", byNames["collections"], []string{"OrderedDict", "defaultdict"})
	impEqual(t, "dot names", byNames["."], []string{"sibling"})
	impEqual(t, "inner names", byNames["inner.mod"], []string{"a", "b"})
	impEqual(t, "star", byNames[".rel"], nil)
}

func TestExtractPythonLines(t *testing.T) {
	refs := impExtract(t, "python", "p.py", "import a\n\nfrom b import (\n  c,\n  d\n)\nimport e\n")
	lines := map[string]int{}
	for _, r := range refs {
		lines[r.Spec] = r.Line
	}
	if lines["a"] != 1 || lines["b"] != 3 || lines["e"] != 7 {
		t.Errorf("lines = %v", lines)
	}
}

func TestExtractPythonBackslashContinuation(t *testing.T) {
	refs := impExtract(t, "python", "p.py", "from a import b, \\\n    c\nimport d\n")
	impEqual(t, "python", impSpecs(refs), []string{"a", "d"})
	impEqual(t, "names", refs[0].Names, []string{"b", "c"})
}
