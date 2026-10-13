package catalogapply

import (
	"fmt"
	"strings"
)

// unifiedDiff renders a unified diff of a to b for one catalog file. Either
// side may be empty (create or delete). Catalog files are small, so a plain
// LCS table is enough.
func unifiedDiff(name, a, b string) string {
	if a == b {
		return ""
	}
	al, bl := splitLines(a), splitLines(b)
	n, m := len(al), len(bl)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if al[i] == bl[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	type op struct {
		kind byte // ' ', '-', '+'
		text string
	}
	var ops []op
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case al[i] == bl[j]:
			ops = append(ops, op{' ', al[i]})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			ops = append(ops, op{'-', al[i]})
			i++
		default:
			ops = append(ops, op{'+', bl[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, op{'-', al[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, op{'+', bl[j]})
	}

	const ctx = 3
	var sb strings.Builder
	oldName, newName := "a/"+name, "b/"+name
	if a == "" {
		oldName = "/dev/null"
	}
	if b == "" {
		newName = "/dev/null"
	}
	fmt.Fprintf(&sb, "--- %s\n+++ %s\n", oldName, newName)

	// Emit hunks: runs of changes with up to ctx lines of context on each side.
	k := 0
	oldLine, newLine := 1, 1
	for k < len(ops) {
		if ops[k].kind == ' ' {
			k++
			oldLine++
			newLine++
			continue
		}
		start := k
		for start > 0 && ops[start-1].kind == ' ' && k-start < ctx {
			start--
		}
		// Find the end of this hunk: extend while changes recur within 2*ctx context lines.
		end := k
		for end < len(ops) {
			if ops[end].kind != ' ' {
				end++
				continue
			}
			gap := 0
			for e := end; e < len(ops) && ops[e].kind == ' '; e++ {
				gap++
			}
			if end+gap < len(ops) && gap <= 2*ctx {
				end += gap
				continue
			}
			if gap > ctx {
				gap = ctx
			}
			end += gap
			break
		}
		oldStart, newStart := oldLine-(k-start), newLine-(k-start)
		oldCount, newCount := 0, 0
		for _, o := range ops[start:end] {
			if o.kind != '+' {
				oldCount++
			}
			if o.kind != '-' {
				newCount++
			}
		}
		fmt.Fprintf(&sb, "@@ -%d,%d +%d,%d @@\n", oldStart, oldCount, newStart, newCount)
		for _, o := range ops[start:end] {
			sb.WriteByte(o.kind)
			sb.WriteString(o.text)
			sb.WriteByte('\n')
		}
		for _, o := range ops[k:end] {
			if o.kind != '+' {
				oldLine++
			}
			if o.kind != '-' {
				newLine++
			}
		}
		k = end
	}
	return sb.String()
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}
