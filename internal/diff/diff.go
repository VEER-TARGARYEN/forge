// Package diff produces line-level diffs for change previews.
//
// The agent shows a diff before every write, so this runs on the human's
// critical path for approving edits. It is line-based and deliberately plain:
// the goal is "can I see what this changes", not minimal edit scripts.
package diff

import (
	"fmt"
	"strings"
)

type Op int

const (
	Equal Op = iota
	Insert
	Delete
)

type Line struct {
	Op   Op
	Text string
}

// maxDP caps the LCS table. Above this the quadratic table stops being worth
// the memory, and a whole-block replacement conveys the change just as well.
const maxDP = 3000

// Lines computes a line diff of a against b.
func Lines(a, b []string) []Line {
	// Trim the common prefix and suffix first. Real edits are local, so this
	// usually shrinks the quadratic part to a handful of lines.
	p := 0
	for p < len(a) && p < len(b) && a[p] == b[p] {
		p++
	}
	s := 0
	for s < len(a)-p && s < len(b)-p && a[len(a)-1-s] == b[len(b)-1-s] {
		s++
	}
	midA, midB := a[p:len(a)-s], b[p:len(b)-s]

	out := make([]Line, 0, len(a)+len(b))
	for _, l := range a[:p] {
		out = append(out, Line{Equal, l})
	}
	out = append(out, diffMiddle(midA, midB)...)
	for _, l := range a[len(a)-s:] {
		out = append(out, Line{Equal, l})
	}
	return out
}

func diffMiddle(a, b []string) []Line {
	switch {
	case len(a) == 0 && len(b) == 0:
		return nil
	case len(a) == 0:
		out := make([]Line, 0, len(b))
		for _, l := range b {
			out = append(out, Line{Insert, l})
		}
		return out
	case len(b) == 0:
		out := make([]Line, 0, len(a))
		for _, l := range a {
			out = append(out, Line{Delete, l})
		}
		return out
	case len(a) > maxDP || len(b) > maxDP:
		// Too large to align economically; present it as a wholesale swap.
		out := make([]Line, 0, len(a)+len(b))
		for _, l := range a {
			out = append(out, Line{Delete, l})
		}
		for _, l := range b {
			out = append(out, Line{Insert, l})
		}
		return out
	}

	// Standard LCS dynamic programme over the trimmed middle.
	n, m := len(a), len(b)
	dp := make([]int, (n+1)*(m+1))
	at := func(i, j int) int { return i*(m+1) + j }
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[at(i, j)] = dp[at(i+1, j+1)] + 1
			} else if dp[at(i+1, j)] >= dp[at(i, j+1)] {
				dp[at(i, j)] = dp[at(i+1, j)]
			} else {
				dp[at(i, j)] = dp[at(i, j+1)]
			}
		}
	}

	out := make([]Line, 0, n+m)
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			out = append(out, Line{Equal, a[i]})
			i++
			j++
		case dp[at(i+1, j)] >= dp[at(i, j+1)]:
			out = append(out, Line{Delete, a[i]})
			i++
		default:
			out = append(out, Line{Insert, b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		out = append(out, Line{Delete, a[i]})
	}
	for ; j < m; j++ {
		out = append(out, Line{Insert, b[j]})
	}
	return out
}

// Stat counts inserted and deleted lines.
func Stat(ls []Line) (added, removed int) {
	for _, l := range ls {
		switch l.Op {
		case Insert:
			added++
		case Delete:
			removed++
		}
	}
	return
}

// Unified renders a diff with the given context, in the familiar +/- form.
// Runs of unchanged lines longer than 2*context collapse to a marker.
func Unified(path, before, after string, context int) string {
	if context < 0 {
		context = 3
	}
	a, b := splitLines(before), splitLines(after)
	ls := Lines(a, b)
	added, removed := Stat(ls)

	var sb strings.Builder
	fmt.Fprintf(&sb, "--- %s\n+++ %s\n", path, path)
	if added == 0 && removed == 0 {
		sb.WriteString("(no changes)\n")
		return sb.String()
	}

	// Mark which lines to keep: every change, plus `context` around it.
	keep := make([]bool, len(ls))
	for i, l := range ls {
		if l.Op == Equal {
			continue
		}
		lo, hi := i-context, i+context
		if lo < 0 {
			lo = 0
		}
		if hi >= len(ls) {
			hi = len(ls) - 1
		}
		for k := lo; k <= hi; k++ {
			keep[k] = true
		}
	}

	lineA, lineB := 1, 1
	skipping := false
	for i, l := range ls {
		if !keep[i] {
			if !skipping {
				fmt.Fprintf(&sb, "@@ ... @@\n")
				skipping = true
			}
			switch l.Op {
			case Equal:
				lineA++
				lineB++
			case Delete:
				lineA++
			case Insert:
				lineB++
			}
			continue
		}
		skipping = false
		switch l.Op {
		case Equal:
			fmt.Fprintf(&sb, " %4d %4d  %s\n", lineA, lineB, l.Text)
			lineA++
			lineB++
		case Delete:
			fmt.Fprintf(&sb, "-%4d       %s\n", lineA, l.Text)
			lineA++
		case Insert:
			fmt.Fprintf(&sb, "+     %4d  %s\n", lineB, l.Text)
			lineB++
		}
	}
	fmt.Fprintf(&sb, "(+%d -%d)\n", added, removed)
	return sb.String()
}

// Summary returns just the change counts, for one-line reporting.
func Summary(before, after string) (added, removed int) {
	return Stat(Lines(splitLines(before), splitLines(after)))
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	// A trailing newline yields a final empty element that is not a real line.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}
