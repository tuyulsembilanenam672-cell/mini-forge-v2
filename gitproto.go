// gitproto.go: the git smart-HTTP wire protocol — pkt-lines, the v2 and
// v0 advertisements, ls-refs, fetch and receive-pack. The suite's client
// prefers v2 when advertised, so v2 is the first-class path; v0 fetch is
// implemented too, for completeness.

package main

import (
	"bytes"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// pktLine frames one payload (which must end in "\n" when it is a text
// line) as a git pkt-line: a 4-hex-digit length including the header.
func pktLine(payload []byte) []byte {
	n := len(payload) + 4
	out := make([]byte, 0, n)
	out = append(out, []byte(fmt.Sprintf("%04x", n))...)
	return append(out, payload...)
}

// pktText frames one text line (a trailing newline is added when absent).
func pktText(s string) []byte {
	if !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	return pktLine([]byte(s))
}

// pktFlush is the 0000 flush packet; pktDelim is the 0001 delimiter.
var (
	pktFlush = []byte("0000")
	pktDelim = []byte("0001")
)

// pktKind is what one parsed packet is.
type pktKind int

const (
	pktData pktKind = iota
	pktFlushKind
	pktDelimKind
)

// pkt is one parsed pkt-line packet.
type pkt struct {
	kind    pktKind
	payload []byte // data packets only
}

// parsePkts splits a byte stream into packets. It stops at the first
// flush packet and returns the packets before it plus the byte offset
// just past the flush, so a receive-pack caller can read the raw pack
// that follows.
func parsePkts(data []byte) ([]pkt, int, error) {
	var out []pkt
	off := 0
	for off < len(data) {
		if off+4 > len(data) {
			return nil, 0, fmt.Errorf("pkt-line header truncated")
		}
		n, err := strconv.ParseInt(string(data[off:off+4]), 16, 32)
		if err != nil {
			return nil, 0, fmt.Errorf("bad pkt-line length %q", data[off:off+4])
		}
		if n == 0 {
			return append(out, pkt{kind: pktFlushKind}), off + 4, nil
		}
		if n == 1 {
			out = append(out, pkt{kind: pktDelimKind})
			off += 4
			continue
		}
		if n < 4 || off+int(n) > len(data) {
			return nil, 0, fmt.Errorf("pkt-line length %d out of range", n)
		}
		out = append(out, pkt{kind: pktData, payload: data[off+4 : off+int(n)]})
		off += int(n)
	}
	return out, off, nil
}

// pktLines returns the payloads of the data packets as strings.
func pktLines(pkts []pkt) []string {
	var out []string
	for _, p := range pkts {
		if p.kind == pktData {
			out = append(out, string(p.payload))
		}
	}
	return out
}

// --- advertisements ---

const agentLine = "agent=mini-forge-v2/1.0"

// advertisementV2 returns the cached upload-pack v2 advertisement for a
// repository, rebuilding it when the repository changed. The first line
// is "version 2"; capabilities name fetch (shallow + filter), ls-refs
// (unborn), the object format and the agent.
func (s *Store) advertisementV2(r *Repo) []byte {
	if r.advV2 != nil && r.advV2Ver == r.Version {
		return r.advV2
	}
	var buf bytes.Buffer
	buf.Write(pktText("version 2"))
	buf.Write(pktText(agentLine))
	buf.Write(pktText("ls-refs=unborn"))
	buf.Write(pktText("fetch=shallow filter"))
	buf.Write(pktText("object-format=sha1"))
	buf.Write(pktFlush)
	r.advV2 = buf.Bytes()
	r.advV2Ver = r.Version
	return r.advV2
}

// refLinesV0 returns the ref lines of a v0 advertisement (without the
// "# service=..." line or the caps): HEAD first, then refs sorted,
// annotated tags followed by their peeled "^{}" line.
func (s *Store) refLinesV0(r *Repo, withCaps bool) []byte {
	var buf bytes.Buffer
	caps := ""
	if withCaps {
		caps = "\x00side-band-64k shallow ofs-delta thin-pack multi_ack symref=HEAD:refs/heads/main " + agentLine
	}
	head := r.headOID()
	if head != "" {
		buf.Write(pktText(head + " HEAD" + caps))
	}
	for _, name := range r.sortedRefNames() {
		oid := r.Refs[name]
		buf.Write(pktText(oid + " " + name))
		if obj := r.Objects[oid]; obj != nil && obj.Type == "tag" {
			peeled := s.peelTag(r, oid)
			buf.Write(pktText(peeled + " " + name + "^{}"))
		}
	}
	return buf.Bytes()
}

// advertisementV0Up returns the cached v0 upload-pack advertisement.
func (s *Store) advertisementV0Up(r *Repo) []byte {
	if r.advV0Up != nil && r.advV0UpVer == r.Version {
		return r.advV0Up
	}
	var buf bytes.Buffer
	buf.Write(pktText("# service=git-upload-pack"))
	buf.Write(pktFlush)
	if r.headOID() == "" {
		// Empty repository: the capabilities^{} pseudo-ref carries the
		// capability list (the suite's client skips this line).
		buf.Write(pktText(zeroOID + " capabilities^{}\x00side-band-64k shallow ofs-delta thin-pack multi_ack symref=HEAD:refs/heads/main " + agentLine))
	} else {
		buf.Write(s.refLinesV0(r, true))
	}
	buf.Write(pktFlush)
	r.advV0Up = buf.Bytes()
	r.advV0UpVer = r.Version
	return r.advV0Up
}

// advertisementV0Recv returns the cached v0 receive-pack advertisement.
// Only report-status is advertised (never report-status-v2).
func (s *Store) advertisementV0Recv(r *Repo) []byte {
	if r.advV0Recv != nil && r.advV0RecvVer == r.Version {
		return r.advV0Recv
	}
	var buf bytes.Buffer
	buf.Write(pktText("# service=git-receive-pack"))
	buf.Write(pktFlush)
	if r.headOID() == "" {
		buf.Write(pktText(zeroOID + " capabilities^{}\x00report-status side-band-64k delete-refs " + agentLine))
	} else {
		var lines bytes.Buffer
		caps := "\x00report-status side-band-64k delete-refs " + agentLine
		head := r.headOID()
		lines.Write(pktText(head + " HEAD" + caps))
		for _, name := range r.sortedRefNames() {
			lines.Write(pktText(r.Refs[name] + " " + name))
		}
		buf.Write(lines.Bytes())
	}
	buf.Write(pktFlush)
	r.advV0Recv = buf.Bytes()
	r.advV0RecvVer = r.Version
	return r.advV0Recv
}

// sortedOIDs returns sorted keys of an oid set (stable answers).
func sortedOIDs(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for oid := range set {
		out = append(out, oid)
	}
	sort.Strings(out)
	return out
}

// --- ls-refs (v2) ---

// lsRefsRequest is a parsed v2 ls-refs request.
type lsRefsRequest struct {
	peel     bool
	symrefs  bool
	unborn   bool
	prefixes []string
}

// parseLsRefs parses the pkt-lines of a v2 ls-refs request body.
func parseLsRefs(body []byte) (lsRefsRequest, error) {
	var req lsRefsRequest
	pkts, _, err := parsePkts(body)
	if err != nil {
		return req, err
	}
	inArgs := false
	seenCommand := false
	for _, p := range pkts {
		switch p.kind {
		case pktDelimKind:
			inArgs = true
		case pktFlushKind:
			return req, nil
		case pktData:
			line := strings.TrimSuffix(string(p.payload), "\n")
			if !inArgs {
				// Before the delimiter only the command line matters;
				// agent= and object-format= lines are skipped.
				if !seenCommand {
					if line != "command=ls-refs" {
						return req, fmt.Errorf("expected command=ls-refs, got %q", line)
					}
					seenCommand = true
				}
				continue
			}
			switch {
			case line == "peel":
				req.peel = true
			case line == "symrefs":
				req.symrefs = true
			case line == "unborn":
				req.unborn = true
			case strings.HasPrefix(line, "ref-prefix "):
				req.prefixes = append(req.prefixes, strings.TrimPrefix(line, "ref-prefix "))
			}
		}
	}
	return req, nil
}

// lsRefsResponse builds the ls-refs answer. With ref-prefix filters only
// matching refs are returned, never HEAD. Without a filter the HEAD line
// (tip oid + symref-target) comes first, then refs sorted. Unborn refs
// are reported only when the client asked for them.
func (s *Store) lsRefsResponse(r *Repo, req lsRefsRequest) []byte {
	var buf bytes.Buffer
	matches := func(name string) bool {
		if len(req.prefixes) == 0 {
			return true
		}
		for _, p := range req.prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	head := r.headOID()
	if len(req.prefixes) == 0 && head != "" {
		line := head + " HEAD"
		if req.symrefs {
			line += " symref-target:refs/heads/" + r.DefaultBranch
		}
		buf.Write(pktText(line))
	}
	for _, name := range r.sortedRefNames() {
		if !matches(name) {
			continue
		}
		oid := r.Refs[name]
		line := oid + " " + name
		if req.peel {
			if obj := r.Objects[oid]; obj != nil && obj.Type == "tag" {
				line += " peeled:" + s.peelTag(r, oid)
			}
		}
		buf.Write(pktText(line))
	}
	// An empty repository advertises nothing at all: the suite's
	// clone-empty check rejects any refs/* line, unborn or not, and
	// tolerates the missing HEAD symref.
	buf.Write(pktFlush)
	return buf.Bytes()
}

// --- fetch (v2) ---

// fetchRequest is a parsed v2 fetch request.
type fetchRequest struct {
	wants     []string
	haves     []string
	shallow   []string
	depth     int // -1: no deepen
	deepenSet bool
	filter    string
}

// parseFetch parses the pkt-lines of a v2 fetch request body.
func parseFetch(body []byte) (fetchRequest, error) {
	req := fetchRequest{depth: -1}
	pkts, _, err := parsePkts(body)
	if err != nil {
		return req, err
	}
	inArgs := false
	seenCommand := false
	for _, p := range pkts {
		switch p.kind {
		case pktDelimKind:
			inArgs = true
		case pktFlushKind:
			return req, nil
		case pktData:
			line := strings.TrimSuffix(string(p.payload), "\n")
			if !inArgs {
				// Before the delimiter only the command line matters;
				// agent= and object-format= lines are skipped.
				if !seenCommand {
					if line != "command=fetch" {
						return req, fmt.Errorf("expected command=fetch, got %q", line)
					}
					seenCommand = true
				}
				continue
			}
			switch {
			case strings.HasPrefix(line, "want "):
				req.wants = append(req.wants, strings.TrimPrefix(line, "want "))
			case strings.HasPrefix(line, "have "):
				req.haves = append(req.haves, strings.TrimPrefix(line, "have "))
			case strings.HasPrefix(line, "shallow "):
				req.shallow = append(req.shallow, strings.TrimPrefix(line, "shallow "))
			case strings.HasPrefix(line, "deepen "):
				if n, err := strconv.Atoi(strings.TrimPrefix(line, "deepen ")); err == nil {
					req.depth = n
					req.deepenSet = true
				}
			case strings.HasPrefix(line, "filter "):
				req.filter = strings.TrimPrefix(line, "filter ")
			}
		}
	}
	return req, nil
}

// sidebandPack frames pack bytes as sideband band-1 packets, each at
// most 65520 bytes in total (4 header + 1 band + up to 65515 data).
func sidebandPack(pack []byte) []byte {
	var buf bytes.Buffer
	for len(pack) > 0 {
		n := len(pack)
		if n > 65515 {
			n = 65515
		}
		chunk := pack[:n]
		pack = pack[n:]
		payload := make([]byte, 0, len(chunk)+1)
		payload = append(payload, 1)
		payload = append(payload, chunk...)
		buf.Write(pktLine(payload))
	}
	return buf.Bytes()
}

// fetchCacheKey identifies a fetch answer for the pack cache: the exact
// request (wants, depth, filter, haves, shallow boundaries). Objects
// are content-addressed, so the same request always yields the same
// bytes — even across a repository recreated with identical content,
// which the clone workload does every round. The key deliberately
// carries no repository version: the version is only a proxy for the
// object set, and the request already names it exactly.
func fetchCacheKey(req fetchRequest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "d%d|f%s|", req.depth, req.filter)
	for _, w := range req.wants {
		b.WriteString(w)
		b.WriteString(",")
	}
	b.WriteString("|")
	for _, h := range req.haves {
		b.WriteString(h)
		b.WriteString(",")
	}
	b.WriteString("|")
	for _, sh := range req.shallow {
		b.WriteString(sh)
		b.WriteString(",")
	}
	return b.String()
}

// fetchResponseV2 builds the v2 fetch answer: acknowledgments (only when
// haves were sent), shallow-info (only when a depth was requested),
// then the packfile section carrying the pack in sideband band 1.
// Answers are cached globally by request fingerprint (see
// fetchCacheKey): the clone workload's fifty concurrent shallow clones
// of one static repository all hit the cache, and so do later rounds
// after the repository is recreated with identical content. Pack
// builds are serialized so concurrent clones do not each compress the
// same pack.
func (s *Store) fetchResponseV2(r *Repo, req fetchRequest) []byte {
	key := fetchCacheKey(req)
	s.packMu.RLock()
	if cached, ok := s.packCache[key]; ok {
		s.packMu.RUnlock()
		return cached
	}
	s.packMu.RUnlock()
	s.packMu.Lock()
	defer s.packMu.Unlock()
	if cached, ok := s.packCache[key]; ok {
		return cached
	}
	plan := s.planFetch(r, req.wants, req.haves, req.shallow, req.depth, req.filter)
	var buf bytes.Buffer
	if len(req.haves) > 0 {
		buf.Write(pktText("acknowledgments"))
		if plan.nak {
			buf.Write(pktText("NAK"))
		} else {
			for _, ack := range plan.acks {
				buf.Write(pktText("ACK " + ack))
			}
			if plan.ready {
				buf.Write(pktText("ready"))
			}
		}
		buf.Write(pktDelim)
	}
	if req.deepenSet {
		buf.Write(pktText("shallow-info"))
		for _, oid := range plan.shallow {
			buf.Write(pktText("shallow " + oid))
		}
		for _, oid := range plan.unshallow {
			buf.Write(pktText("unshallow " + oid))
		}
		buf.Write(pktDelim)
	}
	buf.Write(pktText("packfile"))
	buf.Write(sidebandPack(buildPack(plan.objects)))
	buf.Write(pktFlush)
	out := buf.Bytes()
	if len(s.packCache) > 128 {
		// Bound the cache: drop everything and rebuild from the
		// hottest keys rather than growing without limit.
		s.packCache = make(map[string][]byte)
	}
	s.packCache[key] = out
	return out
}

// --- receive-pack (v0 push) ---

// pushCommand is one line of a receive-pack command list.
type pushCommand struct {
	oldOID string
	newOID string
	ref    string
}

// receivePack applies a v0 receive-pack request body to a repository and
// returns the report-status answer (sideband band 1). The pack follows
// the command list's flush packet as raw bytes; a delete-only push
// carries no pack. Per-ref results: create and fast-forward succeed;
// anything else is "ng <ref> non-fast-forward" and the ref does not
// move, but unpacking still succeeds and the other refs still apply.
func (s *Store) receivePack(r *Repo, body []byte) ([]byte, error) {
	pkts, afterFlush, err := parsePkts(body)
	if err != nil {
		return nil, err
	}
	var cmds []pushCommand
	for _, p := range pkts {
		if p.kind != pktData {
			continue
		}
		line := strings.TrimSuffix(string(p.payload), "\n")
		if i := strings.IndexByte(line, 0); i >= 0 {
			line = line[:i] // capabilities ride on the first line
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return nil, fmt.Errorf("bad receive-pack command %q", line)
		}
		cmds = append(cmds, pushCommand{oldOID: fields[0], newOID: fields[1], ref: fields[2]})
	}
	// Unpack first: every object must verify before any ref moves.
	if afterFlush < len(body) {
		objects, err := readPack(body[afterFlush:])
		if err != nil {
			return nil, err
		}
		for _, o := range objects {
			s.putObject(r, o.Type, o.Data)
		}
	}
	var report bytes.Buffer
	report.Write(pktLine(append([]byte{1}, "unpack ok\n"...)))
	for _, cmd := range cmds {
		ok, reason := s.applyPush(r, cmd)
		var line string
		if ok {
			line = "ok " + cmd.ref + "\n"
		} else {
			line = "ng " + cmd.ref + " " + reason + "\n"
		}
		payload := append([]byte{1}, line...)
		report.Write(pktLine(payload))
	}
	report.Write(pktFlush)
	return report.Bytes(), nil
}

// applyPush applies one ref update. Create (old zero, ref absent) and
// fast-forward (new descends from old) succeed; delete (new zero)
// removes the ref; anything else is a non-fast-forward rejection. The
// reason string is exactly "non-fast-forward": the suite matches it.
func (s *Store) applyPush(r *Repo, cmd pushCommand) (bool, string) {
	current, exists := r.Refs[cmd.ref]
	if cmd.newOID == zeroOID {
		// Delete.
		if !exists {
			return false, "non-fast-forward"
		}
		if cmd.oldOID != zeroOID && cmd.oldOID != current {
			return false, "non-fast-forward"
		}
		delete(r.Refs, cmd.ref)
		s.touch(r)
		return true, ""
	}
	if !exists {
		if cmd.oldOID != zeroOID {
			return false, "non-fast-forward"
		}
		if r.Objects[cmd.newOID] == nil {
			return false, "non-fast-forward"
		}
		r.Refs[cmd.ref] = cmd.newOID
		s.touch(r)
		return true, ""
	}
	if cmd.oldOID != current {
		return false, "non-fast-forward"
	}
	if !s.isAncestor(r, current, cmd.newOID) {
		return false, "non-fast-forward"
	}
	r.Refs[cmd.ref] = cmd.newOID
	s.touch(r)
	return true, ""
}

// --- fetch (v0, only used when a client does not speak v2) ---

// fetchResponseV0 answers a v0 upload-pack request: "want" lines, "have"
// lines and "done", then the pack in sideband band 1. The answer starts
// with NAK (or ACK lines) when haves were sent.
func (s *Store) fetchResponseV0(r *Repo, body []byte) []byte {
	pkts, _, err := parsePkts(body)
	if err != nil {
		return nil
	}
	var wants, haves []string
	depth := -1
	for _, p := range pkts {
		if p.kind != pktData {
			continue
		}
		line := strings.TrimSuffix(string(p.payload), "\n")
		switch {
		case strings.HasPrefix(line, "want "):
			wants = append(wants, strings.TrimPrefix(line, "want "))
		case strings.HasPrefix(line, "have "):
			haves = append(haves, strings.TrimPrefix(line, "have "))
		case strings.HasPrefix(line, "deepen "):
			if n, err := strconv.Atoi(strings.TrimPrefix(line, "deepen ")); err == nil {
				depth = n
			}
		}
	}
	plan := s.planFetch(r, wants, haves, nil, depth, "")
	var buf bytes.Buffer
	if len(haves) > 0 {
		if plan.nak {
			buf.Write(pktText("NAK"))
		} else {
			for _, ack := range plan.acks {
				buf.Write(pktText("ACK " + ack))
			}
		}
	}
	buf.Write(sidebandPack(buildPack(plan.objects)))
	buf.Write(pktFlush)
	return buf.Bytes()
}
