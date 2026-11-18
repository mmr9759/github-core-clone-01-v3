// REST v3 handlers (/api/v3).
package main

import (
	"encoding/base64"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

// pathUnescape percent-decodes a path segment (+ stays literal).
func pathUnescape(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '%' && i+2 < len(s) {
			if hi, ok1 := hexVal(s[i+1]); ok1 {
				if lo, ok2 := hexVal(s[i+2]); ok2 {
					b.WriteByte(byte(hi<<4 | lo))
					i += 2
					continue
				}
			}
		}
		b.WriteByte(c)
	}
	return b.String()
}

func (s *server) dispatchREST(req *request) response {
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	path := strings.TrimSuffix(req.path, "/")
	if path == "/api/v3" {
		path = "/api/v3/"
	}
	rest := strings.TrimPrefix(path, "/api/v3/")
	segs := []string{}
	if rest != "" {
		for _, seg := range strings.Split(rest, "/") {
			segs = append(segs, pathUnescape(seg))
		}
	}
	if len(segs) == 0 {
		if req.method != "GET" {
			return errJSON(405, "method not allowed")
		}
		resp := jsonResp(200, []byte(`{"message":"forge","current_user_url":"/api/v3/user"}`))
		resp.headers = map[string]string{"X-OAuth-Scopes": "repo"}
		return resp
	}
	switch segs[0] {
	case "meta":
		if req.method == "GET" && len(segs) == 1 {
			return jsonResp(200, []byte(`{"installed_version":"1.0.0"}`))
		}
	case "user":
		return s.restUser(req, segs[1:])
	case "repos":
		return s.restRepos(req, segs[1:])
	}
	return errJSON(404, "Not Found")
}

func (s *server) restUser(req *request, segs []string) response {
	if len(segs) == 0 {
		if req.method != "GET" {
			return errJSON(405, "method not allowed")
		}
		return jsonResp(200, mustJSON(userJSON{Login: s.st.viewer, ID: 1000, Type: "User"}))
	}
	if len(segs) == 1 && segs[0] == "repos" {
		switch req.method {
		case "POST":
			var body struct {
				Name        string `json:"name"`
				Private     bool   `json:"private"`
				Description string `json:"description"`
			}
			if !decodeBody(req.body, &body) {
				return errJSON(400, "malformed JSON")
			}
			if strings.TrimSpace(body.Name) == "" {
				return errJSON(422, "name is required")
			}
			if s.st.getRepo(s.st.viewer, body.Name) != nil {
				return errJSON(422, "Repository "+body.Name+" already exists")
			}
			r := s.st.createRepo(s.st.viewer, body.Name, body.Private, body.Description)
			return jsonResp(201, mustJSON(s.repoJSON(r)))
		case "GET":
			perPage, page := pagination(req)
			var list []repoJSON
			for _, key := range s.st.order {
				r := s.st.repos[key]
				list = append(list, s.repoJSON(r))
			}
			total := len(list)
			pageItems, hasNext := paginate(list, perPage, page)
			resp := jsonResp(200, mustJSON(pageItems))
			if hasNext {
				resp.headers = map[string]string{"Link": s.linkHeader(req, perPage, page+1, total)}
			}
			return resp
		}
	}
	return errJSON(404, "Not Found")
}

func (s *server) linkHeader(req *request, perPage, page, total int) string {
	host := req.headers["host"]
	if host == "" {
		host = "127.0.0.1"
	}
	last := (total + perPage - 1) / perPage
	if last < 1 {
		last = 1
	}
	next := "http://" + host + req.path + "?per_page=" + strconv.Itoa(perPage) + "&page=" + strconv.Itoa(page)
	lastURL := "http://" + host + req.path + "?per_page=" + strconv.Itoa(perPage) + "&page=" + strconv.Itoa(last)
	return "<" + next + `>; rel="next", <` + lastURL + `>; rel="last"`
}

func pagination(req *request) (perPage, page int) {
	perPage = 30
	page = 1
	if v := queryFirst(req.query, "per_page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			perPage = n
		}
	}
	if v := queryFirst(req.query, "page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			page = n
		}
	}
	return perPage, page
}

func queryFirst(q map[string][]string, key string) string {
	if v, ok := q[key]; ok && len(v) > 0 {
		return v[0]
	}
	return ""
}

func paginate[T any](items []T, perPage, page int) ([]T, bool) {
	if perPage <= 0 {
		perPage = 30
	}
	if page <= 0 {
		page = 1
	}
	start := (page - 1) * perPage
	if start >= len(items) {
		return []T{}, false
	}
	end := start + perPage
	if end > len(items) {
		end = len(items)
	}
	return items[start:end], end < len(items)
}

func decodeBody(body []byte, v any) bool {
	if len(body) == 0 {
		return true
	}
	return json.Unmarshal(body, v) == nil
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// ---------------------------------------------------------------------------
// JSON shapes
// ---------------------------------------------------------------------------

type userJSON struct {
	Login string `json:"login"`
	ID    int    `json:"id"`
	Type  string `json:"type"`
}

type repoJSON struct {
	ID              int      `json:"id"`
	NodeID          string   `json:"node_id"`
	Name            string   `json:"name"`
	FullName        string   `json:"full_name"`
	Owner           userJSON `json:"owner"`
	Private         bool     `json:"private"`
	Description     string   `json:"description"`
	DefaultBranch   string   `json:"default_branch"`
	CreatedAt       string   `json:"created_at"`
	UpdatedAt       string   `json:"updated_at"`
	HTMLURL         string   `json:"html_url"`
	CloneURL        string   `json:"clone_url"`
	GitURL          string   `json:"git_url"`
	SSHURL          string   `json:"ssh_url"`
	Homepage        string   `json:"homepage"`
	Language        *string  `json:"language"`
	ForksCount      int      `json:"forks_count"`
	StargazersCount int      `json:"stargazers_count"`
	WatchersCount   int      `json:"watchers_count"`
	OpenIssuesCount int      `json:"open_issues_count"`
	HasIssues       bool     `json:"has_issues"`
}

func globalID(kind, parts string) string {
	return base64.StdEncoding.EncodeToString([]byte(kind + ":" + parts))
}

func (s *server) repoJSON(r *repo) repoJSON {
	open := 0
	for _, iss := range r.issues {
		if iss.state == "open" {
			open++
		}
	}
	return repoJSON{
		ID:              r.id,
		NodeID:          globalID("R", r.owner+"/"+r.name),
		Name:            r.name,
		FullName:        r.owner + "/" + r.name,
		Owner:           userJSON{Login: r.owner, ID: 1000, Type: "User"},
		Private:         r.private,
		Description:     r.description,
		DefaultBranch:   r.defaultBranch,
		CreatedAt:       fixedTime,
		UpdatedAt:       fixedTime,
		HTMLURL:         "https://example.invalid/" + r.owner + "/" + r.name,
		CloneURL:        "https://example.invalid/" + r.owner + "/" + r.name + ".git",
		GitURL:          "git://example.invalid/" + r.owner + "/" + r.name + ".git",
		SSHURL:          "git@example.invalid:" + r.owner + "/" + r.name + ".git",
		Homepage:        "",
		Language:        nil,
		ForksCount:      0,
		StargazersCount: 0,
		WatchersCount:   0,
		OpenIssuesCount: open,
		HasIssues:       true,
	}
}

// ---------------------------------------------------------------------------
// /repos/{owner}/{repo}...
// ---------------------------------------------------------------------------

func (s *server) restRepos(req *request, segs []string) response {
	if len(segs) < 2 {
		return errJSON(404, "Not Found")
	}
	owner, name := segs[0], segs[1]
	if owner != s.st.viewer {
		return errJSON(404, "Not Found")
	}
	r := s.st.getRepo(owner, name)
	rest := segs[2:]
	if len(rest) == 0 {
		if r == nil {
			return errJSON(404, "Not Found")
		}
		switch req.method {
		case "GET":
			return jsonResp(200, mustJSON(s.repoJSON(r)))
		case "PATCH":
			var body struct {
				Description *string `json:"description"`
				Private     *bool   `json:"private"`
			}
			if !decodeBody(req.body, &body) {
				return errJSON(400, "malformed JSON")
			}
			if body.Description != nil {
				r.description = *body.Description
			}
			if body.Private != nil {
				r.private = *body.Private
			}
			return jsonResp(200, mustJSON(s.repoJSON(r)))
		case "DELETE":
			key := repoKey(owner, name)
			delete(s.st.repos, key)
			for i, k := range s.st.order {
				if k == key {
					s.st.order = append(s.st.order[:i], s.st.order[i+1:]...)
					break
				}
			}
			return response{status: 204, contentType: "application/json", body: nil}
		}
		return errJSON(405, "method not allowed")
	}
	if r == nil {
		return errJSON(404, "Not Found")
	}
	switch rest[0] {
	case "git":
		return s.restGitRefs(req, r, rest[1:])
	case "branches":
		return s.restBranches(req, r, rest[1:])
	case "commits":
		return s.restCommits(req, r)
	case "contents":
		return s.restContents(req, r, rest[1:])
	case "labels":
		return s.restLabels(req, r)
	case "issues":
		return s.restIssues(req, r, rest[1:])
	case "pulls":
		return s.restPulls(req, r, rest[1:])
	}
	return errJSON(404, "Not Found")
}

func (s *server) restGitRefs(req *request, r *repo, rest []string) response {
	if len(rest) == 0 {
		return errJSON(404, "Not Found")
	}
	if rest[0] != "refs" {
		return errJSON(404, "Not Found")
	}
	if len(rest) == 1 {
		switch req.method {
		case "POST":
			var body struct {
				Ref string `json:"ref"`
				SHA string `json:"sha"`
			}
			if !decodeBody(req.body, &body) {
				return errJSON(400, "malformed JSON")
			}
			if body.Ref == "" || !strings.HasPrefix(body.Ref, "refs/") {
				return errJSON(422, "invalid ref")
			}
			if _, exists := r.refs[body.Ref]; exists {
				return errJSON(422, "ref already exists")
			}
			o := r.objects[body.SHA]
			if o == nil || (o.typ != "commit" && o.typ != "tag") {
				return errJSON(422, "object does not exist")
			}
			r.refs[body.Ref] = body.SHA
			r.version++
			return jsonResp(201, mustJSON(map[string]any{
				"ref":    body.Ref,
				"object": map[string]string{"sha": body.SHA, "type": o.typ},
			}))
		case "GET":
			var list []map[string]any
			for _, name := range sortedRefNames(r) {
				list = append(list, map[string]any{
					"ref":    name,
					"object": map[string]string{"sha": r.refs[name]},
				})
			}
			if list == nil {
				list = []map[string]any{}
			}
			return jsonResp(200, mustJSON(list))
		}
		return errJSON(405, "method not allowed")
	}
	// GET /git/refs/{type}/{name...}
	if req.method != "GET" {
		return errJSON(405, "method not allowed")
	}
	refName := "refs/" + strings.Join(rest[1:], "/")
	oid, ok := r.refs[refName]
	if !ok {
		return errJSON(404, "Not Found")
	}
	return jsonResp(200, mustJSON(map[string]any{
		"ref":    refName,
		"object": map[string]string{"sha": oid},
	}))
}

type branchJSON struct {
	Name      string `json:"name"`
	Commit    struct {
		SHA string `json:"sha"`
	} `json:"commit"`
	Protected bool `json:"protected"`
}

func (s *server) restBranches(req *request, r *repo, rest []string) response {
	if req.method != "GET" {
		return errJSON(405, "method not allowed")
	}
	if len(rest) == 0 {
		var list []branchJSON
		for _, name := range sortedRefNames(r) {
			if !strings.HasPrefix(name, "refs/heads/") {
				continue
			}
			var b branchJSON
			b.Name = strings.TrimPrefix(name, "refs/heads/")
			b.Commit.SHA = r.refs[name]
			b.Protected = false
			list = append(list, b)
		}
		if list == nil {
			list = []branchJSON{}
		}
		return jsonResp(200, mustJSON(list))
	}
	branch := strings.Join(rest, "/")
	oid, ok := r.refs["refs/heads/"+branch]
	if !ok {
		return errJSON(404, "Not Found")
	}
	var b branchJSON
	b.Name = branch
	b.Commit.SHA = oid
	b.Protected = false
	return jsonResp(200, mustJSON(b))
}

type commitJSON struct {
	SHA     string `json:"sha"`
	Commit  struct {
		Message   string `json:"message"`
		Author    sigJSON `json:"author"`
		Committer sigJSON `json:"committer"`
	} `json:"commit"`
	Author    userJSON `json:"author"`
	Committer userJSON `json:"committer"`
	HTMLURL   string `json:"html_url"`
	URL       string `json:"url"`
}

type sigJSON struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Date  string `json:"date"`
}

func (s *server) restCommits(req *request, r *repo) response {
	if req.method != "GET" {
		return errJSON(405, "method not allowed")
	}
	ref := queryFirst(req.query, "sha")
	tip := r.resolveRef(ref)
	if tip == "" {
		return jsonResp(200, []byte("[]"))
	}
	var list []commitJSON
	for _, oid := range r.commitList(tip) {
		o := r.objects[oid]
		if o == nil {
			continue
		}
		_, _, msg, _ := parseCommit(o.data)
		var c commitJSON
		c.SHA = oid
		c.Commit.Message = msg
		c.Commit.Author = sigJSON{Name: "forge", Email: "forge@example.invalid", Date: fixedTime}
		c.Commit.Committer = sigJSON{Name: "forge", Email: "forge@example.invalid", Date: fixedTime}
		c.Author = userJSON{Login: s.st.viewer, ID: 1000, Type: "User"}
		c.Committer = userJSON{Login: s.st.viewer, ID: 1000, Type: "User"}
		c.HTMLURL = "https://example.invalid/" + r.owner + "/" + r.name + "/commit/" + oid
		c.URL = "/api/v3/repos/" + r.owner + "/" + r.name + "/commits/" + oid
		list = append(list, c)
	}
	if list == nil {
		list = []commitJSON{}
	}
	perPage, page := pagination(req)
	pageItems, _ := paginate(list, perPage, page)
	return jsonResp(200, mustJSON(pageItems))
}

// ---------------------------------------------------------------------------
// contents
// ---------------------------------------------------------------------------

type contentFileJSON struct {
	Type     string `json:"type"`
	Encoding string `json:"encoding"`
	Content  string `json:"content"`
	SHA      string `json:"sha"`
	Size     int    `json:"size"`
	Name     string `json:"name"`
	Path     string `json:"path"`
}

type contentEntryJSON struct {
	Type string `json:"type"`
	Name string `json:"name"`
	Path string `json:"path"`
	SHA  string `json:"sha"`
	Size int    `json:"size"`
}

func (s *server) restContents(req *request, r *repo, rest []string) response {
	path := strings.Join(rest, "/")
	switch req.method {
	case "GET":
		return s.contentsGet(r, path, queryFirst(req.query, "ref"))
	case "PUT":
		return s.contentsPut(req, r, path)
	case "DELETE":
		return s.contentsDelete(req, r, path)
	}
	return errJSON(405, "method not allowed")
}

// dirEntries lists one directory level of a tree.
func (r *repo) dirEntries(treeOID, prefix string) []contentEntryJSON {
	o := r.objects[treeOID]
	if o == nil || o.typ != "tree" {
		return nil
	}
	var out []contentEntryJSON
	for _, e := range parseTree(o.data) {
		full := prefix + e.name
		if e.mode == "40000" {
			out = append(out, contentEntryJSON{Type: "dir", Name: e.name, Path: full, SHA: e.oid, Size: 0})
		} else {
			size := 0
			if bo := r.objects[e.oid]; bo != nil {
				size = len(bo.data)
			}
			out = append(out, contentEntryJSON{Type: "file", Name: e.name, Path: full, SHA: e.oid, Size: size})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *server) contentsGet(r *repo, path, ref string) response {
	tip := r.resolveRef(ref)
	if tip == "" {
		return errJSON(404, "Not Found")
	}
	treeOID := r.commitTree(tip)
	if path == "" {
		entries := r.dirEntries(treeOID, "")
		if entries == nil {
			entries = []contentEntryJSON{}
		}
		return jsonResp(200, mustJSON(entries))
	}
	if blobOID := r.treeGet(treeOID, path); blobOID != "" {
		bo := r.objects[blobOID]
		if bo == nil {
			return errJSON(404, "Not Found")
		}
		name := path
		if idx := strings.LastIndexByte(path, '/'); idx >= 0 {
			name = path[idx+1:]
		}
		return jsonResp(200, mustJSON(contentFileJSON{
			Type:     "file",
			Encoding: "base64",
			Content:  base64.StdEncoding.EncodeToString(bo.data),
			SHA:      blobOID,
			Size:     len(bo.data),
			Name:     name,
			Path:     path,
		}))
	}
	// Directory?
	flat := r.flattenTree(treeOID)
	prefix := path + "/"
	found := false
	for p := range flat {
		if strings.HasPrefix(p, prefix) {
			found = true
			break
		}
	}
	if !found {
		return errJSON(404, "Not Found")
	}
	// Walk to the subtree oid for this path.
	sub := treeOID
	for _, part := range strings.Split(path, "/") {
		o := r.objects[sub]
		if o == nil {
			return errJSON(404, "Not Found")
		}
		next := ""
		for _, e := range parseTree(o.data) {
			if e.name == part && e.mode == "40000" {
				next = e.oid
				break
			}
		}
		if next == "" {
			return errJSON(404, "Not Found")
		}
		sub = next
	}
	entries := r.dirEntries(sub, prefix)
	if entries == nil {
		entries = []contentEntryJSON{}
	}
	return jsonResp(200, mustJSON(entries))
}

func (s *server) contentsPut(req *request, r *repo, path string) response {
	var body struct {
		Message string `json:"message"`
		Content string `json:"content"`
		Branch  string `json:"branch"`
		SHA     string `json:"sha"`
	}
	if !decodeBody(req.body, &body) {
		return errJSON(400, "malformed JSON")
	}
	if body.Message == "" {
		body.Message = "Update " + path
	}
	raw, err := base64.StdEncoding.DecodeString(body.Content)
	if err != nil {
		return errJSON(400, "invalid base64 content")
	}
	branch := body.Branch
	if branch == "" {
		branch = r.defaultBranch
	}
	branchRef := "refs/heads/" + branch
	tip, exists := r.refs[branchRef]
	if !exists {
		// Create the branch from the default-branch tip (or unborn).
		tip = r.refs["refs/heads/"+r.defaultBranch]
	}
	var treeOID string
	var parents []string
	created := false
	if tip == "" {
		treeOID = ""
		parents = nil
		created = true
	} else {
		treeOID = r.commitTree(tip)
		parents = []string{tip}
		current := r.treeGet(treeOID, path)
		if current != "" {
			if body.SHA == "" {
				return errJSON(409, "sha is required to update a file")
			}
			if body.SHA != current {
				return errJSON(409, "sha does not match")
			}
		} else {
			created = true
			if body.SHA != "" {
				return errJSON(409, "sha does not match")
			}
		}
	}
	blobOID := r.storeObject("blob", raw)
	newTree := r.treeSet(treeOID, path, blobOID)
	if newTree == "" {
		newTree = r.emptyTreeOID()
	}
	commitOID := r.storeObject("commit", buildCommitData(newTree, parents, body.Message))
	r.refs[branchRef] = commitOID
	r.version++
	name := path
	if idx := strings.LastIndexByte(path, '/'); idx >= 0 {
		name = path[idx+1:]
	}
	status := 201
	if !created {
		status = 200
	}
	return jsonResp(status, mustJSON(map[string]any{
		"content": map[string]any{
			"name": name, "path": path, "sha": blobOID,
			"encoding": "base64", "size": len(raw), "type": "file",
		},
		"commit": map[string]string{"sha": commitOID, "message": body.Message},
	}))
}

func (s *server) contentsDelete(req *request, r *repo, path string) response {
	var body struct {
		Message string `json:"message"`
		SHA     string `json:"sha"`
		Branch  string `json:"branch"`
	}
	if !decodeBody(req.body, &body) {
		return errJSON(400, "malformed JSON")
	}
	if body.Message == "" {
		body.Message = "Delete " + path
	}
	branch := body.Branch
	if branch == "" {
		branch = r.defaultBranch
	}
	branchRef := "refs/heads/" + branch
	tip, exists := r.refs[branchRef]
	if !exists {
		return errJSON(404, "Not Found")
	}
	treeOID := r.commitTree(tip)
	current := r.treeGet(treeOID, path)
	if current == "" {
		return errJSON(404, "Not Found")
	}
	if body.SHA != "" && body.SHA != current {
		return errJSON(409, "sha does not match")
	}
	newTree := r.treeSet(treeOID, path, "")
	if newTree == "" {
		newTree = r.emptyTreeOID()
	}
	commitOID := r.storeObject("commit", buildCommitData(newTree, []string{tip}, body.Message))
	r.refs[branchRef] = commitOID
	r.version++
	return jsonResp(200, mustJSON(map[string]any{
		"commit":  map[string]string{"sha": commitOID, "message": body.Message},
		"content": nil,
	}))
}
