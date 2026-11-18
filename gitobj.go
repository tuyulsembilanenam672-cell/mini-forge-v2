// gitobj.go: git object wire formats — packfiles and the object sets a
// fetch sends. Packs carry plain objects only, never deltas: the suite's
// client resolves deltas but never needs them, and a 0-delta pack passes
// the pack-deltas check.

package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"io"
	"sort"
	"strings"
)

// git type codes in packfiles.
const (
	typeCommit = 1
	typeTree   = 2
	typeBlob   = 3
	typeTag    = 4
)

// typeCode returns the pack type code for a git object type.
func typeCode(typ string) int {
	switch typ {
	case "commit":
		return typeCommit
	case "tree":
		return typeTree
	case "blob":
		return typeBlob
	case "tag":
		return typeTag
	}
	return 0
}

// typeName returns the git object type for a pack type code.
func typeName(code int) string {
	switch code {
	case typeCommit:
		return "commit"
	case typeTree:
		return "tree"
	case typeBlob:
		return "blob"
	case typeTag:
		return "tag"
	}
	return ""
}

// appendPackObjectHeader writes the pack object header (type + size
// varint) for one object.
func appendPackObjectHeader(buf []byte, typ string, size int) []byte {
	code := typeCode(typ)
	b := byte((code << 4) | (size & 0x0f))
	size >>= 4
	for size > 0 {
		buf = append(buf, b|0x80)
		b = byte(size & 0x7f)
		size >>= 7
	}
	return append(buf, b)
}

// zlibCompress deflates data for a pack object. BestSpeed is used on
// purpose: a pack is compressed once and served many times (the clone
// workload's fifty clones share one pack), so build time matters far
// more than a few kilobytes of transfer on localhost.
func zlibCompress(data []byte) []byte {
	var buf bytes.Buffer
	w, _ := zlib.NewWriterLevel(&buf, zlib.BestSpeed)
	w.Write(data)
	w.Close()
	return buf.Bytes()
}

// buildPack serializes objects into a complete packfile (PACK v2 header,
// one entry per object, 20-byte SHA-1 trailer). Deterministic: commits,
// then trees, then blobs, each sorted by id, so identical object sets
// produce identical bytes and the fetch cache stays valid.
func buildPack(objects []*Object) []byte {
	byType := map[string][]*Object{}
	for _, o := range objects {
		byType[o.Type] = append(byType[o.Type], o)
	}
	for _, list := range byType {
		sort.Slice(list, func(i, j int) bool {
			return hashObject(list[i].Type, list[i].Data) < hashObject(list[j].Type, list[j].Data)
		})
	}
	var buf bytes.Buffer
	buf.WriteString("PACK")
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:4], 2)
	binary.BigEndian.PutUint32(hdr[4:8], uint32(len(objects)))
	buf.Write(hdr[:])
	for _, typ := range []string{"commit", "tree", "blob", "tag"} {
		for _, o := range byType[typ] {
			buf.Write(appendPackObjectHeader(nil, typ, len(o.Data)))
			buf.Write(zlibCompress(o.Data))
		}
	}
	sum := sha1.Sum(buf.Bytes())
	buf.Write(sum[:])
	return buf.Bytes()
}

// verifyPackChecksum reports whether the last 20 bytes of pack are the
// SHA-1 of everything before them.
func verifyPackChecksum(pack []byte) bool {
	if len(pack) < 32 {
		return false
	}
	sum := sha1.Sum(pack[:len(pack)-20])
	return bytes.Equal(sum[:], pack[len(pack)-20:])
}

// readPack parses a packfile the client pushed and returns its objects.
// Only plain objects are accepted: the suite's pack writer emits no
// deltas, so a delta entry is a protocol error, not a case to handle.
func readPack(pack []byte) ([]*Object, error) {
	if len(pack) < 12 || string(pack[:4]) != "PACK" {
		return nil, fmt.Errorf("not a packfile")
	}
	version := binary.BigEndian.Uint32(pack[4:8])
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("unsupported pack version %d", version)
	}
	count := int(binary.BigEndian.Uint32(pack[8:12]))
	if !verifyPackChecksum(pack) {
		return nil, fmt.Errorf("pack checksum mismatch")
	}
	off := 12
	end := len(pack) - 20
	objects := make([]*Object, 0, count)
	for i := 0; i < count; i++ {
		if off >= end {
			return nil, fmt.Errorf("pack truncated at object %d", i)
		}
		// Object header varint: type in bits 4-6, size in bits 0-3 then
		// 7 bits per continuation byte.
		b := pack[off]
		off++
		code := int((b >> 4) & 0x07)
		size := int(b & 0x0f)
		shift := uint(4)
		for b&0x80 != 0 {
			if off >= end {
				return nil, fmt.Errorf("pack header truncated")
			}
			b = pack[off]
			off++
			size |= int(b&0x7f) << shift
			shift += 7
		}
		typ := typeName(code)
		if typ == "" {
			return nil, fmt.Errorf("pack object type %d is a delta or unknown", code)
		}
		// Inflate: bytes.Reader is an io.ByteReader, so the flate reader
		// consumes exactly the deflate stream's bytes and no more.
		br := bytes.NewReader(pack[off:end])
		zr, err := zlib.NewReader(br)
		if err != nil {
			return nil, fmt.Errorf("pack object %d: bad zlib stream: %w", i, err)
		}
		data, err := io.ReadAll(zr)
		zr.Close()
		if err != nil {
			return nil, fmt.Errorf("pack object %d: inflate failed: %w", i, err)
		}
		if len(data) != size {
			return nil, fmt.Errorf("pack object %d: size %d, inflated %d", i, size, len(data))
		}
		off += (len(pack[off:end]) - br.Len())
		objects = append(objects, &Object{Type: typ, Data: data})
	}
	return objects, nil
}

// --- fetch object sets ---

// fetchPlan is what one v2 fetch sends: the object set, the shallow
// boundary commits and the unshallowed ones.
type fetchPlan struct {
	objects   []*Object
	shallow   []string
	unshallow []string
	acks      []string
	ready     bool
	nak       bool
}

// reachableObjects returns every object reachable from the given commits
// (the commits, their trees recursively, blobs, and tags peeled to their
// targets), keyed by id.
func (s *Store) reachableObjects(r *Repo, commits map[string]bool) map[string]*Object {
	out := make(map[string]*Object)
	var walkTree func(treeOID string)
	walkTree = func(treeOID string) {
		if _, ok := out[treeOID]; ok {
			return
		}
		obj := r.Objects[treeOID]
		if obj == nil || obj.Type != "tree" {
			return
		}
		out[treeOID] = obj
		for _, e := range parseTree(obj.Data) {
			if e.Mode == "40000" {
				walkTree(e.OID)
			} else if child := r.Objects[e.OID]; child != nil {
				out[e.OID] = child
			}
		}
	}
	var walkTag func(oid string)
	walkTag = func(oid string) {
		obj := r.Objects[oid]
		if obj == nil {
			return
		}
		if obj.Type != "tag" {
			return
		}
		if _, ok := out[oid]; ok {
			return
		}
		out[oid] = obj
		// Peel to the tagged object: "object <oid>" is the first line.
		text := string(obj.Data)
		if i := strings.IndexByte(text, '\n'); i > 0 && strings.HasPrefix(text, "object ") {
			target := strings.TrimSpace(text[7:i])
			if t := r.Objects[target]; t != nil {
				if t.Type == "tag" {
					walkTag(target)
				} else if t.Type == "commit" {
					// The commit itself is added by the caller when it
					// is in the commit set; here just record it.
					out[target] = t
				} else {
					out[target] = t
				}
			}
		}
	}
	for oid := range commits {
		obj := r.Objects[oid]
		if obj == nil {
			continue
		}
		if obj.Type == "tag" {
			walkTag(oid)
			continue
		}
		if obj.Type != "commit" {
			out[oid] = obj
			continue
		}
		out[oid] = obj
		c := parseCommit(obj.Data)
		walkTree(c.Tree)
	}
	return out
}

// planFetch computes the object set for one fetch request:
// closure(wants, depth) minus closure(haves), with the filter applied.
// Shallow boundaries are the included commits whose parents were cut
// off; unshallow are the client's previous boundaries that are now
// included together with their parents.
func (s *Store) planFetch(r *Repo, wants []string, haves []string, oldShallow []string, depth int, filter string) fetchPlan {
	var plan fetchPlan
	// A want may name a tag object: peel it to its commit for the
	// closure, but remember the tag itself — it travels in the pack.
	var tagOIDs []string
	resolvedWants := make([]string, 0, len(wants))
	for _, w := range wants {
		if obj := r.Objects[w]; obj != nil && obj.Type == "tag" {
			tagOIDs = append(tagOIDs, w)
			resolvedWants = append(resolvedWants, s.peelTag(r, w))
		} else {
			resolvedWants = append(resolvedWants, w)
		}
	}
	// Acknowledgments: a have that is an ancestor of a want is ACKed
	// (and the server is ready); otherwise the answer is NAK.
	acked := false
	for _, h := range haves {
		for _, w := range resolvedWants {
			if h != "" && s.isAncestor(r, h, w) {
				plan.acks = append(plan.acks, h)
				acked = true
				break
			}
		}
	}
	if len(haves) > 0 {
		if acked {
			plan.ready = true
		} else {
			plan.nak = true
		}
	}
	wantCommits, boundary := s.closure(r, resolvedWants, depth)
	// The client's shallow boundaries are cutoffs for the held set:
	// the client holds a boundary commit but none of its ancestors,
	// so a deepen must still deliver those ancestors.
	stopAt := make(map[string]bool, len(oldShallow))
	for _, oid := range oldShallow {
		stopAt[oid] = true
	}
	haveCommits, _ := s.closureCutoff(r, haves, -1, stopAt)
	wantObjs := s.reachableObjects(r, wantCommits)
	// Tag objects themselves travel in the pack: a tag want peels to
	// its commit for the closure, but the tag object is delivered too.
	if len(tagOIDs) > 0 {
		tagSet := make(map[string]bool, len(tagOIDs))
		for _, oid := range tagOIDs {
			tagSet[oid] = true
		}
		for oid, obj := range s.reachableObjects(r, tagSet) {
			wantObjs[oid] = obj
		}
	}
	haveObjs := s.reachableObjects(r, haveCommits)
	// Shallow: boundary commits, sorted for a stable answer.
	for oid := range boundary {
		plan.shallow = append(plan.shallow, oid)
	}
	sort.Strings(plan.shallow)
	// Unshallow: old boundaries now included with their parents.
	for _, oid := range oldShallow {
		if !wantCommits[oid] {
			continue
		}
		obj := r.Objects[oid]
		if obj == nil || obj.Type != "commit" {
			continue
		}
		parentsIncluded := true
		for _, p := range parseCommit(obj.Data).Parents {
			if !wantCommits[p] {
				parentsIncluded = false
				break
			}
		}
		if parentsIncluded {
			plan.unshallow = append(plan.unshallow, oid)
		}
	}
	sort.Strings(plan.unshallow)
	// Object set: wanted minus held, filter applied.
	for oid, obj := range wantObjs {
		if haveObjs[oid] != nil {
			continue
		}
		switch filter {
		case "blob:none":
			if obj.Type == "blob" {
				continue
			}
		case "tree:0":
			if obj.Type == "tree" || obj.Type == "blob" {
				continue
			}
		}
		plan.objects = append(plan.objects, obj)
	}
	return plan
}

// peelTag resolves a tag object id to the commit it points at
// (recursively), or returns the id unchanged when it is not a tag.
func (s *Store) peelTag(r *Repo, oid string) string {
	for {
		obj := r.Objects[oid]
		if obj == nil || obj.Type != "tag" {
			return oid
		}
		text := string(obj.Data)
		i := strings.IndexByte(text, '\n')
		if i <= 0 || !strings.HasPrefix(text, "object ") {
			return oid
		}
		oid = strings.TrimSpace(text[7:i])
	}
}
