package render

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os/exec"
	"strconv"
	"testing"
)

// declaredCodes parses codes.go for every declared Code constant, so a
// constant added without an exit-code mapping fails the build's tests
// rather than silently exiting 4 in production.
func declaredCodes(t *testing.T) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "codes.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing codes.go: %v", err)
	}
	out := map[string]string{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "Code" {
				continue
			}
			for i, name := range vs.Names {
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Fatalf("%s: expected a string literal", name.Name)
				}
				value, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("%s: %v", name.Name, err)
				}
				out[name.Name] = value
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("found no Code constants in codes.go")
	}
	return out
}

func TestEveryCodeHasAnExitCode(t *testing.T) {
	seen := map[string]string{}
	for name, value := range declaredCodes(t) {
		if _, ok := exitCodes[Code(value)]; !ok {
			t.Errorf("%s (%q) is missing from exitCodes", name, value)
		}
		if other, dup := seen[value]; dup {
			t.Errorf("%s and %s share the code %q", name, other, value)
		}
		seen[value] = name
	}
}

func TestExitCodesAreInTaxonomy(t *testing.T) {
	valid := map[int]bool{
		ExitOK: true, ExitProblems: true, ExitUsage: true,
		ExitNoServer: true, ExitCrash: true, ExitNotReady: true,
	}
	for code, exit := range exitCodes {
		if !valid[exit] {
			t.Errorf("code %q maps to %d, which is not in the PLAN §4 taxonomy", code, exit)
		}
		if exit == ExitOK {
			t.Errorf("code %q maps to exit 0, but a code means failure", code)
		}
	}
}

// notReadyError stands in for the readiness error that internal/client
// returns: it names its own exit code and nothing else, and it does not
// import render.
type notReadyError struct{ workspace string }

func (e *notReadyError) Error() string { return "workspace " + e.workspace + " is still indexing" }
func (e *notReadyError) ExitCode() int { return 5 }

// problemError names an exit code that disagrees with what its
// (absent) error code would imply, to prove ExitCoder wins.
type problemError struct{}

func (problemError) Error() string { return "found problems" }
func (problemError) ExitCode() int { return ExitProblems }

func TestExitCode(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, ExitOK},
		{"ExitCoder wins", &notReadyError{"/w"}, ExitNotReady},
		{"ExitCoder through wrapping", fmt.Errorf("references: %w", &notReadyError{"/w"}), ExitNotReady},
		{"ExitCoder by value", problemError{}, ExitProblems},
		{"coded usage", Errorf(CodeUsage, "bad flag"), ExitUsage},
		{"coded not ready", Errorf(CodeNotReady, "still indexing"), ExitNotReady},
		{"coded server error", Errorf(CodeServerError, "boom"), ExitProblems},
		{"coded through wrapping", fmt.Errorf("wrap: %w", Errorf(CodeNoServer, "none")), ExitNoServer},
		{"deadline", context.DeadlineExceeded, ExitCrash},
		{"cancelled", context.Canceled, ExitCrash},
		{"exec not found", fmt.Errorf("gopls: %w", exec.ErrNotFound), ExitNoServer},
		{"missing file", fmt.Errorf("open x: %w", fs.ErrNotExist), ExitUsage},
		{"unclassified", errors.New("something went sideways"), ExitCrash},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExitCode(tt.err); got != tt.want {
				t.Errorf("ExitCode(%v) = %d, want %d", tt.err, got, tt.want)
			}
		})
	}
}

func TestExitCodeForUnknownCodeIsCrash(t *testing.T) {
	if got := ExitCodeForCode("something_we_never_declared"); got != ExitCrash {
		t.Errorf("unknown code mapped to %d, want %d", got, ExitCrash)
	}
}

func TestCodeForError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want Code
	}{
		{"nil", nil, ""},
		{"coded", Errorf(CodeEditConflict, "overlap"), CodeEditConflict},
		{"deadline", fmt.Errorf("hover: %w", context.DeadlineExceeded), CodeTimeout},
		{"cancelled", context.Canceled, CodeCancelled},
		{"exec not found", fmt.Errorf("rust-analyzer: %w", exec.ErrNotFound), CodeServerNotInstalled},
		{"missing file", fmt.Errorf("open: %w", fs.ErrNotExist), CodeNoSuchFile},
		{"exit coder only", &notReadyError{"/w"}, CodeNotReady},
		{"unclassified", errors.New("boom"), CodeInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CodeForError(tt.err); got != tt.want {
				t.Errorf("CodeForError(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}

func TestCodedErrorWraps(t *testing.T) {
	cause := errors.New("underlying")
	err := Errorf(CodeServerCrash, "gopls died: %v", cause)
	if !errors.Is(err, cause) {
		t.Error("Errorf did not wrap its error operand")
	}
	if got := err.ErrorCode(); got != CodeServerCrash {
		t.Errorf("ErrorCode() = %q", got)
	}
	if got := err.ExitCode(); got != ExitCrash {
		t.Errorf("ExitCode() = %d, want %d", got, ExitCrash)
	}
	// A CodedError must not leak a chain into its message; the envelope
	// carries the code separately.
	if got := message(err); got != "gopls died: underlying" {
		t.Errorf("message() = %q", got)
	}
}

func TestWithDetailsDoesNotMutate(t *testing.T) {
	base := Errorf(CodeServerNotInstalled, "gopls not on PATH")
	with := base.WithDetails(map[string]string{"install": "mise use -g go:golang.org/x/tools/gopls"})
	if base.Details != nil {
		t.Error("WithDetails mutated the receiver")
	}
	if with.Details == nil || with.Code != base.Code || with.Message != base.Message {
		t.Errorf("WithDetails lost fields: %+v", with)
	}
}
