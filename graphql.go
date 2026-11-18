// GraphQL v4: a small parser and executor covering exactly the documents the
// suite sends (viewer, repository, issues, pull requests, mutations, node,
// __type introspection, cursor pagination, batching).
package main

import (
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Tokenizer
// ---------------------------------------------------------------------------

type gqlTok struct {
	kind string // name, string, int, float, punct, eof
	val  string
}

func tokenizeGQL(src string) ([]gqlTok, error) {
	var toks []gqlTok
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',':
			i++
		case c == '#':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '.' && i+2 < len(src) && src[i+1] == '.' && src[i+2] == '.':
			toks = append(toks, gqlTok{"punct", "..."})
			i += 3
		case c == '"':
			j := i + 1
			var sb strings.Builder
			for j < len(src) && src[j] != '"' {
				if src[j] == '\\' && j+1 < len(src) {
					j++
					switch src[j] {
					case 'n':
						sb.WriteByte('\n')
					case 't':
						sb.WriteByte('\t')
					case 'r':
						sb.WriteByte('\r')
					case 'b':
						sb.WriteByte('\b')
					case 'f':
						sb.WriteByte('\f')
					case '/':
						sb.WriteByte('/')
					case '\\':
						sb.WriteByte('\\')
					case '"':
						sb.WriteByte('"')
					case 'u':
						if j+4 < len(src) {
							if n, err := strconv.ParseInt(src[j+1:j+5], 16, 32); err == nil {
								sb.WriteRune(rune(n))
								j += 4
							}
						}
					default:
						sb.WriteByte(src[j])
					}
					j++
				} else {
					sb.WriteByte(src[j])
					j++
				}
			}
			if j >= len(src) {
				return nil, simpleError("unterminated string")
			}
			toks = append(toks, gqlTok{"string", sb.String()})
			i = j + 1
		case c == '-' || (c >= '0' && c <= '9'):
			j := i
			if src[j] == '-' {
				j++
			}
			isFloat := false
			for j < len(src) && src[j] >= '0' && src[j] <= '9' {
				j++
			}
			if j < len(src) && src[j] == '.' {
				isFloat = true
				j++
				for j < len(src) && src[j] >= '0' && src[j] <= '9' {
					j++
				}
			}
			if j < len(src) && (src[j] == 'e' || src[j] == 'E') {
				isFloat = true
				j++
				if j < len(src) && (src[j] == '+' || src[j] == '-') {
					j++
				}
				for j < len(src) && src[j] >= '0' && src[j] <= '9' {
					j++
				}
			}
			kind := "int"
			if isFloat {
				kind = "float"
			}
			toks = append(toks, gqlTok{kind, src[i:j]})
			i = j
		case c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'):
			j := i
			for j < len(src) {
				d := src[j]
				if d == '_' || (d >= 'a' && d <= 'z') || (d >= 'A' && d <= 'Z') || (d >= '0' && d <= '9') {
					j++
				} else {
					break
				}
			}
			toks = append(toks, gqlTok{"name", src[i:j]})
			i = j
		case strings.ContainsRune("!$()[]{}:=@", rune(c)):
			toks = append(toks, gqlTok{"punct", string(c)})
			i++
		default:
			return nil, simpleError("unexpected character in document")
		}
	}
	toks = append(toks, gqlTok{"eof", ""})
	return toks, nil
}

// ---------------------------------------------------------------------------
// Parser
// ---------------------------------------------------------------------------

type gqlValue struct {
	kind string // var, string, int, float, bool, null, enum, list, object
	str  string
	list []*gqlValue
	obj  map[string]*gqlValue
}

type gqlField struct {
	alias    string
	name     string
	args     map[string]*gqlValue
	sel      []*gqlField
	fragment bool
	typeCond string
}

type gqlOp struct {
	typ  string // query | mutation
	name string
	sel  []*gqlField
}

type gqlParser struct {
	toks []gqlTok
	pos  int
}

func (p *gqlParser) peek() gqlTok  { return p.toks[p.pos] }
func (p *gqlParser) next() gqlTok  { t := p.toks[p.pos]; p.pos++; return t }
func (p *gqlParser) expect(kind, val string) (gqlTok, error) {
	t := p.next()
	if t.kind != kind || (val != "" && t.val != val) {
		return t, simpleError("unexpected token " + t.val)
	}
	return t, nil
}

func parseGQLDocument(src string) (*gqlOp, error) {
	toks, err := tokenizeGQL(src)
	if err != nil {
		return nil, err
	}
	p := &gqlParser{toks: toks}
	op := &gqlOp{typ: "query"}
	t := p.peek()
	if t.kind == "name" && (t.val == "query" || t.val == "mutation") {
		op.typ = p.next().val
		if p.peek().kind == "name" {
			op.name = p.next().val
		}
	} else if t.kind == "name" && t.val == "subscription" {
		return nil, simpleError("subscriptions not supported")
	}
	if p.peek().kind == "punct" && p.peek().val == "(" {
		if err := p.parseVarDefs(); err != nil {
			return nil, err
		}
	}
	if err := p.skipDirectives(); err != nil {
		return nil, err
	}
	sel, err := p.parseSelectionSet()
	if err != nil {
		return nil, err
	}
	op.sel = sel
	return op, nil
}

func (p *gqlParser) parseVarDefs() error {
	if _, err := p.expect("punct", "("); err != nil {
		return err
	}
	for {
		t := p.next()
		if t.kind == "punct" && t.val == ")" {
			return nil
		}
		if t.kind != "punct" || t.val != "$" {
			return simpleError("bad variable definition")
		}
		if _, err := p.expect("name", ""); err != nil {
			return err
		}
		if _, err := p.expect("punct", ":"); err != nil {
			return err
		}
		if err := p.parseType(); err != nil {
			return err
		}
		if p.peek().kind == "punct" && p.peek().val == "=" {
			p.next()
			if _, err := p.parseValue(); err != nil {
				return err
			}
		}
	}
}

func (p *gqlParser) parseType() error {
	t := p.next()
	if t.kind == "punct" && t.val == "[" {
		if err := p.parseType(); err != nil {
			return err
		}
		if _, err := p.expect("punct", "]"); err != nil {
			return err
		}
	} else if t.kind != "name" {
		return simpleError("bad type")
	}
	if p.peek().kind == "punct" && p.peek().val == "!" {
		p.next()
	}
	return nil
}

func (p *gqlParser) skipDirectives() error {
	for p.peek().kind == "punct" && p.peek().val == "@" {
		p.next()
		if _, err := p.expect("name", ""); err != nil {
			return err
		}
		if p.peek().kind == "punct" && p.peek().val == "(" {
			if _, err := p.parseArguments(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *gqlParser) parseSelectionSet() ([]*gqlField, error) {
	if _, err := p.expect("punct", "{"); err != nil {
		return nil, err
	}
	var fields []*gqlField
	for {
		t := p.peek()
		if t.kind == "punct" && t.val == "}" {
			p.next()
			return fields, nil
		}
		f, err := p.parseField()
		if err != nil {
			return nil, err
		}
		fields = append(fields, f)
	}
}

func (p *gqlParser) parseField() (*gqlField, error) {
	f := &gqlField{args: map[string]*gqlValue{}}
	if p.peek().kind == "punct" && p.peek().val == "..." {
		p.next()
		f.fragment = true
		if p.peek().kind == "name" && p.peek().val == "on" {
			p.next()
			nameTok, err := p.expect("name", "")
			if err != nil {
				return nil, err
			}
			f.typeCond = nameTok.val
		} else if p.peek().kind == "name" {
			// Named fragment spread: unsupported in the suite's documents.
			return nil, simpleError("named fragment spreads not supported")
		}
		if err := p.skipDirectives(); err != nil {
			return nil, err
		}
		sel, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		f.sel = sel
		return f, nil
	}
	nameTok, err := p.expect("name", "")
	if err != nil {
		return nil, err
	}
	f.name = nameTok.val
	if p.peek().kind == "punct" && p.peek().val == ":" {
		p.next()
		aliasTok, err := p.expect("name", "")
		if err != nil {
			return nil, err
		}
		f.alias = f.name
		f.name = aliasTok.val
	}
	if p.peek().kind == "punct" && p.peek().val == "(" {
		args, err := p.parseArguments()
		if err != nil {
			return nil, err
		}
		f.args = args
	}
	if err := p.skipDirectives(); err != nil {
		return nil, err
	}
	if p.peek().kind == "punct" && p.peek().val == "{" {
		sel, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		f.sel = sel
	}
	return f, nil
}

func (p *gqlParser) parseArguments() (map[string]*gqlValue, error) {
	args := map[string]*gqlValue{}
	if _, err := p.expect("punct", "("); err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		if t.kind == "punct" && t.val == ")" {
			p.next()
			return args, nil
		}
		nameTok, err := p.expect("name", "")
		if err != nil {
			return nil, err
		}
		if _, err := p.expect("punct", ":"); err != nil {
			return nil, err
		}
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		args[nameTok.val] = v
	}
}

func (p *gqlParser) parseValue() (*gqlValue, error) {
	t := p.next()
	switch t.kind {
	case "string":
		return &gqlValue{kind: "string", str: t.val}, nil
	case "int":
		return &gqlValue{kind: "int", str: t.val}, nil
	case "float":
		return &gqlValue{kind: "float", str: t.val}, nil
	case "name":
		switch t.val {
		case "true", "false":
			return &gqlValue{kind: "bool", str: t.val}, nil
		case "null":
			return &gqlValue{kind: "null"}, nil
		default:
			return &gqlValue{kind: "enum", str: t.val}, nil
		}
	case "punct":
		switch t.val {
		case "$":
			nameTok, err := p.expect("name", "")
			if err != nil {
				return nil, err
			}
			return &gqlValue{kind: "var", str: nameTok.val}, nil
		case "[":
			v := &gqlValue{kind: "list"}
			for {
				if p.peek().kind == "punct" && p.peek().val == "]" {
					p.next()
					return v, nil
				}
				item, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				v.list = append(v.list, item)
			}
		case "{":
			v := &gqlValue{kind: "object", obj: map[string]*gqlValue{}}
			for {
				if p.peek().kind == "punct" && p.peek().val == "}" {
					p.next()
					return v, nil
				}
				nameTok, err := p.expect("name", "")
				if err != nil {
					return nil, err
				}
				if _, err := p.expect("punct", ":"); err != nil {
					return nil, err
				}
				item, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				v.obj[nameTok.val] = item
			}
		}
	}
	return nil, simpleError("bad value")
}

// ---------------------------------------------------------------------------
// Value resolution
// ---------------------------------------------------------------------------

func gqlResolveValue(v *gqlValue, vars map[string]any) any {
	if v == nil {
		return nil
	}
	switch v.kind {
	case "var":
		return vars[v.str]
	case "string", "enum":
		return v.str
	case "int":
		n, _ := strconv.Atoi(v.str)
		return n
	case "float":
		f, _ := strconv.ParseFloat(v.str, 64)
		return f
	case "bool":
		return v.str == "true"
	case "null":
		return nil
	case "list":
		out := []any{}
		for _, item := range v.list {
			out = append(out, gqlResolveValue(item, vars))
		}
		return out
	case "object":
		out := map[string]any{}
		for k, item := range v.obj {
			out[k] = gqlResolveValue(item, vars)
		}
		return out
	}
	return nil
}

func argValue(f *gqlField, name string, vars map[string]any) (any, bool) {
	v, ok := f.args[name]
	if !ok {
		return nil, false
	}
	return gqlResolveValue(v, vars), true
}

func argString(f *gqlField, name string, vars map[string]any) string {
	if v, ok := argValue(f, name, vars); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func argInt(f *gqlField, name string, vars map[string]any) int {
	if v, ok := argValue(f, name, vars); ok {
		switch n := v.(type) {
		case int:
			return n
		case float64:
			return int(n)
		case json.Number:
			i, _ := n.Int64()
			return int(i)
		}
	}
	return 0
}

func argStringList(f *gqlField, name string, vars map[string]any) []string {
	v, ok := argValue(f, name, vars)
	if !ok {
		return nil
	}
	if list, ok := v.([]any); ok {
		var out []string
		for _, item := range list {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func argObject(f *gqlField, name string, vars map[string]any) map[string]any {
	v, ok := argValue(f, name, vars)
	if !ok {
		return nil
	}
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

func jsonNumberToInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	case string:
		i, _ := strconv.Atoi(n)
		return i
	}
	return 0
}

func mapString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Executor
// ---------------------------------------------------------------------------

type gqlObj struct {
	kind string
	data any
}

type gqlIssueRef struct {
	r   *repo
	iss *issue
}

type gqlPRRef struct {
	r  *repo
	pr *pullRequest
}

type gqlCommentRef struct {
	r      *repo
	issNum int
	c      *comment
}

type gqlIssueConn struct {
	r         *repo
	issues    []*issue
	hasNext   bool
	endCursor string
}

type gqlPRConn struct {
	r   *repo
	prs []*pullRequest
}

type gqlPRFilesConn struct {
	paths []string
}

type gqlPageInfo struct {
	hasNext   bool
	endCursor string
}

func (s *server) dispatchGraphQL(req *request) response {
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	var body any
	dec := json.NewDecoder(strings.NewReader(string(req.body)))
	dec.UseNumber()
	if err := dec.Decode(&body); err != nil {
		return errJSON(400, "malformed JSON")
	}
	// Batching: a JSON array of operations.
	if arr, ok := body.([]any); ok {
		var out []any
		for _, item := range arr {
			out = append(out, s.execGraphQLOp(item))
		}
		return jsonResp(200, mustJSON(out))
	}
	return jsonResp(200, mustJSON(s.execGraphQLOp(body)))
}

func (s *server) execGraphQLOp(body any) map[string]any {
	m, ok := body.(map[string]any)
	if !ok {
		return map[string]any{"data": nil, "errors": []map[string]any{{"message": "invalid request"}}}
	}
	query, _ := m["query"].(string)
	variables, _ := m["variables"].(map[string]any)
	if variables == nil {
		variables = map[string]any{}
	}
	op, err := parseGQLDocument(query)
	if err != nil {
		return map[string]any{"data": nil, "errors": []map[string]any{{"message": "syntax error: " + err.Error()}}}
	}
	rootKind := "Query"
	if op.typ == "mutation" {
		rootKind = "Mutation"
	}
	data, errs := s.execSelection(gqlObj{kind: rootKind}, op.sel, variables)
	if len(errs) > 0 {
		return map[string]any{"data": nil, "errors": errs}
	}
	return map[string]any{"data": data}
}

func gqlErr(msg string) []map[string]any {
	return []map[string]any{{"message": msg}}
}

// execSelection executes a selection set against an object, returning the
// response map and any errors. On error the caller returns data: null.
func (s *server) execSelection(obj gqlObj, sel []*gqlField, vars map[string]any) (map[string]any, []map[string]any) {
	out := map[string]any{}
	for _, f := range sel {
		if f.fragment {
			if f.typeCond != "" && f.typeCond != obj.kind {
				continue
			}
			sub, errs := s.execSelection(obj, f.sel, vars)
			if len(errs) > 0 {
				return nil, errs
			}
			for k, v := range sub {
				out[k] = v
			}
			continue
		}
		key := f.name
		if f.alias != "" {
			key = f.alias
		}
		val, errs := s.resolveField(obj, f, vars)
		if len(errs) > 0 {
			return nil, errs
		}
		if len(f.sel) > 0 {
			switch v := val.(type) {
			case gqlObj:
				sub, errs := s.execSelection(v, f.sel, vars)
				if len(errs) > 0 {
					return nil, errs
				}
				out[key] = sub
			case []gqlObj:
				arr := []any{}
				for _, item := range v {
					sub, errs := s.execSelection(item, f.sel, vars)
					if len(errs) > 0 {
						return nil, errs
					}
					arr = append(arr, sub)
				}
				out[key] = arr
			case nil:
				out[key] = nil
			default:
				out[key] = val
			}
		} else {
			out[key] = val
		}
	}
	return out, nil
}

func fieldErr(kind, name string) []map[string]any {
	return gqlErr("Cannot query field \"" + name + "\" on type \"" + kind + "\".")
}

// resolveField resolves one field on an object (no sub-selections applied).
func (s *server) resolveField(obj gqlObj, f *gqlField, vars map[string]any) (any, []map[string]any) {
	switch obj.kind {
	case "Query":
		return s.resolveQuery(obj, f, vars)
	case "Mutation":
		return s.resolveMutation(obj, f, vars)
	case "User":
		login := obj.data.(string)
		switch f.name {
		case "login":
			return login, nil
		case "id":
			return 1000, nil
		case "name":
			return login, nil
		}
		return nil, fieldErr("User", f.name)
	case "Repository":
		return s.resolveRepository(obj.data.(*repo), f, vars)
	case "Issue":
		ref := obj.data.(gqlIssueRef)
		switch f.name {
		case "id":
			return globalID("I", ref.r.owner+"/"+ref.r.name+"/"+strconv.Itoa(ref.iss.number)), nil
		case "number":
			return ref.iss.number, nil
		case "title":
			return ref.iss.title, nil
		case "state":
			if ref.iss.state == "closed" {
				return "CLOSED", nil
			}
			return "OPEN", nil
		case "body":
			return ref.iss.body, nil
		}
		return nil, fieldErr("Issue", f.name)
	case "IssueConnection":
		conn := obj.data.(*gqlIssueConn)
		switch f.name {
		case "nodes":
			var out []gqlObj
			for _, iss := range conn.issues {
				out = append(out, gqlObj{kind: "Issue", data: gqlIssueRef{r: conn.r, iss: iss}})
			}
			return out, nil
		case "pageInfo":
			return gqlObj{kind: "PageInfo", data: &gqlPageInfo{hasNext: conn.hasNext, endCursor: conn.endCursor}}, nil
		}
		return nil, fieldErr("IssueConnection", f.name)
	case "PageInfo":
		pi := obj.data.(*gqlPageInfo)
		switch f.name {
		case "hasNextPage":
			return pi.hasNext, nil
		case "endCursor":
			if pi.endCursor == "" {
				return nil, nil
			}
			return pi.endCursor, nil
		}
		return nil, fieldErr("PageInfo", f.name)
	case "PullRequest":
		ref := obj.data.(gqlPRRef)
		switch f.name {
		case "id":
			return globalID("P", ref.r.owner+"/"+ref.r.name+"/"+strconv.Itoa(ref.pr.number)), nil
		case "number":
			return ref.pr.number, nil
		case "title":
			return ref.pr.title, nil
		case "body":
			return ref.pr.body, nil
		case "state":
			if ref.pr.merged {
				return "MERGED", nil
			}
			if ref.pr.state == "closed" {
				return "CLOSED", nil
			}
			return "OPEN", nil
		case "merged":
			return ref.pr.merged, nil
		case "headRefName":
			return ref.pr.headRef, nil
		case "baseRefName":
			return ref.pr.baseRef, nil
		case "mergeable":
			conflict, _ := ref.r.mergeCheck(ref.pr.baseSha, ref.pr.headSha)
			if conflict {
				return "CONFLICTING", nil
			}
			return "MERGEABLE", nil
		case "files":
			first := argInt(f, "first", vars)
			paths := []string{}
			for _, pf := range s.prFiles(ref.r, ref.pr) {
				paths = append(paths, pf.Filename)
			}
			if first > 0 && len(paths) > first {
				paths = paths[:first]
			}
			return gqlObj{kind: "PullRequestChangedFileConnection", data: &gqlPRFilesConn{paths: paths}}, nil
		}
		return nil, fieldErr("PullRequest", f.name)
	case "PullRequestConnection":
		conn := obj.data.(*gqlPRConn)
		switch f.name {
		case "nodes":
			var out []gqlObj
			for _, pr := range conn.prs {
				out = append(out, gqlObj{kind: "PullRequest", data: gqlPRRef{r: conn.r, pr: pr}})
			}
			return out, nil
		}
		return nil, fieldErr("PullRequestConnection", f.name)
	case "PullRequestChangedFileConnection":
		conn := obj.data.(*gqlPRFilesConn)
		switch f.name {
		case "nodes":
			var out []gqlObj
			for _, p := range conn.paths {
				out = append(out, gqlObj{kind: "PullRequestChangedFile", data: p})
			}
			return out, nil
		}
		return nil, fieldErr("PullRequestChangedFileConnection", f.name)
	case "PullRequestChangedFile":
		if f.name == "path" {
			return obj.data.(string), nil
		}
		return nil, fieldErr("PullRequestChangedFile", f.name)
	case "Ref":
		if f.name == "name" {
			return obj.data.(string), nil
		}
		return nil, fieldErr("Ref", f.name)
	case "__Type":
		name := obj.data.(string)
		switch f.name {
		case "name":
			return name, nil
		case "fields":
			var out []gqlObj
			for _, fn := range typeFields(name) {
				out = append(out, gqlObj{kind: "__Field", data: fn})
			}
			return out, nil
		case "enumValues":
			var out []gqlObj
			for _, ev := range typeEnumValues(name) {
				out = append(out, gqlObj{kind: "__EnumValue", data: ev})
			}
			return out, nil
		}
		return nil, fieldErr("__Type", f.name)
	case "__Field":
		if f.name == "name" {
			return obj.data.(string), nil
		}
		return nil, fieldErr("__Field", f.name)
	case "__EnumValue":
		if f.name == "name" {
			return obj.data.(string), nil
		}
		return nil, fieldErr("__EnumValue", f.name)
	case "Map":
		m := obj.data.(map[string]any)
		if v, ok := m[f.name]; ok {
			return v, nil
		}
		return nil, fieldErr("Map", f.name)
	case "CommentEdge":
		if f.name == "node" {
			return gqlObj{kind: "Comment", data: obj.data.(gqlCommentRef)}, nil
		}
		return nil, fieldErr("CommentEdge", f.name)
	case "Comment":
		ref := obj.data.(gqlCommentRef)
		switch f.name {
		case "id":
			return globalID("C", ref.r.owner+"/"+ref.r.name+"/"+strconv.Itoa(ref.issNum)+"/"+strconv.Itoa(ref.c.id)), nil
		case "body":
			return ref.c.body, nil
		}
		return nil, fieldErr("Comment", f.name)
	}
	return nil, fieldErr(obj.kind, f.name)
}

// typeFields lists the fields __type must report for a type.
func typeFields(name string) []string {
	switch name {
	case "Issue":
		return []string{"id", "number", "title", "state", "body", "url", "createdAt", "updatedAt", "author"}
	case "PullRequest":
		return []string{"id", "number", "headRefName", "baseRefName", "mergeable", "state", "merged", "files", "title", "body", "url", "createdAt", "updatedAt"}
	case "Repository":
		return []string{"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "description", "url", "createdAt", "updatedAt"}
	}
	return []string{}
}

func typeEnumValues(name string) []string {
	if name == "SearchType" {
		return []string{"ISSUE", "PULL_REQUEST", "REPOSITORY", "USER", "DISCUSSION", "COMMIT"}
	}
	return []string{}
}

func knownType(name string) bool {
	switch name {
	case "Issue", "PullRequest", "Repository", "SearchType", "User":
		return true
	}
	return false
}

func (s *server) resolveQuery(obj gqlObj, f *gqlField, vars map[string]any) (any, []map[string]any) {
	switch f.name {
	case "viewer":
		return gqlObj{kind: "User", data: s.st.viewer}, nil
	case "repository":
		owner := argString(f, "owner", vars)
		name := argString(f, "name", vars)
		r := s.st.getRepo(owner, name)
		if r == nil {
			return nil, nil
		}
		return gqlObj{kind: "Repository", data: r}, nil
	case "node":
		id := argString(f, "id", vars)
		return s.resolveNode(id), nil
	case "__type":
		name := argString(f, "name", vars)
		if !knownType(name) {
			return nil, nil
		}
		return gqlObj{kind: "__Type", data: name}, nil
	}
	return nil, fieldErr("Query", f.name)
}

// resolveNode decodes a global id back to its object.
func (s *server) resolveNode(id string) any {
	raw, err := base64.StdEncoding.DecodeString(id)
	if err != nil {
		return nil
	}
	parts := strings.SplitN(string(raw), ":", 2)
	if len(parts) != 2 {
		return nil
	}
	rest := parts[1]
	switch parts[0] {
	case "R":
		seg := strings.SplitN(rest, "/", 2)
		if len(seg) != 2 {
			return nil
		}
		if r := s.st.getRepo(seg[0], seg[1]); r != nil {
			return gqlObj{kind: "Repository", data: r}
		}
	case "I":
		seg := strings.Split(rest, "/")
		if len(seg) != 3 {
			return nil
		}
		if r := s.st.getRepo(seg[0], seg[1]); r != nil {
			if iss := r.issues[jsonNumberToInt(seg[2])]; iss != nil {
				return gqlObj{kind: "Issue", data: gqlIssueRef{r: r, iss: iss}}
			}
		}
	case "P":
		seg := strings.Split(rest, "/")
		if len(seg) != 3 {
			return nil
		}
		if r := s.st.getRepo(seg[0], seg[1]); r != nil {
			if pr := r.prs[jsonNumberToInt(seg[2])]; pr != nil {
				return gqlObj{kind: "PullRequest", data: gqlPRRef{r: r, pr: pr}}
			}
		}
	}
	return nil
}

func (s *server) resolveRepository(r *repo, f *gqlField, vars map[string]any) (any, []map[string]any) {
	switch f.name {
	case "id":
		return globalID("R", r.owner+"/"+r.name), nil
	case "databaseId":
		return r.id, nil
	case "name":
		return r.name, nil
	case "nameWithOwner":
		return r.owner + "/" + r.name, nil
	case "owner":
		return gqlObj{kind: "User", data: r.owner}, nil
	case "defaultBranchRef":
		return gqlObj{kind: "Ref", data: r.defaultBranch}, nil
	case "viewerPermission":
		return "ADMIN", nil
	case "hasIssuesEnabled":
		return true, nil
	case "issue":
		number := argInt(f, "number", vars)
		if iss := r.issues[number]; iss != nil {
			return gqlObj{kind: "Issue", data: gqlIssueRef{r: r, iss: iss}}, nil
		}
		return nil, nil
	case "issues":
		return gqlObj{kind: "IssueConnection", data: s.issueConnection(r, f, vars)}, nil
	case "pullRequests":
		first := argInt(f, "first", vars)
		var prs []*pullRequest
		for _, pr := range r.prs {
			prs = append(prs, pr)
		}
		sortPullRequests(prs)
		if first > 0 && len(prs) > first {
			prs = prs[:first]
		}
		return gqlObj{kind: "PullRequestConnection", data: &gqlPRConn{r: r, prs: prs}}, nil
	case "pullRequest":
		number := argInt(f, "number", vars)
		if pr := r.prs[number]; pr != nil {
			return gqlObj{kind: "PullRequest", data: gqlPRRef{r: r, pr: pr}}, nil
		}
		return nil, nil
	}
	return nil, fieldErr("Repository", f.name)
}

func sortPullRequests(prs []*pullRequest) {
	for i := 1; i < len(prs); i++ {
		for j := i; j > 0 && prs[j].number < prs[j-1].number; j-- {
			prs[j], prs[j-1] = prs[j-1], prs[j]
		}
	}
}

// issueConnection applies states/orderBy/first/after to the repo's issues.
func (s *server) issueConnection(r *repo, f *gqlField, vars map[string]any) *gqlIssueConn {
	states := argStringList(f, "states", vars)
	first := argInt(f, "first", vars)
	after := argString(f, "after", vars)
	orderBy, _ := argValue(f, "orderBy", vars)
	desc := false
	if m, ok := orderBy.(map[string]any); ok {
		if d, ok := m["direction"].(string); ok && d == "DESC" {
			desc = true
		}
	}
	var list []*issue
	for _, iss := range r.issues {
		if len(states) > 0 {
			match := false
			for _, st := range states {
				if (st == "OPEN" && iss.state == "open") || (st == "CLOSED" && iss.state == "closed") {
					match = true
				}
			}
			if !match {
				continue
			}
		}
		list = append(list, iss)
	}
	// Creation order is the number; sort ascending, reverse for DESC.
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j].number < list[j-1].number; j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
	if desc {
		for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
			list[i], list[j] = list[j], list[i]
		}
	}
	// Cursor: base64("c:<number>"); start after it.
	start := 0
	if after != "" {
		if raw, err := base64.StdEncoding.DecodeString(after); err == nil {
			if strings.HasPrefix(string(raw), "c:") {
				if n, err := strconv.Atoi(string(raw)[2:]); err == nil {
					for i, iss := range list {
						if iss.number == n {
							start = i + 1
							break
						}
					}
				}
			}
		}
	}
	list = list[start:]
	hasNext := false
	endCursor := ""
	if first > 0 && len(list) > first {
		hasNext = true
		list = list[:first]
	}
	if len(list) > 0 {
		endCursor = base64.StdEncoding.EncodeToString([]byte("c:" + strconv.Itoa(list[len(list)-1].number)))
	}
	return &gqlIssueConn{r: r, issues: list, hasNext: hasNext, endCursor: endCursor}
}

// ---------------------------------------------------------------------------
// Mutations
// ---------------------------------------------------------------------------

func (s *server) resolveMutation(obj gqlObj, f *gqlField, vars map[string]any) (any, []map[string]any) {
	switch f.name {
	case "createIssue":
		input := argObject(f, "input", vars)
		repoID := mapString(input, "repositoryId")
		title := mapString(input, "title")
		body := mapString(input, "body")
		r, ok := s.decodeRepoID(repoID)
		if !ok || r == nil {
			return nil, gqlErr("repository not found")
		}
		if strings.TrimSpace(title) == "" {
			return nil, gqlErr("title is required")
		}
		iss := &issue{
			id: s.st.nextEntity, number: r.nextNum, title: title, body: body,
			state: "open", user: s.st.viewer, createdAt: fixedTime, updatedAt: fixedTime,
		}
		s.st.nextEntity++
		r.nextNum++
		r.issues[iss.number] = iss
		return gqlObj{kind: "Map", data: map[string]any{
			"issue": gqlObj{kind: "Issue", data: gqlIssueRef{r: r, iss: iss}},
		}}, nil
	case "closeIssue":
		input := argObject(f, "input", vars)
		issueID := mapString(input, "issueId")
		ref, ok := s.decodeIssueID(issueID)
		if !ok {
			return nil, gqlErr("issue not found")
		}
		ref.iss.state = "closed"
		ref.iss.closedAt = fixedTime
		ref.iss.updatedAt = fixedTime
		ref.r.invalidateIssueCache(ref.iss.number)
		return gqlObj{kind: "Map", data: map[string]any{
			"issue": gqlObj{kind: "Issue", data: ref},
		}}, nil
	case "createPullRequest":
		input := argObject(f, "input", vars)
		repoID := mapString(input, "repositoryId")
		baseRef := mapString(input, "baseRefName")
		headRef := mapString(input, "headRefName")
		title := mapString(input, "title")
		body := mapString(input, "body")
		r, ok := s.decodeRepoID(repoID)
		if !ok || r == nil {
			return nil, gqlErr("repository not found")
		}
		headSha := r.resolveRef(headRef)
		if headSha == "" {
			return nil, gqlErr("head branch does not exist")
		}
		baseSha := r.resolveRef(baseRef)
		if baseSha == "" {
			return nil, gqlErr("base branch does not exist")
		}
		for _, pr := range r.prs {
			if pr.state == "open" && pr.headRef == headRef && pr.baseRef == baseRef {
				return nil, gqlErr("a pull request already exists for these branches")
			}
		}
		pr := &pullRequest{
			id: s.st.nextEntity, number: r.nextNum, title: title, body: body,
			state: "open", headRef: headRef, baseRef: baseRef,
			headSha: headSha, baseSha: baseSha,
			user: s.st.viewer, createdAt: fixedTime, updatedAt: fixedTime,
		}
		s.st.nextEntity++
		r.nextNum++
		r.prs[pr.number] = pr
		return gqlObj{kind: "Map", data: map[string]any{
			"pullRequest": gqlObj{kind: "PullRequest", data: gqlPRRef{r: r, pr: pr}},
		}}, nil
	case "mergePullRequest":
		input := argObject(f, "input", vars)
		prID := mapString(input, "pullRequestId")
		ref, ok := s.decodePRID(prID)
		if !ok {
			return nil, gqlErr("pull request not found")
		}
		if ref.pr.merged {
			return nil, gqlErr("pull request already merged")
		}
		if sha := ref.r.resolveRef(ref.pr.headRef); sha != "" {
			ref.pr.headSha = sha
		}
		if sha := ref.r.resolveRef(ref.pr.baseRef); sha != "" {
			ref.pr.baseSha = sha
		}
		conflict, mergedTree := ref.r.mergeCheck(ref.pr.baseSha, ref.pr.headSha)
		if conflict {
			return nil, gqlErr("merge conflict")
		}
		msg := "Merge pull request #" + strconv.Itoa(ref.pr.number) + " from " + ref.r.owner + "/" + ref.pr.headRef
		mergeOID := ref.r.storeObject("commit", buildCommitData(mergedTree, []string{ref.pr.baseSha}, msg))
		ref.r.refs["refs/heads/"+ref.pr.baseRef] = mergeOID
		ref.r.version++
		ref.pr.state = "closed"
		ref.pr.merged = true
		ref.pr.mergedAt = fixedTime
		ref.pr.mergeSha = mergeOID
		ref.pr.closedAt = fixedTime
		ref.pr.updatedAt = fixedTime
		return gqlObj{kind: "Map", data: map[string]any{
			"pullRequest": gqlObj{kind: "PullRequest", data: ref},
		}}, nil
	case "addComment":
		input := argObject(f, "input", vars)
		subjectID := mapString(input, "subjectId")
		body := mapString(input, "body")
		ref, ok := s.decodeIssueID(subjectID)
		if !ok {
			return nil, gqlErr("subject not found")
		}
		c := &comment{id: ref.r.nextComment, body: body, user: s.st.viewer}
		ref.r.nextComment++
		ref.iss.comments = append(ref.iss.comments, c)
		ref.r.invalidateIssueCache(ref.iss.number)
		return gqlObj{kind: "Map", data: map[string]any{
			"commentEdge": gqlObj{kind: "CommentEdge", data: gqlCommentRef{r: ref.r, issNum: ref.iss.number, c: c}},
		}}, nil
	}
	return nil, fieldErr("Mutation", f.name)
}

func (s *server) decodeRepoID(id string) (*repo, bool) {
	raw, err := base64.StdEncoding.DecodeString(id)
	if err != nil {
		return nil, false
	}
	parts := strings.SplitN(string(raw), ":", 2)
	if len(parts) != 2 || parts[0] != "R" {
		return nil, false
	}
	seg := strings.SplitN(parts[1], "/", 2)
	if len(seg) != 2 {
		return nil, false
	}
	r := s.st.getRepo(seg[0], seg[1])
	if r == nil {
		return nil, false
	}
	return r, true
}

func (s *server) decodeIssueID(id string) (gqlIssueRef, bool) {
	raw, err := base64.StdEncoding.DecodeString(id)
	if err != nil {
		return gqlIssueRef{}, false
	}
	parts := strings.SplitN(string(raw), ":", 2)
	if len(parts) != 2 || parts[0] != "I" {
		return gqlIssueRef{}, false
	}
	seg := strings.Split(parts[1], "/")
	if len(seg) != 3 {
		return gqlIssueRef{}, false
	}
	r := s.st.getRepo(seg[0], seg[1])
	if r == nil {
		return gqlIssueRef{}, false
	}
	iss := r.issues[jsonNumberToInt(seg[2])]
	if iss == nil {
		return gqlIssueRef{}, false
	}
	return gqlIssueRef{r: r, iss: iss}, true
}

func (s *server) decodePRID(id string) (gqlPRRef, bool) {
	raw, err := base64.StdEncoding.DecodeString(id)
	if err != nil {
		return gqlPRRef{}, false
	}
	parts := strings.SplitN(string(raw), ":", 2)
	if len(parts) != 2 || parts[0] != "P" {
		return gqlPRRef{}, false
	}
	seg := strings.Split(parts[1], "/")
	if len(seg) != 3 {
		return gqlPRRef{}, false
	}
	r := s.st.getRepo(seg[0], seg[1])
	if r == nil {
		return gqlPRRef{}, false
	}
	pr := r.prs[jsonNumberToInt(seg[2])]
	if pr == nil {
		return gqlPRRef{}, false
	}
	return gqlPRRef{r: r, pr: pr}, true
}
