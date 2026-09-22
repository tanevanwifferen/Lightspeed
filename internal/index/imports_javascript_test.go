package index

import "testing"

func TestExtractJS(t *testing.T) {
	src := "// import comment from 'line-comment'\n" +
		"/* import x from 'block-comment' */\n" +
		"import def from './def';\n" +
		"import * as ns from \"./ns\"\n" +
		"import { a,\n   b as c,\n   from } from '../multi'\n" +
		"import type { T } from './types'\n" +
		"import './side-effect'\n" +
		"import def2, { d2 } from 'pkg'\n" +
		"export * from './star'\n" +
		"export * as reexp from './star-as'\n" +
		"export { e1, e2 } from './named'\n" +
		"export type { E3 } from './etype'\n" +
		"export const notAReexport = 1\n" +
		"import legacy = require('./legacy')\n" +
		"const cjs = require('./cjs')\n" +
		"const lazy = await import('./lazy')\n" +
		"foo.require('./method')\n" +
		"const s = \"import x from 'in-string'\"\n" +
		"const t = `import y from 'in-template' ${ require('./in-substitution') } and ${`nested ${1}`}`\n" +
		"const re = /import z from 'x'/g\n" +
		"const meta = import.meta.url\n" +
		"const ok = require('./after')\n"
	refs := impExtract(t, "typescript", "a.ts", src)
	impEqual(t, "ts", impKinds(refs), []string{
		"import:./def", "import:./ns", "import:../multi", "import:./types", "import:./side-effect", "import:pkg",
		"reexport:./star", "reexport:./star-as", "reexport:./named", "reexport:./etype",
		"require:./legacy", "require:./cjs", "dynamic:./lazy", "require:./after",
	})
}

func TestExtractJSXApostropheDoesNotSwallowImports(t *testing.T) {
	src := "import a from './a'\nconst x = <p>Don't stop</p>\nimport b from './b'\n"
	refs := impExtract(t, "javascriptreact", "a.jsx", src)
	impEqual(t, "jsx", impSpecs(refs), []string{"./a", "./b"})
}

func TestExtractJSLines(t *testing.T) {
	refs := impExtract(t, "javascript", "a.js", "\n\nimport a from './a'\n\nrequire('./b')\n")
	if len(refs) != 2 || refs[0].Line != 3 || refs[1].Line != 5 {
		t.Errorf("refs = %+v", refs)
	}
}
