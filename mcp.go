// MCP (Model Context Protocol), Streamable HTTP, JSON-RPC 2.0.
// Modern era 2026-07-28 with a legacy initialize fallback.
package main

import (
	"encoding/json"
	"strconv"
	"strings"
)

type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func mcpTools() []mcpTool {
	obj := func(props map[string]any, required []string) map[string]any {
		return map[string]any{"type": "object", "properties": props, "required": required}
	}
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	boolean := func(desc string) map[string]any { return map[string]any{"type": "boolean", "description": desc} }
	return []mcpTool{
		{
			Name:        "create_repo",
			Description: "Create a repository owned by the viewer.",
			InputSchema: obj(map[string]any{
				"name":        str("Repository name"),
				"description": str("Repository description"),
				"private":     boolean("Whether the repository is private"),
			}, []string{"name"}),
		},
		{
			Name:        "get_repo",
			Description: "Get a repository by owner and name.",
			InputSchema: obj(map[string]any{
				"owner":     str("Repository owner login"),
				"repo":      str("Repository name"),
				"full_name": str("Full repository name owner/repo"),
			}, []string{"owner", "repo"}),
		},
		{
			Name:        "create_issue",
			Description: "Create an issue in a repository.",
			InputSchema: obj(map[string]any{
				"owner":     str("Repository owner login"),
				"repo":      str("Repository name"),
				"full_name": str("Full repository name owner/repo"),
				"title":     str("Issue title"),
				"body":      str("Issue body"),
			}, []string{"owner", "repo", "title"}),
		},
		{
			Name:        "list_issues",
			Description: "List issues in a repository.",
			InputSchema: obj(map[string]any{
				"owner":     str("Repository owner login"),
				"repo":      str("Repository name"),
				"full_name": str("Full repository name owner/repo"),
				"state":     str("Issue state: open, closed, or all"),
			}, []string{"owner", "repo"}),
		},
		{
			Name:        "create_pull_request",
			Description: "Create a pull request in a repository.",
			InputSchema: obj(map[string]any{
				"owner":     str("Repository owner login"),
				"repo":      str("Repository name"),
				"full_name": str("Full repository name owner/repo"),
				"title":     str("Pull request title"),
				"body":      str("Pull request body"),
				"head":      str("Head branch name"),
				"base":      str("Base branch name"),
			}, []string{"owner", "repo", "title", "head", "base"}),
		},
		{
			Name:        "merge_pull_request",
			Description: "Merge a pull request by number (squash).",
			InputSchema: obj(map[string]any{
				"owner":       str("Repository owner login"),
				"repo":        str("Repository name"),
				"full_name":   str("Full repository name owner/repo"),
				"pull_number": map[string]any{"type": "integer", "description": "Pull request number"},
				"number":      map[string]any{"type": "integer", "description": "Pull request number"},
			}, []string{"owner", "repo", "pull_number"}),
		},
	}
}

func (s *server) dispatchMCP(req *request) response {
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	var body map[string]any
	dec := json.NewDecoder(strings.NewReader(string(req.body)))
	dec.UseNumber()
	if err := dec.Decode(&body); err != nil {
		return response{
			status:      400,
			contentType: "application/json",
			body:        []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"Parse error"}}`),
		}
	}
	method, _ := body["method"].(string)
	id, hasID := body["id"]
	params, _ := body["params"].(map[string]any)
	if params == nil {
		params = map[string]any{}
	}
	// Notifications (no id) get HTTP 202 with an empty body.
	if !hasID {
		if method == "notifications/initialized" {
			return response{status: 202, contentType: "application/json", body: nil}
		}
		return response{status: 202, contentType: "application/json", body: nil}
	}
	ok := func(result any) response {
		return jsonResp(200, mustJSON(map[string]any{
			"jsonrpc": "2.0", "id": id, "result": result,
		}))
	}
	rpcErr := func(code int, message string) response {
		return jsonResp(200, mustJSON(map[string]any{
			"jsonrpc": "2.0", "id": id,
			"error": map[string]any{"code": code, "message": message},
		}))
	}
	switch method {
	case "server/discover":
		return ok(map[string]any{
			"supportedVersions": []string{"2026-07-28", "2025-11-25"},
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"serverInfo":        map[string]any{"name": "forge", "version": "1.0.0"},
			"instructions":      "A GitHub-compatible git forge: repositories, issues, pull requests, git smart HTTP, GraphQL and MCP tools.",
			"_meta": map[string]any{
				"io.modelcontextprotocol/serverInfo": map[string]any{"name": "forge", "version": "1.0.0"},
			},
		})
	case "initialize":
		version := "2025-11-25"
		if params != nil {
			if pv, ok := params["protocolVersion"].(string); ok && pv != "" {
				version = pv
			}
		}
		return ok(map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "forge", "version": "1.0.0"},
		})
	case "ping":
		return ok(map[string]any{})
	case "tools/list":
		tools := mcpTools()
		var out []any
		for _, t := range tools {
			out = append(out, map[string]any{
				"name": t.Name, "description": t.Description, "inputSchema": t.InputSchema,
			})
		}
		return ok(map[string]any{"tools": out})
	case "tools/call":
		name, _ := params["name"].(string)
		args, _ := params["arguments"].(map[string]any)
		if args == nil {
			args = map[string]any{}
		}
		return s.mcpCall(id, name, args)
	}
	return rpcErr(-32601, "method not found: "+method)
}

func mcpTextResult(id any, structured any) response {
	text, _ := json.Marshal(structured)
	return jsonResp(200, mustJSON(map[string]any{
		"jsonrpc": "2.0", "id": id,
		"result": map[string]any{
			"content":          []any{map[string]any{"type": "text", "text": string(text)}},
			"structuredContent": structured,
			"isError":          false,
		},
	}))
}

func mcpErrorResult(id any, message string) response {
	return jsonResp(200, mustJSON(map[string]any{
		"jsonrpc": "2.0", "id": id,
		"result": map[string]any{
			"content":          []any{map[string]any{"type": "text", "text": message}},
			"structuredContent": map[string]any{"error": message},
			"isError":          true,
		},
	}))
}

func mcpRPCError(id any, code int, message string) response {
	return jsonResp(200, mustJSON(map[string]any{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": code, "message": message},
	}))
}

// mcpOwnerRepo resolves owner/repo from tool arguments, accepting full_name.
func mcpOwnerRepo(args map[string]any) (owner, repoName string, ok bool) {
	owner, _ = args["owner"].(string)
	repoName, _ = args["repo"].(string)
	if full, _ := args["full_name"].(string); full != "" {
		if owner == "" || repoName == "" {
			parts := strings.SplitN(full, "/", 2)
			if len(parts) == 2 {
				if owner == "" {
					owner = parts[0]
				}
				if repoName == "" {
					repoName = parts[1]
				}
			}
		}
	}
	return owner, repoName, owner != "" && repoName != ""
}

func mcpArgString(args map[string]any, key string) string {
	if v, ok := args[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func mcpArgInt(args map[string]any, keys ...string) (int, bool) {
	for _, key := range keys {
		if v, ok := args[key]; ok {
			switch n := v.(type) {
			case json.Number:
				i, err := n.Int64()
				if err == nil {
					return int(i), true
				}
			case float64:
				return int(n), true
			case string:
				if i, err := strconv.Atoi(n); err == nil {
					return i, true
				}
			}
		}
	}
	return 0, false
}

func (s *server) mcpCall(id any, name string, args map[string]any) response {
	switch name {
	case "create_repo":
		repoName := mcpArgString(args, "name")
		if repoName == "" {
			return mcpRPCError(id, -32602, "missing required argument: name")
		}
		if s.st.getRepo(s.st.viewer, repoName) != nil {
			return mcpErrorResult(id, "Repository "+repoName+" already exists")
		}
		r := s.st.createRepo(s.st.viewer, repoName, mcpArgBool(args, "private"), mcpArgString(args, "description"))
		return mcpTextResult(id, s.repoJSON(r))
	case "get_repo":
		owner, repoName, ok := mcpOwnerRepo(args)
		if !ok {
			return mcpRPCError(id, -32602, "missing required arguments: owner, repo")
		}
		r := s.st.getRepo(owner, repoName)
		if r == nil {
			return mcpErrorResult(id, "repository not found")
		}
		return mcpTextResult(id, s.repoJSON(r))
	case "create_issue":
		owner, repoName, ok := mcpOwnerRepo(args)
		title := mcpArgString(args, "title")
		if !ok || title == "" {
			return mcpRPCError(id, -32602, "missing required arguments: owner, repo, title")
		}
		r := s.st.getRepo(owner, repoName)
		if r == nil {
			return mcpErrorResult(id, "repository not found")
		}
		iss := &issue{
			id: s.st.nextEntity, number: r.nextNum, title: title,
			body: mcpArgString(args, "body"), state: "open",
			user: s.st.viewer, createdAt: fixedTime, updatedAt: fixedTime,
		}
		s.st.nextEntity++
		r.nextNum++
		r.issues[iss.number] = iss
		return mcpTextResult(id, s.issueJSON(r, iss))
	case "list_issues":
		owner, repoName, ok := mcpOwnerRepo(args)
		if !ok {
			return mcpRPCError(id, -32602, "missing required arguments: owner, repo")
		}
		r := s.st.getRepo(owner, repoName)
		if r == nil {
			return mcpErrorResult(id, "repository not found")
		}
		state := mcpArgString(args, "state")
		if state == "" {
			state = "open"
		}
		var list []issueJSON
		nums := []int{}
		for n := range r.issues {
			nums = append(nums, n)
		}
		sortInts(nums)
		for _, n := range nums {
			iss := r.issues[n]
			if state != "all" && iss.state != state {
				continue
			}
			list = append(list, s.issueJSON(r, iss))
		}
		if list == nil {
			list = []issueJSON{}
		}
		return mcpTextResult(id, map[string]any{"issues": list})
	case "create_pull_request":
		owner, repoName, ok := mcpOwnerRepo(args)
		title := mcpArgString(args, "title")
		head := mcpArgString(args, "head")
		base := mcpArgString(args, "base")
		if !ok || title == "" || head == "" || base == "" {
			return mcpRPCError(id, -32602, "missing required arguments: owner, repo, title, head, base")
		}
		r := s.st.getRepo(owner, repoName)
		if r == nil {
			return mcpErrorResult(id, "repository not found")
		}
		headSha := r.resolveRef(head)
		if headSha == "" {
			return mcpErrorResult(id, "head branch does not exist")
		}
		baseSha := r.resolveRef(base)
		if baseSha == "" {
			return mcpErrorResult(id, "base branch does not exist")
		}
		for _, pr := range r.prs {
			if pr.state == "open" && pr.headRef == head && pr.baseRef == base {
				return mcpErrorResult(id, "a pull request already exists for these branches")
			}
		}
		pr := &pullRequest{
			id: s.st.nextEntity, number: r.nextNum, title: title,
			body: mcpArgString(args, "body"), state: "open",
			headRef: head, baseRef: base, headSha: headSha, baseSha: baseSha,
			user: s.st.viewer, createdAt: fixedTime, updatedAt: fixedTime,
		}
		s.st.nextEntity++
		r.nextNum++
		r.prs[pr.number] = pr
		return mcpTextResult(id, s.prJSON(r, pr))
	case "merge_pull_request":
		owner, repoName, ok := mcpOwnerRepo(args)
		number, hasNumber := mcpArgInt(args, "pull_number", "number")
		if !ok || !hasNumber {
			return mcpRPCError(id, -32602, "missing required arguments: owner, repo, pull_number")
		}
		r := s.st.getRepo(owner, repoName)
		if r == nil {
			return mcpErrorResult(id, "repository not found")
		}
		pr := r.prs[number]
		if pr == nil {
			return mcpErrorResult(id, "pull request not found")
		}
		if pr.merged {
			return mcpErrorResult(id, "pull request already merged")
		}
		if sha := r.resolveRef(pr.headRef); sha != "" {
			pr.headSha = sha
		}
		if sha := r.resolveRef(pr.baseRef); sha != "" {
			pr.baseSha = sha
		}
		conflict, mergedTree := r.mergeCheck(pr.baseSha, pr.headSha)
		if conflict {
			return mcpErrorResult(id, "merge conflict")
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
		return mcpTextResult(id, map[string]any{
			"number": pr.number, "merged": true, "sha": mergeOID, "message": msg,
		})
	}
	return mcpRPCError(id, -32601, "unknown tool: "+name)
}

func mcpArgBool(args map[string]any, key string) bool {
	if v, ok := args[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return false
}

func sortInts(a []int) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}
