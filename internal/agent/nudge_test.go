package agent

import "testing"

// unappliedCode decides whether a turn that took no action was a real answer
// or a model that forgot to make the edit it just described. Both mistakes are
// costly: a false negative ends the run having written nothing, and a false
// positive argues with a model that was legitimately finished.
func TestUnappliedCode(t *testing.T) {
	fence := "```"

	cases := []struct {
		name    string
		content string
		want    bool
	}{
		{
			// The exact output that ended a run with nothing written.
			name: "placeholder path with a fenced file",
			content: "FILE_PATH_GOES_HERE\n" + fence + "\npackage main\n\n" +
				"func Greet(name string) string {\n\treturn \"Hello, \" + name\n}\n" + fence,
			want: true,
		},
		{
			name: "fenced source, no path, no action",
			content: "Here is the implementation:\n\n" + fence + "go\n" +
				"package main\n\nfunc Greet(n string) string {\n\treturn n\n}\n" + fence,
			want: true,
		},
		{
			name:    "placeholder alone",
			content: "I will edit FILE_PATH_GOES_HERE next.",
			want:    true,
		},

		// Must not fire on a legitimate final answer.
		{
			name:    "plain prose",
			content: "I read the router and the cooldown is persisted in health.json.",
			want:    false,
		},
		{
			name:    "short inline snippet",
			content: "The call is:\n" + fence + "\nforge doctor -reset\n" + fence + "\nRun that first.",
			want:    false,
		},
		{
			name:    "two-line fence",
			content: fence + "go\nx := 1\ny := 2\n" + fence,
			want:    false,
		},
		{
			name:    "unterminated fence",
			content: "Here you go: " + fence + "go\npackage main\nfunc a(){}\nfunc b(){}",
			want:    false,
		},
		{
			name:    "prose mentioning code blocks",
			content: "I did not use a fenced block; the edit went through edit_file.",
			want:    false,
		},
		{
			name:    "empty",
			content: "",
			want:    false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := unappliedCode(tc.content) != ""
			if got != tc.want {
				t.Fatalf("unappliedCode = %v, want %v\nreason: %q", got, tc.want, unappliedCode(tc.content))
			}
		})
	}
}

// The placeholder in the prompt and the one the detector looks for have to be
// the same string, or the most common failure goes unrecognised.
func TestPlaceholderMatchesPrompt(t *testing.T) {
	if !contains(blockSpec, placeholderPath) {
		t.Fatalf("blockSpec does not contain %q; the detector will never match the prompt", placeholderPath)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
