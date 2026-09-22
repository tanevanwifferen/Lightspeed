package index

import "testing"

func TestExtractC(t *testing.T) {
	src := "// #include \"line_comment.h\"\n" +
		"/* #include \"block.h\"\n#include \"still_block.h\" */\n" +
		"#include <stdio.h>\n" +
		"#include \"local.h\"\n" +
		"  #  include  \"spaced.h\"\n" +
		"#import \"objc.h\"\n" +
		"#include_next <next.h>\n" +
		"const char *s = \"#include <in_string.h>\";\n" +
		"const char *u = \"// not a comment\"; \n" +
		"#include \"after_string.h\"\n" +
		"#define X 1 /* #include \"in_define_comment.h\" */\n" +
		"#error don't panic\n" +
		"#include \"after_error.h\"\n" +
		"auto raw = R\"x(\n#include \"in_raw.h\"\n)x\";\n" +
		"#include \"after_raw.h\"\n"
	refs := impExtract(t, "cpp", "a.cpp", src)
	impEqual(t, "c", impKinds(refs), []string{
		"system:stdio.h", "include:local.h", "include:spaced.h", "include:objc.h", "system:next.h",
		"include:after_string.h", "include:after_error.h", "include:after_raw.h",
	})
	if refs[1].Line != 5 {
		t.Errorf("line of local.h = %d, want 5", refs[1].Line)
	}
}
