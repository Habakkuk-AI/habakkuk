package history

import (
	"fmt"
	"strings"
)

// diffContext is how many unchanged lines surround each change.
const diffContext = 3

// maxDiffCells bounds the line-matching table; texts larger than that (well
// beyond any note) are shown as all removed, then all added.
const maxDiffCells = 4 << 20

type diffLine struct {
	kind byte // ' ', '-', '+'
	text string
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.SplitAfter(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// lineDiff is the shortest edit script from a to b (a longest common
// subsequence of lines), with common prefix and suffix trimmed first.
func lineDiff(a, b []string) []diffLine {
	var out []diffLine
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		out = append(out, diffLine{' ', a[pre]})
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	ma, mb := a[pre:len(a)-suf], b[pre:len(b)-suf]
	n, m := len(ma), len(mb)
	if (n+1)*(m+1) > maxDiffCells {
		for _, l := range ma {
			out = append(out, diffLine{'-', l})
		}
		for _, l := range mb {
			out = append(out, diffLine{'+', l})
		}
	} else {
		// lcs[i*(m+1)+j] is the LCS length of ma[i:] and mb[j:].
		lcs := make([]int32, (n+1)*(m+1))
		for i := n - 1; i >= 0; i-- {
			for j := m - 1; j >= 0; j-- {
				switch {
				case ma[i] == mb[j]:
					lcs[i*(m+1)+j] = lcs[(i+1)*(m+1)+j+1] + 1
				case lcs[(i+1)*(m+1)+j] >= lcs[i*(m+1)+j+1]:
					lcs[i*(m+1)+j] = lcs[(i+1)*(m+1)+j]
				default:
					lcs[i*(m+1)+j] = lcs[i*(m+1)+j+1]
				}
			}
		}
		i, j := 0, 0
		for i < n && j < m {
			switch {
			case ma[i] == mb[j]:
				out = append(out, diffLine{' ', ma[i]})
				i++
				j++
			case lcs[(i+1)*(m+1)+j] >= lcs[i*(m+1)+j+1]:
				out = append(out, diffLine{'-', ma[i]})
				i++
			default:
				out = append(out, diffLine{'+', mb[j]})
				j++
			}
		}
		for ; i < n; i++ {
			out = append(out, diffLine{'-', ma[i]})
		}
		for ; j < m; j++ {
			out = append(out, diffLine{'+', mb[j]})
		}
	}
	for k := len(a) - suf; k < len(a); k++ {
		out = append(out, diffLine{' ', a[k]})
	}
	return out
}

// Unified renders a unified diff of two texts, "" when they are equal.
func Unified(aName, bName, a, b string) string {
	if a == b {
		return ""
	}
	lines := lineDiff(splitLines(a), splitLines(b))
	var changed []int
	for i, l := range lines {
		if l.kind != ' ' {
			changed = append(changed, i)
		}
	}
	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n+++ %s\n", aName, bName)
	for h := 0; h < len(changed); {
		first, last := changed[h], changed[h]
		h++
		for h < len(changed) && changed[h]-last <= 2*diffContext {
			last = changed[h]
			h++
		}
		start, end := max(0, first-diffContext), min(len(lines), last+diffContext+1)
		aStart, bStart := 0, 0
		for _, l := range lines[:start] {
			if l.kind != '+' {
				aStart++
			}
			if l.kind != '-' {
				bStart++
			}
		}
		aLen, bLen := 0, 0
		for _, l := range lines[start:end] {
			if l.kind != '+' {
				aLen++
			}
			if l.kind != '-' {
				bLen++
			}
		}
		fmt.Fprintf(&out, "@@ -%s +%s @@\n", hunkRange(aStart, aLen), hunkRange(bStart, bLen))
		for _, l := range lines[start:end] {
			out.WriteByte(l.kind)
			out.WriteString(l.text)
			if !strings.HasSuffix(l.text, "\n") {
				out.WriteString("\n\\ No newline at end of file\n")
			}
		}
	}
	return out.String()
}

func hunkRange(start, n int) string {
	switch n {
	case 0:
		return fmt.Sprintf("%d,0", start)
	case 1:
		return fmt.Sprint(start + 1)
	}
	return fmt.Sprintf("%d,%d", start+1, n)
}
