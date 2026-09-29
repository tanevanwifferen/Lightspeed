# Lightspeed

*Your editor already understands your code. Now everything else can too.*

---

Somewhere on your machine there is a program that knows your codebase better
than you do. It knows that `Find` on line 42 is called from eleven places, that
three of them are in tests, and that renaming it will touch four files. It knows
which interface that struct quietly satisfies. It has read every import.

That program is a language server — `gopls`, `rust-analyzer`, `pyright` and
friends — and for most of its life it only talks to your editor. Ask it a
question from a terminal, a script, or a coding agent, and you get silence.

Lightspeed gives it a voice outside the editor.

```sh
lightspeed references --symbol store.UserRepo.Find
```

One command in, one honest answer out. Lightspeed works out which language
server owns the file, wakes it up, waits until it has actually finished
thinking, asks the question, and hands you back something a person can read and
a program can parse.

## Why this exists

Coding agents are remarkable readers and terrible navigators. Left to their own
devices they `grep` for a function name, open whole files to find ten lines,
and decide that code is unused because a search came back empty. It works right
up until it doesn't — and when it doesn't, live code gets deleted.

The fix is not a cleverer grep. The fix is to ask the tool that already *knows*:
the compiler's own understanding of the code, served fresh from disk every time,
with no index quietly going stale in the background.

Lightspeed was built for agents first, scripted refactors second, and curious
humans third. It turns out all three want the same thing: an answer they can
trust.

## Three small promises

Plenty of tools can put a command line in front of a language server. What makes
Lightspeed worth using is three unglamorous promises it keeps underneath.

**It never mistakes "I don't know yet" for "nothing".** A language server that
is still indexing will cheerfully tell you a function has zero references. It is
not lying, exactly — it just hasn't looked yet. An agent that believes it will
delete working code. Lightspeed watches the server's progress and refuses to
pass along an empty answer it cannot vouch for. Instead of an empty list you
get a clear *not ready yet, ask again* — its own exit code, never confused with
a real "none".

**It counts columns the way you do.** Language servers measure positions in
UTF-16 code units; people and shells think in bytes. The difference is invisible
until an identifier contains an accent or an emoji, and then every result is
off by a few characters. Lightspeed converts with gopls's own battle-tested
mapper, so a location it prints can always be pasted straight back in.

**It changes everything or nothing.** A rename across thirty files either lands
completely or leaves your tree exactly as it was — even when the server sends
edits that overlap or point somewhere they shouldn't. Every change is a preview
until you say `--apply`, and it won't write over uncommitted work unless you
insist.

## A short tour

Read a single function without opening the file it lives in:

```sh
lightspeed outline internal/store/user.go
lightspeed source 'internal/store/user.go::UserRepo.Find#method'
```

Find out what a change would ripple into before you make it:

```sh
lightspeed blast_radius --symbol UserRepo.Find
lightspeed rename_check internal/store/user.go:42:8 Lookup
```

Then make it, safely:

```sh
lightspeed rename internal/store/user.go:42:8 Lookup           # shows the diff
lightspeed rename internal/store/user.go:42:8 Lookup --apply   # writes it
```

Or just describe what you are trying to do, and let it find the relevant code:

```sh
lightspeed task_context "make the retry budget configurable"
```

There's more — call hierarchies, dead-code hunting, import graphs, the symbols a
diff touched, the files that churn the most — but you get the idea. Every
answer is structured JSON when a program is listening and readable text when a
person is.

## Fast after the first question

Language servers are slow to wake. rust-analyzer can spend a minute indexing a
large workspace before it says anything useful. So the first command you run
quietly starts a small background daemon for that workspace, and the servers it
starts stay warm. The second question costs a round trip, not a startup.

The daemon cleans up after itself. Idle servers are shut down after ten minutes,
and the daemon leaves after thirty. If you edit a file, switch branches or
rebuild Lightspeed in between, it notices and never answers from an old
snapshot. If you would rather have none of this, `--no-daemon` gives every
command a fresh server of its own.

## Made for agents

Lightspeed speaks [MCP](https://modelcontextprotocol.io), so an agent can use it
as a set of tools rather than shelling out:

```sh
claude mcp add lightspeed -- lightspeed mcp
```

Every command becomes a tool with typed parameters. The server also ships its
own short guide, telling the agent which tool answers which question, and that
*not ready* means "wait", never "nothing found". The guide is generated from the
same command table as the tools, so the two can't drift apart.
[docs/AGENT-SETUP.md](docs/AGENT-SETUP.md) walks through the full setup.

## Getting it

You need Go 1.27 or newer:

```sh
go install github.com/tanevanwifferen/Lightspeed/cmd/lightspeed@latest
```

Lightspeed never downloads anything behind your back — including language
servers. When one is missing it tells you exactly how to get it, and will run
that step for you if you ask:

```sh
lightspeed install gopls          # shows what it would run
lightspeed install gopls --run    # runs it, through mise
```

Out of the box it knows `gopls` (Go), `rust-analyzer` (Rust), `pyright`
(Python), `vtsls` (TypeScript and JavaScript), `clangd` (C and C++) and
`lua-language-server` (Lua). Teaching it another one is a few lines of TOML.
When something isn't answering, `lightspeed doctor` explains why.

## A word of caution

A language server runs your project's build tooling — `go list`,
`cargo metadata` and the like — and that means running code from the checkout.
Pointing Lightspeed at a repository is roughly as trusting as building it. Use
it on code you would be willing to build.

## Where to go next

- **[docs/REFERENCE.md](docs/REFERENCE.md)** — the complete manual: every
  command, flag, output format and exit code, with the reasoning behind them.
- **[docs/AGENT-SETUP.md](docs/AGENT-SETUP.md)** — wiring Lightspeed into a
  coding agent.
- **[docs/DECISIONS.md](docs/DECISIONS.md)** — the design log: why things are
  the way they are.
- **[PLAN.md](PLAN.md)** — the original design, and what is still to come.

Lightspeed includes code from gopls and the nvim-lspconfig project. See
[ATTRIBUTION](ATTRIBUTION) for details.
