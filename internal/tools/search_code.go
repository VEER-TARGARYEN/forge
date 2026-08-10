package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// SearchHit is one code-search result, kept independent of the index package
// so tools does not depend on it.
type SearchHit struct {
	File   string
	Start  int
	End    int
	Symbol string
	Kind   string
	Score  float64
	Text   string
}

// SearchFunc runs a code search and reports which mode actually ran — hybrid
// degrades to keyword when no embedding backend is configured, and the caller
// deserves to know that happened.
type SearchFunc func(ctx context.Context, query string, limit int, mode string) (hits []SearchHit, modeUsed string, err error)

type SearchCode struct{}

func (SearchCode) Spec() Spec {
	return Spec{
		Name: "search_code",
		Description: "Search the codebase by meaning as well as by keyword. Use it when you do not " +
			"know the exact identifier — 'where is rate limiting handled', 'how are cooldowns " +
			"persisted'. When you DO know the exact string, grep is faster and more precise.",
		Schema: obj(map[string]any{
			"query": str("What you are looking for. A phrase works better than a single word."),
			"limit": integer("Maximum results. Defaults to 8."),
			"mode":  str("hybrid (default), keyword, or semantic."),
		}, "query"),
	}
}

func (SearchCode) Mutates() bool { return false }

func (SearchCode) Run(ctx context.Context, raw json.RawMessage, env *Env) (*Result, error) {
	var a struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
		Mode  string `json:"mode"`
	}
	if err := ParseArgs(raw, &a); err != nil {
		return Errorf("%v", err), nil
	}
	a.Query = strings.TrimSpace(a.Query)
	if a.Query == "" {
		return Errorf("query is required"), nil
	}
	if a.Limit <= 0 {
		a.Limit = 8
	}
	if a.Limit > 40 {
		a.Limit = 40
	}
	if env.SearchCode == nil {
		return Errorf("code search is not available in this run; use grep or glob instead"), nil
	}

	hits, modeUsed, err := env.SearchCode(ctx, a.Query, a.Limit, a.Mode)
	if err != nil {
		return Errorf("search failed: %v", err), nil
	}
	if len(hits) == 0 {
		return Textf("No results for %q. Try different wording, or grep for an exact string.", a.Query), nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "%d results for %q (%s search)\n\n", len(hits), a.Query, modeUsed)
	for i, h := range hits {
		fmt.Fprintf(&sb, "── %d. %s:%d-%d", i+1, h.File, h.Start, h.End)
		if h.Symbol != "" {
			fmt.Fprintf(&sb, "  %s %s", h.Kind, h.Symbol)
		}
		fmt.Fprintf(&sb, "  (score %.3f)\n", h.Score)
		// Cap each excerpt: eight results at full length would put a small
		// file's worth of code into context for what is a locating tool.
		fmt.Fprintf(&sb, "%s\n\n", excerpt(h.Text, 30))
	}
	sb.WriteString("Read a file for the full text of any result.\n")

	body, note := env.Clip("search_code "+a.Query, sb.String())
	return &Result{
		Content: body + note,
		Display: fmt.Sprintf("search_code %q -> %d hits (%s)", a.Query, len(hits), modeUsed),
	}, nil
}

func excerpt(s string, maxLines int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= maxLines {
		return s
	}
	return strings.Join(lines[:maxLines], "\n") +
		fmt.Sprintf("\n… (%d more lines)", len(lines)-maxLines)
}
