package index

import "testing"

func TestExtractLua(t *testing.T) {
	src := "-- require('line.comment')\n" +
		"--[[ require('block.comment')\n require('still.block') ]]\n" +
		"--[==[ require('level.two') ]==]\n" +
		"local a = require('mod.a')\n" +
		"local b = require \"mod.b\"\n" +
		"local c = require(\"mod.c\")\n" +
		"local d = require [[mod.d]]\n" +
		"local s = \"require('in.string')\"\n" +
		"local l = [[ require('in.long.string') ]]\n" +
		"foo.require('method.call')\n" +
		"obj:require('colon.call')\n" +
		"local x = require(name)\n" +
		"local after = require('after')\n"
	refs := impExtract(t, "lua", "a.lua", src)
	impEqual(t, "lua", impSpecs(refs), []string{"mod.a", "mod.b", "mod.c", "mod.d", "after"})
	if refs[0].Line != 5 || refs[4].Line != 14 {
		t.Errorf("lines: %d %d", refs[0].Line, refs[4].Line)
	}
}
