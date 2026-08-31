package docstore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/tanevanwifferen/Lightspeed/internal/gopls/protocol"
)

// LSP text-synchronisation notifications.
const (
	MethodDidOpen   = "textDocument/didOpen"
	MethodDidChange = "textDocument/didChange"
	MethodDidClose  = "textDocument/didClose"
)

// Notifier sends an LSP notification. *client.Conn and
// *client.Session both satisfy it; tests use a recorder.
type Notifier interface {
	Notify(method string, params any) error
}

// ErrNotOpen reports an operation on a document the store has not
// opened.
var ErrNotOpen = errors.New("docstore: document not open")

// Document is one file the store knows about. A document is either
// *open* — announced to the server with didOpen, and versioned — or
// merely *cached*, meaning the store holds its content and Mapper for
// position conversion but the server has never been told about it.
// Results point at files we never opened, and those still need
// UTF-16 conversion.
type Document struct {
	// URI is the document's LSP URI, and the store's key.
	URI protocol.DocumentURI
	// Path is the absolute filesystem path.
	Path string
	// LanguageID is the LSP language identifier sent in didOpen.
	LanguageID string
	// Version is the document version last sent to the server; 0 for
	// a document that is cached but not open.
	Version int32
	// Content is the exact bytes the Mapper was built from.
	Content []byte
	// Mapper converts between byte offsets, 1-based byte columns and
	// LSP UTF-16 positions (PLAN §5.1).
	Mapper *protocol.Mapper
	// Open reports whether the server has been sent didOpen.
	Open bool
}

// Options configures a Store.
type Options struct {
	// LanguageIDs overrides the extension → language-id table, e.g.
	// from a server definition (PLAN §6). Keys are extensions with
	// the leading dot, or whole file names.
	LanguageIDs map[string]string
	// ReadFile replaces os.ReadFile. Tests use it to avoid touching
	// the disk; the daemon may later use it for an overlay.
	ReadFile func(path string) ([]byte, error)
}

// Store tracks open documents and caches one Mapper per file. It is
// safe for concurrent use; the mutex is held across notifications so
// the wire order of didOpen/didChange/didClose matches the version
// order the server sees.
type Store struct {
	notify Notifier
	opts   Options

	mu   sync.Mutex
	docs map[protocol.DocumentURI]*Document
}

// New returns a store that announces documents to notify. A nil
// Notifier is allowed and makes the store position-conversion only,
// which is what the eventual --no-open mode and the unit tests want.
func New(notify Notifier, opts Options) *Store {
	return &Store{notify: notify, opts: opts, docs: map[protocol.DocumentURI]*Document{}}
}

// Open reads path from disk, sends didOpen and returns the document.
// Opening an already-open document is a no-op that returns the
// existing document: a second didOpen for the same URI is a protocol
// violation, and servers punish it.
func (s *Store) Open(path string) (*Document, error) {
	content, err := s.read(path)
	if err != nil {
		return nil, err
	}
	return s.OpenContent(path, "", content)
}

// OpenContent opens a document with caller-supplied content, for
// buffers that differ from disk. An empty languageID is derived from
// the path. If the document is already open with different content,
// the content is pushed as a didChange rather than a second didOpen.
func (s *Store) OpenContent(path, languageID string, content []byte) (*Document, error) {
	abs, uri, err := resolve(path)
	if err != nil {
		return nil, err
	}
	if languageID == "" {
		languageID = s.languageID(abs)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if doc, ok := s.docs[uri]; ok && doc.Open {
		if string(doc.Content) == string(content) {
			return doc, nil
		}
		return s.change(doc, content)
	}

	doc := &Document{
		URI:        uri,
		Path:       abs,
		LanguageID: languageID,
		Version:    1,
		Content:    content,
		Mapper:     protocol.NewMapper(uri, content),
		Open:       true,
	}
	if err := s.send(MethodDidOpen, map[string]any{
		"textDocument": map[string]any{
			"uri":        string(uri),
			"languageId": languageID,
			"version":    doc.Version,
			"text":       string(content),
		},
	}); err != nil {
		return nil, fmt.Errorf("didOpen %s: %w", abs, err)
	}
	s.docs[uri] = doc
	return doc, nil
}

// Change replaces an open document's content with a full-text
// didChange and bumps its version. The Mapper is rebuilt, because a
// stale Mapper turns every later position into a silent off-by-N.
func (s *Store) Change(path string, content []byte) (*Document, error) {
	_, uri, err := resolve(path)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, ok := s.docs[uri]
	if !ok || !doc.Open {
		return nil, fmt.Errorf("%w: %s", ErrNotOpen, path)
	}
	return s.change(doc, content)
}

// change is Change with s.mu held.
func (s *Store) change(doc *Document, content []byte) (*Document, error) {
	next := doc.Version + 1
	if err := s.send(MethodDidChange, map[string]any{
		"textDocument": map[string]any{
			"uri":     string(doc.URI),
			"version": next,
		},
		"contentChanges": []map[string]any{{"text": string(content)}},
	}); err != nil {
		return nil, fmt.Errorf("didChange %s: %w", doc.Path, err)
	}
	doc.Version = next
	doc.Content = content
	doc.Mapper = protocol.NewMapper(doc.URI, content)
	return doc, nil
}

// Close sends didClose and forgets the document. Closing a document
// that is not open is not an error: callers close in defers.
func (s *Store) Close(path string) error {
	_, uri, err := resolve(path)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeURI(uri)
}

// closeURI is Close with s.mu held.
func (s *Store) closeURI(uri protocol.DocumentURI) error {
	doc, ok := s.docs[uri]
	if !ok {
		return nil
	}
	delete(s.docs, uri)
	if !doc.Open {
		return nil // cached for its Mapper only; the server never knew
	}
	if err := s.send(MethodDidClose, map[string]any{
		"textDocument": map[string]any{"uri": string(uri)},
	}); err != nil {
		return fmt.Errorf("didClose %s: %w", doc.Path, err)
	}
	return nil
}

// CloseAll closes every open document, in a deterministic order, and
// reports the first failure while still attempting the rest. It is
// the end-of-command cleanup: a daemon-pooled server outlives the
// command that opened the files.
func (s *Store) CloseAll() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	uris := make([]string, 0, len(s.docs))
	for uri := range s.docs {
		uris = append(uris, string(uri))
	}
	sort.Strings(uris)
	var firstErr error
	for _, uri := range uris {
		if err := s.closeURI(protocol.DocumentURI(uri)); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Get returns the document for a path, if the store knows it.
func (s *Store) Get(path string) (*Document, bool) {
	_, uri, err := resolve(path)
	if err != nil {
		return nil, false
	}
	return s.GetURI(uri)
}

// GetURI returns the document for a URI, if the store knows it.
func (s *Store) GetURI(uri protocol.DocumentURI) (*Document, bool) {
	clean, _, err := parseURI(uri)
	if err != nil {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, ok := s.docs[clean]
	return doc, ok
}

// OpenURIs lists the URIs currently open on the server, sorted.
func (s *Store) OpenURIs() []protocol.DocumentURI {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []protocol.DocumentURI
	for uri, doc := range s.docs {
		if doc.Open {
			out = append(out, uri)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Mapper returns the Mapper for a path, reading and caching the file
// if the store has not seen it. The file is *not* opened on the
// server: results routinely point at files no command opened, and
// they still need UTF-16 conversion.
func (s *Store) Mapper(path string) (*protocol.Mapper, error) {
	abs, uri, err := resolve(path)
	if err != nil {
		return nil, err
	}
	return s.mapperFor(abs, uri)
}

// MapperForURI is Mapper, keyed by URI. A server may answer with a
// URI we cannot read — jdt://, untitled:, or plain nonsense — and
// that is an error to report, not a panic: the vendored
// DocumentURI.Path panics on a non-file URI, so every URI entering
// the store goes through parseURI first.
func (s *Store) MapperForURI(uri protocol.DocumentURI) (*protocol.Mapper, error) {
	clean, path, err := parseURI(uri)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if doc, ok := s.docs[clean]; ok {
		s.mu.Unlock()
		return doc.Mapper, nil
	}
	s.mu.Unlock()
	return s.mapperFor(path, clean)
}

func (s *Store) mapperFor(abs string, uri protocol.DocumentURI) (*protocol.Mapper, error) {
	s.mu.Lock()
	if doc, ok := s.docs[uri]; ok {
		s.mu.Unlock()
		return doc.Mapper, nil
	}
	s.mu.Unlock()

	// Read outside the lock: reading a file must not block a didOpen.
	content, err := s.read(abs)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if doc, ok := s.docs[uri]; ok { // someone opened it while we read
		return doc.Mapper, nil
	}
	doc := &Document{
		URI:        uri,
		Path:       abs,
		LanguageID: s.languageID(abs),
		Content:    content,
		Mapper:     protocol.NewMapper(uri, content),
	}
	s.docs[uri] = doc
	return doc.Mapper, nil
}

// Position converts a 1-based line and 1-based byte column — the
// CLI's location syntax — to an LSP UTF-16 position.
func (s *Store) Position(path string, line, col8 int) (protocol.Position, error) {
	m, err := s.Mapper(path)
	if err != nil {
		return protocol.Position{}, err
	}
	return m.LineCol8Position(line, col8)
}

// OffsetPosition converts a byte offset — the file.go:#offset syntax
// — to an LSP UTF-16 position.
func (s *Store) OffsetPosition(path string, offset int) (protocol.Position, error) {
	m, err := s.Mapper(path)
	if err != nil {
		return protocol.Position{}, err
	}
	return m.OffsetPosition(offset)
}

// LineCol8 converts an LSP position back to the 1-based line and
// 1-based byte column that the text output format prints.
func (s *Store) LineCol8(uri protocol.DocumentURI, pos protocol.Position) (line, col8 int, err error) {
	m, err := s.MapperForURI(uri)
	if err != nil {
		return 0, 0, err
	}
	offset, err := m.PositionOffset(pos)
	if err != nil {
		return 0, 0, err
	}
	line, col8 = m.OffsetLineCol8(offset)
	return line, col8, nil
}

// Offset converts an LSP position to a byte offset.
func (s *Store) Offset(uri protocol.DocumentURI, pos protocol.Position) (int, error) {
	m, err := s.MapperForURI(uri)
	if err != nil {
		return 0, err
	}
	return m.PositionOffset(pos)
}

// RangeText returns the bytes an LSP range covers.
func (s *Store) RangeText(uri protocol.DocumentURI, rng protocol.Range) ([]byte, error) {
	m, err := s.MapperForURI(uri)
	if err != nil {
		return nil, err
	}
	start, end, err := m.RangeOffsets(rng)
	if err != nil {
		return nil, err
	}
	return m.Content[start:end], nil
}

// send is Notify with the nil-Notifier case handled.
func (s *Store) send(method string, params any) error {
	if s.notify == nil {
		return nil
	}
	return s.notify.Notify(method, params)
}

func (s *Store) read(path string) ([]byte, error) {
	read := s.opts.ReadFile
	if read == nil {
		read = os.ReadFile
	}
	content, err := read(path)
	if err != nil {
		return nil, fmt.Errorf("docstore: reading %s: %w", path, err)
	}
	return content, nil
}

// languageID applies the caller's overrides before the built-in table.
func (s *Store) languageID(path string) string {
	if len(s.opts.LanguageIDs) > 0 {
		if id, ok := s.opts.LanguageIDs[filepath.Base(path)]; ok {
			return id
		}
		if id, ok := s.opts.LanguageIDs[filepath.Ext(path)]; ok {
			return id
		}
	}
	return LanguageID(path)
}

// parseURI validates a URI received from a server and returns its
// canonical form and filesystem path.
func parseURI(uri protocol.DocumentURI) (protocol.DocumentURI, string, error) {
	if uri == "" {
		return "", "", errors.New("docstore: empty document URI")
	}
	parsed, err := protocol.ParseDocumentURI(string(uri))
	if err != nil {
		return "", "", fmt.Errorf("docstore: unusable document URI: %w", err)
	}
	clean := parsed.Clean()
	return clean, clean.Path(), nil
}

// resolve turns a possibly-relative path into an absolute path and
// its URI, the store's key.
func resolve(path string) (string, protocol.DocumentURI, error) {
	if path == "" {
		return "", "", errors.New("docstore: empty path")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", "", fmt.Errorf("docstore: resolving %s: %w", path, err)
	}
	return abs, protocol.URIFromPath(abs), nil
}
