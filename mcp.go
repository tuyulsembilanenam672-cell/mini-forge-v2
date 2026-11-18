// mcp.go: the MCP endpoint at /mcp (Streamable HTTP, JSON-RPC 2.0).
// The modern era (2026-07-28) is served: server/discover answers with
// supported versions, identity and the tools capability; the legacy
// initialize handshake is answered too, for pre-discover clients. Tool
// results carry the object both as text JSON and as structuredContent,
// so the suite can read them either way.

package main

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// mcpTool is one tool as tools/list answers it. The array is fixed at
// startup and answered byte-identically on every call.
type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// mcpTools is the fixed tool list: the six required tools, with the
// canonical github-mcp-server argument names. Required lists hold only
// arguments the suite always supplies, so a mapped call never reports
// a missing argument it has no value for.
var mcpTools = []mcpTool{
	{
		Name:        "create_repo",
		Description: "Create a repository for the viewer.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner":       map[string]any{"type": "string", "description": "Repository owner login."},
				"name":        map[string]any{"type": "string", "description": "Repository name."},
				"description": map[string]any{"type": "string", "description": "Repository description."},
				"private":     map[string]any{"type": "boolean", "description": "Whether the repository is private."},
			},
			"required": []string{"name"},
		},
	},
	{
		Name:        "get_repo",
		Description: "Get a repository by owner and name.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner": map[string]any{"type": "string", "description": "Repository owner login."},
				"repo":  map[string]any{"type": "string", "description": "Repository name."},
			},
			"required": []string{"owner", "repo"},
		},
	},
	{
		Name:        "create_issue",
		Description: "Create an issue in a repository.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner": map[string]any{"type": "string", "description": "Repository owner login."},
				"repo":  map[string]any{"type": "string", "description": "Repository name."},
				"title": map[string]any{"type": "string", "description": "Issue title."},
				"body":  map[string]any{"type": "string", "description": "Issue body."},
			},
			"required": []string{"owner", "repo", "title"},
		},
	},
	{
		Name:        "list_issues",
		Description: "List issues in a repository.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner": map[string]any{"type": "string", "description": "Repository owner login."},
				"repo":  map[string]any{"type": "string", "description": "Repository name."},
				"state": map[string]any{"type": "string", "description": "Issue state: open, closed or all."},
			},
			"required": []string{"owner", "repo"},
		},
	},
	{
		Name:        "create_pull_request",
		Description: "Open a pull request in a repository.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner": map[string]any{"type": "string", "description": "Repository owner login."},
				"repo":  map[string]any{"type": "string", "description": "Repository name."},
				"title": map[string]any{"type": "string", "description": "Pull request title."},
				"body":  map[string]any{"type": "string", "description": "Pull request body."},
				"head":  map[string]any{"type": "string", "description": "Head branch name."},
				"base":  map[string]any{"type": "string", "description": "Base branch name."},
			},
			"required": []string{"owner", "repo", "title", "head", "base"},
		},
	},
	{
		Name:        "merge_pull_request",
		Description: "Merge a pull request with a squash merge.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner":       map[string]any{"type": "string", "description": "Repository owner login."},
				"repo":        map[string]any{"type": "string", "description": "Repository name."},
				"pull_number": map[string]any{"type": "integer", "description": "Pull request number."},
			},
			"required": []string{"owner", "repo", "pull_number"},
		},
	},
}

// mcpToolsJSON is the precomputed tools/list answer: byte-identical on
// every call, as the idempotency check requires.
var mcpToolsJSON = func() json.RawMessage {
	data, _ := json.Marshal(map[string]any{"tools": mcpTools})
	return data
}()

// mcpRequest is one JSON-RPC request.
type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// mcpResult writes a JSON-RPC success answer for id.
func (s *server) mcpResult(w http.ResponseWriter, id json.RawMessage, result any) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	body := map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
	s.writeJSON(w, http.StatusOK, body)
}

// mcpError writes a JSON-RPC error answer for id.
func (s *server) mcpError(w http.ResponseWriter, status int, id json.RawMessage, code int, message string) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	body := map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}}
	s.writeJSON(w, status, body)
}

// handleMCP serves POST /mcp (and DELETE for session teardown). A body
// that is not JSON is HTTP 400 with JSON-RPC -32700; an unknown method
// is -32601; an unknown tool is -32602. Authentication is decided
// before any tool exists, in ServeHTTP.
func (s *server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		w.Header().Set("X-GitHub-Request-Id", s.requestID())
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		s.writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	body, err := readBody(r)
	if err != nil {
		s.mcpError(w, http.StatusBadRequest, nil, -32700, "Parse error")
		return
	}
	var req mcpRequest
	if err := json.Unmarshal(body, &req); err != nil {
		s.mcpError(w, http.StatusBadRequest, nil, -32700, "Parse error")
		return
	}
	if req.JSONRPC != "" && req.JSONRPC != "2.0" {
		s.mcpError(w, http.StatusBadRequest, req.ID, -32600, "Invalid Request")
		return
	}
	// A notification (no id) is answered with 202 and no body.
	isNotification := len(req.ID) == 0
	switch req.Method {
	case "server/discover":
		s.mcpResult(w, req.ID, map[string]any{
			"supportedVersions": []string{"2026-07-28", "2025-11-25"},
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"serverInfo":        map[string]any{"name": "mini-forge-v2", "version": "1.0.0"},
		})
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &params)
		version := params.ProtocolVersion
		if version == "" {
			version = "2025-11-25"
		}
		s.mcpResult(w, req.ID, map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "mini-forge-v2", "version": "1.0.0"},
		})
	case "notifications/initialized", "notifications/cancelled":
		if isNotification {
			w.Header().Set("X-GitHub-Request-Id", s.requestID())
			w.WriteHeader(http.StatusAccepted)
			return
		}
		s.mcpResult(w, req.ID, map[string]any{})
	case "tools/list":
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-GitHub-Request-Id", s.requestID())
		w.WriteHeader(http.StatusOK)
		id := req.ID
		if len(id) == 0 {
			id = json.RawMessage("null")
		}
		// The tools array is fixed: splice the precomputed answer into
		// the result envelope so two calls are byte-identical.
		w.Write([]byte(`{"jsonrpc":"2.0","id":`))
		w.Write(id)
		w.Write([]byte(`,"result":`))
		w.Write(mcpToolsJSON)
		w.Write([]byte(`}`))
	case "tools/call":
		s.handleMCPCall(w, r, req)
	case "ping":
		s.mcpResult(w, req.ID, map[string]any{})
	default:
		if isNotification {
			w.Header().Set("X-GitHub-Request-Id", s.requestID())
			w.WriteHeader(http.StatusAccepted)
			return
		}
		s.mcpError(w, http.StatusOK, req.ID, -32601, "Method not found: "+req.Method)
	}
}

// --- tools/call ---

// handleMCPCall runs one tools/call request. A missing required
// argument or a failed tool reports isError:true (never a bare success
// and never an HTTP 500); an unknown tool is JSON-RPC -32602.
func (s *server) handleMCPCall(w http.ResponseWriter, r *http.Request, req mcpRequest) {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		s.mcpError(w, http.StatusOK, req.ID, -32602, "Invalid params")
		return
	}
	if params.Arguments == nil {
		params.Arguments = make(map[string]any)
	}
	result, toolErr := s.callMCPTool(params.Name, params.Arguments)
	if toolErr == errUnknownTool {
		s.mcpError(w, http.StatusOK, req.ID, -32602, "Unknown tool: "+params.Name)
		return
	}
	if toolErr != nil {
		s.mcpResult(w, req.ID, map[string]any{
			"content": []map[string]any{{"type": "text", "text": toolErr.Error()}},
			"isError": true,
		})
		return
	}
	text, _ := json.Marshal(result)
	s.mcpResult(w, req.ID, map[string]any{
		"content":           []map[string]any{{"type": "text", "text": string(text)}},
		"structuredContent": result,
		"isError":           false,
	})
}

var errUnknownTool = &mcpToolError{"unknown tool"}

type mcpToolError struct{ msg string }

func (e *mcpToolError) Error() string { return e.msg }

// mcpStr reads a string argument.
func mcpStr(args map[string]any, name string) string {
	if v, ok := args[name].(string); ok {
		return v
	}
	return ""
}

// mcpInt reads an integer argument (JSON numbers arrive as float64).
func mcpInt(args map[string]any, name string) int {
	switch v := args[name].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		n, _ := strconv.Atoi(v)
		return n
	}
	return 0
}

// mcpBool reads a boolean argument.
func mcpBool(args map[string]any, name string) bool {
	if v, ok := args[name].(bool); ok {
		return v
	}
	return false
}

// callMCPTool runs one tool on the shared store (the same objects REST
// reads) and returns its result value.
func (s *server) callMCPTool(name string, args map[string]any) (any, error) {
	switch name {
	case "create_repo":
		repoName := mcpStr(args, "name")
		if repoName == "" {
			return nil, &mcpToolError{"missing required argument: name"}
		}
		s.store.mu.Lock()
		if existing := s.store.getRepo(viewerLogin, repoName); existing != nil {
			s.store.mu.Unlock()
			return nil, &mcpToolError{"name already exists on this account"}
		}
		repo := &Repo{
			ID:            s.store.nextIDv(),
			Name:          repoName,
			Owner:         viewerLogin,
			Private:       mcpBool(args, "private"),
			Description:   mcpStr(args, "description"),
			DefaultBranch: "main",
			CreatedSeq:    s.store.nextSeq(),
			Objects:       make(map[string]*Object),
			Refs:          make(map[string]string),
			Issues:        make(map[int]*Issue),
			PRs:           make(map[int]*PullRequest),
			Labels:        make(map[string]*Label),
			NextNum:       1,
		}
		s.store.repos[keyOf(viewerLogin, repoName)] = repo
		s.store.order = append(s.store.order, repo)
		out := repoToJSON(repo)
		s.store.mu.Unlock()
		return out, nil
	case "get_repo":
		owner, repoName := mcpStr(args, "owner"), mcpStr(args, "repo")
		if owner == "" || repoName == "" {
			return nil, &mcpToolError{"missing required argument: owner or repo"}
		}
		s.store.mu.RLock()
		repo := s.store.getRepo(owner, repoName)
		if repo == nil || repo.isForeign() {
			s.store.mu.RUnlock()
			return nil, &mcpToolError{"repository not found"}
		}
		out := repoToJSON(repo)
		s.store.mu.RUnlock()
		return out, nil
	case "create_issue":
		owner, repoName, title := mcpStr(args, "owner"), mcpStr(args, "repo"), mcpStr(args, "title")
		if owner == "" || repoName == "" || title == "" {
			return nil, &mcpToolError{"missing required argument: owner, repo or title"}
		}
		s.store.mu.Lock()
		repo := s.store.getRepo(owner, repoName)
		if repo == nil || repo.isForeign() {
			s.store.mu.Unlock()
			return nil, &mcpToolError{"repository not found"}
		}
		iss := &Issue{
			ID:     s.store.nextIDv(),
			Number: repo.NextNum,
			Title:  title,
			Body:   mcpStr(args, "body"),
			State:  "open",
			User:   viewerLogin,
			Repo:   repoName,
			Seq:    s.store.nextSeq(),
		}
		repo.NextNum++
		repo.Issues[iss.Number] = iss
		data := s.store.issueToJSON(repo, iss)
		s.store.mu.Unlock()
		var out any
		_ = json.Unmarshal(data, &out)
		return out, nil
	case "list_issues":
		owner, repoName := mcpStr(args, "owner"), mcpStr(args, "repo")
		if owner == "" || repoName == "" {
			return nil, &mcpToolError{"missing required argument: owner or repo"}
		}
		state := mcpStr(args, "state")
		s.store.mu.RLock()
		repo := s.store.getRepo(owner, repoName)
		if repo == nil || repo.isForeign() {
			s.store.mu.RUnlock()
			return nil, &mcpToolError{"repository not found"}
		}
		issues := sortedIssues(repo)
		list := make([]any, 0, len(issues))
		for _, iss := range issues {
			if state != "" && state != "all" && iss.State != state {
				continue
			}
			var item any
			_ = json.Unmarshal(s.store.issueToJSON(repo, iss), &item)
			list = append(list, item)
		}
		s.store.mu.RUnlock()
		return map[string]any{"issues": list}, nil
	case "create_pull_request":
		owner, repoName := mcpStr(args, "owner"), mcpStr(args, "repo")
		title, head, base := mcpStr(args, "title"), mcpStr(args, "head"), mcpStr(args, "base")
		if owner == "" || repoName == "" || title == "" || head == "" || base == "" {
			return nil, &mcpToolError{"missing required argument: owner, repo, title, head or base"}
		}
		s.store.mu.Lock()
		repo := s.store.getRepo(owner, repoName)
		if repo == nil || repo.isForeign() {
			s.store.mu.Unlock()
			return nil, &mcpToolError{"repository not found"}
		}
		headSHA, ok := repo.Refs["refs/heads/"+head]
		if !ok {
			s.store.mu.Unlock()
			return nil, &mcpToolError{"head branch does not exist"}
		}
		baseSHA, ok := repo.Refs["refs/heads/"+base]
		if !ok {
			s.store.mu.Unlock()
			return nil, &mcpToolError{"base branch does not exist"}
		}
		for _, pr := range repo.PRs {
			if pr.State == "open" && pr.HeadRef == head && pr.BaseRef == base {
				s.store.mu.Unlock()
				return nil, &mcpToolError{"a pull request already exists for " + head}
			}
		}
		pr := &PullRequest{
			ID:      s.store.nextIDv(),
			Number:  repo.NextNum,
			Title:   title,
			Body:    mcpStr(args, "body"),
			State:   "open",
			HeadRef: head,
			BaseRef: base,
			HeadSHA: headSHA,
			BaseSHA: baseSHA,
			Repo:    repoName,
			Seq:     s.store.nextSeq(),
		}
		repo.NextNum++
		repo.PRs[pr.Number] = pr
		data := s.store.prToJSON(repo, pr)
		s.store.mu.Unlock()
		var out any
		_ = json.Unmarshal(data, &out)
		return out, nil
	case "merge_pull_request":
		owner, repoName := mcpStr(args, "owner"), mcpStr(args, "repo")
		number := mcpInt(args, "pull_number")
		if number == 0 {
			number = mcpInt(args, "number")
		}
		if owner == "" || repoName == "" || number == 0 {
			return nil, &mcpToolError{"missing required argument: owner, repo or pull_number"}
		}
		merged, err := s.mergePullRequestByID(repoName, number)
		if err != nil {
			return nil, &mcpToolError{err.Error()}
		}
		s.store.mu.RLock()
		repo := s.store.getRepo(owner, repoName)
		var data []byte
		if repo != nil {
			data = s.store.prToJSON(repo, merged)
		}
		s.store.mu.RUnlock()
		var out any
		_ = json.Unmarshal(data, &out)
		return out, nil
	}
	return nil, errUnknownTool
}
