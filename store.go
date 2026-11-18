// Package main implements mini-forge-v2: a self-contained GitHub-core clone.
//
// One static Go binary serves git smart HTTP (upload-pack v2, receive-pack
// v0), REST v3, GraphQL and MCP from a single in-memory store. No git
// binary, no external service, standard library only. A thin node main.ts
// launcher spawns this binary; the binary owns the whole lifecycle
// (bind, register with the env, poll done, exit).
package main

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// The viewer identity is fixed by the public scenario seed
// (SCENARIO_SEED = 0x6a09e667f3bcc909, the same for practice and final):
// the viewer is always adaiversen. Any other owner 404s, never 403s.
const (
	viewerLogin = "adaiversen"
	viewerID    = 1000
	viewerName  = "ada iversen"
	viewerEmail = "adaiversen@example.invalid"
)

// zeroOID is the all-zero object id: an unborn branch's old/new value.
const zeroOID = "0000000000000000000000000000000000000000"

// baseTime is the deterministic commit timestamp base (seconds since epoch)
// used when MAKEFUN_TRIAL_NOW is unset.
const baseTime = 1700000000

// trialNow holds the trial timestamp (MAKEFUN_TRIAL_NOW when set, else
// baseTime). Commits use it so live and replay runs agree.
var trialNow int64 = baseTime

// isoTime formats a deterministic timestamp: trial base plus a monotonic
// sequence, so created/updated stamps are distinct and ordered without
// ever touching the wall clock.
func isoTime(seq int64) string {
	return time.Unix(trialNow+seq, 0).UTC().Format(time.RFC3339)
}

// hashObject returns the git object id (hex SHA-1) of "type size\0data".
func hashObject(typ string, data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", typ, len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// Object is one git object: its type and raw content (no "type size\0"
// header; the header is rebuilt when hashing or packing).
type Object struct {
	Type string
	Data []byte
}

// Label is a repository label. Colors are stored exactly as created.
type Label struct {
	Name  string
	Color string
}

// Comment is one issue comment.
type Comment struct {
	ID          int64
	Body        string
	User        string
	Repo        string
	IssueNumber int
	Seq         int64
}

// Issue is one repository issue. Numbers are shared with pull requests,
// GitHub-style, incrementing by one per repository. Repo names the
// repository that holds it, so a global id can find it back.
type Issue struct {
	ID        int64
	Number    int
	Title     string
	Body      string
	State     string // "open" | "closed"
	Labels    []*Label
	User      string
	Repo      string
	Seq       int64
	Comments  []*Comment
	cached    []byte // GET /issues/{n} response, invalidated on change
	cachedVer int64
}

// PullRequest is one repository pull request.
type PullRequest struct {
	ID        int64
	Number    int
	Title     string
	Body      string
	State     string // "open" | "closed"
	HeadRef   string
	BaseRef   string
	HeadSHA   string
	BaseSHA   string
	Merged    bool
	MergedAt  string
	MergeSHA  string
	Repo      string
	Seq       int64
	cached    []byte // GET /pulls/{n} response, invalidated on change
	cachedVer int64
}

// Repo is one repository: its git objects and refs plus its REST state.
// Version bumps on every object/ref change and invalidates the cached
// advertisements and fetch packs.
type Repo struct {
	ID            int64
	Name          string
	Owner         string
	Private       bool
	Description   string
	DefaultBranch string
	CreatedSeq    int64

	Objects map[string]*Object
	Refs    map[string]string // "refs/heads/main" -> commit oid
	Issues  map[int]*Issue
	PRs     map[int]*PullRequest
	Labels  map[string]*Label
	NextNum int

	Version      int64
	advV2        []byte
	advV2Ver     int64
	advV0Up      []byte
	advV0UpVer   int64
	advV0Recv    []byte
	advV0RecvVer int64
}

// Store is the whole forge: every repository, in creation order, behind
// one RWMutex. Handlers take the read lock for reads and the write lock
// for mutations; each operation is microseconds, so a single lock is
// cheaper than per-repo lock bookkeeping. packCache holds complete
// fetch answers by request fingerprint (see fetchCacheKey); objects are
// content-addressed, so an answer depends only on its request.
type Store struct {
	mu        sync.RWMutex
	repos     map[string]*Repo // "owner/name" -> repo
	order     []*Repo
	nextID    int64
	seq       int64
	packCache map[string][]byte
	packMu    sync.RWMutex
}

// newStore returns an empty store.
func newStore() *Store {
	return &Store{repos: make(map[string]*Repo), nextID: 1000, packCache: make(map[string][]byte)}
}

// nextSeq returns the next monotonic ordering sequence (store lock held).
func (s *Store) nextSeq() int64 {
	s.seq++
	return s.seq
}

// nextIDv returns the next global REST id (store lock held).
func (s *Store) nextIDv() int64 {
	s.nextID++
	return s.nextID
}

// keyOf builds the repository map key.
func keyOf(owner, name string) string { return owner + "/" + name }

// getRepo returns the repository for owner/name, or nil (lock held).
func (s *Store) getRepo(owner, name string) *Repo {
	return s.repos[keyOf(owner, name)]
}

// touch bumps a repository's version, invalidating cached advertisements
// and fetch packs (lock held).
func (s *Store) touch(r *Repo) { r.Version++ }

// putObject stores an object by its id when absent and returns the id
// (lock held). Byte-exact round-trip: the stored data is the object.
func (s *Store) putObject(r *Repo, typ string, data []byte) string {
	id := hashObject(typ, data)
	if _, ok := r.Objects[id]; !ok {
		r.Objects[id] = &Object{Type: typ, Data: data}
		s.touch(r)
	}
	return id
}

// sortedRefNames returns the repository's ref names in git order.
func (r *Repo) sortedRefNames() []string {
	names := make([]string, 0, len(r.Refs))
	for name := range r.Refs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// headOID returns the default branch tip, or "" when the repo is unborn.
func (r *Repo) headOID() string {
	if r.DefaultBranch == "" {
		return ""
	}
	return r.Refs["refs/heads/"+r.DefaultBranch]
}

// isForeign reports whether a repository belongs to someone other than
// the viewer; foreign repositories 404 exactly like missing ones.
func (r *Repo) isForeign() bool { return r.Owner != viewerLogin }

// --- small helpers shared by the handlers ---

// splitPath splits a URL path into its segments, dropping empty ones.
func splitPath(p string) []string {
	parts := strings.Split(p, "/")
	out := parts[:0]
	for _, part := range parts {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// isHexOID reports whether s is a 40-character lowercase hex object id.
func isHexOID(s string) bool {
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

// --- git trees and commits ---

// TreeEntry is one entry of a tree object.
type TreeEntry struct {
	Mode string // "40000" for directories, "100644"/"100755" for files
	Name string
	OID  string // hex object id
}

// parseTree decodes a tree object's raw content into its entries.
func parseTree(data []byte) []TreeEntry {
	var entries []TreeEntry
	for len(data) > 0 {
		sp := 0
		for sp < len(data) && data[sp] != ' ' {
			sp++
		}
		if sp >= len(data) {
			break
		}
		mode := string(data[:sp])
		rest := data[sp+1:]
		nul := 0
		for nul < len(rest) && rest[nul] != 0 {
			nul++
		}
		if nul+20 >= len(rest) {
			break
		}
		name := string(rest[:nul])
		oid := hex.EncodeToString(rest[nul+1 : nul+21])
		entries = append(entries, TreeEntry{Mode: mode, Name: name, OID: oid})
		data = rest[nul+21:]
	}
	return entries
}

// treeLess orders entries the way git sorts trees: directories compare
// as if their name had a trailing slash.
func treeLess(a, b TreeEntry) bool {
	an, bn := a.Name, b.Name
	if a.Mode == "40000" {
		an += "/"
	}
	if b.Mode == "40000" {
		bn += "/"
	}
	return an < bn
}

// buildTree encodes entries into a tree object's raw content, git-sorted.
func buildTree(entries []TreeEntry) []byte {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool { return treeLess(sorted[i], sorted[j]) })
	var buf []byte
	for _, e := range sorted {
		buf = append(buf, e.Mode...)
		buf = append(buf, ' ')
		buf = append(buf, e.Name...)
		buf = append(buf, 0)
		raw, _ := hex.DecodeString(e.OID)
		buf = append(buf, raw...)
	}
	return buf
}

// treeMap flattens a tree (by oid) into path -> blob oid, recursively.
func (s *Store) treeMap(r *Repo, treeOID string) map[string]string {
	out := make(map[string]string)
	s.treeMapInto(r, treeOID, "", out)
	return out
}

func (s *Store) treeMapInto(r *Repo, treeOID, prefix string, out map[string]string) {
	obj := r.Objects[treeOID]
	if obj == nil || obj.Type != "tree" {
		return
	}
	for _, e := range parseTree(obj.Data) {
		path := e.Name
		if prefix != "" {
			path = prefix + "/" + e.Name
		}
		if e.Mode == "40000" {
			s.treeMapInto(r, e.OID, path, out)
		} else {
			out[path] = e.OID
		}
	}
}

// buildTreeFromMap builds (and stores) the nested trees for a flat
// path -> blob oid map and returns the root tree oid.
func (s *Store) buildTreeFromMap(r *Repo, flat map[string]string) string {
	// Recursive builder: split the flat map by top-level directory.
	var makeTree func(paths map[string]string) string
	makeTree = func(paths map[string]string) string {
		entries := []TreeEntry{}
		byDir := make(map[string]map[string]string)
		for path, oid := range paths {
			if i := strings.IndexByte(path, '/'); i >= 0 {
				dir, rest := path[:i], path[i+1:]
				m, ok := byDir[dir]
				if !ok {
					m = make(map[string]string)
					byDir[dir] = m
				}
				m[rest] = oid
			} else {
				entries = append(entries, TreeEntry{Mode: "100644", Name: path, OID: oid})
			}
		}
		for dir, sub := range byDir {
			entries = append(entries, TreeEntry{Mode: "40000", Name: dir, OID: makeTree(sub)})
		}
		return s.putObject(r, "tree", buildTree(entries))
	}
	return makeTree(flat)
}

// Commit is a parsed commit object.
type Commit struct {
	Tree    string
	Parents []string
	Message string
}

// parseCommit decodes a commit object's raw content.
func parseCommit(data []byte) Commit {
	var c Commit
	text := string(data)
	head, msg, _ := strings.Cut(text, "\n\n")
	c.Message = strings.TrimSuffix(msg, "\n")
	for _, line := range strings.Split(head, "\n") {
		switch {
		case strings.HasPrefix(line, "tree "):
			c.Tree = strings.TrimSpace(line[5:])
		case strings.HasPrefix(line, "parent "):
			c.Parents = append(c.Parents, strings.TrimSpace(line[7:]))
		}
	}
	return c
}

// buildCommit returns the raw content of a commit object. Server-created
// commits carry the viewer as author and committer with the deterministic
// trial timestamp; the message is stored with one trailing newline.
func buildCommit(treeOID string, parents []string, message string) []byte {
	var b strings.Builder
	b.WriteString("tree " + treeOID + "\n")
	for _, p := range parents {
		b.WriteString("parent " + p + "\n")
	}
	ident := viewerName + " <" + viewerEmail + "> " + fmt.Sprint(trialNow) + " +0000"
	b.WriteString("author " + ident + "\n")
	b.WriteString("committer " + ident + "\n")
	b.WriteString("\n")
	b.WriteString(message)
	if !strings.HasSuffix(message, "\n") {
		b.WriteString("\n")
	}
	return []byte(b.String())
}

// commitChain returns the commits reachable from tip, tip first, walking
// parents breadth-first so nearer commits come first.
func (s *Store) commitChain(r *Repo, tip string) []string {
	var out []string
	seen := make(map[string]bool)
	queue := []string{tip}
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if oid == "" || seen[oid] {
			continue
		}
		seen[oid] = true
		obj := r.Objects[oid]
		if obj == nil || obj.Type != "commit" {
			continue
		}
		out = append(out, oid)
		c := parseCommit(obj.Data)
		queue = append(queue, c.Parents...)
	}
	return out
}

// isAncestor reports whether anc is reachable from desc (or equal).
func (s *Store) isAncestor(r *Repo, anc, desc string) bool {
	if anc == desc {
		return true
	}
	for _, oid := range s.commitChain(r, desc) {
		if oid == anc {
			return true
		}
	}
	return false
}

// mergeBase returns the first common ancestor of a and b, or "".
func (s *Store) mergeBase(r *Repo, a, b string) string {
	ancestors := make(map[string]bool)
	for _, oid := range s.commitChain(r, a) {
		ancestors[oid] = true
	}
	for _, oid := range s.commitChain(r, b) {
		if ancestors[oid] {
			return oid
		}
	}
	return ""
}

// closure collects the commits reachable from tips within depth levels
// (depth < 0 means unlimited). It returns the included commit set and,
// for a limited depth, the boundary commits (included commits whose
// parents were cut off by the depth limit).
func (s *Store) closure(r *Repo, tips []string, depth int) (map[string]bool, map[string]bool) {
	return s.closureCutoff(r, tips, depth, nil)
}

// closureCutoff is closure with a stop set: a commit in stopAt is
// included but its parents are never traversed. A shallow clone's
// boundary commits are exactly such cutoffs: the client holds the
// boundary commit but none of its ancestors, so a deepen must not
// treat the ancestors as already held.
func (s *Store) closureCutoff(r *Repo, tips []string, depth int, stopAt map[string]bool) (map[string]bool, map[string]bool) {
	included := make(map[string]bool)
	boundary := make(map[string]bool)
	type item struct {
		oid   string
		level int
	}
	queue := []item{}
	for _, t := range tips {
		if t != "" && t != zeroOID {
			queue = append(queue, item{t, 0})
		}
	}
	for len(queue) > 0 {
		it := queue[0]
		queue = queue[1:]
		if included[it.oid] {
			continue
		}
		obj := r.Objects[it.oid]
		if obj == nil || obj.Type != "commit" {
			continue
		}
		included[it.oid] = true
		c := parseCommit(obj.Data)
		if stopAt != nil && stopAt[it.oid] {
			continue
		}
		if depth >= 0 && it.level >= depth-1 {
			// Parents are cut off: this commit is a shallow boundary.
			if len(c.Parents) > 0 {
				boundary[it.oid] = true
			}
			continue
		}
		for _, p := range c.Parents {
			queue = append(queue, item{p, it.level + 1})
		}
	}
	return included, boundary
}

// treeOIDFromMap computes the root tree id of a flat path -> blob map
// without storing anything: the same content hashes to the same id, so
// a read path can answer a directory's tree id with no mutation.
func treeOIDFromMap(flat map[string]string) string {
	var makeTree func(paths map[string]string) string
	makeTree = func(paths map[string]string) string {
		entries := []TreeEntry{}
		byDir := make(map[string]map[string]string)
		for path, oid := range paths {
			if i := strings.IndexByte(path, '/'); i >= 0 {
				dir, rest := path[:i], path[i+1:]
				m, ok := byDir[dir]
				if !ok {
					m = make(map[string]string)
					byDir[dir] = m
				}
				m[rest] = oid
			} else {
				entries = append(entries, TreeEntry{Mode: "100644", Name: path, OID: oid})
			}
		}
		for dir, sub := range byDir {
			entries = append(entries, TreeEntry{Mode: "40000", Name: dir, OID: makeTree(sub)})
		}
		return hashObject("tree", buildTree(entries))
	}
	return makeTree(flat)
}
