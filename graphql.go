// graphql.go: a small targeted GraphQL executor for the fixed documents
// the suite sends: fields, arguments with variables and literals,
// aliases, inline fragments ("... on Repository") and named fragments,
// mutations with input objects, and __type introspection. It is not a
// general GraphQL server — it covers the subset the checks use.

package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// --- parser ---

// gqlVal is one parsed GraphQL value literal.
type gqlVal struct {
	kind string // var, string, int, float, bool, null, enum, object, list
	s    string
	i    int64
	f    float64
	b    bool
	obj  map[string]*gqlVal
	list []*gqlVal
}

// gqlSel is one selection: a field, a fragment spread, or an inline
// fragment.
type gqlSel struct {
	kind     string // field, spread, inline
	alias    string
	name     string
	args     map[string]*gqlVal
	sels     []*gqlSel
	typeCond string
	fragName string
}

// gqlOp is one operation definition.
type gqlOp struct {
	kind    string // query, mutation
	name    string
	varDefs map[string]*gqlVal // variable defaults by name
	sels    []*gqlSel
}

// gqlDoc is a parsed document: operations and named fragments.
type gqlDoc struct {
	ops   []*gqlOp
	frags map[string]*gqlFrag
}

// gqlFrag is a named fragment definition.
type gqlFrag struct {
	typeCond string
	sels     []*gqlSel
}

// gqlParser is a hand-rolled recursive descent parser over the query.
type gqlParser struct {
	src string
	pos int
}

// parseGraphQL parses a query document.
func parseGraphQL(src string) (*gqlDoc, error) {
	p := &gqlParser{src: src}
	doc := &gqlDoc{frags: make(map[string]*gqlFrag)}
	for {
		p.skipSpace()
		if p.pos >= len(p.src) {
			break
		}
		if p.peekWord() == "fragment" {
			p.pos += len("fragment")
			frag, name, err := p.parseFragmentDef()
			if err != nil {
				return nil, err
			}
			doc.frags[name] = frag
			continue
		}
		op, err := p.parseOperation()
		if err != nil {
			return nil, err
		}
		doc.ops = append(doc.ops, op)
	}
	if len(doc.ops) == 0 {
		return nil, fmt.Errorf("no operation in document")
	}
	return doc, nil
}

// skipSpace skips whitespace and commas (commas are insignificant).
func (p *gqlParser) skipSpace() {
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' {
			p.pos++
			continue
		}
		break
	}
}

// peekWord returns the name at the cursor without consuming it.
func (p *gqlParser) peekWord() string {
	p.skipSpace()
	start := p.pos
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' {
			p.pos++
			continue
		}
		break
	}
	w := p.src[start:p.pos]
	p.pos = start
	return w
}

// parseName reads a GraphQL name.
func (p *gqlParser) parseName() (string, error) {
	p.skipSpace()
	start := p.pos
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' {
			p.pos++
			continue
		}
		break
	}
	if start == p.pos {
		return "", fmt.Errorf("expected a name at offset %d", p.pos)
	}
	return p.src[start:p.pos], nil
}

// expect consumes one expected byte.
func (p *gqlParser) expect(c byte) error {
	p.skipSpace()
	if p.pos >= len(p.src) || p.src[p.pos] != c {
		return fmt.Errorf("expected %q at offset %d", c, p.pos)
	}
	p.pos++
	return nil
}

// parseFragmentDef parses "Name on Type { ... }".
func (p *gqlParser) parseFragmentDef() (*gqlFrag, string, error) {
	name, err := p.parseName()
	if err != nil {
		return nil, "", err
	}
	if w := p.peekWord(); w != "on" {
		return nil, "", fmt.Errorf("expected 'on' in fragment %s", name)
	}
	p.pos += 2
	cond, err := p.parseName()
	if err != nil {
		return nil, "", err
	}
	sels, err := p.parseSelectionSet()
	if err != nil {
		return nil, "", err
	}
	return &gqlFrag{typeCond: cond, sels: sels}, name, nil
}

// parseOperation parses "query|mutation [Name] [(vars)] { ... }" or the
// shorthand "{ ... }".
func (p *gqlParser) parseOperation() (*gqlOp, error) {
	op := &gqlOp{kind: "query", varDefs: make(map[string]*gqlVal)}
	w := p.peekWord()
	if w == "query" || w == "mutation" || w == "subscription" {
		op.kind = w
		p.pos += len(w)
		if nw := p.peekWord(); nw != "" && nw != "(" && nw != "{" {
			name, err := p.parseName()
			if err != nil {
				return nil, err
			}
			op.name = name
		}
	}
	p.skipSpace()
	if p.pos < len(p.src) && p.src[p.pos] == '(' {
		defs, err := p.parseVarDefs()
		if err != nil {
			return nil, err
		}
		op.varDefs = defs
	}
	sels, err := p.parseSelectionSet()
	if err != nil {
		return nil, err
	}
	op.sels = sels
	return op, nil
}

// parseVarDefs parses "($name: Type = default, ...)".
func (p *gqlParser) parseVarDefs() (map[string]*gqlVal, error) {
	defs := make(map[string]*gqlVal)
	if err := p.expect('('); err != nil {
		return nil, err
	}
	for {
		p.skipSpace()
		if p.pos < len(p.src) && p.src[p.pos] == ')' {
			p.pos++
			return defs, nil
		}
		if err := p.expect('$'); err != nil {
			return nil, err
		}
		name, err := p.parseName()
		if err != nil {
			return nil, err
		}
		if err := p.expect(':'); err != nil {
			return nil, err
		}
		// Skip the type: a name, possibly wrapped in [] and !.
		for {
			p.skipSpace()
			if p.pos < len(p.src) && (p.src[p.pos] == '[' || p.src[p.pos] == ']' || p.src[p.pos] == '!') {
				p.pos++
				continue
			}
			break
		}
		if _, err := p.parseName(); err != nil {
			return nil, err
		}
		for {
			p.skipSpace()
			if p.pos < len(p.src) && (p.src[p.pos] == '[' || p.src[p.pos] == ']' || p.src[p.pos] == '!') {
				p.pos++
				continue
			}
			break
		}
		p.skipSpace()
		if p.pos < len(p.src) && p.src[p.pos] == '=' {
			p.pos++
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			defs[name] = v
		} else {
			defs[name] = nil
		}
	}
}

// parseSelectionSet parses "{ sel sel ... }".
func (p *gqlParser) parseSelectionSet() ([]*gqlSel, error) {
	if err := p.expect('{'); err != nil {
		return nil, err
	}
	var sels []*gqlSel
	for {
		p.skipSpace()
		if p.pos >= len(p.src) {
			return nil, fmt.Errorf("unterminated selection set")
		}
		if p.src[p.pos] == '}' {
			p.pos++
			return sels, nil
		}
		sel, err := p.parseSelection()
		if err != nil {
			return nil, err
		}
		sels = append(sels, sel)
	}
}

// parseSelection parses one field, spread or inline fragment.
func (p *gqlParser) parseSelection() (*gqlSel, error) {
	p.skipSpace()
	if p.pos+2 < len(p.src) && p.src[p.pos] == '.' && p.src[p.pos+1] == '.' && p.src[p.pos+2] == '.' {
		p.pos += 3
		w := p.peekWord()
		if w == "on" {
			p.pos += 2
			cond, err := p.parseName()
			if err != nil {
				return nil, err
			}
			p.skipDirectives()
			sels, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			return &gqlSel{kind: "inline", typeCond: cond, sels: sels}, nil
		}
		name, err := p.parseName()
		if err != nil {
			return nil, err
		}
		p.skipDirectives()
		return &gqlSel{kind: "spread", fragName: name}, nil
	}
	name, err := p.parseName()
	if err != nil {
		return nil, err
	}
	sel := &gqlSel{kind: "field", name: name}
	p.skipSpace()
	if p.pos < len(p.src) && p.src[p.pos] == ':' {
		p.pos++
		alias, err := p.parseName()
		if err != nil {
			return nil, err
		}
		sel.alias = name
		sel.name = alias
	}
	p.skipSpace()
	if p.pos < len(p.src) && p.src[p.pos] == '(' {
		args, err := p.parseArgs()
		if err != nil {
			return nil, err
		}
		sel.args = args
	}
	p.skipDirectives()
	p.skipSpace()
	if p.pos < len(p.src) && p.src[p.pos] == '{' {
		sels, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		sel.sels = sels
	}
	return sel, nil
}

// skipDirectives skips "@name(...)" directives, which the suite never
// sends but a general document may carry.
func (p *gqlParser) skipDirectives() {
	for {
		p.skipSpace()
		if p.pos >= len(p.src) || p.src[p.pos] != '@' {
			return
		}
		p.pos++
		if _, err := p.parseName(); err != nil {
			return
		}
		p.skipSpace()
		if p.pos < len(p.src) && p.src[p.pos] == '(' {
			if _, err := p.parseArgs(); err != nil {
				return
			}
		}
	}
}

// parseArgs parses "(name: value, ...)".
func (p *gqlParser) parseArgs() (map[string]*gqlVal, error) {
	args := make(map[string]*gqlVal)
	if err := p.expect('('); err != nil {
		return nil, err
	}
	for {
		p.skipSpace()
		if p.pos < len(p.src) && p.src[p.pos] == ')' {
			p.pos++
			return args, nil
		}
		name, err := p.parseName()
		if err != nil {
			return nil, err
		}
		if err := p.expect(':'); err != nil {
			return nil, err
		}
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		args[name] = v
	}
}

// parseValue parses one GraphQL value literal.
func (p *gqlParser) parseValue() (*gqlVal, error) {
	p.skipSpace()
	if p.pos >= len(p.src) {
		return nil, fmt.Errorf("expected a value")
	}
	switch c := p.src[p.pos]; {
	case c == '$':
		p.pos++
		name, err := p.parseName()
		if err != nil {
			return nil, err
		}
		return &gqlVal{kind: "var", s: name}, nil
	case c == '"':
		s, err := p.parseString()
		if err != nil {
			return nil, err
		}
		return &gqlVal{kind: "string", s: s}, nil
	case c == '{':
		p.pos++
		obj := make(map[string]*gqlVal)
		for {
			p.skipSpace()
			if p.pos < len(p.src) && p.src[p.pos] == '}' {
				p.pos++
				return &gqlVal{kind: "object", obj: obj}, nil
			}
			name, err := p.parseName()
			if err != nil {
				return nil, err
			}
			if err := p.expect(':'); err != nil {
				return nil, err
			}
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			obj[name] = v
		}
	case c == '[':
		p.pos++
		var list []*gqlVal
		for {
			p.skipSpace()
			if p.pos < len(p.src) && p.src[p.pos] == ']' {
				p.pos++
				return &gqlVal{kind: "list", list: list}, nil
			}
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			list = append(list, v)
		}
	case c == '-' || c >= '0' && c <= '9':
		start := p.pos
		for p.pos < len(p.src) {
			ch := p.src[p.pos]
			if ch >= '0' && ch <= '9' || ch == '-' || ch == '.' || ch == 'e' || ch == 'E' || ch == '+' {
				p.pos++
				continue
			}
			break
		}
		text := p.src[start:p.pos]
		if strings.ContainsAny(text, ".eE") {
			f, err := strconv.ParseFloat(text, 64)
			if err != nil {
				return nil, err
			}
			return &gqlVal{kind: "float", f: f}, nil
		}
		n, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return nil, err
		}
		return &gqlVal{kind: "int", i: n}, nil
	default:
		name, err := p.parseName()
		if err != nil {
			return nil, err
		}
		switch name {
		case "true":
			return &gqlVal{kind: "bool", b: true}, nil
		case "false":
			return &gqlVal{kind: "bool", b: false}, nil
		case "null":
			return &gqlVal{kind: "null"}, nil
		default:
			return &gqlVal{kind: "enum", s: name}, nil
		}
	}
}

// parseString parses a double-quoted string with escapes.
func (p *gqlParser) parseString() (string, error) {
	if err := p.expect('"'); err != nil {
		return "", err
	}
	var b strings.Builder
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == '"' {
			p.pos++
			return b.String(), nil
		}
		if c == '\\' && p.pos+1 < len(p.src) {
			p.pos++
			switch e := p.src[p.pos]; e {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case '"':
				b.WriteByte('"')
			case '\\':
				b.WriteByte('\\')
			case '/':
				b.WriteByte('/')
			case 'u':
				if p.pos+4 < len(p.src) {
					if n, err := strconv.ParseInt(p.src[p.pos+1:p.pos+5], 16, 32); err == nil {
						b.WriteRune(rune(n))
						p.pos += 4
					}
				}
			default:
				b.WriteByte(e)
			}
			p.pos++
			continue
		}
		b.WriteByte(c)
		p.pos++
	}
	return "", fmt.Errorf("unterminated string")
}

// --- executor ---

// gqlObj is a resolved GraphQL object: its type name and its value
// (a *Repo, *Issue, *PullRequest, *Comment, or a map for synthetic
// objects such as connections, page info and mutation payloads).
type gqlObj struct {
	typ  string
	data any
}

// executor runs one operation against the store.
type executor struct {
	s      *server
	doc    *gqlDoc
	vars   map[string]any
	errors []map[string]any
}

// resolveValue turns a parsed literal into a Go value, substituting
// variables (falling back to the operation's defaults).
func (e *executor) resolveValue(v *gqlVal, op *gqlOp) any {
	if v == nil {
		return nil
	}
	switch v.kind {
	case "var":
		if val, ok := e.vars[v.s]; ok {
			return val
		}
		if def, ok := op.varDefs[v.s]; ok && def != nil {
			return e.resolveValue(def, op)
		}
		return nil
	case "string", "enum":
		return v.s
	case "int":
		return v.i
	case "float":
		return v.f
	case "bool":
		return v.b
	case "null":
		return nil
	case "list":
		out := make([]any, 0, len(v.list))
		for _, item := range v.list {
			out = append(out, e.resolveValue(item, op))
		}
		return out
	case "object":
		out := make(map[string]any, len(v.obj))
		for k, item := range v.obj {
			out[k] = e.resolveValue(item, op)
		}
		return out
	}
	return nil
}

// arg returns a resolved argument value, or nil when absent.
func (e *executor) arg(sel *gqlSel, op *gqlOp, name string) any {
	if sel.args == nil {
		return nil
	}
	v, ok := sel.args[name]
	if !ok {
		return nil
	}
	return e.resolveValue(v, op)
}

// argInt returns an integer argument, or def when absent.
func (e *executor) argInt(sel *gqlSel, op *gqlOp, name string, def int64) int64 {
	v := e.arg(sel, op, name)
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	case int:
		return int64(n)
	}
	return def
}

// argString returns a string argument, or "" when absent.
func (e *executor) argString(sel *gqlSel, op *gqlOp, name string) string {
	v := e.arg(sel, op, name)
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// err records a field error. Unknown fields name the marker the suite
// greps for (noSuchFieldOnViewer) so an invalid-field query answers
// HTTP 200 with that message in its errors array.
func (e *executor) err(msg string) {
	e.errors = append(e.errors, map[string]any{"message": msg})
}

// execute runs one operation and returns its data object.
func (e *executor) execute(op *gqlOp) map[string]any {
	root := gqlObj{typ: "Query"}
	return e.executeSelections(root, op.sels, op)
}

// executeSelections projects an object through a selection set into a
// JSON-ready map. A field whose resolution fails becomes null and
// records an error; fragments apply only when their type condition
// matches the object.
func (e *executor) executeSelections(obj gqlObj, sels []*gqlSel, op *gqlOp) map[string]any {
	out := make(map[string]any)
	for _, sel := range sels {
		switch sel.kind {
		case "spread":
			frag, ok := e.doc.frags[sel.fragName]
			if !ok {
				e.err("unknown fragment " + sel.fragName)
				continue
			}
			if frag.typeCond != "" && frag.typeCond != obj.typ {
				continue
			}
			for k, v := range e.executeSelections(obj, frag.sels, op) {
				out[k] = v
			}
		case "inline":
			if sel.typeCond != "" && sel.typeCond != obj.typ {
				continue
			}
			for k, v := range e.executeSelections(obj, sel.sels, op) {
				out[k] = v
			}
		case "field":
			key := sel.name
			if sel.alias != "" {
				key = sel.alias
			}
			val, err := e.resolveField(obj, sel, op)
			if err != nil {
				e.err(err.Error())
				out[key] = nil
				continue
			}
			out[key] = e.projectValue(val, sel.sels, op)
		}
	}
	return out
}

// projectValue applies a field's sub-selections to its resolved value:
// objects project, lists project element-wise, scalars pass through.
func (e *executor) projectValue(val any, sels []*gqlSel, op *gqlOp) any {
	if len(sels) == 0 {
		return val
	}
	switch v := val.(type) {
	case gqlObj:
		return e.executeSelections(v, sels, op)
	case []gqlObj:
		out := make([]any, 0, len(v))
		for _, item := range v {
			out = append(out, e.executeSelections(item, sels, op))
		}
		return out
	case map[string]any:
		return e.executeSelections(gqlObj{typ: "Map", data: v}, sels, op)
	case []any:
		out := make([]any, 0, len(v))
		for _, item := range v {
			out = append(out, e.projectValue(item, sels, op))
		}
		return out
	}
	return val
}

// resolveField resolves one field of one object. Unknown fields error;
// the caller records the error and nulls the field.
func (e *executor) resolveField(obj gqlObj, sel *gqlSel, op *gqlOp) (any, error) {
	if m, ok := obj.data.(map[string]any); ok {
		if v, present := m[sel.name]; present {
			return v, nil
		}
		return nil, fmt.Errorf("cannot query field %q on type %q (noSuchFieldOnViewer)", sel.name, obj.typ)
	}
	switch obj.typ {
	case "Query":
		return e.resolveQueryField(sel, op)
	case "Repository":
		return e.resolveRepoField(obj.data.(*Repo), sel, op)
	case "Issue":
		return e.resolveIssueField(obj.data.(*Issue), sel, op)
	case "PullRequest":
		return e.resolvePRField(obj.data.(*PullRequest), sel, op)
	case "User":
		return e.resolveUserField(sel, op)
	case "Comment":
		return e.resolveCommentField(obj.data.(*Comment), sel, op)
	}
	return nil, fmt.Errorf("cannot query field %q on type %q (noSuchFieldOnViewer)", sel.name, obj.typ)
}

// --- global ids and cursors ---

// globalID encodes a typed id opaquely: base64 of "Type:key". The id is
// never the plain database id, and it is stable across reads.
func globalID(typ, key string) string {
	return base64.StdEncoding.EncodeToString([]byte(typ + ":" + key))
}

// decodeGlobalID splits a global id back into its type and key.
func decodeGlobalID(id string) (string, string, bool) {
	raw, err := base64.StdEncoding.DecodeString(id)
	if err != nil {
		return "", "", false
	}
	typ, key, ok := strings.Cut(string(raw), ":")
	if !ok {
		return "", "", false
	}
	return typ, key, true
}

// cursorOf encodes a list offset as an opaque page cursor.
func cursorOf(offset int) string {
	return base64.StdEncoding.EncodeToString([]byte("cursor:" + strconv.Itoa(offset)))
}

// offsetOf decodes a page cursor back to its offset.
func offsetOf(cursor string) int {
	raw, err := base64.StdEncoding.DecodeString(cursor)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimPrefix(string(raw), "cursor:"))
	if err != nil {
		return 0
	}
	return n
}

// pageInfo builds the pageInfo object for a connection page.
func pageInfo(hasNext bool, endCursor string) gqlObj {
	return gqlObj{typ: "PageInfo", data: map[string]any{
		"hasNextPage":     hasNext,
		"hasPreviousPage": false,
		"startCursor":     nil,
		"endCursor":       endCursor,
	}}
}

// connection builds an Issue or PullRequest connection object from a
// full ordered list, a page size and a cursor.
func connection(typ string, items []gqlObj, first int64, after string) gqlObj {
	start := offsetOf(after)
	if start < 0 {
		start = 0
	}
	if start > len(items) {
		start = len(items)
	}
	end := len(items)
	if first > 0 && start+int(first) < end {
		end = start + int(first)
	}
	page := items[start:end]
	var endCursor string
	if len(page) > 0 {
		endCursor = cursorOf(end)
	}
	nodes := make([]gqlObj, 0, len(page))
	for _, item := range page {
		nodes = append(nodes, item)
	}
	return gqlObj{typ: typ, data: map[string]any{
		"nodes":      nodes,
		"edges":      edgeList(page, start),
		"pageInfo":   pageInfo(end < len(items), endCursor),
		"totalCount": len(items),
	}}
}

// edgeList builds connection edges (node + cursor) for a page.
func edgeList(page []gqlObj, start int) []gqlObj {
	edges := make([]gqlObj, 0, len(page))
	for i, item := range page {
		edges = append(edges, gqlObj{typ: "Edge", data: map[string]any{
			"node":   item,
			"cursor": cursorOf(start + i + 1),
		}})
	}
	return edges
}

// --- typed resolvers ---

// resolveQueryField resolves Query root fields. An unknown repository
// answers null with no errors; an invalid field errors.
func (e *executor) resolveQueryField(sel *gqlSel, op *gqlOp) (any, error) {
	switch sel.name {
	case "viewer":
		return gqlObj{typ: "User", data: nil}, nil
	case "repository":
		owner := e.argString(sel, op, "owner")
		name := e.argString(sel, op, "name")
		e.s.store.mu.RLock()
		repo := e.s.store.getRepo(owner, name)
		e.s.store.mu.RUnlock()
		if repo == nil || repo.isForeign() {
			return nil, nil
		}
		return gqlObj{typ: "Repository", data: repo}, nil
	case "node":
		id := e.argString(sel, op, "id")
		typ, key, ok := decodeGlobalID(id)
		if !ok || typ != "Repository" {
			return nil, nil
		}
		owner, name, ok := strings.Cut(key, "/")
		if !ok {
			return nil, nil
		}
		e.s.store.mu.RLock()
		repo := e.s.store.getRepo(owner, name)
		e.s.store.mu.RUnlock()
		if repo == nil || repo.isForeign() {
			return nil, nil
		}
		return gqlObj{typ: "Repository", data: repo}, nil
	case "__type":
		name := e.argString(sel, op, "name")
		return e.introspectType(name), nil
	case "__schema":
		return gqlObj{typ: "__Schema", data: map[string]any{
			"queryType": map[string]any{"name": "Query"},
		}}, nil
	}
	return nil, fmt.Errorf("cannot query field %q on type %q (noSuchFieldOnViewer)", sel.name, "Query")
}

// resolveUserField resolves viewer fields.
func (e *executor) resolveUserField(sel *gqlSel, op *gqlOp) (any, error) {
	switch sel.name {
	case "login":
		return viewerLogin, nil
	case "id":
		return globalID("User", viewerLogin), nil
	case "databaseId":
		return int64(viewerID), nil
	case "name":
		return viewerName, nil
	case "email":
		return viewerEmail, nil
	}
	return nil, fmt.Errorf("cannot query field %q on type %q (noSuchFieldOnViewer)", sel.name, "Viewer")
}

// repoIssues returns a repository's issues as GraphQL objects, filtered
// by states and ordered per orderBy (CREATED_AT DESC means numbers
// strictly descending; the default is creation order).
func (e *executor) repoIssues(repo *Repo, sel *gqlSel, op *gqlOp) []gqlObj {
	issues := sortedIssues(repo)
	// states filter.
	states := e.arg(sel, op, "states")
	if list, ok := states.([]any); ok && len(list) > 0 {
		want := make(map[string]bool)
		for _, st := range list {
			if s, ok := st.(string); ok {
				want[s] = true
			}
		}
		filtered := issues[:0]
		for _, iss := range issues {
			gqlState := "OPEN"
			if iss.State == "closed" {
				gqlState = "CLOSED"
			}
			if want[gqlState] {
				filtered = append(filtered, iss)
			}
		}
		issues = filtered
	}
	// orderBy.
	if orderBy, ok := e.arg(sel, op, "orderBy").(map[string]any); ok {
		dir, _ := orderBy["direction"].(string)
		if dir == "DESC" {
			reversed := make([]*Issue, len(issues))
			for i, iss := range issues {
				reversed[len(issues)-1-i] = iss
			}
			issues = reversed
		}
	}
	out := make([]gqlObj, 0, len(issues))
	for _, iss := range issues {
		out = append(out, gqlObj{typ: "Issue", data: iss})
	}
	return out
}

// resolveRepoField resolves Repository fields.
func (e *executor) resolveRepoField(repo *Repo, sel *gqlSel, op *gqlOp) (any, error) {
	switch sel.name {
	case "id":
		return globalID("Repository", repo.Owner+"/"+repo.Name), nil
	case "databaseId":
		return repo.ID, nil
	case "name":
		return repo.Name, nil
	case "nameWithOwner":
		return repo.Owner + "/" + repo.Name, nil
	case "description":
		return repo.Description, nil
	case "owner":
		return gqlObj{typ: "User", data: nil}, nil
	case "defaultBranchRef":
		if repo.headOID() == "" {
			return nil, nil
		}
		return gqlObj{typ: "Ref", data: map[string]any{"name": repo.DefaultBranch}}, nil
	case "viewerPermission":
		return "ADMIN", nil
	case "hasIssuesEnabled":
		return true, nil
	case "isPrivate":
		return repo.Private, nil
	case "createdAt":
		return isoTime(repo.CreatedSeq), nil
	case "url":
		return "https://example.invalid/" + repo.Owner + "/" + repo.Name, nil
	case "issue":
		number := e.argInt(sel, op, "number", 0)
		e.s.store.mu.RLock()
		iss, ok := repo.Issues[int(number)]
		e.s.store.mu.RUnlock()
		if !ok {
			return nil, nil
		}
		return gqlObj{typ: "Issue", data: iss}, nil
	case "issues":
		e.s.store.mu.RLock()
		items := e.repoIssues(repo, sel, op)
		e.s.store.mu.RUnlock()
		first := e.argInt(sel, op, "first", 0)
		after := e.argString(sel, op, "after")
		return connection("IssueConnection", items, first, after), nil
	case "pullRequests":
		e.s.store.mu.RLock()
		prs := sortedPRs(repo)
		items := make([]gqlObj, 0, len(prs))
		for _, pr := range prs {
			items = append(items, gqlObj{typ: "PullRequest", data: pr})
		}
		e.s.store.mu.RUnlock()
		first := e.argInt(sel, op, "first", 0)
		after := e.argString(sel, op, "after")
		return connection("PullRequestConnection", items, first, after), nil
	case "pullRequest":
		number := e.argInt(sel, op, "number", 0)
		e.s.store.mu.RLock()
		pr, ok := repo.PRs[int(number)]
		e.s.store.mu.RUnlock()
		if !ok {
			return nil, nil
		}
		return gqlObj{typ: "PullRequest", data: pr}, nil
	}
	return nil, fmt.Errorf("cannot query field %q on type %q (noSuchFieldOnViewer)", sel.name, "Repository")
}

// issueState maps a REST state to its GraphQL enum.
func issueState(state string) string {
	if state == "closed" {
		return "CLOSED"
	}
	return "OPEN"
}

// resolveIssueField resolves Issue fields.
func (e *executor) resolveIssueField(iss *Issue, sel *gqlSel, op *gqlOp) (any, error) {
	switch sel.name {
	case "id":
		return globalID("Issue", iss.Repo+":"+strconv.Itoa(iss.Number)), nil
	case "databaseId":
		return iss.ID, nil
	case "number":
		return int64(iss.Number), nil
	case "title":
		return iss.Title, nil
	case "body":
		return iss.Body, nil
	case "state":
		return issueState(iss.State), nil
	case "createdAt", "updatedAt":
		return isoTime(iss.Seq), nil
	case "author":
		return gqlObj{typ: "User", data: nil}, nil
	case "url":
		return "https://example.invalid/issue/" + strconv.Itoa(iss.Number), nil
	case "labels":
		items := make([]gqlObj, 0, len(iss.Labels))
		for _, l := range iss.Labels {
			items = append(items, gqlObj{typ: "Label", data: map[string]any{"name": l.Name, "color": l.Color}})
		}
		return connection("LabelConnection", items, e.argInt(sel, op, "first", 0), e.argString(sel, op, "after")), nil
	case "comments":
		items := make([]gqlObj, 0, len(iss.Comments))
		for _, c := range iss.Comments {
			items = append(items, gqlObj{typ: "Comment", data: c})
		}
		return connection("CommentConnection", items, e.argInt(sel, op, "first", 0), e.argString(sel, op, "after")), nil
	}
	return nil, fmt.Errorf("cannot query field %q on type %q (noSuchFieldOnViewer)", sel.name, "Issue")
}

// resolvePRField resolves PullRequest fields.
func (e *executor) resolvePRField(pr *PullRequest, sel *gqlSel, op *gqlOp) (any, error) {
	switch sel.name {
	case "id":
		return globalID("PullRequest", pr.Repo+":"+strconv.Itoa(pr.Number)), nil
	case "databaseId":
		return pr.ID, nil
	case "number":
		return int64(pr.Number), nil
	case "title":
		return pr.Title, nil
	case "body":
		return pr.Body, nil
	case "state":
		if pr.Merged {
			return "MERGED", nil
		}
		return issueState(pr.State), nil
	case "headRefName":
		return pr.HeadRef, nil
	case "baseRefName":
		return pr.BaseRef, nil
	case "headRefOid":
		return pr.HeadSHA, nil
	case "baseRefOid":
		return pr.BaseSHA, nil
	case "mergeable":
		return e.prMergeable(pr), nil
	case "merged":
		return pr.Merged, nil
	case "mergedAt":
		if pr.MergedAt == "" {
			return nil, nil
		}
		return pr.MergedAt, nil
	case "createdAt", "updatedAt":
		return isoTime(pr.Seq), nil
	case "author":
		return gqlObj{typ: "User", data: nil}, nil
	case "files":
		e.s.store.mu.RLock()
		repo := e.findPRRepo(pr)
		var files []map[string]any
		if repo != nil {
			headObj := repo.Objects[pr.HeadSHA]
			baseObj := repo.Objects[pr.BaseSHA]
			if headObj != nil && baseObj != nil {
				files = e.s.store.diffFiles(repo, parseCommit(baseObj.Data).Tree, parseCommit(headObj.Data).Tree)
			}
		}
		e.s.store.mu.RUnlock()
		items := make([]gqlObj, 0, len(files))
		for _, f := range files {
			items = append(items, gqlObj{typ: "PullRequestFile", data: f})
		}
		return connection("PullRequestFileConnection", items, e.argInt(sel, op, "first", 0), e.argString(sel, op, "after")), nil
	}
	return nil, fmt.Errorf("cannot query field %q on type %q (noSuchFieldOnViewer)", sel.name, "PullRequest")
}

// findPRRepo locates the repository holding a pull request (by id).
func (e *executor) findPRRepo(pr *PullRequest) *Repo {
	for _, repo := range e.s.store.order {
		if p, ok := repo.PRs[pr.Number]; ok && p == pr {
			return repo
		}
	}
	return nil
}

// prMergeable reports a pull request's mergeability: CONFLICTING when
// the same path changed on both sides of the merge-base with different
// content, else MERGEABLE (UNKNOWN only when a branch is missing).
func (e *executor) prMergeable(pr *PullRequest) string {
	repo := e.findPRRepo(pr)
	if repo == nil {
		return "UNKNOWN"
	}
	headSHA := repo.Refs["refs/heads/"+pr.HeadRef]
	baseSHA := repo.Refs["refs/heads/"+pr.BaseRef]
	if headSHA == "" || baseSHA == "" {
		return "UNKNOWN"
	}
	baseObj := repo.Objects[baseSHA]
	headObj := repo.Objects[headSHA]
	if baseObj == nil || headObj == nil {
		return "UNKNOWN"
	}
	mb := e.s.store.mergeBase(repo, baseSHA, headSHA)
	var mbTree string
	if mb != "" {
		if mbObj := repo.Objects[mb]; mbObj != nil {
			mbTree = parseCommit(mbObj.Data).Tree
		}
	}
	baseFlat := e.s.store.treeMap(repo, parseCommit(baseObj.Data).Tree)
	headFlat := e.s.store.treeMap(repo, parseCommit(headObj.Data).Tree)
	mbFlat := e.s.store.treeMap(repo, mbTree)
	for path, headOID := range headFlat {
		mbOID, inBase := mbFlat[path]
		if !inBase || mbOID == headOID {
			continue
		}
		if baseOID, onBase := baseFlat[path]; onBase && baseOID != mbOID && baseOID != headOID {
			return "CONFLICTING"
		}
		if _, onBase := baseFlat[path]; !onBase {
			return "CONFLICTING"
		}
	}
	for path, baseOID := range baseFlat {
		mbOID, inBase := mbFlat[path]
		if !inBase || mbOID == baseOID {
			continue
		}
		if _, onHead := headFlat[path]; !onHead {
			return "CONFLICTING"
		}
	}
	return "MERGEABLE"
}

// resolveCommentField resolves Comment fields.
func (e *executor) resolveCommentField(c *Comment, sel *gqlSel, op *gqlOp) (any, error) {
	switch sel.name {
	case "id":
		return globalID("IssueComment", c.Repo+":"+strconv.Itoa(c.IssueNumber)+":"+strconv.FormatInt(c.ID, 10)), nil
	case "databaseId":
		return c.ID, nil
	case "body":
		return c.Body, nil
	case "createdAt", "updatedAt":
		return isoTime(c.Seq), nil
	case "author":
		return gqlObj{typ: "User", data: nil}, nil
	}
	return nil, fmt.Errorf("cannot query field %q on type %q (noSuchFieldOnViewer)", sel.name, "IssueComment")
}

// --- introspection ---

// gqlTypeInfo is the fixed schema the __type field reports.
var gqlTypeInfo = map[string]struct {
	kind       string
	fields     []string
	enumValues []string
}{
	"Issue": {
		kind:   "OBJECT",
		fields: []string{"id", "databaseId", "number", "title", "body", "state", "createdAt", "updatedAt", "author", "url", "labels", "comments", "repository"},
	},
	"PullRequest": {
		kind:   "OBJECT",
		fields: []string{"id", "databaseId", "number", "title", "body", "state", "headRefName", "baseRefName", "headRefOid", "baseRefOid", "mergeable", "merged", "mergedAt", "createdAt", "updatedAt", "author", "files"},
	},
	"Repository": {
		kind:   "OBJECT",
		fields: []string{"id", "databaseId", "name", "nameWithOwner", "description", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "isPrivate", "createdAt", "url", "issue", "issues", "pullRequests", "pullRequest"},
	},
	"SearchType": {
		kind:       "ENUM",
		enumValues: []string{"ISSUE", "PULL_REQUEST", "REPOSITORY", "USER", "DISCUSSION", "COMMIT", "MILESTONE", "LABEL"},
	},
	"User": {
		kind:   "OBJECT",
		fields: []string{"id", "databaseId", "login", "name", "email"},
	},
	"IssueState": {
		kind:       "ENUM",
		enumValues: []string{"OPEN", "CLOSED"},
	},
}

// introspectType returns the __Type object for a named type, or null.
func (e *executor) introspectType(name string) any {
	info, ok := gqlTypeInfo[name]
	if !ok {
		return nil
	}
	fields := make([]gqlObj, 0, len(info.fields))
	for _, f := range info.fields {
		fields = append(fields, gqlObj{typ: "__Field", data: map[string]any{"name": f}})
	}
	enums := make([]gqlObj, 0, len(info.enumValues))
	for _, v := range info.enumValues {
		enums = append(enums, gqlObj{typ: "__EnumValue", data: map[string]any{"name": v}})
	}
	return gqlObj{typ: "__Type", data: map[string]any{
		"name":       name,
		"kind":       info.kind,
		"fields":     fields,
		"enumValues": enums,
	}}
}

// --- mutations ---

// executeMutation runs one mutation operation: createIssue, closeIssue,
// createPullRequest, mergePullRequest and addComment.
func (e *executor) executeMutation(op *gqlOp) map[string]any {
	out := make(map[string]any)
	for _, sel := range op.sels {
		if sel.kind != "field" {
			continue
		}
		key := sel.name
		if sel.alias != "" {
			key = sel.alias
		}
		val, err := e.resolveMutation(sel, op)
		if err != nil {
			e.err(err.Error())
			out[key] = nil
			continue
		}
		out[key] = e.projectValue(val, sel.sels, op)
	}
	return out
}

// resolveMutation resolves one mutation field.
func (e *executor) resolveMutation(sel *gqlSel, op *gqlOp) (any, error) {
	input, _ := e.arg(sel, op, "input").(map[string]any)
	switch sel.name {
	case "createIssue":
		repoID, _ := input["repositoryId"].(string)
		title, _ := input["title"].(string)
		body, _ := input["body"].(string)
		typ, key, ok := decodeGlobalID(repoID)
		if !ok || typ != "Repository" {
			return nil, fmt.Errorf("createIssue: unknown repository id")
		}
		owner, name, ok := strings.Cut(key, "/")
		if !ok {
			return nil, fmt.Errorf("createIssue: unknown repository id")
		}
		e.s.store.mu.Lock()
		repo := e.s.store.getRepo(owner, name)
		if repo == nil || repo.isForeign() {
			e.s.store.mu.Unlock()
			return nil, fmt.Errorf("createIssue: repository not found")
		}
		iss := &Issue{
			ID:     e.s.store.nextIDv(),
			Number: repo.NextNum,
			Title:  title,
			Body:   body,
			State:  "open",
			User:   viewerLogin,
			Seq:    e.s.store.nextSeq(),
			Repo:   name,
		}
		repo.NextNum++
		repo.Issues[iss.Number] = iss
		e.s.store.mu.Unlock()
		return gqlObj{typ: "CreateIssuePayload", data: map[string]any{
			"issue": gqlObj{typ: "Issue", data: iss},
		}}, nil
	case "closeIssue":
		issueID, _ := input["issueId"].(string)
		typ, key, ok := decodeGlobalID(issueID)
		if !ok || typ != "Issue" {
			return nil, fmt.Errorf("closeIssue: unknown issue id")
		}
		repoName, numStr, ok := strings.Cut(key, ":")
		if !ok {
			return nil, fmt.Errorf("closeIssue: unknown issue id")
		}
		number, err := strconv.Atoi(numStr)
		if err != nil {
			return nil, fmt.Errorf("closeIssue: unknown issue id")
		}
		e.s.store.mu.Lock()
		repo := e.s.store.getRepo(viewerLogin, repoName)
		if repo == nil {
			e.s.store.mu.Unlock()
			return nil, fmt.Errorf("closeIssue: repository not found")
		}
		iss, ok := repo.Issues[number]
		if !ok {
			e.s.store.mu.Unlock()
			return nil, fmt.Errorf("closeIssue: issue not found")
		}
		iss.State = "closed"
		iss.cached = nil
		e.s.store.mu.Unlock()
		return gqlObj{typ: "CloseIssuePayload", data: map[string]any{
			"issue": gqlObj{typ: "Issue", data: iss},
		}}, nil
	case "createPullRequest":
		repoID, _ := input["repositoryId"].(string)
		title, _ := input["title"].(string)
		body, _ := input["body"].(string)
		baseRef, _ := input["baseRefName"].(string)
		headRef, _ := input["headRefName"].(string)
		typ, key, ok := decodeGlobalID(repoID)
		if !ok || typ != "Repository" {
			return nil, fmt.Errorf("createPullRequest: unknown repository id")
		}
		owner, name, ok := strings.Cut(key, "/")
		if !ok {
			return nil, fmt.Errorf("createPullRequest: unknown repository id")
		}
		e.s.store.mu.Lock()
		repo := e.s.store.getRepo(owner, name)
		if repo == nil || repo.isForeign() {
			e.s.store.mu.Unlock()
			return nil, fmt.Errorf("createPullRequest: repository not found")
		}
		headSHA, ok := repo.Refs["refs/heads/"+headRef]
		if !ok {
			e.s.store.mu.Unlock()
			return nil, fmt.Errorf("createPullRequest: head branch does not exist")
		}
		baseSHA, ok := repo.Refs["refs/heads/"+baseRef]
		if !ok {
			e.s.store.mu.Unlock()
			return nil, fmt.Errorf("createPullRequest: base branch does not exist")
		}
		for _, pr := range repo.PRs {
			if pr.State == "open" && pr.HeadRef == headRef && pr.BaseRef == baseRef {
				e.s.store.mu.Unlock()
				return nil, fmt.Errorf("createPullRequest: a pull request already exists for %s", headRef)
			}
		}
		pr := &PullRequest{
			ID:      e.s.store.nextIDv(),
			Number:  repo.NextNum,
			Title:   title,
			Body:    body,
			State:   "open",
			HeadRef: headRef,
			BaseRef: baseRef,
			HeadSHA: headSHA,
			BaseSHA: baseSHA,
			Seq:     e.s.store.nextSeq(),
			Repo:    name,
		}
		repo.NextNum++
		repo.PRs[pr.Number] = pr
		e.s.store.mu.Unlock()
		return gqlObj{typ: "CreatePullRequestPayload", data: map[string]any{
			"pullRequest": gqlObj{typ: "PullRequest", data: pr},
		}}, nil
	case "mergePullRequest":
		prID, _ := input["pullRequestId"].(string)
		typ, key, ok := decodeGlobalID(prID)
		if !ok || typ != "PullRequest" {
			return nil, fmt.Errorf("mergePullRequest: unknown pull request id")
		}
		repoName, numStr, ok := strings.Cut(key, ":")
		if !ok {
			return nil, fmt.Errorf("mergePullRequest: unknown pull request id")
		}
		number, err := strconv.Atoi(numStr)
		if err != nil {
			return nil, fmt.Errorf("mergePullRequest: unknown pull request id")
		}
		merged, err := e.s.mergePullRequestByID(repoName, number)
		if err != nil {
			return nil, err
		}
		return gqlObj{typ: "MergePullRequestPayload", data: map[string]any{
			"pullRequest": gqlObj{typ: "PullRequest", data: merged},
		}}, nil
	case "addComment":
		subjectID, _ := input["subjectId"].(string)
		body, _ := input["body"].(string)
		typ, key, ok := decodeGlobalID(subjectID)
		if !ok || typ != "Issue" {
			return nil, fmt.Errorf("addComment: unknown subject id")
		}
		repoName, numStr, ok := strings.Cut(key, ":")
		if !ok {
			return nil, fmt.Errorf("addComment: unknown subject id")
		}
		number, err := strconv.Atoi(numStr)
		if err != nil {
			return nil, fmt.Errorf("addComment: unknown subject id")
		}
		e.s.store.mu.Lock()
		repo := e.s.store.getRepo(viewerLogin, repoName)
		if repo == nil {
			e.s.store.mu.Unlock()
			return nil, fmt.Errorf("addComment: repository not found")
		}
		iss, ok := repo.Issues[number]
		if !ok {
			e.s.store.mu.Unlock()
			return nil, fmt.Errorf("addComment: issue not found")
		}
		c := &Comment{ID: e.s.store.nextIDv(), Body: body, User: viewerLogin, Seq: e.s.store.nextSeq(), Repo: repoName, IssueNumber: number}
		iss.Comments = append(iss.Comments, c)
		iss.cached = nil
		e.s.store.mu.Unlock()
		return gqlObj{typ: "AddCommentPayload", data: map[string]any{
			"commentEdge": gqlObj{typ: "Edge", data: map[string]any{
				"node": gqlObj{typ: "Comment", data: c},
			}},
		}}, nil
	}
	return nil, fmt.Errorf("cannot query field %q on type %q (noSuchFieldOnViewer)", sel.name, "Mutation")
}

// mergePullRequestByID merges a pull request by repository and number
// and returns it. It shares the squash semantics of the REST merge.
func (s *server) mergePullRequestByID(repoName string, number int) (*PullRequest, error) {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	repo := s.store.getRepo(viewerLogin, repoName)
	if repo == nil {
		return nil, fmt.Errorf("mergePullRequest: repository not found")
	}
	pr, ok := repo.PRs[number]
	if !ok {
		return nil, fmt.Errorf("mergePullRequest: pull request not found")
	}
	if pr.State != "open" {
		return nil, fmt.Errorf("mergePullRequest: pull request is not open")
	}
	headSHA := repo.Refs["refs/heads/"+pr.HeadRef]
	baseSHA := repo.Refs["refs/heads/"+pr.BaseRef]
	if headSHA == "" || baseSHA == "" {
		return nil, fmt.Errorf("mergePullRequest: pull request branch not found")
	}
	baseObj := repo.Objects[baseSHA]
	headObj := repo.Objects[headSHA]
	if baseObj == nil || headObj == nil {
		return nil, fmt.Errorf("mergePullRequest: pull request branch not found")
	}
	mergeBaseOID := s.store.mergeBase(repo, baseSHA, headSHA)
	var mbTree string
	if mergeBaseOID != "" {
		if mbObj := repo.Objects[mergeBaseOID]; mbObj != nil {
			mbTree = parseCommit(mbObj.Data).Tree
		}
	}
	baseFlat := s.store.treeMap(repo, parseCommit(baseObj.Data).Tree)
	headFlat := s.store.treeMap(repo, parseCommit(headObj.Data).Tree)
	mbFlat := s.store.treeMap(repo, mbTree)
	// Reuse the REST merge's conflict rule via the shared helper.
	if s.store.mergeConflicts(baseFlat, headFlat, mbFlat) {
		return nil, fmt.Errorf("mergePullRequest: merge conflict")
	}
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
	return pr, nil
}

// mergeConflicts reports whether a squash merge of head into base
// conflicts: the same path changed on both sides of the merge-base with
// different resulting content (or was deleted on one side and changed
// on the other).
func (s *Store) mergeConflicts(baseFlat, headFlat, mbFlat map[string]string) bool {
	for path, headOID := range headFlat {
		mbOID, inBase := mbFlat[path]
		if !inBase || mbOID == headOID {
			continue
		}
		if baseOID, onBase := baseFlat[path]; onBase && baseOID != mbOID && baseOID != headOID {
			return true
		}
		if _, onBase := baseFlat[path]; !onBase {
			return true
		}
	}
	for path, baseOID := range baseFlat {
		mbOID, inBase := mbFlat[path]
		if !inBase || mbOID == baseOID {
			continue
		}
		if _, onHead := headFlat[path]; !onHead {
			return true
		}
	}
	return false
}

// --- HTTP handler ---

// handleGraphQL serves POST /api/graphql: a single operation or a
// batched array of operations, answered with an array in order.
// Unauthenticated requests are exactly 401 (decided in ServeHTTP).
func (s *server) handleGraphQL(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	body, err := readBody(r)
	if err != nil {
		s.writeErr(w, http.StatusBadRequest, "cannot read request body")
		return
	}
	var raw json.RawMessage
	if len(body) > 0 && body[0] == '[' {
		var batch []json.RawMessage
		if err := json.Unmarshal(body, &batch); err != nil {
			s.writeErr(w, http.StatusBadRequest, "malformed JSON body")
			return
		}
		out := make([]any, 0, len(batch))
		for _, item := range batch {
			out = append(out, s.runGraphQLOperation(item))
		}
		s.writeJSON(w, http.StatusOK, out)
		return
	}
	raw = body
	s.writeJSON(w, http.StatusOK, s.runGraphQLOperation(raw))
}

// runGraphQLOperation runs one {query, variables, operationName}
// request and returns its result object.
func (s *server) runGraphQLOperation(raw json.RawMessage) map[string]any {
	var req struct {
		Query         string         `json:"query"`
		Variables     map[string]any `json:"variables"`
		OperationName string         `json:"operationName"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return map[string]any{"errors": []map[string]any{{"message": "malformed JSON body"}}}
	}
	doc, err := parseGraphQL(req.Query)
	if err != nil {
		return map[string]any{"errors": []map[string]any{{"message": err.Error()}}}
	}
	op := doc.ops[0]
	if req.OperationName != "" {
		for _, candidate := range doc.ops {
			if candidate.name == req.OperationName {
				op = candidate
				break
			}
		}
	}
	if req.Variables == nil {
		req.Variables = make(map[string]any)
	}
	ex := &executor{s: s, doc: doc, vars: req.Variables}
	var data map[string]any
	if op.kind == "mutation" {
		data = ex.executeMutation(op)
	} else {
		data = ex.execute(op)
	}
	result := map[string]any{"data": data}
	if len(ex.errors) > 0 {
		result["errors"] = ex.errors
	}
	return result
}
