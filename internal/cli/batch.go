package cli

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/tanevanwifferen/Lightspeed/internal/render"
)

// `lightspeed batch` — one query per input line, one envelope per
// output line.
//
// This is the command that makes lightspeed cheap for an agent to
// drive: an agent that wants twelve answers writes twelve lines
// instead of paying process startup and an LSP handshake twelve times
// over a shell loop.
//
// Two decisions are worth spelling out, because both had a plausible
// alternative.
//
// *One envelope per line, not one envelope containing all results.*
// A single wrapping envelope would mean nothing is printed until the
// last query finishes, and a batch is exactly where the last query is
// the one that hangs. Per-line envelopes stream: an agent reading
// stdout has answer 1 while answer 7 is still waiting on
// rust-analyzer, and a batch killed by a timeout still leaves the
// answers it did produce, each one a complete, valid envelope. It is
// also JSON-lines, which every consumer already has a parser for.
// Each line is byte-for-byte what the standalone command would have
// printed, plus a `query` field naming the invocation — so a caller
// can develop against `lightspeed references …` and batch it later
// without re-reading anything.
//
// *The exit code is the most severe failure, not the last or the
// first.* See batchExit.
//
// Input is a command line per line, tokenized like a shell without the
// shell: quotes and backslashes, no globbing, no substitution, no
// pipes. The alternative — a JSON object per line — is more precise
// but means an agent has to learn a second calling convention for the
// same commands, and translate flags into it. A line of `batch` input
// is a line an agent could have typed at a prompt.

// batchCommand implements `lightspeed batch`.
func batchCommand(e *env, c *command, args []string) int {
	fs := flag.NewFlagSet(c.Name, flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	fs.Usage = func() {
		fmt.Fprintf(e.stderr, "usage: lightspeed %s %s\n\n%s\n\nflags:\n", c.Name, c.Args, c.Summary)
		fs.PrintDefaults()
	}
	file := fs.String("file", "-", "read queries from this file (\"-\" is standard input)")
	failFast := fs.Bool("fail-fast", false, "stop at the first query that does not exit 0")
	summary := fs.Bool("summary", false, "print a final envelope counting the queries and their exit codes")
	indent := fs.Bool("indent", false,
		"pretty-print each envelope, for reading by eye (breaks the one-envelope-per-line contract; a per-query --indent is ignored)")
	if err := fs.Parse(args); err != nil {
		return e.flagError(render.Errorf(render.CodeUsage, "batch: %v", err))
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return e.usagef("batch: unexpected argument %q; queries are read from %s, not the command line",
			fs.Arg(0), *file)
	}

	input, closer, err := batchInput(e, *file)
	if err != nil {
		return e.fail(err)
	}
	if closer != nil {
		defer closer.Close()
	}

	var (
		exits   []int
		index   int
		scanner = bufio.NewScanner(input)
	)
	// A query line is a command line, and a command line with a long
	// --params blob is easily longer than bufio's default 64KiB.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		index++
		exit := e.runBatchLine(index, line, *indent)
		exits = append(exits, exit)
		if *failFast && exit != render.ExitOK {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return e.fail(render.Errorf(render.CodeIOError, "reading queries: %v", err))
	}
	if index == 0 {
		return e.usagef("batch: no queries on the input; write one command line per line")
	}
	if *summary {
		e.writeBatchSummary(exits, *indent)
	}
	return batchExit(exits)
}

// batchInput opens the query stream. "-" is the stream Main was given,
// which is os.Stdin in production and the test's own reader under test;
// a command running *inside* a batch has none, which is what makes
// `batch` inside `batch` impossible rather than merely discouraged.
func batchInput(e *env, file string) (io.Reader, io.Closer, error) {
	if file != "-" {
		f, err := os.Open(file)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, nil, render.Errorf(render.CodeNoSuchFile, "%s: no such file", file)
			}
			return nil, nil, render.Errorf(render.CodeIOError, "opening %s: %v", file, err)
		}
		return f, f, nil
	}
	if e.stdin == nil {
		return nil, nil, render.Errorf(render.CodeUsage,
			"batch: no input stream; a batch cannot be run from inside a batch")
	}
	return e.stdin, nil, nil
}

// batchQuery identifies one query in the output, so that a consumer
// need not count lines to know which answer it is reading.
type batchQuery struct {
	// Index is the 1-based position of the query in the input,
	// counting only the lines that were queries.
	Index int `json:"index"`
	// Command is the subcommand name, "" if the line named none.
	Command string `json:"command,omitempty"`
	// Argv is the line as tokenized, which is what actually ran.
	Argv []string `json:"argv"`
	// Exit is the exit code the query would have had on its own. The
	// batch's own exit code is derived from all of them; see
	// batchExit.
	Exit int `json:"exit"`
}

// runBatchLine runs one query and writes its envelope. It returns the
// exit code the query would have had as a standalone invocation.
func (e *env) runBatchLine(index int, line string, indent bool) int {
	argv, err := splitCommandLine(line)
	if err != nil {
		return e.writeBatchFailure(batchQuery{Index: index, Argv: argv}, err, indent)
	}
	if len(argv) == 0 {
		return e.writeBatchFailure(batchQuery{Index: index, Argv: argv},
			render.Errorf(render.CodeUsage, "line %d is empty after tokenizing", index), indent)
	}

	query := batchQuery{Index: index, Command: argv[0], Argv: argv}
	c := lookupCommand(argv[0])
	switch {
	case c == nil:
		return e.writeBatchFailure(query, render.Errorf(render.CodeUsage,
			"unknown subcommand %q (known: %v)", argv[0], commandNames()), indent)
	case c.Name == "batch":
		return e.writeBatchFailure(query, render.Errorf(render.CodeUsage,
			"a batch cannot contain a batch"), indent)
	}

	// The sub-command writes to a buffer so that its envelope can be
	// annotated before it reaches stdout, and shares stderr, so that
	// server logs and usage text still arrive where a human is
	// looking. It gets no stdin: see batchInput.
	var out strings.Builder
	sub := &env{stdout: &out, stderr: e.stderr}
	query.Exit = c.Run(sub, c, argv[1:])
	e.writeBatchLine(query, out.String(), indent)
	return query.Exit
}

// writeBatchLine emits one output line for a query that ran.
//
// The sub-command's own envelope is annotated rather than nested: the
// line stays exactly the shape a standalone invocation produces, with
// one extra key. A sub-command whose output is not an envelope — text,
// diff, or SARIF, all of which are deliberately envelope-free — is
// wrapped in one instead, with its bytes as a string, because a batch
// line that is not JSON would break every consumer of every other
// line.
func (e *env) writeBatchLine(query batchQuery, output string, indent bool) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(output), &envelope); err == nil && envelope["version"] != nil {
		e.writeBatchJSON(envelope, query, indent)
		return
	}
	data, err := json.Marshal(map[string]any{"format": "raw", "output": output})
	if err != nil { // unreachable: a string and two constants
		data = json.RawMessage(`{"format":"raw"}`)
	}
	e.writeBatchJSON(map[string]json.RawMessage{
		"version": json.RawMessage(fmt.Sprintf("%d", render.EnvelopeVersion)),
		"ok":      json.RawMessage(boolJSON(rawOutputIsAnswer(query.Exit))),
		"data":    data,
	}, query, indent)
}

// writeBatchFailure emits a line for a query that never ran: an
// unparseable line, an unknown subcommand, a nested batch. It is the
// same envelope shape any other failure has, so a consumer needs no
// special case for it.
func (e *env) writeBatchFailure(query batchQuery, err error, indent bool) int {
	query.Exit = render.ExitCode(err)
	var out strings.Builder
	_ = render.FailError(&out, err)
	e.writeBatchLine(query, out.String(), indent)
	return query.Exit
}

// writeBatchJSON writes an annotated envelope as one line.
//
// The answer is re-encoded, so a per-query --indent has no effect and the
// stream stays JSON-lines whatever a query asked for; `batch --indent`
// is the way to ask for readable output. Re-encoding also reorders the
// keys of the sub-envelope, which JSON does not care about and no
// consumer may.
func (e *env) writeBatchJSON(envelope map[string]json.RawMessage, query batchQuery, indent bool) {
	q, err := json.Marshal(query)
	if err == nil {
		envelope["query"] = q
	}
	enc := json.NewEncoder(e.stdout)
	if indent {
		enc.SetIndent("", "  ")
	}
	// Encode adds the newline that makes the stream JSON-lines.
	_ = enc.Encode(envelope)
}

// rawOutputIsAnswer reports whether a non-JSON output should be
// reported as ok. Exit 0 and exit 1 are both answers — "here it is"
// and "here it is, and it is bad news"; everything else is a
// malfunction or a refusal, and the envelope should not claim
// otherwise.
func rawOutputIsAnswer(exit int) bool {
	return exit == render.ExitOK || exit == render.ExitProblems
}

func boolJSON(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// batchSummaryData is the payload of the optional final envelope.
type batchSummaryData struct {
	Queries int `json:"queries"`
	// OK is how many queries exited 0, Failed the rest.
	OK     int `json:"ok"`
	Failed int `json:"failed"`
	// ByExit counts the queries per exit code, keyed by the code as a
	// string because JSON object keys are strings.
	ByExit map[string]int `json:"by_exit"`
	// Exit is the batch's own exit code, so a consumer that only reads
	// the summary line does not have to re-derive it.
	Exit int `json:"exit"`
}

// writeBatchSummary emits the final counting envelope of --summary. It
// carries no `query` field, which is how a consumer tells it from an
// answer.
func (e *env) writeBatchSummary(exits []int, indent bool) {
	data := batchSummaryData{Queries: len(exits), ByExit: map[string]int{}, Exit: batchExit(exits)}
	for _, exit := range exits {
		data.ByExit[fmt.Sprintf("%d", exit)]++
		if exit == render.ExitOK {
			data.OK++
		} else {
			data.Failed++
		}
	}
	_ = render.WriteEnvelope(e.stdout, render.Envelope{
		Version: render.EnvelopeVersion,
		OK:      data.Failed == 0,
		Data:    data,
	}, render.Options{Indent: indent})
}

// batchSeverity ranks the exit codes so that a batch's own exit code
// can be a single number without hiding the worst thing that happened.
//
// The order is "how much should the caller worry", not the numeric
// order, and it is deliberate:
//
//	ok        nothing to say
//	problems  a real answer that is bad news (exit 1)
//	no server something is missing and installing it would fix it (3)
//	not ready an answer existed but its authority could not be
//	          established, which is the one thing an agent must not
//	          treat as an answer (5)
//	usage     the batch input itself is wrong: the caller has a bug in
//	          what it asked for, and every later line is suspect (2)
//	crash     we do not know what happened (4)
//
// Reporting the *first* failure instead would let a batch whose second
// line found problems hide a crash on its tenth; reporting the last
// would depend on input order.
var batchSeverity = map[int]int{
	render.ExitOK:       0,
	render.ExitProblems: 1,
	render.ExitNoServer: 2,
	render.ExitNotReady: 3,
	render.ExitUsage:    4,
	render.ExitCrash:    5,
}

// batchExit reduces the per-query exit codes to one. An empty batch is
// handled by the caller (it is a usage error).
func batchExit(exits []int) int {
	worst := render.ExitOK
	for _, exit := range exits {
		if batchRank(exit) > batchRank(worst) {
			worst = exit
		}
	}
	return worst
}

// batchRank is batchSeverity with unknown codes ranked as crashes: an
// exit code this table does not know is, by definition, not something
// we can reason about.
func batchRank(exit int) int {
	if rank, ok := batchSeverity[exit]; ok {
		return rank
	}
	return batchSeverity[render.ExitCrash]
}

// splitCommandLine tokenizes one query line.
//
// It is a shell's quoting rules and nothing else: single quotes are
// literal, double quotes allow backslash escapes, and a backslash
// outside quotes escapes the next character. There is no globbing, no
// variable substitution, no command substitution and no pipeline —
// this is an argument vector, not a shell, and a batch file that looks
// like it can do more than it can is a trap.
//
// An unterminated quote is an error rather than a guess: the tokens
// that would result are not the ones the caller meant.
func splitCommandLine(line string) ([]string, error) {
	var (
		fields  []string
		current strings.Builder
		quoted  bool // current has been started, possibly as ""
	)
	flush := func() {
		if current.Len() > 0 || quoted {
			fields = append(fields, current.String())
			current.Reset()
			quoted = false
		}
	}
	runes := []rune(line)
	for i := 0; i < len(runes); i++ {
		switch c := runes[i]; c {
		case ' ', '\t':
			flush()
		case '\'':
			quoted = true
			end := slices.Index(runes[i+1:], '\'')
			if end < 0 {
				return fields, render.Errorf(render.CodeUsage,
					"unterminated single quote in %q", line)
			}
			current.WriteString(string(runes[i+1 : i+1+end]))
			i += end + 1
		case '"':
			quoted = true
			closed := false
			for i++; i < len(runes); i++ {
				if runes[i] == '\\' && i+1 < len(runes) {
					i++
					current.WriteRune(runes[i])
					continue
				}
				if runes[i] == '"' {
					closed = true
					break
				}
				current.WriteRune(runes[i])
			}
			if !closed {
				return fields, render.Errorf(render.CodeUsage,
					"unterminated double quote in %q", line)
			}
		case '\\':
			if i+1 >= len(runes) {
				return fields, render.Errorf(render.CodeUsage,
					"trailing backslash in %q", line)
			}
			i++
			current.WriteRune(runes[i])
		default:
			current.WriteRune(c)
		}
	}
	flush()
	return fields, nil
}
