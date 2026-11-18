// http.go: the HTTP server — routing, authentication, JSON replies.
// Routing is hand-rolled on path segments: the perf workloads hit the
// same few paths thousands of times, and a map lookup plus a switch
// beats a generic router's allocations.

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
)

// server holds the forge's runtime state.
type server struct {
	store     *Store
	shopToken string // MAKEFUN_SHOP_TOKEN when set: the exact token required
	reqSeq    atomic.Uint64
}

// jsonError is the {"message": ...} error body every error reply uses.
type jsonError struct {
	Message string `json:"message"`
}

// newServer builds a server for a store. The shop token comes from the
// environment; when it is empty any non-empty Bearer token is accepted
// (local runs), and a missing token is always refused.
func newServer(store *Store, shopToken string) *server {
	return &server{store: store, shopToken: shopToken}
}

// requestID returns the next request id. Every reply carries a non-empty
// X-GitHub-Request-Id; a monotonic counter is the cheapest unique value.
func (s *server) requestID() string {
	return "req-" + strconv.FormatUint(s.reqSeq.Add(1), 10)
}

// authorized reports whether the request carries an acceptable token:
// the exact shop token when one is configured, else any non-empty
// Bearer token. A missing or empty token is never accepted.
func (s *server) authorized(r *http.Request) bool {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return false
	}
	token := strings.TrimSpace(auth[len("Bearer "):])
	if token == "" {
		return false
	}
	if s.shopToken != "" {
		return token == s.shopToken
	}
	return true
}

// writeJSON writes v as JSON with the given status. Headers are set
// first: application/json and a fresh request id on every reply.
func (s *server) writeJSON(w http.ResponseWriter, status int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "marshal failed")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("X-GitHub-Request-Id", s.requestID())
	w.WriteHeader(status)
	w.Write(data)
}

// writeRawJSON writes already-marshaled JSON bytes (cached issue and PR
// bodies) without re-marshaling.
func (s *server) writeRawJSON(w http.ResponseWriter, status int, data []byte) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("X-GitHub-Request-Id", s.requestID())
	w.WriteHeader(status)
	w.Write(data)
}

// writeErr writes a JSON {"message": ...} error reply.
func (s *server) writeErr(w http.ResponseWriter, status int, msg string) {
	s.writeJSON(w, status, jsonError{Message: msg})
}

// readBody reads a request body, bounded to 64 MB (packs are small).
func readBody(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	return io.ReadAll(io.LimitReader(r.Body, 64<<20))
}

// decodeJSON reads a request body as JSON into v. A malformed body is a
// 400 {"message": ...}; an empty body decodes as a zero value.
func (s *server) decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	body, err := readBody(r)
	if err != nil {
		s.writeErr(w, http.StatusBadRequest, "cannot read request body")
		return false
	}
	if len(body) == 0 {
		return true
	}
	if err := json.Unmarshal(body, v); err != nil {
		s.writeErr(w, http.StatusBadRequest, "malformed JSON body")
		return false
	}
	return true
}

// --- routing ---

// ServeHTTP dispatches every request: health and the API root need no
// auth; everything else requires a token. Git smart HTTP lives under
// /<owner>/<repo>.git; REST under /api/v3; GraphQL at /api/graphql;
// MCP at /mcp.
func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case path == "/healthz":
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	case path == "/api/v3" || path == "/api/v3/":
		w.Header().Set("X-OAuth-Scopes", "repo, read:org")
		s.writeJSON(w, http.StatusOK, map[string]any{
			"current_user_url":  "/api/v3/user",
			"repository_url":    "/api/v3/repos/{owner}/{repo}",
			"rate_limit_url":    "/api/v3/rate_limit",
			"message":           "mini-forge-v2",
			"documentation_url": "https://make.fun",
		})
		return
	case path == "/api/v3/meta":
		s.writeJSON(w, http.StatusOK, map[string]any{"installed_version": "1.0.0"})
		return
	}
	if !s.authorized(r) {
		s.writeErr(w, http.StatusUnauthorized, "requires authentication")
		return
	}
	switch {
	case path == "/api/graphql":
		s.handleGraphQL(w, r)
	case path == "/mcp":
		s.handleMCP(w, r)
	case strings.HasPrefix(path, "/api/v3/"):
		s.handleREST(w, r)
	case strings.HasSuffix(path, ".git/info/refs") || strings.HasSuffix(path, ".git/git-upload-pack") || strings.HasSuffix(path, ".git/git-receive-pack"):
		s.handleGit(w, r)
	default:
		s.writeErr(w, http.StatusNotFound, "not found")
	}
}

// requireRepo resolves owner/name to a repository the viewer may see:
// a missing or foreign repository is a 404, never a 403.
func (s *server) requireRepo(w http.ResponseWriter, owner, name string) *Repo {
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	r := s.store.getRepo(owner, name)
	if r == nil || r.isForeign() {
		s.writeErr(w, http.StatusNotFound, "repository not found")
		return nil
	}
	return r
}

// fmt.Sprintf helper kept short for the handlers.
func sprintf(format string, args ...any) string { return fmt.Sprintf(format, args...) }

// --- git smart HTTP ---

// handleGit serves /<owner>/<repo>.git/info/refs and the upload-pack /
// receive-pack endpoints. Missing repositories 404 and the server stays
// up; a malformed upload-pack POST is a 400, never a crash.
func (s *server) handleGit(w http.ResponseWriter, r *http.Request) {
	// Path shape: /<owner>/<repo>.git/<endpoint>, where the endpoint is
	// "info/refs" (two segments) or "git-upload-pack" / "git-receive-pack".
	segs := splitPath(r.URL.Path)
	if len(segs) < 3 || !strings.HasSuffix(segs[1], ".git") {
		s.writeErr(w, http.StatusNotFound, "not found")
		return
	}
	owner, repoName := segs[0], strings.TrimSuffix(segs[1], ".git")
	endpoint := strings.Join(segs[2:], "/")

	s.store.mu.RLock()
	repo := s.store.getRepo(owner, repoName)
	s.store.mu.RUnlock()
	if repo == nil || repo.isForeign() {
		s.writeErr(w, http.StatusNotFound, "repository not found")
		return
	}

	switch endpoint {
	case "info/refs":
		if r.Method != http.MethodGet {
			s.writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		service := r.URL.Query().Get("service")
		s.store.mu.RLock()
		var body []byte
		var ctype string
		if r.Header.Get("Git-Protocol") == "version=2" {
			body = s.store.advertisementV2(repo)
			ctype = "application/x-git-upload-pack-advertisement"
		} else if service == "git-receive-pack" {
			body = s.store.advertisementV0Recv(repo)
			ctype = "application/x-git-receive-pack-advertisement"
		} else {
			body = s.store.advertisementV0Up(repo)
			ctype = "application/x-git-upload-pack-advertisement"
		}
		s.store.mu.RUnlock()
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("X-GitHub-Request-Id", s.requestID())
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	case "git-upload-pack":
		if r.Method != http.MethodPost {
			s.writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		body, err := readBody(r)
		if err != nil {
			s.writeErr(w, http.StatusBadRequest, "cannot read request body")
			return
		}
		if r.Header.Get("Git-Protocol") == "version=2" {
			// ls-refs rides on the same endpoint with a different
			// first command line; detect it before parsing a fetch.
			if isLsRefsBody(body) {
				lsReq, lerr := parseLsRefs(body)
				if lerr != nil {
					s.writeErr(w, http.StatusBadRequest, "malformed ls-refs request")
					return
				}
				s.store.mu.RLock()
				out := s.store.lsRefsResponse(repo, lsReq)
				s.store.mu.RUnlock()
				w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
				w.Header().Set("X-GitHub-Request-Id", s.requestID())
				w.WriteHeader(http.StatusOK)
				w.Write(out)
				return
			}
			req, err := parseFetch(body)
			if err != nil {
				s.writeErr(w, http.StatusBadRequest, "malformed fetch request")
				return
			}
			s.store.mu.RLock()
			out := s.store.fetchResponseV2(repo, req)
			s.store.mu.RUnlock()
			w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
			w.Header().Set("X-GitHub-Request-Id", s.requestID())
			w.WriteHeader(http.StatusOK)
			w.Write(out)
			return
		}
		s.store.mu.RLock()
		out := s.store.fetchResponseV0(repo, body)
		s.store.mu.RUnlock()
		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		w.Header().Set("X-GitHub-Request-Id", s.requestID())
		w.WriteHeader(http.StatusOK)
		w.Write(out)
	case "git-receive-pack":
		if r.Method != http.MethodPost {
			s.writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		body, err := readBody(r)
		if err != nil {
			s.writeErr(w, http.StatusBadRequest, "cannot read request body")
			return
		}
		s.store.mu.Lock()
		out, perr := s.store.receivePack(repo, body)
		s.store.mu.Unlock()
		if perr != nil {
			s.writeErr(w, http.StatusBadRequest, perr.Error())
			return
		}
		w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
		w.Header().Set("X-GitHub-Request-Id", s.requestID())
		w.WriteHeader(http.StatusOK)
		w.Write(out)
	default:
		s.writeErr(w, http.StatusNotFound, "not found")
	}
}

// isLsRefsBody reports whether a v2 upload-pack body is an ls-refs
// request rather than a fetch: its first data line names ls-refs.
func isLsRefsBody(body []byte) bool {
	pkts, _, err := parsePkts(body)
	if err != nil {
		return false
	}
	for _, p := range pkts {
		if p.kind == pktData {
			return strings.HasPrefix(string(p.payload), "command=ls-refs")
		}
	}
	return false
}
