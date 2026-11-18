// Merge and diff logic, shared by the REST merge endpoint and GraphQL mergeable.
package main

import "strings"

// diffFlat compares two flat path->blob maps. Each entry is [old, new] with ""
// meaning absent on that side.
func diffFlat(base, tip map[string]string) map[string][2]string {
	out := map[string][2]string{}
	for p, b := range base {
		if t, ok := tip[p]; !ok {
			out[p] = [2]string{b, ""}
		} else if t != b {
			out[p] = [2]string{b, t}
		}
	}
	for p, t := range tip {
		if _, ok := base[p]; !ok {
			out[p] = [2]string{"", t}
		}
	}
	return out
}

// mergeCheck computes whether head can merge into baseTip (squash semantics).
// It returns (conflict, mergedTreeOID). When there is no common ancestor the
// merge conflicts.
func (r *repo) mergeCheck(baseTip, headTip string) (bool, string) {
	base := r.mergeBase(baseTip, headTip)
	if base == "" {
		return true, ""
	}
	baseTree := r.flattenTree(r.commitTree(base))
	baseTipTree := r.flattenTree(r.commitTree(baseTip))
	headTree := r.flattenTree(r.commitTree(headTip))
	changeBase := diffFlat(baseTree, baseTipTree)
	changeHead := diffFlat(baseTree, headTree)
	for path, h := range changeHead {
		if b, ok := changeBase[path]; ok {
			if b[1] != h[1] {
				return true, ""
			}
		}
	}
	merged := map[string]string{}
	for p, oid := range baseTipTree {
		merged[p] = oid
	}
	for path, h := range changeHead {
		if h[1] == "" {
			delete(merged, path)
		} else {
			merged[path] = h[1]
		}
	}
	return false, r.buildTreeFromPaths(merged)
}

// splitLines splits content into lines, dropping the empty tail after a final newline.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// lineDiff counts added and deleted lines between two texts (small LCS).
func lineDiff(oldText, newText string) (add, del int) {
	oldLines := splitLines(oldText)
	newLines := splitLines(newText)
	n, m := len(oldLines), len(newLines)
	if n == 0 && m == 0 {
		return 0, 0
	}
	if n > 600 || m > 600 {
		// Fallback for unexpectedly large files: count by size difference.
		if m > n {
			return m - n, 0
		}
		return 0, n - m
	}
	// LCS dynamic programming, two rows.
	prev := make([]int, m+1)
	cur := make([]int, m+1)
	for i := 1; i <= n; i++ {
		for j := 1; j <= m; j++ {
			if oldLines[i-1] == newLines[j-1] {
				cur[j] = prev[j-1] + 1
			} else if prev[j] >= cur[j-1] {
				cur[j] = prev[j]
			} else {
				cur[j] = cur[j-1]
			}
		}
		prev, cur = cur, prev
	}
	lcs := prev[m]
	return m - lcs, n - lcs
}
