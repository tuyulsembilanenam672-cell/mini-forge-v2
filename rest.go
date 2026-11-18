// rest.go: the REST v3 surface under /api/v3. Every reply is JSON and
// carries a non-empty X-GitHub-Request-Id; unknown routes are JSON 404s
// and malformed JSON bodies are 400s. The viewer is fixed (adaiversen);
// any other owner 404s, never 403s.

package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// --- JSON shapes (GitHub-like, only the fields the suite reads) ---

type userJSON struct {
	Login string `json:"login"`
	ID    int64  `json:"id"`
	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`
	Type  string `json:"type"`
}

func viewerUserJSON() userJSON {
	return userJSON{Login: viewerLogin, ID: viewerID, Name: viewerName, Email: viewerEmail, Type: "User"}
}

type repoJSON struct {
	ID            int64    `json:"id"`
	Name          string   `json:"name"`
	FullName      string   `json:"full_name"`
	Owner         userJSON `json:"owner"`
	Private       bool     `json:"private"`
	Description   string   `json:"description"`
	DefaultBranch string   `json:"default_branch"`
	CreatedAt     string   `json:"created_at"`
	UpdatedAt     string   `json:"updated_at"`
}

func repoToJSON(r *Repo) repoJSON {
	return repoJSON{
		ID:            r.ID,
		Name:          r.Name,
		FullName:      r.Owner + "/" + r.Name,
		Owner:         viewerUserJSON(),
		Private:       r.Private,
		Description:   r.Description,
		DefaultBranch: r.DefaultBranch,
		CreatedAt:     isoTime(r.CreatedSeq),
		UpdatedAt:     isoTime(r.CreatedSeq),
	}
}

type labelJSON struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

func labelsToJSON(labels []*Label) []labelJSON {
	out := make([]labelJSON, 0, len(labels))
	for _, l := range labels {
		out = append(out, labelJSON{Name: l.Name, Color: l.Color})
	}
	return out
}

type issueJSON struct {
	ID        int64       `json:"id"`
	Number    int         `json:"number"`
	Title     string      `json:"title"`
	Body      string      `json:"body"`
	State     string      `json:"state"`
	Labels    []labelJSON `json:"labels"`
	User      userJSON    `json:"user"`
	CreatedAt string      `json:"created_at"`
	UpdatedAt string      `json:"updated_at"`
	Comments  int         `json:"comments"`
}

func (s *Store) issueToJSON(r *Repo, iss *Issue) []byte {
	if iss.cached != nil {
		return iss.cached
	}
	v := issueJSON{
		ID:        iss.ID,
		Number:    iss.Number,
		Title:     iss.Title,
		Body:      iss.Body,
		State:     iss.State,
		Labels:    labelsToJSON(iss.Labels),
		User:      viewerUserJSON(),
		CreatedAt: isoTime(iss.Seq),
		UpdatedAt: isoTime(iss.Seq),
		Comments:  len(iss.Comments),
	}
	data, _ := json.Marshal(v)
	iss.cached = data
	return data
}

type prJSON struct {
	ID             int64     `json:"id"`
	Number         int       `json:"number"`
	Title          string    `json:"title"`
	Body           string    `json:"body"`
	State          string    `json:"state"`
	Head           prRefJSON `json:"head"`
	Base           prRefJSON `json:"base"`
	Merged         bool      `json:"merged"`
	MergedAt       string    `json:"merged_at,omitempty"`
	MergeCommitSHA string    `json:"merge_commit_sha,omitempty"`
	User           userJSON  `json:"user"`
	CreatedAt      string    `json:"created_at"`
	UpdatedAt      string    `json:"updated_at"`
}

type prRefJSON struct {
	Ref   string `json:"ref"`
	SHA   string `json:"sha"`
	Label string `json:"label"`
}

func (s *Store) prToJSON(r *Repo, pr *PullRequest) []byte {
	if pr.cached != nil {
		return pr.cached
	}
	v := prJSON{
		ID:        pr.ID,
		Number:    pr.Number,
		Title:     pr.Title,
		Body:      pr.Body,
		State:     pr.State,
		Head:      prRefJSON{Ref: pr.HeadRef, SHA: pr.HeadSHA, Label: viewerLogin + ":" + pr.HeadRef},
		Base:      prRefJSON{Ref: pr.BaseRef, SHA: pr.BaseSHA, Label: viewerLogin + ":" + pr.BaseRef},
		Merged:    pr.Merged,
		User:      viewerUserJSON(),
		CreatedAt: isoTime(pr.Seq),
		UpdatedAt: isoTime(pr.Seq),
	}
	if pr.Merged {
		v.MergedAt = pr.MergedAt
		v.MergeCommitSHA = pr.MergeSHA
	}
	data, _ := json.Marshal(v)
	pr.cached = data
	return data
}

type commentJSON struct {
	ID        int64    `json:"id"`
	Body      string   `json:"body"`
	User      userJSON `json:"user"`
	CreatedAt string   `json:"created_at"`
	UpdatedAt string   `json:"updated_at"`
}

// --- routing ---

// handleREST dispatches /api/v3/... after the leading segments.
func (s *server) handleREST(w http.ResponseWriter, r *http.Request) {
	segs := splitPath(strings.TrimPrefix(r.URL.Path, "/api/v3/"))
	if len(segs) == 0 {
		s.writeErr(w, http.StatusNotFound, "not found")
		return
	}
	switch segs[0] {
	case "user":
		s.handleUser(w, r, segs[1:])
	case "users":
		if len(segs) == 2 && segs[1] == viewerLogin {
			s.writeJSON(w, http.StatusOK, viewerUserJSON())
			return
		}
		s.writeErr(w, http.StatusNotFound, "user not found")
	case "repos":
		s.handleRepos(w, r, segs[1:])
	default:
		s.writeErr(w, http.StatusNotFound, "not found")
	}
}

// handleUser serves /user and /user/repos.
func (s *server) handleUser(w http.ResponseWriter, r *http.Request, segs []string) {
	if len(segs) == 0 {
		if r.Method != http.MethodGet {
			s.writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.writeJSON(w, http.StatusOK, viewerUserJSON())
		return
	}
	if segs[0] != "repos" {
		s.writeErr(w, http.StatusNotFound, "not found")
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.listUserRepos(w, r)
	case http.MethodPost:
		var body struct {
			Name        string `json:"name"`
			Private     bool   `json:"private"`
			Description string `json:"description"`
		}
		if !s.decodeJSON(w, r, &body) {
			return
		}
		if body.Name == "" {
			s.writeErr(w, http.StatusUnprocessableEntity, "name is required")
			return
		}
		s.store.mu.Lock()
		if existing := s.store.getRepo(viewerLogin, body.Name); existing != nil {
			s.store.mu.Unlock()
			s.writeErr(w, http.StatusUnprocessableEntity, "name already exists on this account")
			return
		}
		repo := &Repo{
			ID:            s.store.nextIDv(),
			Name:          body.Name,
			Owner:         viewerLogin,
			Private:       body.Private,
			Description:   body.Description,
			DefaultBranch: "main",
			CreatedSeq:    s.store.nextSeq(),
			Objects:       make(map[string]*Object),
			Refs:          make(map[string]string),
			Issues:        make(map[int]*Issue),
			PRs:           make(map[int]*PullRequest),
			Labels:        make(map[string]*Label),
			NextNum:       1,
		}
		s.store.repos[keyOf(viewerLogin, body.Name)] = repo
		s.store.order = append(s.store.order, repo)
		s.store.mu.Unlock()
		s.writeJSON(w, http.StatusCreated, repoToJSON(repo))
	default:
		s.writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// listUserRepos serves GET /user/repos with per_page pagination in
// creation order and a Link header while more pages exist.
func (s *server) listUserRepos(w http.ResponseWriter, r *http.Request) {
	perPage := queryInt(r, "per_page", 30)
	if perPage < 1 {
		perPage = 30
	}
	page := queryInt(r, "page", 1)
	if page < 1 {
		page = 1
	}
	s.store.mu.RLock()
	total := len(s.store.order)
	start := (page - 1) * perPage
	var out []repoJSON
	if start < total {
		end := start + perPage
		if end > total {
			end = total
		}
		out = make([]repoJSON, 0, end-start)
		for _, repo := range s.store.order[start:end] {
			out = append(out, repoToJSON(repo))
		}
	}
	s.store.mu.RUnlock()
	if out == nil {
		out = []repoJSON{}
	}
	if start+perPage < total {
		next := *r.URL
		q := next.Query()
		q.Set("page", strconv.Itoa(page+1))
		q.Set("per_page", strconv.Itoa(perPage))
		next.RawQuery = q.Encode()
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		url := scheme + "://" + r.Host + next.RequestURI()
		w.Header().Set("Link", "<"+url+">; rel=\"next\"")
	}
	s.writeJSON(w, http.StatusOK, out)
}

// queryInt reads an integer query parameter with a default.
func queryInt(r *http.Request, name string, def int) int {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// handleRepos dispatches /repos/{owner}/{repo}[/...].
func (s *server) handleRepos(w http.ResponseWriter, r *http.Request, segs []string) {
	if len(segs) < 2 {
		s.writeErr(w, http.StatusNotFound, "not found")
		return
	}
	owner, name := segs[0], segs[1]
	rest := segs[2:]
	if len(rest) == 0 {
		s.handleRepo(w, r, owner, name)
		return
	}
	switch rest[0] {
	case "branches":
		s.handleBranches(w, r, owner, name, rest[1:])
	case "commits":
		s.handleCommits(w, r, owner, name)
	case "git":
		s.handleGitData(w, r, owner, name, rest[1:])
	case "contents":
		s.handleContents(w, r, owner, name, rest[1:])
	case "labels":
		s.handleLabels(w, r, owner, name)
	case "issues":
		s.handleIssues(w, r, owner, name, rest[1:])
	case "pulls":
		s.handlePulls(w, r, owner, name, rest[1:])
	default:
		s.writeErr(w, http.StatusNotFound, "not found")
	}
}

// handleRepo serves GET/PATCH/DELETE /repos/{owner}/{repo}.
func (s *server) handleRepo(w http.ResponseWriter, r *http.Request, owner, name string) {
	switch r.Method {
	case http.MethodGet:
		repo := s.requireRepo(w, owner, name)
		if repo == nil {
			return
		}
		s.writeJSON(w, http.StatusOK, repoToJSON(repo))
	case http.MethodPatch:
		var body struct {
			Description *string `json:"description"`
			Private     *bool   `json:"private"`
			Name        *string `json:"name"`
		}
		if !s.decodeJSON(w, r, &body) {
			return
		}
		s.store.mu.Lock()
		repo := s.store.getRepo(owner, name)
		if repo == nil || repo.isForeign() {
			s.store.mu.Unlock()
			s.writeErr(w, http.StatusNotFound, "repository not found")
			return
		}
		if body.Description != nil {
			repo.Description = *body.Description
		}
		if body.Private != nil {
			repo.Private = *body.Private
		}
		out := repoToJSON(repo)
		s.store.mu.Unlock()
		s.writeJSON(w, http.StatusOK, out)
	case http.MethodDelete:
		s.store.mu.Lock()
		repo := s.store.getRepo(owner, name)
		if repo == nil || repo.isForeign() {
			s.store.mu.Unlock()
			s.writeErr(w, http.StatusNotFound, "repository not found")
			return
		}
		delete(s.store.repos, keyOf(owner, name))
		for i, other := range s.store.order {
			if other == repo {
				s.store.order = append(s.store.order[:i], s.store.order[i+1:]...)
				break
			}
		}
		s.store.mu.Unlock()
		w.Header().Set("X-GitHub-Request-Id", s.requestID())
		w.WriteHeader(http.StatusNoContent)
	default:
		s.writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleBranches serves GET /branches and /branches/{branch}.
func (s *server) handleBranches(w http.ResponseWriter, r *http.Request, owner, name string, rest []string) {
	if r.Method != http.MethodGet {
		s.writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	repo := s.requireRepo(w, owner, name)
	if repo == nil {
		return
	}
	type branchJSON struct {
		Name   string `json:"name"`
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
		Protected bool `json:"protected"`
	}
	mk := func(branch, sha string) branchJSON {
		var b branchJSON
		b.Name = branch
		b.Commit.SHA = sha
		return b
	}
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	if len(rest) == 0 {
		out := []branchJSON{}
		for _, ref := range repo.sortedRefNames() {
			if strings.HasPrefix(ref, "refs/heads/") {
				out = append(out, mk(strings.TrimPrefix(ref, "refs/heads/"), repo.Refs[ref]))
			}
		}
		s.writeJSON(w, http.StatusOK, out)
		return
	}
	branch := rest[0]
	sha, ok := repo.Refs["refs/heads/"+branch]
	if !ok {
		s.writeErr(w, http.StatusNotFound, "branch not found")
		return
	}
	s.writeJSON(w, http.StatusOK, mk(branch, sha))
}

// handleCommits serves GET /commits[?sha=<ref>]: commits reachable from
// the ref (or the default branch), tip first; branch-only commits are
// never listed under another ref.
func (s *server) handleCommits(w http.ResponseWriter, r *http.Request, owner, name string) {
	if r.Method != http.MethodGet {
		s.writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	repo := s.requireRepo(w, owner, name)
	if repo == nil {
		return
	}
	ref := r.URL.Query().Get("sha")
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	var tip string
	if ref == "" {
		tip = repo.headOID()
	} else if isHexOID(ref) {
		tip = ref
	} else {
		tip = repo.Refs["refs/heads/"+ref]
		if tip == "" {
			tip = repo.Refs[ref]
		}
	}
	type commitJSON struct {
		SHA    string `json:"sha"`
		Commit struct {
			Message string `json:"message"`
			Author  struct {
				Name  string `json:"name"`
				Email string `json:"email"`
				Date  string `json:"date"`
			} `json:"author"`
			Committer struct {
				Name  string `json:"name"`
				Email string `json:"email"`
				Date  string `json:"date"`
			} `json:"committer"`
		} `json:"commit"`
		Author    userJSON `json:"author"`
		Committer userJSON `json:"committer"`
		Parents   []struct {
			SHA string `json:"sha"`
		} `json:"parents"`
	}
	out := []commitJSON{}
	for _, oid := range s.store.commitChain(repo, tip) {
		obj := repo.Objects[oid]
		if obj == nil {
			continue
		}
		c := parseCommit(obj.Data)
		var entry commitJSON
		entry.SHA = oid
		entry.Commit.Message = c.Message
		entry.Commit.Author.Name = viewerName
		entry.Commit.Author.Email = viewerEmail
		entry.Commit.Author.Date = isoTime(0)
		entry.Commit.Committer = entry.Commit.Author
		entry.Author = viewerUserJSON()
		entry.Committer = viewerUserJSON()
		for _, p := range c.Parents {
			entry.Parents = append(entry.Parents, struct {
				SHA string `json:"sha"`
			}{SHA: p})
		}
		out = append(out, entry)
	}
	s.writeJSON(w, http.StatusOK, out)
}

// handleGitData serves the Git Data API: POST /git/refs and
// GET /git/refs/heads/{branch}.
func (s *server) handleGitData(w http.ResponseWriter, r *http.Request, owner, name string, rest []string) {
	if len(rest) == 0 || rest[0] != "refs" {
		s.writeErr(w, http.StatusNotFound, "not found")
		return
	}
	switch r.Method {
	case http.MethodPost:
		if len(rest) != 1 {
			s.writeErr(w, http.StatusNotFound, "not found")
			return
		}
		var body struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		}
		if !s.decodeJSON(w, r, &body) {
			return
		}
		if body.Ref == "" || body.SHA == "" {
			s.writeErr(w, http.StatusUnprocessableEntity, "ref and sha are required")
			return
		}
		s.store.mu.Lock()
		repo := s.store.getRepo(owner, name)
		if repo == nil || repo.isForeign() {
			s.store.mu.Unlock()
			s.writeErr(w, http.StatusNotFound, "repository not found")
			return
		}
		if _, exists := repo.Refs[body.Ref]; exists {
			s.store.mu.Unlock()
			s.writeErr(w, http.StatusUnprocessableEntity, "reference already exists")
			return
		}
		if repo.Objects[body.SHA] == nil {
			s.store.mu.Unlock()
			s.writeErr(w, http.StatusUnprocessableEntity, "object does not exist")
			return
		}
		repo.Refs[body.Ref] = body.SHA
		s.store.touch(repo)
		s.store.mu.Unlock()
		s.writeJSON(w, http.StatusCreated, map[string]any{
			"ref":    body.Ref,
			"object": map[string]any{"sha": body.SHA},
		})
	case http.MethodGet:
		if len(rest) < 3 || rest[1] != "heads" {
			s.writeErr(w, http.StatusNotFound, "not found")
			return
		}
		repo := s.requireRepo(w, owner, name)
		if repo == nil {
			return
		}
		s.store.mu.RLock()
		sha, ok := repo.Refs["refs/heads/"+rest[2]]
		s.store.mu.RUnlock()
		if !ok {
			s.writeErr(w, http.StatusNotFound, "reference not found")
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]any{
			"ref":    "refs/heads/" + rest[2],
			"object": map[string]any{"sha": sha},
		})
	default:
		s.writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// sortedIssues returns a repository's issues in creation order.
func sortedIssues(r *Repo) []*Issue {
	out := make([]*Issue, 0, len(r.Issues))
	for _, iss := range r.Issues {
		out = append(out, iss)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

// sortedPRs returns a repository's pull requests in creation order.
func sortedPRs(r *Repo) []*PullRequest {
	out := make([]*PullRequest, 0, len(r.PRs))
	for _, pr := range r.PRs {
		out = append(out, pr)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

// --- contents ---

// resolveRef turns a ?ref= value (branch name, full ref, or commit id)
// into a commit id, or "" when it names nothing.
func (s *Store) resolveRef(r *Repo, ref string) string {
	if ref == "" {
		return r.headOID()
	}
	if isHexOID(ref) {
		if r.Objects[ref] != nil {
			return ref
		}
		return ""
	}
	if oid, ok := r.Refs["refs/heads/"+ref]; ok {
		return oid
	}
	if oid, ok := r.Refs[ref]; ok {
		return oid
	}
	return ""
}

// contentsCommit creates a commit that sets path to blobOID on top of
// baseCommit (or from nothing when baseCommit is ""), stores it, moves
// branch to it and returns the new commit id.
func (s *Store) contentsCommit(r *Repo, branch, path, blobOID, message, baseCommit string) string {
	var flat map[string]string
	if baseCommit != "" {
		if obj := r.Objects[baseCommit]; obj != nil && obj.Type == "commit" {
			flat = s.treeMap(r, parseCommit(obj.Data).Tree)
		}
	}
	if flat == nil {
		flat = make(map[string]string)
	}
	flat[path] = blobOID
	treeOID := s.buildTreeFromMap(r, flat)
	var parents []string
	if baseCommit != "" {
		parents = []string{baseCommit}
	}
	commitOID := s.putObject(r, "commit", buildCommit(treeOID, parents, message))
	r.Refs["refs/heads/"+branch] = commitOID
	s.touch(r)
	return commitOID
}

type contentEntryJSON struct {
	Type     string `json:"type"`
	Encoding string `json:"encoding,omitempty"`
	Size     int    `json:"size,omitempty"`
	Name     string `json:"name"`
	Path     string `json:"path"`
	Content  string `json:"content,omitempty"`
	SHA      string `json:"sha"`
}

// handleContents serves PUT/GET/DELETE /contents/{path}.
func (s *server) handleContents(w http.ResponseWriter, r *http.Request, owner, name string, rest []string) {
	if len(rest) == 0 {
		s.writeErr(w, http.StatusNotFound, "not found")
		return
	}
	path := strings.Join(rest, "/")
	switch r.Method {
	case http.MethodPut:
		var body struct {
			Message string `json:"message"`
			Content string `json:"content"`
			Branch  string `json:"branch"`
			SHA     string `json:"sha"`
		}
		if !s.decodeJSON(w, r, &body) {
			return
		}
		if body.Message == "" || body.Content == "" {
			s.writeErr(w, http.StatusUnprocessableEntity, "message and content are required")
			return
		}
		raw, err := base64.StdEncoding.DecodeString(body.Content)
		if err != nil {
			s.writeErr(w, http.StatusUnprocessableEntity, "content is not valid base64")
			return
		}
		s.store.mu.Lock()
		repo := s.store.getRepo(owner, name)
		if repo == nil || repo.isForeign() {
			s.store.mu.Unlock()
			s.writeErr(w, http.StatusNotFound, "repository not found")
			return
		}
		branch := body.Branch
		if branch == "" {
			branch = repo.DefaultBranch
		}
		// The base commit: the branch tip when it exists, else the
		// default branch tip (a new branch starts from it), else none.
		baseCommit := repo.Refs["refs/heads/"+branch]
		if baseCommit == "" {
			baseCommit = repo.headOID()
		}
		// A stale sha is a 409: the file changed since the caller read it.
		if body.SHA != "" && baseCommit != "" {
			flat := s.store.treeMap(repo, parseCommit(repo.Objects[baseCommit].Data).Tree)
			if current, ok := flat[path]; ok && current != body.SHA {
				s.store.mu.Unlock()
				s.writeErr(w, http.StatusConflict, "sha does not match the current file")
				return
			}
		}
		isNew := true
		if baseCommit != "" {
			if obj := repo.Objects[baseCommit]; obj != nil && obj.Type == "commit" {
				if _, ok := s.store.treeMap(repo, parseCommit(obj.Data).Tree)[path]; ok {
					isNew = false
				}
			}
		}
		blobOID := s.store.putObject(repo, "blob", raw)
		commitOID := s.store.contentsCommit(repo, branch, path, blobOID, body.Message, baseCommit)
		entry := contentEntryJSON{
			Type:     "file",
			Encoding: "base64",
			Size:     len(raw),
			Name:     baseName(path),
			Path:     path,
			Content:  base64.StdEncoding.EncodeToString(raw),
			SHA:      blobOID,
		}
		status := http.StatusCreated
		if !isNew {
			status = http.StatusOK
		}
		resp := map[string]any{
			"content": entry,
			"commit":  map[string]any{"sha": commitOID, "message": body.Message},
		}
		s.store.mu.Unlock()
		s.writeJSON(w, status, resp)
	case http.MethodGet:
		repo := s.requireRepo(w, owner, name)
		if repo == nil {
			return
		}
		ref := r.URL.Query().Get("ref")
		s.store.mu.RLock()
		commitOID := s.store.resolveRef(repo, ref)
		if commitOID == "" {
			s.store.mu.RUnlock()
			s.writeErr(w, http.StatusNotFound, "not found")
			return
		}
		obj := repo.Objects[commitOID]
		if obj == nil || obj.Type != "commit" {
			s.store.mu.RUnlock()
			s.writeErr(w, http.StatusNotFound, "not found")
			return
		}
		flat := s.store.treeMap(repo, parseCommit(obj.Data).Tree)
		// Exact file match first.
		if blobOID, ok := flat[path]; ok {
			blob := repo.Objects[blobOID]
			entry := contentEntryJSON{
				Type:     "file",
				Encoding: "base64",
				Size:     len(blob.Data),
				Name:     baseName(path),
				Path:     path,
				Content:  base64.StdEncoding.EncodeToString(blob.Data),
				SHA:      blobOID,
			}
			s.store.mu.RUnlock()
			s.writeJSON(w, http.StatusOK, entry)
			return
		}
		// Directory listing: immediate children of path (or the root).
		prefix := path + "/"
		if path == "" {
			prefix = ""
		}
		seen := make(map[string]contentEntryJSON)
		subMaps := make(map[string]map[string]string)
		for p, blobOID := range flat {
			if !strings.HasPrefix(p, prefix) {
				continue
			}
			rest := strings.TrimPrefix(p, prefix)
			seg := rest
			isDir := false
			if i := strings.IndexByte(rest, '/'); i >= 0 {
				seg = rest[:i]
				isDir = true
			}
			childPath := prefix + seg
			if _, ok := seen[seg]; ok {
				if isDir {
					subMaps[seg][strings.TrimPrefix(rest, seg+"/")] = blobOID
				}
				continue
			}
			if isDir {
				subMaps[seg] = map[string]string{strings.TrimPrefix(rest, seg+"/"): blobOID}
				seen[seg] = contentEntryJSON{Type: "dir", Name: seg, Path: childPath, SHA: ""}
			} else {
				seen[seg] = contentEntryJSON{Type: "file", Size: len(repo.Objects[blobOID].Data), Name: seg, Path: childPath, SHA: blobOID}
			}
		}
		if len(seen) == 0 {
			s.store.mu.RUnlock()
			s.writeErr(w, http.StatusNotFound, "not found")
			return
		}
		out := make([]contentEntryJSON, 0, len(seen))
		for _, e := range seen {
			if e.Type == "dir" {
				// The directory's tree id, computed from its files
				// (identical content hashes to the stored tree).
				e.SHA = treeOIDFromMap(subMaps[e.Name])
			}
			out = append(out, e)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		s.store.mu.RUnlock()
		s.writeJSON(w, http.StatusOK, out)
	case http.MethodDelete:
		var body struct {
			Message string `json:"message"`
			SHA     string `json:"sha"`
		}
		if !s.decodeJSON(w, r, &body) {
			return
		}
		s.store.mu.Lock()
		repo := s.store.getRepo(owner, name)
		if repo == nil || repo.isForeign() {
			s.store.mu.Unlock()
			s.writeErr(w, http.StatusNotFound, "repository not found")
			return
		}
		branch := repo.DefaultBranch
		tip := repo.headOID()
		if tip == "" {
			s.store.mu.Unlock()
			s.writeErr(w, http.StatusNotFound, "not found")
			return
		}
		flat := s.store.treeMap(repo, parseCommit(repo.Objects[tip].Data).Tree)
		blobOID, ok := flat[path]
		if !ok {
			s.store.mu.Unlock()
			s.writeErr(w, http.StatusNotFound, "not found")
			return
		}
		if body.SHA != "" && body.SHA != blobOID {
			s.store.mu.Unlock()
			s.writeErr(w, http.StatusConflict, "sha does not match the current file")
			return
		}
		delete(flat, path)
		treeOID := s.store.buildTreeFromMap(repo, flat)
		commitOID := s.store.putObject(repo, "commit", buildCommit(treeOID, []string{tip}, body.Message))
		repo.Refs["refs/heads/"+branch] = commitOID
		s.store.touch(repo)
		s.store.mu.Unlock()
		s.writeJSON(w, http.StatusOK, map[string]any{
			"commit": map[string]any{"sha": commitOID, "message": body.Message},
		})
	default:
		s.writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// baseName returns the last segment of a slash path.
func baseName(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[i+1:]
	}
	return path
}

// --- labels ---

// handleLabels serves POST /labels and GET /labels.
func (s *server) handleLabels(w http.ResponseWriter, r *http.Request, owner, name string) {
	switch r.Method {
	case http.MethodPost:
		var body struct {
			Name  string `json:"name"`
			Color string `json:"color"`
		}
		if !s.decodeJSON(w, r, &body) {
			return
		}
		if body.Name == "" {
			s.writeErr(w, http.StatusUnprocessableEntity, "name is required")
			return
		}
		s.store.mu.Lock()
		repo := s.store.getRepo(owner, name)
		if repo == nil || repo.isForeign() {
			s.store.mu.Unlock()
			s.writeErr(w, http.StatusNotFound, "repository not found")
			return
		}
		if body.Color == "" {
			body.Color = "ededed"
		}
		label := &Label{Name: body.Name, Color: body.Color}
		repo.Labels[body.Name] = label
		s.store.mu.Unlock()
		s.writeJSON(w, http.StatusCreated, labelJSON{Name: label.Name, Color: label.Color})
	case http.MethodGet:
		repo := s.requireRepo(w, owner, name)
		if repo == nil {
			return
		}
		s.store.mu.RLock()
		out := []labelJSON{}
		names := make([]string, 0, len(repo.Labels))
		for n := range repo.Labels {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			out = append(out, labelJSON{Name: n, Color: repo.Labels[n].Color})
		}
		s.store.mu.RUnlock()
		s.writeJSON(w, http.StatusOK, out)
	default:
		s.writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// --- issues ---

// handleIssues dispatches /issues, /issues/{n} and /issues/{n}/comments.
func (s *server) handleIssues(w http.ResponseWriter, r *http.Request, owner, name string, rest []string) {
	if len(rest) == 0 {
		switch r.Method {
		case http.MethodGet:
			s.listIssues(w, r, owner, name)
		case http.MethodPost:
			s.createIssue(w, r, owner, name)
		default:
			s.writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		}
		return
	}
	number, err := strconv.Atoi(rest[0])
	if err != nil {
		s.writeErr(w, http.StatusNotFound, "not found")
		return
	}
	if len(rest) == 2 && rest[1] == "comments" {
		switch r.Method {
		case http.MethodGet:
			s.listComments(w, r, owner, name, number)
		case http.MethodPost:
			s.createComment(w, r, owner, name, number)
		default:
			s.writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		}
		return
	}
	if len(rest) != 1 {
		s.writeErr(w, http.StatusNotFound, "not found")
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.getIssue(w, r, owner, name, number)
	case http.MethodPatch:
		s.patchIssue(w, r, owner, name, number)
	default:
		s.writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// listIssues serves GET /issues?state=&sort=created&direction=.
// Filtering is by state; ordering is by creation sequence (never the
// wall clock), ascending or descending as asked.
func (s *server) listIssues(w http.ResponseWriter, r *http.Request, owner, name string) {
	repo := s.requireRepo(w, owner, name)
	if repo == nil {
		return
	}
	state := r.URL.Query().Get("state")
	direction := r.URL.Query().Get("direction")
	s.store.mu.RLock()
	issues := sortedIssues(repo)
	out := make([]json.RawMessage, 0, len(issues))
	for _, iss := range issues {
		if state != "" && state != "all" && iss.State != state {
			continue
		}
		out = append(out, s.store.issueToJSON(repo, iss))
	}
	s.store.mu.RUnlock()
	if direction == "desc" {
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	s.writeJSON(w, http.StatusOK, out)
}

// createIssue serves POST /issues. Issue numbers increment by one per
// repository and share the counter with pull requests. An unknown label
// is a 422.
func (s *server) createIssue(w http.ResponseWriter, r *http.Request, owner, name string) {
	var body struct {
		Title  string   `json:"title"`
		Body   string   `json:"body"`
		Labels []string `json:"labels"`
	}
	if !s.decodeJSON(w, r, &body) {
		return
	}
	if body.Title == "" {
		s.writeErr(w, http.StatusUnprocessableEntity, "title is required")
		return
	}
	s.store.mu.Lock()
	repo := s.store.getRepo(owner, name)
	if repo == nil || repo.isForeign() {
		s.store.mu.Unlock()
		s.writeErr(w, http.StatusNotFound, "repository not found")
		return
	}
	var labels []*Label
	for _, lname := range body.Labels {
		l, ok := repo.Labels[lname]
		if !ok {
			s.store.mu.Unlock()
			s.writeErr(w, http.StatusUnprocessableEntity, "label does not exist: "+lname)
			return
		}
		labels = append(labels, l)
	}
	iss := &Issue{
		ID:     s.store.nextIDv(),
		Number: repo.NextNum,
		Title:  body.Title,
		Body:   body.Body,
		State:  "open",
		Labels: labels,
		User:   viewerLogin,
		Repo:   name,
		Seq:    s.store.nextSeq(),
	}
	repo.NextNum++
	repo.Issues[iss.Number] = iss
	data := s.store.issueToJSON(repo, iss)
	s.store.mu.Unlock()
	s.writeRawJSON(w, http.StatusCreated, data)
}

// getIssue serves GET /issues/{n}. The response is the cached issue
// JSON: the read workload's two thousand sequential GETs are map lookups
// plus a write.
func (s *server) getIssue(w http.ResponseWriter, r *http.Request, owner, name string, number int) {
	repo := s.requireRepo(w, owner, name)
	if repo == nil {
		return
	}
	s.store.mu.RLock()
	iss, ok := repo.Issues[number]
	if !ok {
		s.store.mu.RUnlock()
		s.writeErr(w, http.StatusNotFound, "issue not found")
		return
	}
	data := s.store.issueToJSON(repo, iss)
	s.store.mu.RUnlock()
	s.writeRawJSON(w, http.StatusOK, data)
}

// patchIssue serves PATCH /issues/{n}: state (and title/body) updates.
// The cached JSON is dropped so the next read rebuilds it.
func (s *server) patchIssue(w http.ResponseWriter, r *http.Request, owner, name string, number int) {
	var body struct {
		State *string `json:"state"`
		Title *string `json:"title"`
		Body  *string `json:"body"`
	}
	if !s.decodeJSON(w, r, &body) {
		return
	}
	s.store.mu.Lock()
	repo := s.store.getRepo(owner, name)
	if repo == nil || repo.isForeign() {
		s.store.mu.Unlock()
		s.writeErr(w, http.StatusNotFound, "repository not found")
		return
	}
	iss, ok := repo.Issues[number]
	if !ok {
		s.store.mu.Unlock()
		s.writeErr(w, http.StatusNotFound, "issue not found")
		return
	}
	if body.State != nil {
		iss.State = *body.State
	}
	if body.Title != nil {
		iss.Title = *body.Title
	}
	if body.Body != nil {
		iss.Body = *body.Body
	}
	iss.cached = nil
	data := s.store.issueToJSON(repo, iss)
	s.store.mu.Unlock()
	s.writeRawJSON(w, http.StatusOK, data)
}

// listComments serves GET /issues/{n}/comments in creation order.
func (s *server) listComments(w http.ResponseWriter, r *http.Request, owner, name string, number int) {
	repo := s.requireRepo(w, owner, name)
	if repo == nil {
		return
	}
	s.store.mu.RLock()
	iss, ok := repo.Issues[number]
	if !ok {
		s.store.mu.RUnlock()
		s.writeErr(w, http.StatusNotFound, "issue not found")
		return
	}
	out := make([]commentJSON, 0, len(iss.Comments))
	for _, c := range iss.Comments {
		out = append(out, commentJSON{ID: c.ID, Body: c.Body, User: viewerUserJSON(), CreatedAt: isoTime(c.Seq), UpdatedAt: isoTime(c.Seq)})
	}
	s.store.mu.RUnlock()
	s.writeJSON(w, http.StatusOK, out)
}

// createComment serves POST /issues/{n}/comments.
func (s *server) createComment(w http.ResponseWriter, r *http.Request, owner, name string, number int) {
	var body struct {
		Body string `json:"body"`
	}
	if !s.decodeJSON(w, r, &body) {
		return
	}
	if body.Body == "" {
		s.writeErr(w, http.StatusUnprocessableEntity, "body is required")
		return
	}
	s.store.mu.Lock()
	repo := s.store.getRepo(owner, name)
	if repo == nil || repo.isForeign() {
		s.store.mu.Unlock()
		s.writeErr(w, http.StatusNotFound, "repository not found")
		return
	}
	iss, ok := repo.Issues[number]
	if !ok {
		s.store.mu.Unlock()
		s.writeErr(w, http.StatusNotFound, "issue not found")
		return
	}
	c := &Comment{ID: s.store.nextIDv(), Body: body.Body, User: viewerLogin, Repo: name, IssueNumber: number, Seq: s.store.nextSeq()}
	iss.Comments = append(iss.Comments, c)
	iss.cached = nil
	resp := commentJSON{ID: c.ID, Body: c.Body, User: viewerUserJSON(), CreatedAt: isoTime(c.Seq), UpdatedAt: isoTime(c.Seq)}
	s.store.mu.Unlock()
	s.writeJSON(w, http.StatusCreated, resp)
}

// --- pull requests ---

// handlePulls dispatches /pulls, /pulls/{n}, /pulls/{n}/files and
// /pulls/{n}/merge.
func (s *server) handlePulls(w http.ResponseWriter, r *http.Request, owner, name string, rest []string) {
	if len(rest) == 0 {
		switch r.Method {
		case http.MethodGet:
			s.listPulls(w, r, owner, name)
		case http.MethodPost:
			s.createPull(w, r, owner, name)
		default:
			s.writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		}
		return
	}
	number, err := strconv.Atoi(rest[0])
	if err != nil {
		s.writeErr(w, http.StatusNotFound, "not found")
		return
	}
	if len(rest) == 2 && rest[1] == "files" {
		if r.Method != http.MethodGet {
			s.writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.listPullFiles(w, r, owner, name, number)
		return
	}
	if len(rest) == 2 && rest[1] == "merge" {
		if r.Method != http.MethodPut {
			s.writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.mergePull(w, r, owner, name, number)
		return
	}
	if len(rest) != 1 || r.Method != http.MethodGet {
		s.writeErr(w, http.StatusNotFound, "not found")
		return
	}
	s.getPull(w, r, owner, name, number)
}

// listPulls serves GET /pulls?state=: open by default, "all" lists open
// and merged.
func (s *server) listPulls(w http.ResponseWriter, r *http.Request, owner, name string) {
	repo := s.requireRepo(w, owner, name)
	if repo == nil {
		return
	}
	state := r.URL.Query().Get("state")
	s.store.mu.RLock()
	prs := sortedPRs(repo)
	out := make([]json.RawMessage, 0, len(prs))
	for _, pr := range prs {
		if state == "" || state == "open" {
			if pr.State != "open" {
				continue
			}
		} else if state == "closed" {
			if pr.State != "closed" {
				continue
			}
		}
		out = append(out, s.store.prToJSON(repo, pr))
	}
	s.store.mu.RUnlock()
	s.writeJSON(w, http.StatusOK, out)
}

// createPull serves POST /pulls. A duplicate open pull request for the
// same head and base is a 422.
func (s *server) createPull(w http.ResponseWriter, r *http.Request, owner, name string) {
	var body struct {
		Head  string `json:"head"`
		Base  string `json:"base"`
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	if !s.decodeJSON(w, r, &body) {
		return
	}
	if body.Head == "" || body.Base == "" || body.Title == "" {
		s.writeErr(w, http.StatusUnprocessableEntity, "head, base and title are required")
		return
	}
	s.store.mu.Lock()
	repo := s.store.getRepo(owner, name)
	if repo == nil || repo.isForeign() {
		s.store.mu.Unlock()
		s.writeErr(w, http.StatusNotFound, "repository not found")
		return
	}
	headSHA, ok := repo.Refs["refs/heads/"+body.Head]
	if !ok {
		s.store.mu.Unlock()
		s.writeErr(w, http.StatusUnprocessableEntity, "head branch does not exist")
		return
	}
	baseSHA, ok := repo.Refs["refs/heads/"+body.Base]
	if !ok {
		s.store.mu.Unlock()
		s.writeErr(w, http.StatusUnprocessableEntity, "base branch does not exist")
		return
	}
	for _, pr := range repo.PRs {
		if pr.State == "open" && pr.HeadRef == body.Head && pr.BaseRef == body.Base {
			s.store.mu.Unlock()
			s.writeErr(w, http.StatusUnprocessableEntity, "a pull request already exists for "+body.Head)
			return
		}
	}
	pr := &PullRequest{
		ID:      s.store.nextIDv(),
		Number:  repo.NextNum,
		Title:   body.Title,
		Body:    body.Body,
		State:   "open",
		HeadRef: body.Head,
		BaseRef: body.Base,
		HeadSHA: headSHA,
		BaseSHA: baseSHA,
		Repo:    name,
		Seq:     s.store.nextSeq(),
	}
	repo.NextNum++
	repo.PRs[pr.Number] = pr
	data := s.store.prToJSON(repo, pr)
	s.store.mu.Unlock()
	s.writeRawJSON(w, http.StatusCreated, data)
}

// getPull serves GET /pulls/{n}.
func (s *server) getPull(w http.ResponseWriter, r *http.Request, owner, name string, number int) {
	repo := s.requireRepo(w, owner, name)
	if repo == nil {
		return
	}
	s.store.mu.RLock()
	pr, ok := repo.PRs[number]
	if !ok {
		s.store.mu.RUnlock()
		s.writeErr(w, http.StatusNotFound, "pull request not found")
		return
	}
	data := s.store.prToJSON(repo, pr)
	s.store.mu.RUnlock()
	s.writeRawJSON(w, http.StatusOK, data)
}

// diffFiles returns the file diff between two trees: one entry per path
// that differs, with status added/modified and line counts.
func (s *Store) diffFiles(r *Repo, baseTree, headTree string) []map[string]any {
	base := s.treeMap(r, baseTree)
	head := s.treeMap(r, headTree)
	out := []map[string]any{}
	for path, headOID := range head {
		baseOID, ok := base[path]
		if !ok {
			out = append(out, fileDiff(r, path, "added", "", headOID))
			continue
		}
		if baseOID != headOID {
			out = append(out, fileDiff(r, path, "modified", baseOID, headOID))
		}
	}
	for path := range base {
		if _, ok := head[path]; !ok {
			out = append(out, fileDiff(r, path, "removed", base[path], ""))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["filename"].(string) < out[j]["filename"].(string) })
	return out
}

// fileDiff builds one pull-request file entry. Additions and deletions
// are counted by lines; a file that only changed content counts at
// least one addition, as the suite requires.
func fileDiff(r *Repo, path, status, baseOID, headOID string) map[string]any {
	additions, deletions := 0, 0
	countLines := func(oid string) int {
		if oid == "" {
			return 0
		}
		obj := r.Objects[oid]
		if obj == nil {
			return 0
		}
		return strings.Count(string(obj.Data), "\n") + 1
	}
	switch status {
	case "added":
		additions = countLines(headOID)
	case "removed":
		deletions = countLines(baseOID)
	case "modified":
		additions = countLines(headOID)
		deletions = countLines(baseOID)
		if additions == 0 {
			additions = 1
		}
	}
	return map[string]any{
		"filename":   path,
		"path":       path,
		"status":     status,
		"changeType": strings.ToUpper(status),
		"additions":  additions,
		"deletions":  deletions,
		"changes":    additions + deletions,
	}
}

// listPullFiles serves GET /pulls/{n}/files: the diff of the base tip
// tree against the head tip tree.
func (s *server) listPullFiles(w http.ResponseWriter, r *http.Request, owner, name string, number int) {
	repo := s.requireRepo(w, owner, name)
	if repo == nil {
		return
	}
	s.store.mu.RLock()
	pr, ok := repo.PRs[number]
	if !ok {
		s.store.mu.RUnlock()
		s.writeErr(w, http.StatusNotFound, "pull request not found")
		return
	}
	headObj := repo.Objects[pr.HeadSHA]
	baseObj := repo.Objects[pr.BaseSHA]
	if headObj == nil || baseObj == nil {
		s.store.mu.RUnlock()
		s.writeErr(w, http.StatusNotFound, "pull request not found")
		return
	}
	out := s.store.diffFiles(repo, parseCommit(baseObj.Data).Tree, parseCommit(headObj.Data).Tree)
	s.store.mu.RUnlock()
	s.writeJSON(w, http.StatusOK, out)
}

// mergePull serves PUT /pulls/{n}/merge (squash only). Unknown pull
// requests 404; a conflicted pull request is a 405. A squash merge
// writes one single-parent commit on the base tip whose tree is the
// base tree plus the head's changes against the merge-base.
func (s *server) mergePull(w http.ResponseWriter, r *http.Request, owner, name string, number int) {
	// The body is optional; read and discard it so the connection stays
	// reusable. Squash is the only merge method.
	_, _ = readBody(r)
	s.store.mu.Lock()
	repo := s.store.getRepo(owner, name)
	if repo == nil || repo.isForeign() {
		s.store.mu.Unlock()
		s.writeErr(w, http.StatusNotFound, "repository not found")
		return
	}
	pr, ok := repo.PRs[number]
	if !ok {
		s.store.mu.Unlock()
		s.writeErr(w, http.StatusNotFound, "pull request not found")
		return
	}
	if pr.State != "open" {
		s.store.mu.Unlock()
		s.writeErr(w, http.StatusMethodNotAllowed, "pull request is not open")
		return
	}
	headSHA := repo.Refs["refs/heads/"+pr.HeadRef]
	baseSHA := repo.Refs["refs/heads/"+pr.BaseRef]
	if headSHA == "" || baseSHA == "" {
		s.store.mu.Unlock()
		s.writeErr(w, http.StatusNotFound, "pull request branch not found")
		return
	}
	baseObj := repo.Objects[baseSHA]
	headObj := repo.Objects[headSHA]
	if baseObj == nil || headObj == nil {
		s.store.mu.Unlock()
		s.writeErr(w, http.StatusNotFound, "pull request branch not found")
		return
	}
	mergeBaseOID := s.store.mergeBase(repo, baseSHA, headSHA)
	var baseTree, headTree, mergeBaseTree string
	baseTree = parseCommit(baseObj.Data).Tree
	headTree = parseCommit(headObj.Data).Tree
	if mergeBaseOID != "" {
		if mbObj := repo.Objects[mergeBaseOID]; mbObj != nil {
			mergeBaseTree = parseCommit(mbObj.Data).Tree
		}
	}
	// Conflict: the same path changed on both sides of the merge-base
	// with different resulting content.
	baseFlat := s.store.treeMap(repo, baseTree)
	headFlat := s.store.treeMap(repo, headTree)
	mbFlat := s.store.treeMap(repo, mergeBaseTree)
	conflicted := s.store.mergeConflicts(baseFlat, headFlat, mbFlat)
	if conflicted {
		s.store.mu.Unlock()
		s.writeErr(w, http.StatusMethodNotAllowed, "merge conflict")
		return
	}
	// Squash tree: the base tree plus the head's changes vs merge-base.
	merged := make(map[string]string, len(baseFlat))
	for path, oid := range baseFlat {
		merged[path] = oid
	}
	for path, headOID := range headFlat {
		mbOID, inBase := mbFlat[path]
		if !inBase || mbOID != headOID {
			merged[path] = headOID
		}
	}
	for path := range mbFlat {
		if _, onHead := headFlat[path]; !onHead {
			delete(merged, path)
		}
	}
	treeOID := s.store.buildTreeFromMap(repo, merged)
	message := pr.Title
	if message == "" {
		message = sprintf("Merge pull request #%d", pr.Number)
	}
	mergeOID := s.store.putObject(repo, "commit", buildCommit(treeOID, []string{baseSHA}, message))
	repo.Refs["refs/heads/"+pr.BaseRef] = mergeOID
	s.store.touch(repo)
	pr.Merged = true
	pr.MergedAt = isoTime(s.store.nextSeq())
	pr.MergeSHA = mergeOID
	pr.State = "closed"
	pr.cached = nil
	resp := map[string]any{
		"merged":  true,
		"sha":     mergeOID,
		"message": message,
	}
	s.store.mu.Unlock()
	s.writeJSON(w, http.StatusOK, resp)
}
