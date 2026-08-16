package provider

import (
	"encoding/json"
	"strings"
)

// Recovering tool calls a model wrote as prose instead of calling.
//
// Small local models frequently "describe" a tool call rather than emitting
// one: qwen2.5-coder:3b, asked to edit a file, reliably answers with a fenced
// ```json block containing {"name": "edit_file", "arguments": {...}} and an
// empty tool_calls field. The intent is unambiguous and the arguments are
// usually valid — the model simply did not use the transport.
//
// Without recovery the agent sees a turn with no tool calls, concludes the
// model is finished, and stops having done nothing. That is the difference
// between a 3B being unusable and being useful, so it is worth parsing.
//
// The guards matter more than the parsing. This only ever runs when a turn
// produced no real tool calls and no edit blocks, and a candidate is accepted
// only if its name matches a tool that is actually registered. A model
// discussing JSON, or printing an example, cannot trigger it by accident.

// ParseTextToolCalls extracts tool calls embedded in message content.
// known reports whether a name corresponds to a registered tool; candidates
// that fail it are ignored.
func ParseTextToolCalls(content string, known func(string) bool) []ToolCall {
	if content == "" || known == nil {
		return nil
	}
	var out []ToolCall
	seen := map[string]bool{}

	add := func(cands []ToolCall) {
		for _, tc := range cands {
			// The same call often appears twice — once inside a fence and
			// once as a bare object found by the balanced-brace scan.
			key := tc.Function.Name + "\x00" + tc.Function.Arguments
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, tc)
		}
	}

	for _, blob := range jsonCandidates(content) {
		add(decodeCallShapes(blob, known))
	}
	add(labeledCalls(content, known))
	return out
}

// labeledCalls handles the shape where the model writes the tool name on its
// own line and the arguments beneath it, with no wrapping object:
//
//	search_code
//	{"query": "func main()", "mode": "keyword"}
//
// qwen2.5-coder:7b emits this under a long system prompt. A registered tool
// name alone on a line, immediately followed by a JSON object, is a specific
// enough pattern that prose does not trip it.
func labeledCalls(content string, known func(string) bool) []ToolCall {
	var out []ToolCall
	lines := strings.Split(content, "\n")

	for i, ln := range lines {
		name := strings.Trim(strings.TrimSpace(ln), "`*_\"'#:- ")
		if name == "" || !known(name) {
			continue
		}
		// Find where the arguments begin: skip blank lines and an opening
		// fence, then require an object.
		rest := strings.Join(lines[i+1:], "\n")
		rest = strings.TrimLeft(rest, " \t\r\n")
		if strings.HasPrefix(rest, "```") {
			if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
				rest = strings.TrimLeft(rest[nl+1:], " \t\r\n")
			}
		}
		if !strings.HasPrefix(rest, "{") {
			continue
		}
		objs := balancedObjects(rest)
		if len(objs) == 0 || !json.Valid([]byte(objs[0])) {
			continue
		}
		// A wrapped call that happens to sit below its own name is already
		// handled by the object scan; do not also read it as bare arguments.
		if hasCallKeys(objs[0]) {
			continue
		}
		out = append(out, ToolCall{
			Type:     "function",
			Function: FunctionCall{Name: name, Arguments: objs[0]},
		})
	}
	return out
}

// hasCallKeys reports whether an object looks like a call envelope rather
// than a bare argument set.
func hasCallKeys(blob string) bool {
	var probe struct {
		Name      string          `json:"name"`
		ToolCalls json.RawMessage `json:"tool_calls"`
		Function  json.RawMessage `json:"function"`
	}
	if json.Unmarshal([]byte(blob), &probe) != nil {
		return false
	}
	return probe.Name != "" || len(probe.ToolCalls) > 0 || len(probe.Function) > 0
}

// decodeCallShapes tries every layout a model plausibly emits.
func decodeCallShapes(blob string, known func(string) bool) []ToolCall {
	raw := json.RawMessage(blob)

	// Shape 1: a wrapper holding a list, e.g. {"tool_calls": [...]}.
	var wrapper struct {
		ToolCalls []json.RawMessage `json:"tool_calls"`
	}
	if err := json.Unmarshal(raw, &wrapper); err == nil && len(wrapper.ToolCalls) > 0 {
		var out []ToolCall
		for _, inner := range wrapper.ToolCalls {
			out = append(out, decodeCallShapes(string(inner), known)...)
		}
		return out
	}

	// Shape 2: the OpenAI nesting, {"function": {"name":..., "arguments":...}}.
	var nested struct {
		Function json.RawMessage `json:"function"`
	}
	if err := json.Unmarshal(raw, &nested); err == nil && len(nested.Function) > 0 {
		if inner := decodeCallShapes(string(nested.Function), known); len(inner) > 0 {
			return inner
		}
	}

	// Shape 3: the flat form models actually write by hand. `parameters` and
	// `input` appear as often as `arguments`.
	var flat struct {
		Name       string          `json:"name"`
		Arguments  json.RawMessage `json:"arguments"`
		Parameters json.RawMessage `json:"parameters"`
		Input      json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(raw, &flat); err != nil || flat.Name == "" {
		return nil
	}
	if !known(flat.Name) {
		return nil
	}

	args := firstNonEmptyRaw(flat.Arguments, flat.Parameters, flat.Input)
	if len(args) == 0 {
		// A no-argument tool is legitimate; hand the executor an empty object.
		args = json.RawMessage("{}")
	}
	// Arguments may itself be a JSON-encoded string rather than an object.
	if args[0] == '"' {
		var s string
		if json.Unmarshal(args, &s) == nil {
			args = json.RawMessage(s)
		}
	}
	if !json.Valid(args) {
		return nil
	}

	return []ToolCall{{
		Type:     "function",
		Function: FunctionCall{Name: flat.Name, Arguments: string(args)},
	}}
}

func firstNonEmptyRaw(vals ...json.RawMessage) json.RawMessage {
	for _, v := range vals {
		if len(v) > 0 && string(v) != "null" {
			return v
		}
	}
	return nil
}

// jsonCandidates returns substrings worth attempting to decode: the body of
// every fenced code block, plus every balanced brace span in the text.
func jsonCandidates(s string) []string {
	var out []string
	for _, fence := range fencedBlocks(s) {
		if t := strings.TrimSpace(fence); strings.HasPrefix(t, "{") {
			out = append(out, t)
		}
	}
	out = append(out, balancedObjects(s)...)
	return out
}

// fencedBlocks returns the contents of ``` blocks, dropping any language tag.
func fencedBlocks(s string) []string {
	var out []string
	rest := s
	for {
		open := strings.Index(rest, "```")
		if open < 0 {
			return out
		}
		rest = rest[open+3:]
		// Skip the language tag on the fence line.
		if nl := strings.IndexByte(rest, '\n'); nl >= 0 && !strings.Contains(rest[:nl], "{") {
			rest = rest[nl+1:]
		}
		close := strings.Index(rest, "```")
		if close < 0 {
			out = append(out, rest) // unterminated fence: take the remainder
			return out
		}
		out = append(out, rest[:close])
		rest = rest[close+3:]
	}
}

// balancedObjects finds every top-level {...} span, ignoring braces that sit
// inside string literals.
func balancedObjects(s string) []string {
	var out []string
	depth, start := 0, -1
	inStr, esc := false, false

	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case esc:
			esc = false
		case c == '\\' && inStr:
			esc = true
		case c == '"':
			inStr = !inStr
		case inStr:
			// braces inside strings are data
		case c == '{':
			if depth == 0 {
				start = i
			}
			depth++
		case c == '}':
			if depth > 0 {
				depth--
				if depth == 0 && start >= 0 {
					out = append(out, s[start:i+1])
					start = -1
				}
			}
		}
	}
	return out
}
