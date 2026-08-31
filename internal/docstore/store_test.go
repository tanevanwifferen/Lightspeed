package docstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/tanevanwifferen/Lightspeed/internal/gopls/protocol"
)

// recorder is a Notifier that records what the store would have sent
// to a language server. It keeps the docstore tests free of a server
// entirely; the wiring to a real one is covered in session_test.go.
type recorder struct {
	mu    sync.Mutex
	sent  []sentNotification
	fail  error
	calls int
}

type sentNotification struct {
	Method string
	Params map[string]any
}

func (r *recorder) Notify(method string, params any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.fail != nil {
		return r.fail
	}
	body, err := json.Marshal(params)
	if err != nil {
		return err
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return err
	}
	r.sent = append(r.sent, sentNotification{Method: method, Params: decoded})
	return nil
}

func (r *recorder) methods() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, n := range r.sent {
		out = append(out, n.Method)
	}
	return out
}

func (r *recorder) last() sentNotification {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.sent) == 0 {
		return sentNotification{}
	}
	return r.sent[len(r.sent)-1]
}

// textDocument digs the textDocument object out of a notification.
func (n sentNotification) textDocument() map[string]any {
	td, _ := n.Params["textDocument"].(map[string]any)
	return td
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestOpenSendsDidOpen: most servers answer nothing about a file they
// have not been told about (PLAN §5.4), so this notification is the
// precondition for every read-only command in M1.
func TestOpenSendsDidOpen(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "a.go", "package main\n")
	rec := &recorder{}
	s := New(rec, Options{})

	doc, err := s.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !doc.Open || doc.Version != 1 {
		t.Errorf("doc = %+v, want open at version 1", doc)
	}
	if doc.URI != protocol.URIFromPath(path) {
		t.Errorf("URI = %q, want %q", doc.URI, protocol.URIFromPath(path))
	}

	n := rec.last()
	if n.Method != MethodDidOpen {
		t.Fatalf("method = %q, want %q", n.Method, MethodDidOpen)
	}
	td := n.textDocument()
	if td["uri"] != string(doc.URI) {
		t.Errorf("didOpen uri = %v, want %v", td["uri"], doc.URI)
	}
	if td["languageId"] != "go" {
		t.Errorf("didOpen languageId = %v, want go", td["languageId"])
	}
	if td["version"] != float64(1) {
		t.Errorf("didOpen version = %v, want 1", td["version"])
	}
	if td["text"] != "package main\n" {
		t.Errorf("didOpen text = %q", td["text"])
	}
}

// TestOpenIsIdempotent: a second didOpen for the same URI is a
// protocol violation, and servers punish it.
func TestOpenIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "a.go", "package main\n")
	rec := &recorder{}
	s := New(rec, Options{})

	first, err := s.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Error("the second Open returned a different document")
	}
	if got := rec.methods(); len(got) != 1 {
		t.Errorf("notifications = %v, want a single didOpen", got)
	}

	// A relative path naming the same file is the same document.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(cwd, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(rel); err != nil {
		t.Fatal(err)
	}
	if got := rec.methods(); len(got) != 1 {
		t.Errorf("notifications = %v after opening the same file by relative path", got)
	}
}

// TestVersionsIncrease: a document's version must increase
// monotonically, and the Mapper must be rebuilt with it — a stale
// Mapper turns every later position into a silent off-by-N.
func TestVersionsIncrease(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "a.go", "package main\n")
	rec := &recorder{}
	s := New(rec, Options{})

	doc, err := s.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	firstMapper := doc.Mapper

	for want := 2; want <= 4; want++ {
		content := fmt.Sprintf("package main\n// edit %d\n", want)
		doc, err = s.Change(path, []byte(content))
		if err != nil {
			t.Fatalf("Change: %v", err)
		}
		if int(doc.Version) != want {
			t.Errorf("version = %d, want %d", doc.Version, want)
		}
		n := rec.last()
		if n.Method != MethodDidChange {
			t.Fatalf("method = %q, want %q", n.Method, MethodDidChange)
		}
		if n.textDocument()["version"] != float64(want) {
			t.Errorf("didChange version = %v, want %d", n.textDocument()["version"], want)
		}
		changes, ok := n.Params["contentChanges"].([]any)
		if !ok || len(changes) != 1 {
			t.Fatalf("contentChanges = %v", n.Params["contentChanges"])
		}
		if changes[0].(map[string]any)["text"] != content {
			t.Errorf("contentChanges text = %v", changes[0])
		}
	}

	if doc.Mapper == firstMapper {
		t.Error("the Mapper was not rebuilt after the content changed")
	}
	if string(doc.Mapper.Content) != string(doc.Content) {
		t.Error("the Mapper and the document disagree about the content")
	}
}

// TestOpenContentDivergingFromDisk: reopening with different content
// becomes a didChange, never a second didOpen.
func TestOpenContentDivergingFromDisk(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "a.go", "package main\n")
	rec := &recorder{}
	s := New(rec, Options{})

	if _, err := s.Open(path); err != nil {
		t.Fatal(err)
	}
	doc, err := s.OpenContent(path, "", []byte("package main\nvar x = 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if doc.Version != 2 {
		t.Errorf("version = %d, want 2", doc.Version)
	}
	want := []string{MethodDidOpen, MethodDidChange}
	got := rec.methods()
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("notifications = %v, want %v", got, want)
	}

	// Reopening with identical content changes nothing.
	if _, err := s.OpenContent(path, "", []byte("package main\nvar x = 1\n")); err != nil {
		t.Fatal(err)
	}
	if len(rec.methods()) != 2 {
		t.Errorf("notifications = %v, want no extra traffic for unchanged content", rec.methods())
	}
}

// TestCloseLifecycle: didClose is sent once, closing an unknown file
// is not an error, and CloseAll cleans up everything the command
// opened — the server outlives the command in a daemon (PLAN §3).
func TestCloseLifecycle(t *testing.T) {
	dir := t.TempDir()
	a := writeFile(t, dir, "a.go", "package main\n")
	b := writeFile(t, dir, "b.go", "package main\n")
	rec := &recorder{}
	s := New(rec, Options{})

	if _, err := s.Open(a); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(b); err != nil {
		t.Fatal(err)
	}
	if got := len(s.OpenURIs()); got != 2 {
		t.Errorf("OpenURIs = %d, want 2", got)
	}

	if err := s.Close(a); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if n := rec.last(); n.Method != MethodDidClose || n.textDocument()["uri"] != string(protocol.URIFromPath(a)) {
		t.Errorf("last notification = %+v, want didClose for a.go", n)
	}
	if _, ok := s.Get(a); ok {
		t.Error("a closed document is still in the store")
	}
	if err := s.Close(a); err != nil {
		t.Errorf("closing an unknown document = %v, want nil", err)
	}
	if err := s.Close(filepath.Join(dir, "never-opened.go")); err != nil {
		t.Errorf("closing a never-opened file = %v, want nil", err)
	}

	if err := s.CloseAll(); err != nil {
		t.Fatalf("CloseAll: %v", err)
	}
	if got := s.OpenURIs(); len(got) != 0 {
		t.Errorf("OpenURIs after CloseAll = %v", got)
	}
	want := []string{MethodDidOpen, MethodDidOpen, MethodDidClose, MethodDidClose}
	if got := rec.methods(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("notifications = %v, want %v", got, want)
	}
}

// TestOpenFailureIsNotRecorded: if the notification fails, the
// document must not be remembered as open — otherwise every later
// command talks about a file the server has never heard of.
func TestOpenFailureIsNotRecorded(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "a.go", "package main\n")
	rec := &recorder{fail: errors.New("broken pipe")}
	s := New(rec, Options{})

	if _, err := s.Open(path); err == nil {
		t.Fatal("Open succeeded with a failing notifier")
	}
	if _, ok := s.Get(path); ok {
		t.Error("the failed document was recorded as open")
	}
}

// TestMissingFile reports the read error rather than opening an empty
// document.
func TestMissingFile(t *testing.T) {
	rec := &recorder{}
	s := New(rec, Options{})
	if _, err := s.Open(filepath.Join(t.TempDir(), "nope.go")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Open of a missing file = %v, want a not-exist error", err)
	}
	if rec.calls != 0 {
		t.Error("a notification was sent for a file that could not be read")
	}
}

// TestMapperWithoutOpening: results point at files no command opened,
// and those still need UTF-16 conversion. Caching a Mapper must not
// announce the file to the server.
func TestMapperWithoutOpening(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "a.go", "package main\n")
	rec := &recorder{}
	s := New(rec, Options{})

	m, err := s.Mapper(path)
	if err != nil {
		t.Fatalf("Mapper: %v", err)
	}
	if rec.calls != 0 {
		t.Errorf("Mapper sent %d notifications, want none", rec.calls)
	}
	doc, ok := s.Get(path)
	if !ok || doc.Open {
		t.Errorf("doc = %+v, want cached but not open", doc)
	}
	if again, err := s.Mapper(path); err != nil || again != m {
		t.Errorf("Mapper is not cached: %v", err)
	}

	// Opening it afterwards upgrades the entry and does notify.
	if _, err := s.Open(path); err != nil {
		t.Fatal(err)
	}
	if rec.calls != 1 {
		t.Errorf("notifications = %d, want the didOpen", rec.calls)
	}
	// Closing a merely-cached document sends nothing.
	other := writeFile(t, dir, "b.go", "package main\n")
	if _, err := s.Mapper(other); err != nil {
		t.Fatal(err)
	}
	before := rec.calls
	if err := s.Close(other); err != nil {
		t.Fatal(err)
	}
	if rec.calls != before {
		t.Error("closing a cached-but-never-opened document notified the server")
	}
}

// TestMapperForURINonFile: a server may return a URI we cannot read.
// That is an error, not a panic and not an empty file.
func TestMapperForURINonFile(t *testing.T) {
	s := New(nil, Options{})
	if _, err := s.MapperForURI("untitled:Untitled-1"); err == nil {
		t.Error("MapperForURI accepted a non-file URI")
	}
}

// cjkFixture mixes ASCII, CJK and a non-BMP emoji, the three cases
// where byte columns, UTF-16 columns and rune counts all disagree.
// M1 is "done when references on a CJK fixture is byte-exact"
// (PLAN §8), and this is where that is decided.
const cjkFixture = "package main\n" + // line 1
	"// 日本語 comment\n" + // line 2
	"func 変数名() {}\n" + // line 3
	"var 🎉 = 1\n" // line 4

// TestPositionsUTF16 checks byte column ⇄ UTF-16 character in both
// directions on the fixture. The numbers are worked out by hand: CJK
// characters are 3 bytes and 1 UTF-16 code unit, the emoji is 4 bytes
// and 2 UTF-16 code units.
func TestPositionsUTF16(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "u.go", cjkFixture)
	s := New(nil, Options{})
	uri := protocol.URIFromPath(path)

	cases := []struct {
		name      string
		line      int // 1-based
		col8      int // 1-based byte column
		character uint32
	}{
		{"ascii start of line", 1, 1, 0},
		{"ascii mid line", 1, 9, 8},
		{"before CJK", 2, 4, 3},
		{"identifier after 'func '", 3, 6, 5},
		{"inside CJK identifier", 3, 9, 6},
		{"paren after three CJK runes", 3, 15, 8},
		{"before the emoji", 4, 5, 4},
		{"space after the emoji", 4, 9, 6},
		{"equals after the emoji", 4, 10, 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pos, err := s.Position(path, tc.line, tc.col8)
			if err != nil {
				t.Fatalf("Position: %v", err)
			}
			if pos.Line != uint32(tc.line-1) {
				t.Errorf("Line = %d, want %d (0-based)", pos.Line, tc.line-1)
			}
			if pos.Character != tc.character {
				t.Errorf("Character = %d, want %d (UTF-16 code units)", pos.Character, tc.character)
			}

			// And back again: the text output format prints byte
			// columns, so the round trip has to be exact.
			line, col8, err := s.LineCol8(uri, pos)
			if err != nil {
				t.Fatalf("LineCol8: %v", err)
			}
			if line != tc.line || col8 != tc.col8 {
				t.Errorf("round trip = %d:%d, want %d:%d", line, col8, tc.line, tc.col8)
			}
		})
	}
}

// TestOffsetsAndRangeText: the file.go:#offset syntax and the text a
// server-reported range actually covers.
func TestOffsetsAndRangeText(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "u.go", cjkFixture)
	s := New(nil, Options{})
	uri := protocol.URIFromPath(path)

	// Byte offset of 変数名 in the fixture.
	offset := len("package main\n// 日本語 comment\nfunc ")
	pos, err := s.OffsetPosition(path, offset)
	if err != nil {
		t.Fatalf("OffsetPosition: %v", err)
	}
	if pos.Line != 2 || pos.Character != 5 {
		t.Errorf("position = %d:%d, want 2:5", pos.Line, pos.Character)
	}
	back, err := s.Offset(uri, pos)
	if err != nil {
		t.Fatalf("Offset: %v", err)
	}
	if back != offset {
		t.Errorf("Offset = %d, want %d", back, offset)
	}

	end := protocol.Position{Line: 2, Character: 8} // just past 変数名
	text, err := s.RangeText(uri, protocol.Range{Start: pos, End: end})
	if err != nil {
		t.Fatalf("RangeText: %v", err)
	}
	if string(text) != "変数名" {
		t.Errorf("RangeText = %q, want 変数名", text)
	}
}

// TestPositionOutOfRange: a column past the end of a line is a user
// error with a message, not a panic or a silent clamp.
func TestPositionOutOfRange(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "a.go", "package main\n")
	s := New(nil, Options{})

	if _, err := s.Position(path, 99, 1); err == nil {
		t.Error("Position accepted a line past the end of the file")
	}
	if _, err := s.Position(path, 1, 999); err == nil {
		t.Error("Position accepted a column past the end of the line")
	}
	if _, err := s.OffsetPosition(path, 10_000); err == nil {
		t.Error("OffsetPosition accepted an offset past the end of the file")
	}
}

// TestChangeRequiresOpen: didChange on a document the server never
// saw would be a protocol violation.
func TestChangeRequiresOpen(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "a.go", "package main\n")
	s := New(&recorder{}, Options{})

	if _, err := s.Change(path, []byte("x")); !errors.Is(err, ErrNotOpen) {
		t.Errorf("Change on an unopened document = %v, want ErrNotOpen", err)
	}
	if _, err := s.Mapper(path); err != nil { // cached, still not open
		t.Fatal(err)
	}
	if _, err := s.Change(path, []byte("x")); !errors.Is(err, ErrNotOpen) {
		t.Errorf("Change on a cached document = %v, want ErrNotOpen", err)
	}
}

// TestOptionsOverrides: a server definition may name the language id
// (PLAN §6), and the daemon may supply content from elsewhere.
func TestOptionsOverrides(t *testing.T) {
	rec := &recorder{}
	files := map[string]string{"/virtual/a.zzz": "content\n"}
	s := New(rec, Options{
		LanguageIDs: map[string]string{".zzz": "zeta", "special.txt": "specialtext"},
		ReadFile: func(path string) ([]byte, error) {
			content, ok := files[path]
			if !ok {
				return nil, os.ErrNotExist
			}
			return []byte(content), nil
		},
	})

	doc, err := s.Open("/virtual/a.zzz")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if doc.LanguageID != "zeta" {
		t.Errorf("languageId = %q, want zeta", doc.LanguageID)
	}
	if string(doc.Content) != "content\n" {
		t.Errorf("content = %q", doc.Content)
	}

	files["/virtual/special.txt"] = "hi\n"
	doc, err = s.Open("/virtual/special.txt")
	if err != nil {
		t.Fatal(err)
	}
	if doc.LanguageID != "specialtext" {
		t.Errorf("languageId = %q, want specialtext", doc.LanguageID)
	}
}

// TestNilNotifier: position conversion must work without a server at
// all, which is what makes the store testable and the eventual
// offline formatting paths possible.
func TestNilNotifier(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "a.go", "package main\n")
	s := New(nil, Options{})
	if _, err := s.Open(path); err != nil {
		t.Fatalf("Open with a nil notifier: %v", err)
	}
	if _, err := s.Position(path, 1, 1); err != nil {
		t.Fatalf("Position: %v", err)
	}
}

// TestConcurrentAccess runs the store the way a daemon would, from
// several goroutines at once; go test -race is the assertion.
func TestConcurrentAccess(t *testing.T) {
	dir := t.TempDir()
	var paths []string
	for i := range 8 {
		paths = append(paths, writeFile(t, dir, fmt.Sprintf("f%d.go", i), "package main\n// 日本語\n"))
	}
	s := New(&recorder{}, Options{})

	var wg sync.WaitGroup
	for _, path := range paths {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Open(path); err != nil {
				t.Errorf("Open: %v", err)
				return
			}
			if _, err := s.Position(path, 2, 4); err != nil {
				t.Errorf("Position: %v", err)
			}
			if _, err := s.Change(path, []byte("package main\n")); err != nil {
				t.Errorf("Change: %v", err)
			}
			if err := s.Close(path); err != nil {
				t.Errorf("Close: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := s.OpenURIs(); len(got) != 0 {
		t.Errorf("OpenURIs = %v, want none", got)
	}
}
