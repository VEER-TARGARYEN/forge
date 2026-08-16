package provider

import (
	"encoding/json"
	"strings"
	"testing"
)

func jsonIsObject(s string) bool {
	return json.Valid([]byte(s)) && strings.HasPrefix(strings.TrimSpace(s), "{")
}

func known(names ...string) func(string) bool {
	set := map[string]bool{}
	for _, n := range names {
		set[n] = true
	}
	return func(n string) bool { return set[n] }
}

// The exact output that made qwen2.5-coder:3b useless: a correct call, in a
// fenced block, with tool_calls empty.
func TestParseTextToolCalls_FencedRealWorld(t *testing.T) {
	content := "I'll create the file now.\n\n```json\n" +
		`{
  "name": "edit_file",
  "arguments": {
    "path": "main.go",
    "content": "package main\n\nfunc Greet(name string) string {\n\treturn \"Hello, \" + name\n}\n"
  }
}` + "\n```\n"

	got := ParseTextToolCalls(content, known("edit_file", "read_file"))
	if len(got) != 1 {
		t.Fatalf("got %d calls, want 1", len(got))
	}
	if got[0].Function.Name != "edit_file" {
		t.Errorf("name = %q", got[0].Function.Name)
	}
	// The arguments must survive intact — the embedded \n escapes are the
	// file's real content and mangling them would write a broken file.
	var args struct{ Path, Content string }
	if err := json.Unmarshal([]byte(got[0].Function.Arguments), &args); err != nil {
		t.Fatalf("arguments not decodable: %v", err)
	}
	if args.Path != "main.go" {
		t.Errorf("path = %q", args.Path)
	}
	if want := "package main\n\nfunc Greet"; args.Content[:len(want)] != want {
		t.Errorf("content mangled: %q", args.Content)
	}
}

func TestParseTextToolCalls_Shapes(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string // expected tool name, "" for no match
	}{
		{"bare object", `{"name":"read_file","arguments":{"path":"a.go"}}`, "read_file"},
		{"unfenced prose", "Let me read it.\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"a.go\"}}\nDone.", "read_file"},
		{"parameters key", `{"name":"read_file","parameters":{"path":"a.go"}}`, "read_file"},
		{"input key", `{"name":"read_file","input":{"path":"a.go"}}`, "read_file"},
		{"nested function", `{"function":{"name":"read_file","arguments":{"path":"a.go"}}}`, "read_file"},
		{"tool_calls wrapper", `{"tool_calls":[{"name":"read_file","arguments":{"path":"a.go"}}]}`, "read_file"},
		{"double-encoded args", `{"name":"read_file","arguments":"{\"path\":\"a.go\"}"}`, "read_file"},
		{"no-arg tool", `{"name":"list_files"}`, "list_files"},
		{"plain fence", "```\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"a.go\"}}\n```", "read_file"},

		// Guards: none of these may fire.
		{"unregistered name", `{"name":"rm_rf","arguments":{"path":"/"}}`, ""},
		{"no name field", `{"arguments":{"path":"a.go"}}`, ""},
		{"prose only", "I read the file and it looks fine.", ""},
		{"not json", "use {braces} in your answer", ""},
		{"name in a string", `The struct has a field {"label":"name","value":"read_file"}`, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseTextToolCalls(tc.content, known("read_file", "list_files"))
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("got %d calls, want none: %+v", len(got), got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("got %d calls, want 1: %+v", len(got), got)
			}
			if got[0].Function.Name != tc.want {
				t.Errorf("name = %q, want %q", got[0].Function.Name, tc.want)
			}
			if !jsonIsObject(got[0].Function.Arguments) {
				t.Errorf("arguments not an object: %q", got[0].Function.Arguments)
			}
		})
	}
}

// A fenced call is also found by the bare-object scan. It must be executed
// once, not twice — a duplicated edit_file would apply the same write twice.
func TestParseTextToolCalls_Deduplicates(t *testing.T) {
	content := "```json\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"a.go\"}}\n```"
	if got := ParseTextToolCalls(content, known("read_file")); len(got) != 1 {
		t.Fatalf("got %d calls, want 1", len(got))
	}
}

func TestParseTextToolCalls_Multiple(t *testing.T) {
	content := "```json\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"a.go\"}}\n```\n" +
		"then\n```json\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"b.go\"}}\n```"
	got := ParseTextToolCalls(content, known("read_file"))
	if len(got) != 2 {
		t.Fatalf("got %d calls, want 2: %+v", len(got), got)
	}
}

// Braces inside string literals must not terminate the scan early.
func TestBalancedObjects_IgnoresBracesInStrings(t *testing.T) {
	got := balancedObjects(`{"a":"} not the end {","b":1}`)
	if len(got) != 1 {
		t.Fatalf("got %d objects, want 1: %q", len(got), got)
	}
	if !jsonIsObject(got[0]) {
		t.Errorf("not valid JSON: %q", got[0])
	}
}

// The shape qwen2.5-coder:7b emits inside the agent loop: name on its own
// line, arguments beneath, no wrapping object.
func TestParseTextToolCalls_LabeledForm(t *testing.T) {
	content := "todo_write\n[\n {\"content\":\"x\"}\n]\n\nsearch_code\n{\n  \"query\": \"func main()\",\n  \"mode\": \"keyword\"\n}\n"
	got := ParseTextToolCalls(content, known("search_code", "todo_write"))
	if len(got) != 1 {
		t.Fatalf("got %d calls, want 1 (array payload is not wrappable): %+v", len(got), got)
	}
	if got[0].Function.Name != "search_code" {
		t.Fatalf("name = %q", got[0].Function.Name)
	}
	var args struct{ Query, Mode string }
	if err := json.Unmarshal([]byte(got[0].Function.Arguments), &args); err != nil {
		t.Fatalf("arguments not decodable: %v", err)
	}
	if args.Query != "func main()" || args.Mode != "keyword" {
		t.Errorf("args = %+v", args)
	}
}

func TestLabeledCalls_Guards(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    int
	}{
		{"fenced label", "read_file\n```json\n{\"path\":\"a.go\"}\n```", 1},
		{"label with colon", "read_file:\n{\"path\":\"a.go\"}", 1},
		{"name mentioned in prose", "I will use read_file to look at it.\n{\"path\":\"a.go\"}", 0},
		{"no object follows", "read_file\nand then I stopped", 0},
		{"unregistered label", "delete_everything\n{\"path\":\"/\"}", 0},
		// A full envelope under its own name must be counted once, not twice.
		{"envelope not double counted", "read_file\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"a.go\"}}", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseTextToolCalls(tc.content, known("read_file"))
			if len(got) != tc.want {
				t.Fatalf("got %d calls, want %d: %+v", len(got), tc.want, got)
			}
		})
	}
}

func TestParseTextToolCalls_NilGuards(t *testing.T) {
	if got := ParseTextToolCalls("", known("read_file")); got != nil {
		t.Errorf("empty content returned %+v", got)
	}
	if got := ParseTextToolCalls(`{"name":"read_file"}`, nil); got != nil {
		t.Errorf("nil validator returned %+v", got)
	}
}
