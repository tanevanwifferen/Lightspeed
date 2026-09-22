package index

import "testing"

func TestExtractRust(t *testing.T) {
	src := "// use in::comment;\n" +
		"/* use in::block; /* nested use in::nested; */ use still::comment; */\n" +
		"extern crate serde;\n" +
		"mod parser;\n" +
		"pub mod inline { pub fn f() {} }\n" +
		"mod other_decl;\n" +
		"use std::collections::HashMap;\n" +
		"use crate::a::{b, c::d, self};\n" +
		"pub(crate) use super::e as ee;\n" +
		"use self::f::*;\n" +
		"use ::serde::Serialize;\n" +
		"use a::{b::{c, d}, e};\n" +
		"const S: &str = \"use in::string;\";\n" +
		"const R: &str = r#\"use in::raw; \"quoted\" \"#;\n" +
		"const C: char = '\"'; fn life<'a>(x: &'a str) {}\n" +
		"#[cfg(test)] use tests::helper;\n" +
		"fn f() { let r#use = 1; }\n"
	refs := impExtract(t, "rust", "lib.rs", src)
	impEqual(t, "rust", impKinds(refs), []string{
		"extern:serde", "mod:parser", "mod:other_decl",
		"use:std::collections::HashMap",
		"use:crate::a::b", "use:crate::a::c::d", "use:crate::a",
		"use:super::e", "use:self::f",
		"use:serde::Serialize",
		"use:a::b::c", "use:a::b::d", "use:a::e",
		"use:tests::helper",
	})
}
