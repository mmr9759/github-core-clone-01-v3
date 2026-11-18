// In-memory store: repositories, git objects, refs, issues, pull requests.
// Everything is deterministic: fixed timestamps, sequential ids, sorted output.
package main

import (
	"crypto/sha1"
	"encoding/hex"
	"sort"
	"strings"
	"sync"
)

const fixedTime = "2025-01-01T00:00:00Z"
const commitIdentity = "forge <forge@example.invalid> 1700000000 +0000"
const zeroOID = "0000000000000000000000000000000000000000"

func isOID(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func hashObject(typ string, data []byte) string {
	h := sha1.New()
	h.Write([]byte(typ))
	h.Write([]byte(" "))
	h.Write([]byte(itoa(len(data))))
	h.Write([]byte{0})
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// ---------------------------------------------------------------------------
// Git objects
// ---------------------------------------------------------------------------

type obj struct {
	typ  string // commit, tree, blob, tag
	data []byte
}

type treeEntry struct {
	mode string // "100644" or "40000"
	name string
	oid  string
}

// parseTree decodes raw tree bytes into entries.
func parseTree(data []byte) []treeEntry {
	var out []treeEntry
	i := 0
	for i < len(data) {
		sp := strings.IndexByte(string(data[i:]), ' ')
		if sp < 0 {
			break
		}
		mode := string(data[i : i+sp])
		i += sp + 1
		nul := strings.IndexByte(string(data[i:]), 0)
		if nul < 0 || i+nul+21 > len(data) {
			break
		}
		name := string(data[i : i+nul])
		i += nul + 1
		oid := hex.EncodeToString(data[i : i+20])
		i += 20
		out = append(out, treeEntry{mode: mode, name: name, oid: oid})
	}
	return out
}

// treeSortKey orders entries the way git does: directories compare as name+"/".
func treeSortKey(e treeEntry) string {
	if e.mode == "40000" {
		return e.name + "/"
	}
	return e.name
}

func sortTreeEntries(es []treeEntry) {
	sort.Slice(es, func(i, j int) bool { return treeSortKey(es[i]) < treeSortKey(es[j]) })
}

func encodeTree(es []treeEntry) []byte {
	sortTreeEntries(es)
	var b []byte
	for _, e := range es {
		b = append(b, []byte(e.mode+" "+e.name)...)
		b = append(b, 0)
		raw, _ := hex.DecodeString(e.oid)
		b = append(b, raw...)
	}
	return b
}

// parseCommit returns tree, parents, message and committer timestamp.
func parseCommit(data []byte) (tree string, parents []string, msg string, ts int64) {
	text := string(data)
	head, body, found := strings.Cut(text, "\n\n")
	if !found {
		head = text
		body = ""
	}
	for _, line := range strings.Split(head, "\n") {
		switch {
		case strings.HasPrefix(line, "tree "):
			tree = strings.TrimSpace(line[5:])
		case strings.HasPrefix(line, "parent "):
			parents = append(parents, strings.TrimSpace(line[7:]))
		case strings.HasPrefix(line, "committer "):
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				ts = atoi64(fields[len(fields)-2])
			}
		}
	}
	msg = strings.TrimSuffix(body, "\n")
	return tree, parents, msg, ts
}

func atoi64(s string) int64 {
	var n int64
	neg := false
	if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			break
		}
		n = n*10 + int64(s[i]-'0')
	}
	if neg {
		return -n
	}
	return n
}

func buildCommitData(tree string, parents []string, message string) []byte {
	var b strings.Builder
	b.WriteString("tree " + tree + "\n")
	for _, p := range parents {
		b.WriteString("parent " + p + "\n")
	}
	b.WriteString("author " + commitIdentity + "\n")
	b.WriteString("committer " + commitIdentity + "\n")
	b.WriteString("\n")
	b.WriteString(message)
	b.WriteString("\n")
	return []byte(b.String())
}

// ---------------------------------------------------------------------------
// Store
// ---------------------------------------------------------------------------

type comment struct {
	id   int
	body string
	user string
}

type issue struct {
	id        int
	number    int
	title     string
	body      string
	state     string // open | closed
	labels    []string
	user      string
	createdAt string
	updatedAt string
	closedAt  string
	comments  []*comment
}

type pullRequest struct {
	id         int
	number     int
	title      string
	body       string
	state      string // open | closed
	headRef    string
	baseRef    string
	headSha    string
	baseSha    string
	merged     bool
	mergedAt   string
	mergeSha   string
	user       string
	createdAt  string
	updatedAt  string
	closedAt   string
}

type label struct {
	id    int
	name  string
	color string
}

type repo struct {
	id            int
	owner         string
	name          string
	private       bool
	description   string
	defaultBranch string
	objects       map[string]*obj
	refs          map[string]string // full ref -> commit oid
	issues        map[int]*issue
	prs           map[int]*pullRequest
	labels        map[string]*label
	labelOrder    []string
	nextNum       int
	nextComment   int
	nextLabelID   int
	version       uint64 // bumps on any object/ref mutation; part of pack-cache keys
	issueCache    map[int][]byte
	packCache     map[string][]byte
}

type store struct {
	mu         sync.RWMutex
	repos      map[string]*repo
	order      []string // creation order of repo keys
	nextRepoID int
	nextEntity int
	viewer     string
}

func newStore(viewer string) *store {
	return &store{
		repos:      map[string]*repo{},
		nextRepoID: 1,
		nextEntity: 1,
		viewer:     viewer,
	}
}

func repoKey(owner, name string) string { return owner + "/" + name }

func (s *store) getRepo(owner, name string) *repo {
	return s.repos[repoKey(owner, name)]
}

func (s *store) createRepo(owner, name string, private bool, description string) *repo {
	r := &repo{
		id:            s.nextRepoID,
		owner:         owner,
		name:          name,
		private:       private,
		description:   description,
		defaultBranch: "main",
		objects:       map[string]*obj{},
		refs:          map[string]string{},
		issues:        map[int]*issue{},
		prs:           map[int]*pullRequest{},
		labels:        map[string]*label{},
		nextNum:       1,
		nextComment:   1,
		nextLabelID:   1,
		issueCache:    map[int][]byte{},
		packCache:     map[string][]byte{},
	}
	s.nextRepoID++
	key := repoKey(owner, name)
	s.repos[key] = r
	s.order = append(s.order, key)
	return r
}

// storeObject stores a new object, returning its oid. Bumps the repo version
// when the object is new, which invalidates cached clone packs.
func (r *repo) storeObject(typ string, data []byte) string {
	oid := hashObject(typ, data)
	if _, ok := r.objects[oid]; !ok {
		r.objects[oid] = &obj{typ: typ, data: data}
		r.version++
	}
	return oid
}

func (r *repo) getObject(oid string) *obj {
	return r.objects[oid]
}

func (r *repo) commitTree(commitOID string) string {
	o := r.objects[commitOID]
	if o == nil || o.typ != "commit" {
		return ""
	}
	tree, _, _, _ := parseCommit(o.data)
	return tree
}

// addTreeClosure adds a tree and everything under it (subtrees + blobs) to set.
func (r *repo) addTreeClosure(set map[string]bool, treeOID string) {
	o := r.objects[treeOID]
	if o == nil || o.typ != "tree" || set[treeOID] {
		return
	}
	set[treeOID] = true
	for _, e := range parseTree(o.data) {
		if e.mode == "40000" {
			r.addTreeClosure(set, e.oid)
		} else {
			set[e.oid] = true
		}
	}
}

// flattenTree maps every file path under a tree oid to its blob oid.
func (r *repo) flattenTree(treeOID string) map[string]string {
	out := map[string]string{}
	var walk func(treeOID, prefix string)
	walk = func(treeOID, prefix string) {
		o := r.objects[treeOID]
		if o == nil || o.typ != "tree" {
			return
		}
		for _, e := range parseTree(o.data) {
			path := prefix + e.name
			if e.mode == "40000" {
				walk(e.oid, path+"/")
			} else {
				out[path] = e.oid
			}
		}
	}
	walk(treeOID, "")
	return out
}

// treeGet returns the blob oid at path, or "" when absent.
func (r *repo) treeGet(treeOID, path string) string {
	flat := r.flattenTree(treeOID)
	return flat[path]
}

// treeSet returns a new tree oid with path set to blobOID (or deleted when
// blobOID is ""). Intermediate trees are created as needed. "" means the
// resulting tree is empty.
func (r *repo) treeSet(treeOID, path, blobOID string) string {
	parts := strings.Split(path, "/")
	return r.treeSetParts(treeOID, parts, blobOID)
}

func (r *repo) treeSetParts(treeOID string, parts []string, blobOID string) string {
	var entries []treeEntry
	if treeOID != "" {
		if o := r.objects[treeOID]; o != nil && o.typ == "tree" {
			entries = append(entries, parseTree(o.data)...)
		}
	}
	name := parts[0]
	if len(parts) == 1 {
		kept := entries[:0]
		for _, e := range entries {
			if e.name != name {
				kept = append(kept, e)
			}
		}
		entries = kept
		if blobOID != "" {
			entries = append(entries, treeEntry{mode: "100644", name: name, oid: blobOID})
		}
	} else {
		var sub string
		kept := entries[:0]
		for _, e := range entries {
			if e.name == name && e.mode == "40000" {
				sub = e.oid
			} else if e.name != name {
				kept = append(kept, e)
			}
		}
		entries = kept
		sub = r.treeSetParts(sub, parts[1:], blobOID)
		if sub != "" {
			entries = append(entries, treeEntry{mode: "40000", name: name, oid: sub})
		}
	}
	if len(entries) == 0 {
		return ""
	}
	return r.storeObject("tree", encodeTree(entries))
}

// emptyTreeOID returns the canonical empty-tree oid, storing the object.
func (r *repo) emptyTreeOID() string {
	return r.storeObject("tree", []byte{})
}

// buildTreeFromPaths builds nested trees for a flat path->blob map.
func (r *repo) buildTreeFromPaths(paths map[string]string) string {
	var rec func(prefix string) string
	rec = func(prefix string) string {
		dirs := map[string]bool{}
		var entries []treeEntry
		for path, blob := range paths {
			if !strings.HasPrefix(path, prefix) {
				continue
			}
			rest := path[len(prefix):]
			if idx := strings.IndexByte(rest, '/'); idx >= 0 {
				dirs[rest[:idx]] = true
			} else {
				entries = append(entries, treeEntry{mode: "100644", name: rest, oid: blob})
			}
		}
		for d := range dirs {
			sub := rec(prefix + d + "/")
			if sub != "" {
				entries = append(entries, treeEntry{mode: "40000", name: d, oid: sub})
			}
		}
		if len(entries) == 0 {
			return ""
		}
		return r.storeObject("tree", encodeTree(entries))
	}
	if oid := rec(""); oid != "" {
		return oid
	}
	return r.emptyTreeOID()
}

// resolveRef turns a branch name, full ref, or commit oid into a commit oid.
func (r *repo) resolveRef(ref string) string {
	if ref == "" {
		ref = r.defaultBranch
	}
	if isOID(ref) {
		if o := r.objects[ref]; o != nil && o.typ == "commit" {
			return ref
		}
		return ""
	}
	if oid, ok := r.refs[ref]; ok {
		return oid
	}
	if oid, ok := r.refs["refs/heads/"+ref]; ok {
		return oid
	}
	if oid, ok := r.refs["refs/tags/"+ref]; ok {
		return oid
	}
	return ""
}

// commitList walks the first-parent chain from tip.
func (r *repo) commitList(tip string) []string {
	var out []string
	seen := map[string]bool{}
	cur := tip
	for cur != "" && !seen[cur] {
		seen[cur] = true
		o := r.objects[cur]
		if o == nil || o.typ != "commit" {
			break
		}
		out = append(out, cur)
		_, parents, _, _ := parseCommit(o.data)
		if len(parents) == 0 {
			break
		}
		cur = parents[0]
	}
	return out
}

// ancestors returns every commit reachable from tip (following all parents).
func (r *repo) ancestors(tip string) map[string]bool {
	out := map[string]bool{}
	queue := []string{tip}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur == "" || out[cur] {
			continue
		}
		o := r.objects[cur]
		if o == nil || o.typ != "commit" {
			continue
		}
		out[cur] = true
		_, parents, _, _ := parseCommit(o.data)
		queue = append(queue, parents...)
	}
	return out
}

// commitsWithin returns commits reachable from tips with their distance
// (0 = tip). When maxDist >= 0, only commits with distance <= maxDist.
func (r *repo) commitsWithin(tips []string, maxDist int) map[string]int {
	dist := map[string]int{}
	queue := []string{}
	for _, t := range tips {
		if t == "" || t == zeroOID {
			continue
		}
		if _, ok := dist[t]; !ok {
			dist[t] = 0
			queue = append(queue, t)
		}
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		d := dist[cur]
		if maxDist >= 0 && d >= maxDist {
			continue
		}
		o := r.objects[cur]
		if o == nil || o.typ != "commit" {
			continue
		}
		_, parents, _, _ := parseCommit(o.data)
		for _, p := range parents {
			if _, ok := dist[p]; !ok {
				dist[p] = d + 1
				queue = append(queue, p)
			}
		}
	}
	return dist
}

// isAncestor reports whether anc is reachable from desc (or equal).
func (r *repo) isAncestor(anc, desc string) bool {
	if anc == desc {
		return true
	}
	return r.ancestors(desc)[anc]
}

// mergeBase returns the nearest common ancestor of a and b ("" when none).
func (r *repo) mergeBase(a, b string) string {
	if a == "" || b == "" {
		return ""
	}
	aSet := r.ancestors(a)
	// Breadth-first from b: the first commit also in aSet is the nearest base.
	seen := map[string]bool{}
	queue := []string{b}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur == "" || seen[cur] {
			continue
		}
		seen[cur] = true
		if aSet[cur] {
			return cur
		}
		o := r.objects[cur]
		if o == nil || o.typ != "commit" {
			continue
		}
		_, parents, _, _ := parseCommit(o.data)
		queue = append(queue, parents...)
	}
	return ""
}

// invalidateIssueCache drops a cached single-issue JSON body.
func (r *repo) invalidateIssueCache(number int) {
	delete(r.issueCache, number)
}
