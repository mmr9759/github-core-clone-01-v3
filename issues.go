// REST v3: labels, issues, comments, pull requests.
package main

import (
	"sort"
	"strconv"
	"strings"
)

type labelJSON struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

func (s *server) labelJSON(l *label) labelJSON {
	return labelJSON{ID: l.id, Name: l.name, Color: l.color, Description: ""}
}

// ---------------------------------------------------------------------------
// labels
// ---------------------------------------------------------------------------

func (s *server) restLabels(req *request, r *repo) response {
	switch req.method {
	case "GET":
		var list []labelJSON
		for _, name := range r.labelOrder {
			list = append(list, s.labelJSON(r.labels[name]))
		}
		if list == nil {
			list = []labelJSON{}
		}
		return jsonResp(200, mustJSON(list))
	case "POST":
		var body struct {
			Name  string `json:"name"`
			Color string `json:"color"`
		}
		if !decodeBody(req.body, &body) {
			return errJSON(400, "malformed JSON")
		}
		if strings.TrimSpace(body.Name) == "" {
			return errJSON(422, "name is required")
		}
		if l, ok := r.labels[body.Name]; ok {
			return jsonResp(201, mustJSON(s.labelJSON(l)))
		}
		color := strings.TrimPrefix(body.Color, "#")
		if color == "" {
			color = "ededed"
		}
		l := &label{id: r.nextLabelID, name: body.Name, color: color}
		r.nextLabelID++
		r.labels[body.Name] = l
		r.labelOrder = append(r.labelOrder, body.Name)
		return jsonResp(201, mustJSON(s.labelJSON(l)))
	}
	return errJSON(405, "method not allowed")
}

// ---------------------------------------------------------------------------
// issues
// ---------------------------------------------------------------------------

type issueJSON struct {
	ID        int         `json:"id"`
	NodeID    string      `json:"node_id"`
	Number    int         `json:"number"`
	Title     string      `json:"title"`
	Body      string      `json:"body"`
	State     string      `json:"state"`
	Labels    []labelJSON `json:"labels"`
	User      userJSON    `json:"user"`
	Assignee  *userJSON   `json:"assignee"`
	Locked    bool        `json:"locked"`
	Comments  int         `json:"comments"`
	CreatedAt string      `json:"created_at"`
	UpdatedAt string      `json:"updated_at"`
	ClosedAt  *string     `json:"closed_at"`
	HTMLURL   string      `json:"html_url"`
	URL       string      `json:"url"`
}

func (s *server) issueJSON(r *repo, iss *issue) issueJSON {
	labels := []labelJSON{}
	for _, name := range iss.labels {
		if l, ok := r.labels[name]; ok {
			labels = append(labels, s.labelJSON(l))
		}
	}
	var closedAt *string
	if iss.state == "closed" {
		v := fixedTime
		if iss.closedAt != "" {
			v = iss.closedAt
		}
		closedAt = &v
	}
	return issueJSON{
		ID:        iss.id,
		NodeID:    globalID("I", r.owner+"/"+r.name+"/"+strconv.Itoa(iss.number)),
		Number:    iss.number,
		Title:     iss.title,
		Body:      iss.body,
		State:     iss.state,
		Labels:    labels,
		User:      userJSON{Login: iss.user, ID: 1000, Type: "User"},
		Assignee:  nil,
		Locked:    false,
		Comments:  len(iss.comments),
		CreatedAt: iss.createdAt,
		UpdatedAt: iss.updatedAt,
		ClosedAt:  closedAt,
		HTMLURL:   "https://example.invalid/" + r.owner + "/" + r.name + "/issues/" + strconv.Itoa(iss.number),
		URL:       "/api/v3/repos/" + r.owner + "/" + r.name + "/issues/" + strconv.Itoa(iss.number),
	}
}

// cachedIssueJSON returns the cached (or freshly built) single-issue JSON body.
func (s *server) cachedIssueJSON(r *repo, iss *issue) []byte {
	if b, ok := r.issueCache[iss.number]; ok {
		return b
	}
	b := mustJSON(s.issueJSON(r, iss))
	r.issueCache[iss.number] = b
	return b
}

func (s *server) restIssues(req *request, r *repo, rest []string) response {
	if len(rest) == 0 {
		switch req.method {
		case "POST":
			var body struct {
				Title  string   `json:"title"`
				Body   string   `json:"body"`
				Labels []string `json:"labels"`
			}
			if !decodeBody(req.body, &body) {
				return errJSON(400, "malformed JSON")
			}
			if strings.TrimSpace(body.Title) == "" {
				return errJSON(422, "title is required")
			}
			for _, name := range body.Labels {
				if _, ok := r.labels[name]; !ok {
					return errJSON(422, "Label \""+name+"\" does not exist")
				}
			}
			iss := &issue{
				id:        s.st.nextEntity,
				number:    r.nextNum,
				title:     body.Title,
				body:      body.Body,
				state:     "open",
				labels:    append([]string(nil), body.Labels...),
				user:      s.st.viewer,
				createdAt: fixedTime,
				updatedAt: fixedTime,
			}
			s.st.nextEntity++
			r.nextNum++
			r.issues[iss.number] = iss
			return jsonResp(201, mustJSON(s.issueJSON(r, iss)))
		case "GET":
			state := queryFirst(req.query, "state")
			if state == "" {
				state = "open"
			}
			sortParam := queryFirst(req.query, "sort")
			direction := queryFirst(req.query, "direction")
			if direction == "" {
				direction = "desc"
			}
			var list []*issue
			for _, iss := range r.issues {
				if state != "all" && iss.state != state {
					continue
				}
				list = append(list, iss)
			}
			_ = sortParam // only "created" is used; numbers are creation order
			sort.Slice(list, func(i, j int) bool {
				if direction == "asc" {
					return list[i].number < list[j].number
				}
				return list[i].number > list[j].number
			})
			var out []issueJSON
			for _, iss := range list {
				out = append(out, s.issueJSON(r, iss))
			}
			if out == nil {
				out = []issueJSON{}
			}
			perPage, page := pagination(req)
			pageItems, _ := paginate(out, perPage, page)
			return jsonResp(200, mustJSON(pageItems))
		}
		return errJSON(405, "method not allowed")
	}
	number, err := strconv.Atoi(rest[0])
	if err != nil {
		return errJSON(404, "Not Found")
	}
	iss := r.issues[number]
	if len(rest) == 2 && rest[1] == "comments" {
		if iss == nil {
			return errJSON(404, "Not Found")
		}
		switch req.method {
		case "GET":
			var list []map[string]any
			for _, c := range iss.comments {
				list = append(list, map[string]any{
					"id": c.id, "body": c.body,
					"user":       map[string]any{"login": c.user, "id": 1000, "type": "User"},
					"created_at": fixedTime, "updated_at": fixedTime,
				})
			}
			if list == nil {
				list = []map[string]any{}
			}
			return jsonResp(200, mustJSON(list))
		case "POST":
			var body struct {
				Body string `json:"body"`
			}
			if !decodeBody(req.body, &body) {
				return errJSON(400, "malformed JSON")
			}
			c := &comment{id: r.nextComment, body: body.Body, user: s.st.viewer}
			r.nextComment++
			iss.comments = append(iss.comments, c)
			r.invalidateIssueCache(number)
			return jsonResp(201, mustJSON(map[string]any{
				"id": c.id, "body": c.body,
				"user":       map[string]any{"login": c.user, "id": 1000, "type": "User"},
				"created_at": fixedTime, "updated_at": fixedTime,
			}))
		}
		return errJSON(405, "method not allowed")
	}
	if len(rest) != 1 {
		return errJSON(404, "Not Found")
	}
	if iss == nil {
		return errJSON(404, "Not Found")
	}
	switch req.method {
	case "GET":
		return jsonResp(200, s.cachedIssueJSON(r, iss))
	case "PATCH":
		var body struct {
			State  *string  `json:"state"`
			Title  *string  `json:"title"`
			Body   *string  `json:"body"`
			Labels []string `json:"labels"`
		}
		// Distinguish "labels absent" from "labels: []": re-decode raw.
		var raw map[string]any
		_ = decodeBody(req.body, &raw)
		if !decodeBody(req.body, &body) {
			return errJSON(400, "malformed JSON")
		}
		if body.State != nil {
			if *body.State != "open" && *body.State != "closed" {
				return errJSON(422, "invalid state")
			}
			iss.state = *body.State
			if iss.state == "closed" {
				iss.closedAt = fixedTime
			} else {
				iss.closedAt = ""
			}
		}
		if body.Title != nil {
			iss.title = *body.Title
		}
		if body.Body != nil {
			iss.body = *body.Body
		}
		if _, hasLabels := raw["labels"]; hasLabels {
			for _, name := range body.Labels {
				if _, ok := r.labels[name]; !ok {
					return errJSON(422, "Label \""+name+"\" does not exist")
				}
			}
			iss.labels = append([]string(nil), body.Labels...)
		}
		iss.updatedAt = fixedTime
		r.invalidateIssueCache(number)
		return jsonResp(200, mustJSON(s.issueJSON(r, iss)))
	}
	return errJSON(405, "method not allowed")
}

// ---------------------------------------------------------------------------
// pull requests
// ---------------------------------------------------------------------------

type prRefJSON struct {
	Label string `json:"label"`
	Ref   string `json:"ref"`
	SHA   string `json:"sha"`
}

type prJSON struct {
	ID             int       `json:"id"`
	NodeID         string    `json:"node_id"`
	Number         int       `json:"number"`
	State          string    `json:"state"`
	Title          string    `json:"title"`
	Body           string    `json:"body"`
	User           userJSON  `json:"user"`
	Head           prRefJSON `json:"head"`
	Base           prRefJSON `json:"base"`
	Merged         bool      `json:"merged"`
	Mergeable      bool      `json:"mergeable"`
	MergeCommitSHA string    `json:"merge_commit_sha"`
	MergedAt       *string   `json:"merged_at"`
	CreatedAt      string    `json:"created_at"`
	UpdatedAt      string    `json:"updated_at"`
	ClosedAt       *string   `json:"closed_at"`
	Draft          bool      `json:"draft"`
	HTMLURL        string    `json:"html_url"`
	URL            string    `json:"url"`
}

func (s *server) prJSON(r *repo, pr *pullRequest) prJSON {
	conflict, _ := r.mergeCheck(pr.baseSha, pr.headSha)
	var mergedAt, closedAt *string
	if pr.merged {
		v := pr.mergedAt
		if v == "" {
			v = fixedTime
		}
		mergedAt = &v
	}
	if pr.state == "closed" {
		v := pr.closedAt
		if v == "" {
			v = fixedTime
		}
		closedAt = &v
	}
	return prJSON{
		ID:             pr.id,
		NodeID:         globalID("P", r.owner+"/"+r.name+"/"+strconv.Itoa(pr.number)),
		Number:         pr.number,
		State:          pr.state,
		Title:          pr.title,
		Body:           pr.body,
		User:           userJSON{Login: pr.user, ID: 1000, Type: "User"},
		Head:           prRefJSON{Label: r.owner + ":" + pr.headRef, Ref: pr.headRef, SHA: pr.headSha},
		Base:           prRefJSON{Label: r.owner + ":" + pr.baseRef, Ref: pr.baseRef, SHA: pr.baseSha},
		Merged:         pr.merged,
		Mergeable:      !conflict,
		MergeCommitSHA: pr.mergeSha,
		MergedAt:       mergedAt,
		CreatedAt:      pr.createdAt,
		UpdatedAt:      pr.updatedAt,
		ClosedAt:       closedAt,
		Draft:          false,
		HTMLURL:        "https://example.invalid/" + r.owner + "/" + r.name + "/pull/" + strconv.Itoa(pr.number),
		URL:            "/api/v3/repos/" + r.owner + "/" + r.name + "/pulls/" + strconv.Itoa(pr.number),
	}
}

func (s *server) restPulls(req *request, r *repo, rest []string) response {
	if len(rest) == 0 {
		switch req.method {
		case "POST":
			var body struct {
				Title string `json:"title"`
				Body  string `json:"body"`
				Head  string `json:"head"`
				Base  string `json:"base"`
			}
			if !decodeBody(req.body, &body) {
				return errJSON(400, "malformed JSON")
			}
			if strings.TrimSpace(body.Title) == "" {
				return errJSON(422, "title is required")
			}
			headSha := r.resolveRef(body.Head)
			if headSha == "" {
				return errJSON(422, "head branch does not exist")
			}
			baseSha := r.resolveRef(body.Base)
			if baseSha == "" {
				return errJSON(422, "base branch does not exist")
			}
			for _, pr := range r.prs {
				if pr.state == "open" && pr.headRef == body.Head && pr.baseRef == body.Base {
					return errJSON(422, "a pull request already exists for these branches")
				}
			}
			pr := &pullRequest{
				id:        s.st.nextEntity,
				number:    r.nextNum,
				title:     body.Title,
				body:      body.Body,
				state:     "open",
				headRef:   body.Head,
				baseRef:   body.Base,
				headSha:   headSha,
				baseSha:   baseSha,
				user:      s.st.viewer,
				createdAt: fixedTime,
				updatedAt: fixedTime,
			}
			s.st.nextEntity++
			r.nextNum++
			r.prs[pr.number] = pr
			return jsonResp(201, mustJSON(s.prJSON(r, pr)))
		case "GET":
			state := queryFirst(req.query, "state")
			if state == "" {
				state = "open"
			}
			var list []*pullRequest
			for _, pr := range r.prs {
				if state != "all" && pr.state != state {
					continue
				}
				list = append(list, pr)
			}
			sort.Slice(list, func(i, j int) bool { return list[i].number > list[j].number })
			var out []prJSON
			for _, pr := range list {
				out = append(out, s.prJSON(r, pr))
			}
			if out == nil {
				out = []prJSON{}
			}
			perPage, page := pagination(req)
			pageItems, _ := paginate(out, perPage, page)
			return jsonResp(200, mustJSON(pageItems))
		}
		return errJSON(405, "method not allowed")
	}
	number, err := strconv.Atoi(rest[0])
	if err != nil {
		return errJSON(404, "Not Found")
	}
	pr := r.prs[number]
	if pr == nil {
		return errJSON(404, "Not Found")
	}
	if len(rest) == 2 && rest[1] == "files" {
		if req.method != "GET" {
			return errJSON(405, "method not allowed")
		}
		return jsonResp(200, mustJSON(s.prFiles(r, pr)))
	}
	if len(rest) == 2 && rest[1] == "merge" {
		if req.method != "PUT" {
			return errJSON(405, "method not allowed")
		}
		return s.prMerge(req, r, pr)
	}
	if len(rest) != 1 {
		return errJSON(404, "Not Found")
	}
	if req.method != "GET" {
		return errJSON(405, "method not allowed")
	}
	return jsonResp(200, mustJSON(s.prJSON(r, pr)))
}

type prFileJSON struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Changes   int    `json:"changes"`
}

func (s *server) prFiles(r *repo, pr *pullRequest) []prFileJSON {
	baseTree := r.flattenTree(r.commitTree(pr.baseSha))
	headTree := r.flattenTree(r.commitTree(pr.headSha))
	diff := diffFlat(baseTree, headTree)
	var out []prFileJSON
	paths := make([]string, 0, len(diff))
	for p := range diff {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		d := diff[p]
		f := prFileJSON{Filename: p}
		var oldText, newText string
		if d[0] != "" {
			if o := r.objects[d[0]]; o != nil {
				oldText = string(o.data)
			}
		}
		if d[1] != "" {
			if o := r.objects[d[1]]; o != nil {
				newText = string(o.data)
			}
		}
		add, del := lineDiff(oldText, newText)
		f.Additions, f.Deletions, f.Changes = add, del, add+del
		switch {
		case d[0] == "":
			f.Status = "added"
		case d[1] == "":
			f.Status = "removed"
		default:
			f.Status = "modified"
		}
		out = append(out, f)
	}
	if out == nil {
		out = []prFileJSON{}
	}
	return out
}

func (s *server) prMerge(req *request, r *repo, pr *pullRequest) response {
	if pr.merged {
		return errJSON(405, "pull request already merged")
	}
	// Refresh head/base tips: the PR may have been created before new pushes.
	if sha := r.resolveRef(pr.headRef); sha != "" {
		pr.headSha = sha
	}
	if sha := r.resolveRef(pr.baseRef); sha != "" {
		pr.baseSha = sha
	}
	conflict, mergedTree := r.mergeCheck(pr.baseSha, pr.headSha)
	if conflict {
		return errJSON(405, "merge conflict")
	}
	msg := "Merge pull request #" + strconv.Itoa(pr.number) + " from " + r.owner + "/" + pr.headRef
	mergeOID := r.storeObject("commit", buildCommitData(mergedTree, []string{pr.baseSha}, msg))
	r.refs["refs/heads/"+pr.baseRef] = mergeOID
	r.version++
	pr.state = "closed"
	pr.merged = true
	pr.mergedAt = fixedTime
	pr.mergeSha = mergeOID
	pr.closedAt = fixedTime
	pr.updatedAt = fixedTime
	return jsonResp(200, mustJSON(map[string]any{
		"sha": mergeOID, "merged": true, "message": msg,
	}))
}
