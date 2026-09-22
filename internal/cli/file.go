package cli

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/tanevanwifferen/Lightspeed/internal/render"
	"github.com/tanevanwifferen/Lightspeed/internal/router"
)

// `file`: a line range of a file, inside the workspace (docs/DECISIONS.md D23).
// It is the fallback for what has no symbol — a config file, a README, a
// stretch between declarations — and the one command whose whole job is to
// read a path it was given, which is why it is confined to the workspace.

const (
	// maxFileBytes is the largest file `file` reads. Past it the answer is
	// almost certainly not something an agent should be handed a slice of
	// blind, and reading it whole to find lines 10 to 20 is not free.
	maxFileBytes = 16 << 20
	// binarySniff is how much of a file is looked at for a NUL byte.
	binarySniff = 8192
)

// fileData is the payload of `file`.
type fileData struct {
	// File is the path relative to the workspace root.
	File     string `json:"file"`
	Language string `json:"language,omitempty"`
	// StartLine and EndLine are 1-based and inclusive, and describe Source:
	// EndLine stops where a cap cut.
	StartLine int `json:"start_line"`
	EndLine   int `json:"end_line"`
	// TotalLines is how many lines the file has.
	TotalLines int    `json:"total_lines"`
	Source     string `json:"source"`
	// Hash is of the requested lines, before any cap.
	Hash string `json:"hash"`
	// Truncated is true when --max-lines or --max-bytes cut Source short.
	Truncated bool `json:"truncated"`
}

// insideWorkspace resolves a path the command was given to a real path inside
// the workspace, and refuses one that is not. Symlinks are followed first: a
// link in the workspace that points out of it is out of it.
func insideWorkspace(root, arg string) (real, rel string, err error) {
	abs, err := filepath.Abs(arg)
	if err != nil {
		return "", "", render.Errorf(render.CodeUsage, "resolving %s: %v", arg, err)
	}
	// A path that is outside the workspace is refused whether or not it
	// exists: "no such file" for a path that leaves the workspace would say
	// which of them are there.
	if _, in := relToRoot(root, abs); !in {
		return "", "", render.Errorf(render.CodeOutsideWorkspace,
			"%s is outside the workspace %s; only files inside it can be read", arg, root)
	}
	real, err = filepath.EvalSymlinks(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return "", "", render.Errorf(render.CodeNoSuchFile, "%s: no such file", arg)
		}
		return "", "", render.Errorf(render.CodeIOError, "%s: %v", arg, err)
	}
	rel, in := relToRoot(root, real)
	if !in {
		return "", "", render.Errorf(render.CodeOutsideWorkspace,
			"%s is outside the workspace %s; only files inside it can be read", arg, root)
	}
	return real, rel, nil
}

// fileCommand implements `lightspeed file <path> [--start N --end M]`.
func fileCommand(e *env, c *command, args []string) int {
	var (
		sf         sourceFlags
		start, end int
	)
	common, positional, err := parseFlags(e, c, args, 1, func(fs *flag.FlagSet) {
		sf.register(fs)
		fs.IntVar(&start, "start", 0, "first line to return, 1-based (default 1)")
		fs.IntVar(&end, "end", 0, "last line to return, inclusive (default the last line of the file)")
	})
	if err != nil {
		return e.flagError(err)
	}
	if err := sf.validate(c.Name); err != nil {
		return e.fail(err)
	}
	format, err := managementFormat(common, "file", e.stdout)
	if err != nil {
		return e.fail(err)
	}
	if start < 0 || end < 0 {
		return e.usagef("file: --start and --end are 1-based line numbers, not negative")
	}
	root, err := commandWorkspace(sf.path)
	if err != nil {
		return e.fail(err)
	}
	real, rel, err := insideWorkspace(root, positional[0])
	if err != nil {
		return e.fail(err)
	}
	if err := mustBeFile(real); err != nil {
		return e.fail(err)
	}
	info, err := os.Stat(real)
	if err != nil {
		return e.fail(render.Errorf(render.CodeIOError, "%s: %v", positional[0], err))
	}
	if info.Size() > maxFileBytes {
		return e.usagef("file: %s is %d bytes, over the %d this command reads", rel, info.Size(), maxFileBytes)
	}
	content, err := os.ReadFile(real)
	if err != nil {
		return e.fail(render.Errorf(render.CodeIOError, "%s: %v", positional[0], err))
	}
	if bytes.IndexByte(content[:min(len(content), binarySniff)], 0) >= 0 {
		return e.usagef("file: %s is a binary file", rel)
	}

	x := newLineIndex(content)
	total := x.Count()
	first, last := max(start, 1), end
	switch {
	case total == 0 && start <= 1:
		first, last = 1, 0
	case first > total:
		return e.usagef("file: --start %d is past the end of %s, which has %d line(s)", start, rel, total)
	}
	if last == 0 || last > total {
		last = total
	}
	if last < first && total > 0 {
		return e.usagef("file: --end %d is before --start %d", end, first)
	}

	whole := x.Slice(first-1, last-1)
	src, cut := capSource(whole, sf.maxLines, sf.maxBytes)
	data := fileData{
		File:       rel,
		Language:   router.LanguageID(real),
		StartLine:  first,
		EndLine:    first - 1 + lineCount(src),
		TotalLines: total,
		Source:     string(src),
		Hash:       contentHash(whole),
		Truncated:  cut,
	}
	var warnings []string
	if cut {
		warnings = append(warnings, fmt.Sprintf("cut at the cap: lines %d-%d of the %d requested were returned", data.StartLine, data.EndLine, last-first+1))
	}
	if format == render.FormatText {
		writeFileText(e.stdout, data)
		return ExitOK
	}
	return e.writeData(common, data, warnings, ExitOK)
}

// lineCount is how many lines a slice of source holds: none for nothing.
func lineCount(src []byte) int {
	if len(src) == 0 {
		return 0
	}
	return bytes.Count(src, []byte("\n")) + 1
}

// writeFileText prints one line per line, `file:line: text`, grep-compatible.
func writeFileText(w io.Writer, d fileData) {
	if d.Source == "" {
		return
	}
	for i, line := range strings.Split(d.Source, "\n") {
		fmt.Fprintf(w, "%s:%d: %s\n", d.File, d.StartLine+i, strings.TrimSuffix(line, "\r"))
	}
}
