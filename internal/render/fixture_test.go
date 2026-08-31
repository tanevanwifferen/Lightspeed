package render

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tanevanwifferen/Lightspeed/internal/gopls/protocol"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

// fixtureRoot is the workspace root the fixtures pretend to live in. It
// is absolute and stable across machines so that golden files can pin
// exact paths; Options.Root relativises it back out.
const fixtureRoot = "/w"

// asciiSource is an ordinary all-ASCII file: the control case where
// byte and UTF-16 columns agree.
const asciiSource = `package store

// UserRepo stores users.
type UserRepo struct {
	db *DB
}

func (r *UserRepo) Get(id int) *User {
	return r.db.user(id)
}
`

// cjkSource is the fixture of PLAN §5.1: every result in it has a byte
// column that differs from its UTF-16 column, and the emoji is a
// surrogate pair so UTF-16 columns differ from rune counts too.
const cjkSource = `package fixture

// 日本語のコメント: ユーザー名 を返す。
var 名前, ユーザー名 = "名", "🎉 party"

func 使う() string {
	return ユーザー名 + "🎉"
}
`

// crlfSource exercises the CRLF handling of lineBounds: no rendered
// line may carry a stray carriage return.
const crlfSource = "package win\r\n\r\nvar Path = \"C:\\\\tmp\"\r\n"

// mapper builds a Mapper for a fixture at a path under fixtureRoot.
func mapper(rel, content string) *protocol.Mapper {
	uri := protocol.URIFromPath(filepath.Join(fixtureRoot, rel))
	return protocol.NewMapper(uri, []byte(content))
}

// rangeOf returns the LSP range of the nth (0-based) occurrence of
// needle in m's content, converted from byte offsets by the vendored
// Mapper. Positions are therefore derived the same way a server derives
// them, not hand-computed.
func rangeOf(t *testing.T, m *protocol.Mapper, needle string, n int) protocol.Range {
	t.Helper()
	off := -1
	for range n + 1 {
		i := bytes.Index(m.Content[off+1:], []byte(needle))
		if i < 0 {
			t.Fatalf("occurrence %d of %q not found in %s", n, needle, m.URI)
		}
		off += 1 + i
	}
	rng, err := m.OffsetRange(off, off+len(needle))
	if err != nil {
		t.Fatalf("OffsetRange(%d, %d): %v", off, off+len(needle), err)
	}
	return rng
}

// golden compares got against testdata/name, or rewrites it under
// -update.
func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("writing golden %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading golden %s: %v (run go test -run %s -update)", path, err, t.Name())
	}
	if !bytes.Equal(got, want) {
		t.Errorf("output does not match %s\n--- want ---\n%s\n--- got ---\n%s",
			path, indentBlock(want), indentBlock(got))
	}
}

func indentBlock(b []byte) string {
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	for i, l := range lines {
		lines[i] = "\t" + l
	}
	return strings.Join(lines, "\n")
}

// mustRender runs a renderer into a buffer, failing the test on error.
func mustRender(t *testing.T, fn func(w *bytes.Buffer) error) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := fn(&buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	return buf.Bytes()
}
