package agent

import (
	"fmt"
	"runtime"
	"strings"

	"github.com/VEER-TARGARYEN/forge/internal/tools"
)

// EditProtocol selects how the model is told to express file changes.
type EditProtocol string

const (
	// ProtoBlocks instructs SEARCH/REPLACE blocks in message content. Best
	// for small local models: code never has to survive JSON escaping.
	ProtoBlocks EditProtocol = "blocks"
	// ProtoTool instructs the edit_file JSON tool. Fine on strong hosted
	// models, and lets the edit participate in normal tool-call plumbing.
	ProtoTool EditProtocol = "tool"
	// ProtoAuto documents both and accepts either. Forgiving, which is what
	// you want when the same binary talks to a 7B and a 480B.
	ProtoAuto EditProtocol = "auto"
)

func ParseProtocol(s string) (EditProtocol, error) {
	switch EditProtocol(strings.ToLower(strings.TrimSpace(s))) {
	case ProtoBlocks:
		return ProtoBlocks, nil
	case ProtoTool:
		return ProtoTool, nil
	case ProtoAuto, "":
		return ProtoAuto, nil
	}
	return "", fmt.Errorf("unknown edit protocol %q (use blocks, tool, or auto)", s)
}

const blockSpec = `EDITING FILES — SEARCH/REPLACE blocks

Write edits directly in your message, not as tool arguments. Use exactly this shape:

path/to/file.go
<<<<<<< SEARCH
the exact existing lines
=======
the replacement lines
>>>>>>> REPLACE

Rules:
- SEARCH must match the file character for character, including indentation.
  Read the file first and copy from what you read.
- Keep each block small: the lines that change, plus just enough surrounding
  lines to be unique in the file.
- If SEARCH would match more than one place, add more surrounding lines.
- Several blocks per message is fine. Each needs its own path line above it.
- To create a new file, leave the SEARCH section empty.
- To delete lines, leave the REPLACE section empty.`

const toolSpec = `EDITING FILES — edit_file tool

Use edit_file with the exact existing text as old_string. It must match the
file character for character and be unique unless you set replace_all.
Read the file before editing it so old_string comes from what is actually there.`

// System builds the system prompt.
//
// It stays deliberately short. Every token here is paid on every turn, and on
// a local model at ~7 tok/s prefill dominates wall-clock: a 3,000-token
// preamble costs real minutes across a session.
func System(ws *tools.Workspace, reg *tools.Registry, proto EditProtocol) string {
	var sb strings.Builder

	fmt.Fprintf(&sb, `You are forge, a coding agent working directly in a local repository.

WORKSPACE: %s
PLATFORM:  %s

You act by calling tools. You cannot see the repository except through them,
so never guess at file contents, APIs, or project layout — look.

WORKFLOW
1. If the task needs three or more steps, call todo_write first and keep it current.
2. Locate the relevant code with grep or glob before reading whole files.
3. Read what you are about to change.
4. Make the smallest change that fully solves the task.
5. Verify it: call verify if it is available, otherwise build or test with run_command.
6. Stop calling tools and write a short summary of what changed and why.

When you stop, the project's checks are run automatically. If they fail you
will be given the located failures and asked to fix them, so verifying yourself
first is cheaper than being told.

`, ws.Root(), runtime.GOOS)

	switch proto {
	case ProtoBlocks:
		sb.WriteString(blockSpec)
	case ProtoTool:
		sb.WriteString(toolSpec)
	default:
		sb.WriteString(blockSpec)
		sb.WriteString("\n\nFor a one-line change you may instead call edit_file. For anything larger,\nuse the block form above.")
	}

	sb.WriteString(`

RULES
- Read a file before you edit it. An edit built from memory will not match.
- Do not invent function names, flags, or fields. Check the real code.
- Prefer editing an existing file over creating a new one.
- Do not commit, push, or install anything unless you were asked to.
- Never write secrets, tokens, or keys into files.
- If a tool fails, read the error and fix the cause. Do not retry it unchanged.
- If the task is ambiguous in a way that changes the outcome, ask instead of guessing.

TOOLS AVAILABLE
`)
	for _, s := range reg.Specs() {
		fmt.Fprintf(&sb, "- %s\n", s.Name)
	}
	return sb.String()
}

// SubSystem builds the system prompt for a delegated context.
//
// It is deliberately much shorter than the top-level prompt: a sub-agent has
// one question, a small step budget, and no ability to edit, so the workflow
// and editing sections would be pure overhead paid on every one of its turns.
func SubSystem(ws *tools.Workspace, reg *tools.Registry, role string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, `You are a read-only research agent working in a local repository.

WORKSPACE: %s
PLATFORM:  %s

You can only look, not change anything. Never guess at file contents — read them.

%s

TOOLS AVAILABLE
`, ws.Root(), runtime.GOOS, role)
	for _, s := range reg.Specs() {
		fmt.Fprintf(&sb, "- %s\n", s.Name)
	}
	return sb.String()
}
