# forge

A from-scratch coding-agent stack. **Zero external dependencies** — Go standard
library only, one static binary.

**Phase 0 + 1** benchmark what your hardware and your free tiers actually
deliver, then expose all of them as a single OpenAI-compatible endpoint that
fails over automatically when one runs out.

**Phase 2** adds the agent: a ReAct loop, ten tools, an approval gate, and an
edit protocol chosen so that small models can actually use it.

**Phase 3** adds the context engine — the repo map, overflow handles, and
compaction. This is the part that actually fixes running out of context.

**Phase 4** adds retrieval: semantic chunking, BM25, a binary-quantized vector
store, and rank fusion over both.

**Phase 5** adds the verify loop: the project's own build and tests run when the
model thinks it is done, failures are parsed down to their locations, and the
model gets another attempt.

**Phase 6** adds sub-agents: delegated work runs in its own context and returns
only its conclusion.

**Phase 7** adds the terminal UI: a live status region and a scrollable
approval surface, written from scratch on console syscalls and ANSI.

**Phase 8** adds the embedding model runtime — safetensors, WordPiece, and a
BERT forward pass in pure Go — so semantic search needs no server at all.

All eight phases are done.

---

## Why this exists

Free model tiers each have a limit. Stacked behind one router with
rate-limit-aware failover, they behave like one endpoint that doesn't run out.
A local model sits at the end of every chain as the backstop that is never
rate limited and never costs anything.

```
   class "coder"  ──▶  cerebras ──▶ groq ──▶ gemini ──▶ openrouter ──▶ local
                        (429)      (quota)    ✓
                          │           │
                     parked 20s   parked 1h
```

Cooldowns persist to disk. Restarting does not re-hammer a provider that just
rate-limited you — which is the fastest way to lose a free tier.

---

## Build

```bash
go build -o forge.exe ./cmd/forge
```

## Quick start

```bash
forge init
```

Writes `~/.forge/config.json` with the free providers pre-wired. Then export
whichever keys you have — any provider without a key is skipped automatically,
so one key is enough to start:

```bash
setx CEREBRAS_API_KEY "..."
```

| Provider | Key env var | Where |
|---|---|---|
| Cerebras | `CEREBRAS_API_KEY` | cloud.cerebras.ai |
| Groq | `GROQ_API_KEY` | console.groq.com/keys |
| Gemini | `GEMINI_API_KEY` | aistudio.google.com/apikey |
| OpenRouter | `OPENROUTER_API_KEY` | openrouter.ai/keys |
| GitHub Models | `GITHUB_MODELS_TOKEN` | GitHub PAT, `models:read` scope |

`setx` only affects **new** terminals. Open a fresh one, then:

```bash
forge doctor
```

Shows every provider's reachability and every class chain's resolved state.

---

## Commands

| Command | What it does |
|---|---|
| `forge init` | Write a starter config |
| `forge doctor` | Check providers and class chains; `-reset` clears cooldowns |
| `forge models <provider>` | List the model ids a provider actually offers |
| `forge bench` | Measure prefill/decode throughput |
| `forge chat -class coder "..."` | One-shot prompt through a class |
| `forge do "..."` | Run the coding agent on a task |
| `forge map` | Print the ranked repository map the agent sees |
| `forge index` | Build or update the code search index |
| `forge search "..."` | Query the index directly |
| `forge verify` | Run the project's build, lint, and test checks |
| `forge embed -bench` | Measure the built-in encoder's throughput |
| `forge app` | Open FORGE as a desktop app in its own window |
| `forge gui` | Browser interface on `127.0.0.1:4100` |
| `forge serve` | OpenAI-compatible endpoint on `127.0.0.1:4000` |
| `forge usage -since 24h -by provider` | Summarize the token ledger |
| `forge selfcheck` | Run routing/streaming invariants against stub servers |

**Model ids drift.** If `doctor` reports a model-not-found, run
`forge models cerebras` and correct the id in the config. Never guess.

---

## Install as a desktop app

```bash
go run ./cmd/mkdist
```

Produces `dist/Forge.exe` — one file with the application inside it. Run it and
it installs for the current user: binaries under `%LOCALAPPDATA%\Programs\FORGE`,
Start Menu and Desktop shortcuts, and `forge` on your PATH. **No administrator
rights and no UAC prompt**, because nothing is written outside your own profile.
`go run ./cmd/mkdist -all` cross-builds installers for Windows, macOS, and Linux.

Uninstalling removes exactly what installation created and leaves `~/.forge`
— your config, history, and index — alone:

```bash
"%LOCALAPPDATA%\Programs\FORGE\forge-setup.exe" -uninstall
```

Two binaries are installed. `forge.exe` is the CLI. `forge-app.exe` is the same
program linked for the GUI subsystem, so double-clicking it opens the app with
no console window flashing up behind it. The icon is generated from geometry at
install time rather than shipped as a file, so it is sharp at every size the
shell asks for.

**On any desktop, without the installer:**

```bash
forge app
```

Opens FORGE in its own window using a Chromium-family browser's app mode —
no tab strip, no address bar, its own taskbar entry and profile. Closing the
window stops the server, so a run never keeps going unwatched. There is no
Electron runtime to bundle and no Rust toolchain to build against; a desktop
shell that needed a C compiler would undo the single-static-binary property the
whole project is organised around.

Failing that, the interface installs straight from the browser as a PWA —
Chrome and Edge on desktop, Android, and iOS all give it a real application
icon and window. Settings has an **Install** button when the browser offers one.

---

## The browser interface

```bash
forge gui -dir .
```

Opens `127.0.0.1:4100`. Same agent, same config, same workspace sandbox as
`forge do` — the difference is that a run becomes something you can watch,
interrupt, and approve a diff at a time.

- **Sessions** stream live over server-sent events: prose as it is generated,
  each tool call as one dense line that fills in its result, files as they are
  written, tokens and elapsed time as they accrue.
- **Approval** is a full-bleed diff. Added and removed lines carry a colour
  tint instead of `+`/`-` columns, and `Y`/`N` work without reaching for the
  mouse. The agent is genuinely blocked while you decide.
- **Reconnects replay.** Every session keeps an event log and the stream
  resumes from the last sequence number you saw, so a reload or a closed
  laptop lid does not lose the middle of a run.
- **Search, repository map, providers, usage, verification** are the same
  engines the CLI uses, rendered.

The whole front end is compiled into the binary and makes **no network
requests** — no CDN, no web fonts, no analytics. It renders on a machine with
the network cable pulled, which is the point of a local-first agent.

Type is the Apple system stack (`SF Pro Text` / `SF Pro Display` / `SF Mono`,
falling back through Helvetica Neue and Segoe UI), so it looks native on macOS
and iOS and stays metrically close everywhere else.

Flags worth knowing: `-addr` to move the port, `-open=false` to skip launching
a browser, `-dir` to set the workspace the agent may act in. Everything else
matches `forge do`.

Because it is one static bundle over HTTP on localhost, it installs as a PWA
from the browser on desktop, Android, and iOS, and wraps in Tauri for a ~5 MB
desktop app without a second implementation.

---

## Use it from any editor today

```bash
forge serve
```

Then point Cline, Roo Code, Continue, Aider, or anything else that speaks the
OpenAI API at:

- **Base URL:** `http://127.0.0.1:4000/v1`
- **API key:** any non-empty string
- **Model:** `forge-planner`, `forge-coder`, `forge-cheap`, or `forge-local`

You inherit the whole fallback chain without the client knowing anything about
it. `GET /v1/status` shows live per-target health.

You can also pin a specific backend by using `provider:model` as the model
name — e.g. `cerebras:qwen-3-coder-480b` — which bypasses the chain entirely.

---

## The agent

```bash
forge do -dir . -approval ask "make the bench harness accept a -json flag"
```

The loop is small on purpose: the model proposes actions, the actions run,
results go back into context, repeat until it stops asking. What determines
whether it works is everything around that loop — the tool contracts, the
failure feedback, and the stop conditions.

### Tools

| Tool | Mutates | Notes |
|---|---|---|
| `read_file` | | Line-numbered, with `offset`/`limit` and a continuation hint |
| `glob` | | `**` supported; newest files first |
| `grep` | | RE2 regex, parallel scan, skips deps and binaries |
| `list_dir` | | Depth-limited tree |
| `edit_file` | ✓ | Exact-string replace; refuses ambiguous matches |
| `write_file` | ✓ | Whole-file write; shows a diff first |
| `run_command` | ✓ | Timeout + kill; destructive-pattern classifier |
| `todo_write` | | Keeps a multi-step plan in view cheaply |
| `expand` | | Retrieves more of a truncated result by handle |
| `remember` | | Saves a durable fact for future sessions |
| `search_code` | | Hybrid keyword + semantic search over the repo |
| `verify` | ✓ | Runs the project's checks; summarised to located failures |
| `task` | | Delegates to a read-only sub-agent in its own context |

### Editing: why SEARCH/REPLACE blocks

A JSON tool call has to carry code as an escaped string, and small models mangle
that constantly — one stray backslash loses the whole call. Blocks put the code
in the message body instead, where no escaping is involved:

```
path/to/file.go
<<<<<<< SEARCH
the exact existing lines
=======
the replacement lines
>>>>>>> REPLACE
```

Empty SEARCH creates a file; empty REPLACE deletes lines. Pick the protocol with
`-edit`:

| Mode | Behaviour |
|---|---|
| `blocks` | Blocks only; `edit_file` is hidden from the model. Best for a local 7B. |
| `tool` | JSON `edit_file` only. Fine on strong hosted models. |
| `auto` *(default)* | Documents blocks, accepts either. |

Matching escalates through three strategies before giving up: exact, then
ignoring trailing whitespace, then normalising indentation — and in that last
case the replacement is re-indented to match the file, so a model that sends
spaces into a tab-indented file still produces a correct edit. An ambiguous
match is always refused rather than guessed, and CRLF files stay CRLF.

### Approval modes

The workspace root bounds *where* the agent can act. Approval bounds *what* it
may do without asking. They are separate: a permissive mode never widens the
path sandbox.

| `-approval` | Behaviour |
|---|---|
| `readonly` | Denies every mutation. Good for "explain this codebase". |
| `ask` *(default)* | Prompts for every mutation, except allowlisted inspection commands. |
| `auto-edit` | File edits go through silently; commands still prompt. |
| `yolo` | Everything auto **except** destructive commands, which always prompt. |

Commands matching destructive patterns (`rm -rf`, `git push --force`,
`Remove-Item -Recurse`, `curl … | sh`, …) require typing `yes` in full, in every
mode. Chaining defeats the allowlist: `git status && rm -rf /` is not
allowlisted just because it starts with `git status`.

If stdin is not a terminal, anything that would prompt is **denied**, not
allowed. Use `-approval auto-edit` for unattended runs.

### Stop conditions

The loop ends on: a turn with no tool calls and no edit blocks (done), the step
limit (`-max-steps`, default 30), the token budget (`-max-tokens`), Ctrl-C, or a
user abort at an approval prompt. There is also a loop guard — a model that
issues the same failing call four times gets told so explicitly rather than
burning the remaining budget on it.

## The context engine

Running out of context is not a model problem. It is what happens when every
tool result lands in the conversation and stays there. Three mechanisms deal
with it, and they compose.

### 1. The repo map

A model with no map orients itself by reading files — thousands of tokens and,
on a CPU-bound local model, minutes of prefill before any work starts. The map
puts the shape of the whole codebase in about a thousand tokens instead.

On this repository:

| | tokens |
|---|---:|
| Reading all Go source (9,498 lines) | ~86,000 |
| Repo map at the default budget | ~1,060 |

**81× smaller**, built in 15 ms, and it covers every file rather than the three
the model happened to guess at.

Ranking is PageRank over a file-to-file reference graph, so "important" means
*much of the rest of the code depends on this* — not "large" or "recently
edited". That property is what makes a short map useful; a size-ranked list
would be dominated by generated files.

```bash
forge map -tokens 1024
forge map -ranked
forge do -focus internal/router/router.go "..."
```

`-focus` biases ranking toward the area you are working in. Symbol extraction
is regex-based rather than a real parser: it occasionally misses a declaration
or invents one, which moves a file a place or two in the ordering and changes
nothing else. That is a cheap error; a parser dependency is not.

### 2. Overflow handles

A wide grep can put 12,000 tokens into context in one call. Clipping alone
loses the rest. Clipping *with a handle* keeps it retrievable:

```
[showing lines 1-120 of 4,312. Call expand(id="r3", offset=121) for the rest.]
```

The full text is parked on disk; the model pulls only the part it needs. The
truncator does not know which 40 lines matter — the model does.

### 3. Compaction

When the conversation approaches the window, the middle is replaced by a
summary. The system prompt and the original task stay verbatim at the head; the
last few turns stay verbatim at the tail. Only the exploration that already
served its purpose gets collapsed.

It runs on the `cheap` class — local-first, since this is exactly the call not
worth a hosted quota — and triggers two ways:

- **Proactively** at `-compact-at` (default 0.7) of the context budget.
- **Reactively** when every target rejects the request as too long, then
  retries once.

The tail boundary is slid forward off any tool message. A tool result whose
originating `tool_calls` message was compacted away is a dangling reference,
and providers reject the whole request over it.

### Prompt prefix stability

Everything invariant — system prompt, notes, repo map — lives in a single
system message built once per run. Keeping that prefix byte-identical across
turns is what lets llama.cpp reuse its KV cache, which on a CPU-bound local
model turns a multi-minute prefill into a near-instant one. Nothing volatile
goes in there, and the repo map is deliberately *not* rebuilt mid-run.

### Cross-session notes

`remember` saves a durable fact — a build command that works, a non-obvious
constraint — loaded into the system prompt on the next run. Notes live in
forge's state directory, never in your repository: an agent writing a NOTES.md
into your project is a side effect you did not ask for, and it would show up in
your next `git status`. Disable with `-no-notes`.

## The terminal UI

```bash
forge do "..."            # rich when the terminal supports it
forge do "..." -ui plain  # force plain
```

### Not a full-screen TUI

The transcript stays in your terminal's own scrollback, where you can scroll,
select, and copy it with the tools you already have. Taking over the screen
with an alternate buffer would trade all of that for a redraw loop.

Instead there is a **pinned two-line status region** at the bottom. Before
emitting new output, the renderer walks the cursor back over those lines and
erases to the end of the screen; afterwards it redraws them. Nothing else
moves.

The status shows step, current activity, elapsed time, live token counts, the
provider actually serving the request, files changed, and delegations. The token
counter earns its place: on a free-tier stack the question that matters mid-run
is how much budget this has eaten, and the budget fraction turns yellow at 70%
and red at 90% — while there is still room to act on it.

### Approvals you can actually read

The old flow dumped a diff into scrollback. On anything longer than a screen
the top scrolled away before you could read it, and the part you were being
asked to judge was the part you could not see.

Now it is a scrollable pane: `↑ ↓` line, `PgUp`/`PgDn`/space by screen,
`Home`/`End`, with `y` / `n` / `a` / `q` answering. Destructive operations still
demand the typed word `yes`, and `a` (always allow this tool) is refused on them
— the point of that flag is that each irreversible action gets its own decision.

The UI approver and the plain console **share one `Policy` object**. Two
implementations of "may this run" would eventually disagree, and the failure
mode is a mutation happening that you believed was gated.

### Degradation

Rich mode needs three separate things: stdout must accept ANSI, stdin must be a
terminal, and raw mode must actually engage. Any one failing drops to plain
rather than producing a half-working interface. The chosen mode is printed at
startup, so a silent fallback is visible rather than mysterious:

```
ui:        plain (stdin is not a terminal)
```

`NO_COLOR` and `TERM=dumb` are honoured. Piped output contains no escape
sequences at all.

### Written from scratch

`golang.org/x/term` is a perfectly good package and exactly the kind of thing
this project does not take. What it does is small — a few console-mode syscalls
per platform, plus an escape-sequence decoder:

- **Windows**: `GetConsoleMode`/`SetConsoleMode` via `syscall.NewLazyDLL`, with
  `ENABLE_VIRTUAL_TERMINAL_INPUT` so arrows arrive as the same escape sequences
  a Unix terminal sends — which is what lets one decoder serve both platforms.
- **Linux/Darwin**: `termios` through `SYS_IOCTL`, with the ioctl constants
  split into per-OS files (`TCGETS` vs `TIOCGETA`).
- **Anything else**: a stub that reports no TTY, so forge still builds and runs.

All layout logic is pure — content and a viewport in, lines out — so wrapping,
truncation, and paging are tested with no terminal attached. That is where the
bugs actually are.

## Sub-agents

An exploration that reads nine files to answer one question leaves all nine in
the main conversation forever, re-sent on every subsequent turn until
compaction throws them away. Running it in a separate context and returning
only the conclusion keeps the finding and discards the search.

Measured on a delegation that read a file and reported back:

| | |
|---|---:|
| tokens spent inside the sub-agent | 1,335 |
| characters returned to the caller | 39 |

The corollary, which the sub-agent's prompt states in as many words: **its final
message is its entire output.** Nothing else it does is visible. A sub-agent
that answers "I found it in the config package" has failed, however good the
work behind that sentence was.

### Roles

| Role | Class | For |
|---|---|---|
| `explore` | cheap | "Where is X handled", "what calls Y" |
| `review` | coder | Adversarial bug-hunting on changed code |
| `plan` | planner | Scoping a change before any edits happen |

If your config doesn't define a role's preferred class, it falls back to the
parent's — refusing to delegate over a missing convenience class would trade
away the isolation, which is the part that matters.

### Sub-agents are read-only

They can read, glob, grep, and search. They cannot write, run commands, verify,
or delegate further. That is deliberate: parallel delegations editing the same
tree would race, and an approval prompt attributed to an invisible sub-context
is not something a human can meaningfully answer. No role lists the `task` tool,
so recursion is impossible rather than merely discouraged.

### Parallelism

Delegations issued in one turn run concurrently — 3 × 250 ms of work completes
in 257 ms. Everything touching the filesystem or the approver stays sequential.
Results are always appended in the order the model requested them, because
providers reject tool results that don't line up with their `tool_calls`.

```bash
forge do "..." -parallel-subagents 1   # for a local-only setup, where
                                       # concurrent requests just queue
```

### Accounting

Tokens spent inside a delegation are real spend and roll up into the run's
total, even though none of them ever entered the conversation:

```
3 delegation(s) spent 2,220 tok in contexts this conversation never saw
```

Bounded by `-max-subagents` (default 8), so a loop of "explore some more"
can't run up an unbounded bill. An over-long report is clamped to the role's
word cap, visibly — a chatty sub-agent would otherwise reintroduce the exact
cost delegation exists to avoid.

## The verify loop

A model's confidence that it is finished is not evidence. When it stops calling
tools, `forge` runs the project's own checks, and if they fail it hands back the
located failures and asks for another attempt.

```bash
forge verify                     # standalone
forge do "..." -max-repairs 3    # automatic; 3 is the default
```

### Detection

Commands are inferred from marker files — `go.mod`, `Cargo.toml`,
`package.json` scripts, `pyproject.toml`, declared `Makefile` targets. Only
targets that actually exist are claimed; there is no invented `make test`.
Override with `-verify-cmd "..."`, disable with `-no-verify`.

Checks run in stage order — **build → lint → test** — stopping at the first
failing required stage. Running tests against a tree that does not compile
produces failures that describe the build error less usefully, and a model shown
both will often chase the wrong one. Linters are optional: a style complaint
should not block an otherwise correct fix.

### Parsing is where the value is

Handing a model raw test output costs a fortune and buries the three lines that
identify the bug. Measured on a realistic failing build:

| | bytes |
|---|---:|
| raw compiler output | 29,083 |
| parsed, located failures | 103 |

**282× smaller.** Parsers cover Go build and test, rustc, tsc, pytest, and jest,
each degrading to a generic `path:line: message` form rather than returning
nothing on an unrecognised toolchain.

Go test failures carry their test name, because a bare `main_test.go:7: want 5`
does not say which case produced it:

```
main_test.go:7 [TestAdd]  Add(2,3) = -1, want 5
```

Repeats are collapsed and the list is capped at 12 — a cascade of two hundred
errors from one missing import is noise.

### Undo journal, not auto-commits

Every editing tool records a file's original bytes immediately before writing
it, so a whole run can be undone with `-revert-on-fail`.

This is deliberately **not** automatic git commits. Writing commits into your
repository is a side effect you did not ask for: it pollutes your history and
reflog, interacts badly with an in-progress rebase or a dirty index, and does
not work outside a git repo at all. The journal has none of those problems, and
capture is free — the editing tools already read the original bytes to build
their diff.

A declined approval records nothing. Originals are saved under `~/.forge/undo/`
so a bad edit stays recoverable even after the process exits.

### Honest reporting

`Verified` is true only when checks **ran and passed**. No checks configured
means "not run", never "verified clean". If nothing was changed, verification is
skipped entirely — nothing can have broken, and a full test run to confirm that
is pure latency.

## Retrieval

```bash
forge index                  # chunk + keyword index, no network
forge index -embed           # add vectors for semantic search
forge search "how are cooldowns persisted"
```

### Why there is no vector database

The argument is arithmetic, not preference. For a corpus of 50,000 chunks at
768 dimensions:

| representation | size | full scan |
|---|---:|---:|
| float32 | 154 MB | ~4.4 ms (memory-bound) |
| 1 bit per dimension | 4.8 MB | ~0.14 ms (XOR + POPCNT) |

So: rank everything by Hamming distance on the binary codes, keep a few
thousand candidates, re-score only those in float32. Sub-millisecond, no index
build step, no server, no dependency. HNSW exists to solve the ten-million-
vector problem; a repository is not that problem.

Measured recall@10 against exact brute force, 25 queries each:

- **clustered data** (what real embeddings look like — related chunks at cosine
  0.7–0.9): **1.000**
- **uniform random data** (the pathological floor — everything near-orthogonal
  in 256 dims, so the gap between the true 10th and 500th neighbour is a cosine
  difference of ~0.01): **0.968**

`forge index` prints the actual split for your repo, so the trade-off is
inspectable rather than asserted.

### Chunking

Code is split at **declaration boundaries** using the same regex extractor the
repo map uses, so a hit points at a function rather than at an arbitrary window
straddling two of them. That boundary choice is most of what separates useful
code retrieval from the sliding-window kind. Oversized declarations are capped;
prose and config files fall back to overlapping windows.

### Keyword search

BM25, with code-aware tokenization. The identifier splitting is what makes it
work at all: `parseArgs` indexes as `parseargs`, `parse`, `args`, and
`HTTPServer` as `httpserver`, `http`, `server` — not as individual letters. A
query of "parse arguments" has to reach `parseArgs`, and plain whitespace
tokenization gets you neither.

### Fusion

Hybrid mode runs both arms and fuses by **Reciprocal Rank Fusion**. Fusing on
rank rather than score is the point: BM25 scores and cosine similarities live
on incompatible scales, and any weighted sum of them needs per-corpus tuning
that then silently rots.

### Honest limits

For code, **grep is usually better**. When you know the identifier, an exact
match beats retrieval on both precision and speed. `search_code` earns its
place on conceptual queries — "where is rate limiting handled" — where you
don't know what the thing is called. The tool description says exactly that, so
the model picks correctly.

Without an embedding backend configured, hybrid degrades to keyword-only rather
than failing. That degradation is visible in the result header.

### The built-in embedder

Semantic search needs vectors, and vectors normally need a server. Phase 8
removes that: a BERT encoder written from scratch in Go, loading the exact
files HuggingFace publishes.

```bash
git clone https://huggingface.co/sentence-transformers/all-MiniLM-L6-v2
forge index -embed -embed-model ./all-MiniLM-L6-v2
forge do "..." -embed-model ./all-MiniLM-L6-v2
```

No Ollama, no API key, no quota, no network. A local model takes precedence
over the router when both are available — it is strictly the better option.

**Why an encoder is the right thing to own.** A decoder needs a KV cache,
sampling, and incremental generation, and a hand-written one runs 5–15× slower
than llama.cpp's tuned kernels. An encoder has none of that: no cache, no
sampling, fixed shapes, one pass. Roughly 900 lines covers it, and the result
is a retrieval stack with no external moving parts.

Three pieces:

- **safetensors** — 8-byte header length, JSON header, raw tensor data. That is
  the whole format. F32, F16, and BF16 all widen to float32 on load.
- **WordPiece** — greedy longest-match subwords, plus the basic-tokenization
  stage that decides what a word is: punctuation splitting, CJK separation,
  case folding, accent stripping.
- **The forward pass** — embeddings, N transformer blocks, mean pooling, L2
  normalise.

Measured on an i7-1165G7 (4 cores), `forge embed -bench`:

| sequence | per sequence | throughput |
|---:|---:|---:|
| 64 tokens | 143 ms | 9.5 GFLOP/s |
| 128 tokens | 367 ms | 7.4 GFLOP/s |
| 256 tokens | 736 ms | 7.4 GFLOP/s |

Go has no SIMD intrinsics, so the levers are memory layout and instruction-level
parallelism: keep both operands contiguous along the reduction axis, and unroll
the accumulator so the CPU keeps several independent FMA chains in flight
instead of serialising on one running sum.

Indexing this repository (about 1,000 chunks) takes a few minutes on that
machine. It is a one-time cost — see incremental updates below.

**Honest limits.** This handles the standard BERT architecture:
all-MiniLM-L6-v2, bge-small, e5-small, and their relatives. Models with a
modified architecture — nomic-embed's rotary embeddings, for instance — will
load and then fail on a missing tensor rather than silently producing wrong
vectors. Accent handling uses a Latin folding table rather than full Unicode
NFD, which covers Latin-1 and Latin Extended-A; shipping the decomposition
tables would be a large dependency for a narrow case.

### Incremental updates

Embeddings are keyed by the **hash of the chunk text**, not by position. A
function that moves down a file, or into a different file, keeps its vector, so
re-indexing after an edit re-embeds only what actually changed. The selfcheck
verifies this: after editing one file of three, 1 of 4 chunks was re-embedded.

## Routing classes

| Class | Purpose | Chain shape |
|---|---|---|
| `planner` | Hard reasoning, whole-repo context | Big-window models first |
| `coder` | The agent loop workhorse | Fast + good tool calling |
| `cheap` | Summarization, compaction, classification | **Local first** — not worth a hosted quota |
| `local` | Forced offline path for private work | Local only |

### Failure policy

Errors are classified because "rate limited" and "bad model name" demand
opposite reactions — one means come back later, the other means stop trying.

| Error | Reaction |
|---|---|
| 429 rate limit | Cooldown from `Retry-After`, else 20s, doubling per consecutive failure |
| Quota exhausted | 1 hour, no exponentiation — just wait it out |
| Auth / model-not-found | 24h; structural misconfiguration, surfaced by `doctor` |
| Context too long | **No cooldown** — the next request may well fit |
| 5xx / network | 30s doubling, capped at 15 min, with one same-target retry |

A provider's own `Retry-After` always wins when it sends one.

---

## Running a local model

`forge` talks to any OpenAI-compatible server. For `llama-server` on a 4-core
laptop:

```bash
llama-server -m qwen2.5-coder-7b-instruct-q4_k_m.gguf --host 127.0.0.1 --port 8080 -c 16384 -t 4 --cache-type-k q8_0 --cache-type-v q8_0
```

- `-t 4` — **physical** cores, not threads. Hyperthreading hurts here; the
  workload is memory-bandwidth-bound, not ALU-bound.
- `--cache-type-k/v q8_0` — halves KV cache RAM, which is what actually limits
  your usable context on a 16 GB machine.
- If you built with the Vulkan backend, `-ngl 99` offloads to the Iris Xe iGPU.
  It usually helps prefill noticeably and decode barely — worth benchmarking
  both ways.

Then:

```bash
forge bench -target local:qwen2.5-coder-7b-instruct -sizes 512,2048,8192
```

---

## Reading the benchmark

- **decode tok/s** — generation speed. This decides whether an agent loop is
  usable at all.
- **prefill tok/s** — prompt processing. On CPU this dominates the first turn
  of every conversation.
- **TTFT** — for a local model, essentially prefill time. For hosted providers
  it includes network round-trip and queueing, so treat it as a latency upper
  bound, not a hardware measurement.

The bench salts each prompt at the **front** specifically to defeat server-side
prefix KV caching. Benchmarking a cache hit reports a prefill rate you will
never see on a real first turn.

A rate of `n/a` means the span was shorter than the 2 ms clock floor, not that
the call failed.

---

## Platform notes

**Clock granularity.** Go's monotonic clock on Windows ticks at roughly 0.6 ms;
back-to-back `time.Since` reads return identical values ~99.99% of the time.
Timing helpers therefore distinguish "measured, same tick" from "never
observed", and refuse to divide by a span below `provider.ClockFloor` rather
than reporting a noise-derived throughput.

**Smart App Control.** With SAC in enforced mode, Windows blocks `go test`
binaries from executing while permitting ordinary `go build` output. That is
why `forge selfcheck` exists: it runs the same invariants as the `_test.go`
files but ships as a normal command, so the stack stays verifiable on a
locked-down machine. The unit tests remain the right artifact everywhere else:

```bash
go test ./...
```

---

## Layout

```
cmd/forge/            CLI entry point
internal/config/      forge.json loading, ${ENV} expansion, defaults
internal/provider/    OpenAI-compatible client, SSE streaming, error taxonomy
internal/router/      class chains, cooldown policy, usage ledger
internal/bench/       throughput harness
internal/serve/       OpenAI-compatible HTTP endpoint
internal/agent/       the ReAct loop, system prompts, sub-agent roles
internal/tools/       workspace confinement, the ten tools, overflow store
internal/approval/    approval policy, shared by both approvers
internal/term/        console modes, size, raw mode, key decoding
internal/ui/          status region, pager, styles, terminal approver
internal/diff/        line diffs for change previews
internal/repomap/     symbol extraction and PageRank ranking
internal/index/       chunking, BM25, binary vector search, RRF
internal/embed/       safetensors, WordPiece, BERT forward pass, kernels
internal/verify/      project detection, staged runs, failure parsing
internal/checkpoint/  file-level undo journal
internal/compact/     conversation compaction
internal/fsx/         shared walk conventions (skip lists, binary detection)
internal/selfcheck/   end-to-end invariants as a runnable command
```

State lives next to the config (`~/.forge/`):

- `health.json` — per-target cooldowns, persisted across restarts
- `usage.jsonl` — append-only ledger, one line per call
- `bench-*.json` — raw benchmark reports
- `repomap-*.json` — symbol extraction cache, keyed by file mtime and size
- `notes/*.md` — cross-session notes, one file per workspace
- `overflow/r*.txt` — full text of clipped tool results
- `undo/<timestamp>/` — original contents of every file a run modified
- `index/<hash>/` — search index: `index.gob` (chunks + BM25) and `index.vec`
  (raw little-endian float32; gob on a few million floats is both slow and
  several times larger than the data)

---

## Next phases

| # | Phase | What it adds |
|---|---|---|
| ~~2~~ | ~~Agent core~~ | **done** — ReAct loop, tool dispatch, approval gates |
| ~~3~~ | ~~Context engine~~ | **done** — repo map, overflow handles, compaction |
| ~~4~~ | ~~Retrieval~~ | **done** — chunker, BM25, binary vector search, RRF |
| ~~5~~ | ~~Verify loop~~ | **done** — detection, failure parsing, self-repair, undo journal |
| ~~6~~ | ~~Sub-agents~~ | **done** — isolated contexts, parallel delegation, roll-up accounting |
| ~~7~~ | ~~TUI~~ | **done** — raw mode, pinned status, scrollable approvals |
| ~~8~~ | ~~Embedder~~ | **done** — safetensors, WordPiece, BERT forward pass |

All eight phases are complete. `forge selfcheck` runs 131 invariants.
