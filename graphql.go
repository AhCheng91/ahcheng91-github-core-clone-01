// GitHub GraphQL v4: POST /api/graphql. Parses the document the suite sends (gh and github-mcp-server
// speak githubv4), resolves it against the shared store through service.go so REST reads back exactly
// what GraphQL wrote, and answers {data} or {data, errors}. One request body is one operation
// {query, variables} (an array answers operation batching). Nothing here depends on the clock: global
// ids are base64 of "<Type>:<repoId>[:<itemId>]" and cursors are base64 of "cursor:<index>".

package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// A small GraphQL document reader: fields with aliases, arguments (strings,
// ints, enums, lists, object literals and $variables), selection sets, inline
// fragments and comments. It covers what the suite sends.
// ---------------------------------------------------------------------------

type vNode struct {
	t      string    // var / str / int / float / bool / null / name / list / obj
	name   string    // for var, name
	value  any       // str: string; int/float: float64; bool: bool
	items  []vNode   // list
	fields []argNode // obj
}

type argNode struct {
	name  string
	value vNode
}

type sel struct {
	kind       string // field / frag
	alias      string
	name       string
	args       []argNode
	selections []sel
	fragType   string
}

type op struct {
	opType     string // query / mutation
	selections []sel
}

/** The name grammar GraphQL field and type names follow. */
var gqlNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*`)

/** The number grammar GraphQL literal numbers follow. */
var gqlNumberRe = regexp.MustCompile(`^(-?(?:0|[1-9]\d*)(?:\.\d+)?(?:[eE][+-]?\d+)?)`)

var itemIdRe = regexp.MustCompile(`^Issue:(\d+):(\d+)$`)
var pullIdRe = regexp.MustCompile(`^PullRequest:(\d+):(\d+)$`)
var cursorRe = regexp.MustCompile(`^cursor:(\d+)$`)

type parser struct {
	src string
	pos int
}

/** Skip whitespace, commas, a BOM and line comments, exactly as the TypeScript reference does. */
func (p *parser) skip() {
	s := p.src
	n := len(s)
	for {
		i := p.pos
		for i < n {
			c := s[i]
			if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' {
				i++
				continue
			}
			if c == 0xef && i+2 < n && s[i+1] == 0xbb && s[i+2] == 0xbf {
				i += 3
				continue
			}
			break
		}
		if i < n && s[i] == '#' {
			for i < n && s[i] != '\n' {
				i++
			}
			continue
		}
		p.pos = i
		return
	}
}

/** The character at the cursor, or "" at the end of the document. */
func (p *parser) peek() string {
	p.skip()
	if p.pos >= len(p.src) {
		return ""
	}
	return p.src[p.pos : p.pos+1]
}

func (p *parser) at(punct string) bool {
	p.skip()
	return strings.HasPrefix(p.src[p.pos:], punct)
}

func (p *parser) accept(punct string) bool {
	p.skip()
	if strings.HasPrefix(p.src[p.pos:], punct) {
		p.pos += len(punct)
		return true
	}
	return false
}

func (p *parser) expect(punct string) {
	p.skip()
	if strings.HasPrefix(p.src[p.pos:], punct) {
		p.pos += len(punct)
		return
	}
	panic(fmt.Sprintf("GraphQL parse error: expected %s", punct))
}

func (p *parser) name() string {
	p.skip()
	m := gqlNameRe.FindString(p.src[p.pos:])
	if m == "" {
		panic("GraphQL parse error: expected a name")
	}
	p.pos += len(m)
	return m
}

func parseDocument(src string) (_ []op, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	p := &parser{src: src}
	var ops []op
	for {
		p.skip()
		if p.pos >= len(src) {
			break
		}
		if p.at("fragment") {
			p.maybeReadFragmentDefinition()
			continue
		}
		ops = append(ops, p.readOperation())
	}
	return ops, nil
}

/** `fragment F on Type { ... }` — parse and drop the body; the suite never needs the definition. */
func (p *parser) maybeReadFragmentDefinition() {
	p.name() // 'fragment'
	p.name() // fragment name
	p.expect("on")
	p.name() // type condition
	p.readDirectives()
	p.readSelectionSet()
}

func (p *parser) readOperation() op {
	p.skip()
	opType := "query"
	first := p.peek()
	if first != "{" {
		word := p.name()
		if word == "query" || word == "mutation" || word == "subscription" {
			if word == "mutation" || word == "subscription" {
				opType = "mutation"
			} else {
				opType = "query"
			}
			// optional operation name
			if !p.at("(") && !p.at("{") {
				_ = p.name()
			}
		}
	} else {
		return op{opType: "query", selections: p.readSelectionSet()}
	}
	if p.at("(") {
		p.readVariableDefs()
	}
	p.readDirectives()
	return op{opType: opType, selections: p.readSelectionSet()}
}

func (p *parser) readVariableDefs() {
	p.expect("(")
	for {
		p.skip()
		if p.accept(")") {
			return
		}
		p.expect("$")
		p.name()
		p.expect(":")
		p.readType()
		if p.accept("=") {
			_ = p.readValue()
		}
		if p.accept("&") {
			p.readType()
		}
	}
}

func (p *parser) readType() {
	p.skip()
	if p.accept("[") {
		p.readType()
		p.expect("]")
	} else {
		p.name()
	}
	p.accept("!")
}

func (p *parser) readDirectives() {
	for {
		p.skip()
		if !p.accept("@") {
			return
		}
		p.name()
		if p.at("(") {
			p.readArgs()
		}
	}
}

func (p *parser) readSelectionSet() []sel {
	p.expect("{")
	out := []sel{}
	for {
		p.skip()
		if p.accept("}") {
			return out
		}
		if p.accept("@") {
			p.name()
			if p.at("(") {
				p.readArgs()
			}
			continue
		}
		if p.at("...") {
			p.pos += 3
			p.skip()
			if p.at("on") {
				p.name() // 'on'
				tpe := p.name()
				p.readDirectives()
				out = append(out, sel{kind: "frag", fragType: tpe, selections: p.readSelectionSet()})
			} else if p.at("{") {
				out = append(out, sel{kind: "frag", selections: p.readSelectionSet()})
			} else {
				// a fragment spread: consume its name and directives.
				p.name()
				p.readDirectives()
			}
			continue
		}
		out = append(out, p.readField())
	}
}

func (p *parser) readField() sel {
	first := p.name()
	alias := ""
	fieldName := first
	if p.accept(":") {
		alias = first
		fieldName = p.name()
	}
	var args []argNode
	if p.at("(") {
		args = p.readArgs()
	}
	if p.accept("@") {
		p.name()
		if p.at("(") {
			p.readArgs()
		}
	}
	var selections []sel
	if p.at("{") {
		selections = p.readSelectionSet()
	}
	return sel{kind: "field", alias: alias, name: fieldName, args: args, selections: selections}
}

func (p *parser) readArgs() []argNode {
	p.expect("(")
	out := []argNode{}
	for {
		p.skip()
		if p.accept(")") {
			return out
		}
		name := p.name()
		p.expect(":")
		out = append(out, argNode{name: name, value: p.readValue()})
	}
}

func (p *parser) readValue() vNode {
	p.skip()
	c := ""
	if p.pos < len(p.src) {
		c = p.src[p.pos : p.pos+1]
	}
	if c == "$" {
		p.pos++
		return vNode{t: "var", name: p.name()}
	}
	if c == "[" {
		p.pos++
		items := []vNode{}
		for {
			p.skip()
			if p.accept("]") {
				return vNode{t: "list", items: items}
			}
			items = append(items, p.readValue())
		}
	}
	if c == "{" {
		p.pos++
		fields := []argNode{}
		for {
			p.skip()
			if p.accept("}") {
				return vNode{t: "obj", fields: fields}
			}
			name := p.name()
			p.expect(":")
			fields = append(fields, argNode{name: name, value: p.readValue()})
		}
	}
	if c == "\"" {
		return vNode{t: "str", value: p.readString()}
	}
	if c == "-" || (c >= "0" && c <= "9") {
		return p.readNumber()
	}
	word := p.name()
	if word == "true" {
		return vNode{t: "bool", value: true}
	}
	if word == "false" {
		return vNode{t: "bool", value: false}
	}
	if word == "null" {
		return vNode{t: "null"}
	}
	return vNode{t: "name", value: word}
}

func (p *parser) readString() string {
	p.expect("\"")
	var out strings.Builder
	for {
		if p.pos >= len(p.src) || p.src[p.pos] == '"' {
			p.pos++
			return out.String()
		}
		c := p.src[p.pos]
		if c == '\\' {
			e := byte(0)
			if p.pos+1 < len(p.src) {
				e = p.src[p.pos+1]
			}
			switch e {
			case 'n':
				out.WriteByte('\n')
			case 't':
				out.WriteByte('\t')
			case 'r':
				out.WriteByte('\r')
			case '"':
				out.WriteByte('"')
			case '\\':
				out.WriteByte('\\')
			case '/':
				out.WriteByte('/')
			case 'u':
				start := p.pos + 2
				end := p.pos + 6
				if end > len(p.src) {
					end = len(p.src)
				}
				if start > end {
					start = end
				}
				out.Write([]byte(p.src[start:end]))
				p.pos += 4
			default:
				if e != 0 {
					out.WriteByte(e)
				}
			}
			p.pos += 2
			continue
		}
		out.WriteByte(c)
		p.pos++
	}
}

func (p *parser) readNumber() vNode {
	p.skip()
	m := gqlNumberRe.FindString(p.src[p.pos:])
	if m == "" {
		panic("GraphQL parse error: expected a number")
	}
	p.pos += len(m)
	if strings.ContainsAny(m, ".eE") {
		f, _ := strconv.ParseFloat(m, 64)
		return vNode{t: "float", value: f}
	}
	f, _ := strconv.ParseFloat(m, 64)
	return vNode{t: "int", value: f}
}

/** The runtime value of an argument node, with $variables substituted. */
func valueOf(node vNode, vars map[string]any) any {
	switch node.t {
	case "var":
		if v, ok := vars[node.name]; ok {
			return v
		}
		return nil
	case "str", "int", "float", "bool", "name":
		return node.value
	case "null":
		return nil
	case "list":
		items := make([]any, 0, len(node.items))
		for _, item := range node.items {
			items = append(items, valueOf(item, vars))
		}
		return items
	case "obj":
		o := map[string]any{}
		for _, f := range node.fields {
			o[f.name] = valueOf(f.value, vars)
		}
		return o
	}
	return nil
}

// ---------------------------------------------------------------------------
// Global ids and cursors
// ---------------------------------------------------------------------------

func repoGlobalId(id int) string {
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("Repository:%d", id)))
}

func issueGlobalId(repoId, issueId int) string {
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("Issue:%d:%d", repoId, issueId)))
}

func pullGlobalId(repoId, pullId int) string {
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("PullRequest:%d:%d", repoId, pullId)))
}

func commentGlobalId(repoId, commentId int) string {
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("IssueComment:%d:%d", repoId, commentId)))
}

/** Decode a root id ("Repository:<id>"); -1 when the id is not that shape. */
func decodeRootId(id any, prefix string) int {
	str, ok := id.(string)
	if !ok || str == "" {
		return -1
	}
	text := string(nodeBase64(str))
	if !strings.HasPrefix(text, prefix+":") {
		return -1
	}
	first := strings.Split(text[len(prefix)+1:], ":")[0]
	n, err := strconv.Atoi(first)
	if err != nil || n <= 0 {
		return -1
	}
	return n
}

/** Decode an item id ("Issue:<repoId>:<itemId>" or the PullRequest form). */
func decodeItemId(id any) (repoID, itemID int, ok bool) {
	str, isString := id.(string)
	if !isString || str == "" {
		return 0, 0, false
	}
	text := string(nodeBase64(str))
	var r, it int
	var err error
	if m := itemIdRe.FindStringSubmatch(text); m != nil {
		if r, err = strconv.Atoi(m[1]); err != nil {
			return 0, 0, false
		}
		if it, err = strconv.Atoi(m[2]); err != nil {
			return 0, 0, false
		}
		return r, it, true
	}
	if m := pullIdRe.FindStringSubmatch(text); m != nil {
		if r, err = strconv.Atoi(m[1]); err != nil {
			return 0, 0, false
		}
		if it, err = strconv.Atoi(m[2]); err != nil {
			return 0, 0, false
		}
		return r, it, true
	}
	return 0, 0, false
}

func encodeCursor(index int) string {
	return base64.StdEncoding.EncodeToString([]byte("cursor:" + strconv.Itoa(index)))
}

func decodeCursor(cursor string) int {
	text := string(nodeBase64(cursor))
	m := cursorRe.FindStringSubmatch(text)
	if m == nil {
		return -1
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return -1
	}
	return n
}

func repoById(store *Store, id int) *Repo {
	for _, repo := range store.Repos {
		if repo.ID == id {
			return repo
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Execution
// ---------------------------------------------------------------------------

type gqlError struct {
	Message string `json:"message"`
}

type gqlCtx struct {
	store  *Store
	vars   map[string]any
	errors []gqlError
}

type stepKind string

const (
	stepScalar             stepKind = "scalar"
	stepQuery              stepKind = "query"
	stepViewer             stepKind = "viewer"
	stepRepository         stepKind = "repository"
	stepIssue              stepKind = "issue"
	stepIssueConn          stepKind = "issueConn"
	stepPageInfo           stepKind = "pageInfo"
	stepUser               stepKind = "user"
	stepRef                stepKind = "ref"
	stepPull               stepKind = "pull"
	stepPullConn           stepKind = "pullConn"
	stepFile               stepKind = "file"
	stepFilesConn          stepKind = "filesConn"
	stepNode               stepKind = "node"
	stepGqlType            stepKind = "gqlType"
	stepNamed              stepKind = "named"
	stepCreateIssuePayload stepKind = "createIssuePayload"
	stepCloseIssuePayload  stepKind = "closeIssuePayload"
	stepCreatePullPayload  stepKind = "createPullPayload"
	stepMergePullPayload   stepKind = "mergePullPayload"
	stepAddCommentPayload  stepKind = "addCommentPayload"
	stepCommentEdge        stepKind = "commentEdge"
	stepComment            stepKind = "comment"
	stepList               stepKind = "list"
)

type step struct {
	kind     stepKind
	typeName string
	value    any
}

func scalarString(value string) *step {
	return &step{kind: stepScalar, typeName: "String", value: value}
}

func scalarNumber(value float64) *step {
	return &step{kind: stepScalar, typeName: "Int", value: value}
}

func scalarBool(value bool) *step {
	return &step{kind: stepScalar, typeName: "Boolean", value: value}
}

func errorStep(typeName string, kind stepKind) *step {
	return &step{kind: kind, typeName: typeName, value: nil}
}

// Value boards for the composite steps.
type repoIssueValue struct {
	repo  *Repo
	issue issue
}

type repoPullValue struct {
	repo *Repo
	pull pull
}

type issueConnValue struct {
	repo      *Repo
	nodes     []issue
	hasNext   bool
	endCursor any
}

type pullConnValue struct {
	repo  *Repo
	nodes []pull
}

type nodeValue struct {
	nodeID string
	repo   *Repo
}

type commentValue struct {
	repo    *Repo
	comment issueComment
}

type pageInfoValue struct {
	hasNext   bool
	endCursor any
}

type userValue struct {
	login string
}

type refValue struct {
	name string
}

type fileValue struct {
	path      string
	additions int
	deletions int
}

type namedValue struct {
	name string
}

type commentNodeValue struct {
	id   string
	body string
}

// The introspection surface gh probes for, plus whatever a realistic schema holds.
type gqlTypeDef struct {
	kind       string
	fields     []string
	enumValues []string
}

var gqlTypes = map[string]gqlTypeDef{
	"Issue":       {kind: "OBJECT", fields: []string{"id", "number", "title", "state", "body"}},
	"PullRequest": {kind: "OBJECT", fields: []string{"id", "number", "headRefName", "baseRefName", "mergeable", "state", "merged", "files"}},
	"Repository":  {kind: "OBJECT", fields: []string{"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest"}},
	"SearchType":  {kind: "ENUM", fields: []string{}, enumValues: []string{"ISSUE", "REPOSITORY", "USER", "COMMIT", "CODE"}},
	"User":        {kind: "OBJECT", fields: []string{"login", "id"}},
	"Viewer":      {kind: "OBJECT", fields: []string{"login"}},
	"Node":        {kind: "INTERFACE", fields: []string{"id"}},
}

func issueOrderDesc(a, b issue, field string, commentCounts map[int]int) int {
	c := 0
	if field == "UPDATED_AT" {
		if a.UpdatedAt < b.UpdatedAt {
			c = 1
		} else if a.UpdatedAt > b.UpdatedAt {
			c = -1
		}
	} else if field == "COMMENTS" {
		c = (commentCounts[b.Number]) - (commentCounts[a.Number])
	} else {
		if a.CreatedAt < b.CreatedAt {
			c = 1
		} else if a.CreatedAt > b.CreatedAt {
			c = -1
		}
	}
	if c == 0 {
		c = b.Number - a.Number
	}
	return c
}

func issuesConnection(repo *Repo, args map[string]any) *step {
	nodes := repo.Issues
	if states, hasStates := args["states"]; hasStates && states != nil {
		wanted := []string{}
		if arr, isArray := states.([]any); isArray {
			for _, item := range arr {
				wanted = append(wanted, fmt.Sprint(item))
			}
		} else {
			wanted = append(wanted, fmt.Sprint(states))
		}
		if len(wanted) > 0 {
			filtered := []issue{}
			for _, iss := range nodes {
				stateName := "CLOSED"
				if iss.State == "open" {
					stateName = "OPEN"
				}
				if containsStr(wanted, stateName) {
					filtered = append(filtered, iss)
				}
			}
			nodes = filtered
		}
	}
	field := "CREATED_AT"
	direction := "DESC"
	if orderBy, ok := args["orderBy"].(map[string]any); ok {
		if f, ok := orderBy["field"].(string); ok {
			field = f
		}
		if d, ok := orderBy["direction"].(string); ok {
			direction = d
		}
	}
	commentCounts := map[int]int{}
	for _, comment := range repo.Comments {
		commentCounts[comment.IssueNumber]++
	}
	sign := 1
	if direction == "ASC" {
		sign = -1
	}
	sorted := append([]issue(nil), nodes...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sign*issueOrderDesc(sorted[i], sorted[j], field, commentCounts) < 0
	})
	start := 0
	if after, ok := args["after"].(string); ok && after != "" {
		if index := decodeCursor(after); index >= 0 {
			start = index + 1
			if start > len(sorted) {
				start = len(sorted)
			}
		}
	}
	first := len(sorted)
	if f, ok := args["first"].(float64); ok && f > 0 {
		first = int(f)
	}
	end := start + first
	if end > len(sorted) {
		end = len(sorted)
	}
	slice := sorted[start:end]
	hasNext := end < len(sorted)
	var endCursor any
	if len(slice) > 0 {
		endCursor = encodeCursor(start + len(slice) - 1)
	}
	return &step{kind: stepIssueConn, typeName: "IssueConnection", value: issueConnValue{repo: repo, nodes: slice, hasNext: hasNext, endCursor: endCursor}}
}

func pullFilesField(repo *Repo, p pull, args map[string]any) *step {
	base := peelShallow(repo.Objects, p.BaseSha)
	headTip := refSha(repo, branchRef(p.HeadRef))
	if headTip == "" {
		headTip = p.HeadSha
	}
	diff := diffFiles(repo, base, peelShallow(repo.Objects, headTip))
	first := len(diff)
	if f, ok := args["first"].(float64); ok && f > 0 {
		first = int(f)
	}
	if first > len(diff) {
		first = len(diff)
	}
	files := make([]*step, 0, first)
	for _, file := range diff[:first] {
		files = append(files, &step{kind: stepFile, typeName: "PullRequestChangedFile", value: fileValue{path: file.Filename, additions: file.Additions, deletions: file.Deletions}})
	}
	return &step{kind: stepFilesConn, typeName: "PullRequestChangedFileConnection", value: files}
}

func argStr(args map[string]any, name string) string {
	if s, ok := args[name].(string); ok {
		return s
	}
	return ""
}

/** `number` is a number when the caller passed one, otherwise the JS `Number(...)` coercion. */
func argInt(args map[string]any, name string) int {
	if f, ok := args[name].(float64); ok {
		return int(f)
	}
	if s, ok := args[name].(string); ok {
		if n, err := strconv.Atoi(s); err == nil {
			return n
		}
	}
	return 0
}

func containsStr(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

func toObj(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func fieldOf(s *step, name string, args map[string]any, ctx *gqlCtx) *step {
	switch s.kind {
	case stepQuery:
		switch name {
		case "viewer":
			return &step{kind: stepViewer, typeName: "Viewer", value: viewer.Login}
		case "repository":
			owner := argStr(args, "owner")
			repoName := argStr(args, "name")
			repo, ok := getRepo(ctx.store, owner, repoName)
			if !ok {
				return &step{kind: stepRepository, typeName: "Repository", value: nil}
			}
			return &step{kind: stepRepository, typeName: "Repository", value: repo}
		case "node":
			id := argStr(args, "id")
			repoID := decodeRootId(id, "Repository")
			var repo *Repo
			if repoID > 0 {
				repo = repoById(ctx.store, repoID)
			}
			return &step{kind: stepNode, typeName: "Node", value: nodeValue{nodeID: id, repo: repo}}
		case "__type":
			typeName := ""
			if t, ok := args["name"].(string); ok && t != "" {
				typeName = t
			}
			var value any
			if typeName == "" {
				value = nil
			} else {
				value = typeName
			}
			return &step{kind: stepGqlType, typeName: "__Type", value: value}
		case "createIssue":
			input := toObj(args["input"])
			repoID := decodeRootId(input["repositoryId"], "Repository")
			var repo *Repo
			if repoID > 0 {
				repo = repoById(ctx.store, repoID)
			}
			if repo == nil {
				ctx.errors = append(ctx.errors, gqlError{Message: "Could not resolve to a Repository with the provided global ID."})
				return errorStep("CreateIssuePayload", stepCreateIssuePayload)
			}
			title := argStr(input, "title")
			body := argStr(input, "body")
			made, err := createIssueRecord(repo, issueInput{Title: title, Body: &body})
			if err != nil {
				ctx.errors = append(ctx.errors, gqlError{Message: err.Error()})
				return errorStep("CreateIssuePayload", stepCreateIssuePayload)
			}
			return &step{kind: stepCreateIssuePayload, typeName: "CreateIssuePayload", value: repoIssueValue{repo: repo, issue: made}}
		case "closeIssue":
			input := toObj(args["input"])
			found := itemFromId(ctx.store, input["issueId"])
			if found == nil || found.kind != "issue" {
				ctx.errors = append(ctx.errors, gqlError{Message: "Could not resolve to a Issue with the provided global ID."})
				return errorStep("CloseIssuePayload", stepCloseIssuePayload)
			}
			closeIssue(found.repo, &found.repo.Issues[found.index])
			return &step{kind: stepCloseIssuePayload, typeName: "CloseIssuePayload", value: repoIssueValue{repo: found.repo, issue: found.repo.Issues[found.index]}}
		case "addComment":
			input := toObj(args["input"])
			found := itemFromId(ctx.store, input["subjectId"])
			if found == nil || found.kind != "issue" {
				ctx.errors = append(ctx.errors, gqlError{Message: "Could not resolve to a Issue with the provided global ID."})
				return errorStep("AddCommentPayload", stepAddCommentPayload)
			}
			comment := addComment(found.repo, &found.repo.Issues[found.index], argStr(input, "body"))
			return &step{kind: stepAddCommentPayload, typeName: "AddCommentPayload", value: commentValue{repo: found.repo, comment: comment}}
		case "createPullRequest":
			input := toObj(args["input"])
			repoID := decodeRootId(input["repositoryId"], "Repository")
			var repo *Repo
			if repoID > 0 {
				repo = repoById(ctx.store, repoID)
			}
			if repo == nil {
				ctx.errors = append(ctx.errors, gqlError{Message: "Could not resolve to a Repository with the provided global ID."})
				return errorStep("CreatePullRequestPayload", stepCreatePullPayload)
			}
			made, err := createPullRecord(repo, pullInput{
				Title: argStr(input, "title"),
				Body:  argStr(input, "body"),
				Head:  argStr(input, "headRefName"),
				Base:  argStr(input, "baseRefName"),
			})
			if err != nil {
				ctx.errors = append(ctx.errors, gqlError{Message: err.Error()})
				return errorStep("CreatePullRequestPayload", stepCreatePullPayload)
			}
			return &step{kind: stepCreatePullPayload, typeName: "CreatePullRequestPayload", value: repoPullValue{repo: repo, pull: made}}
		case "mergePullRequest":
			input := toObj(args["input"])
			found := itemFromId(ctx.store, input["pullRequestId"])
			if found == nil || found.kind != "pull" {
				ctx.errors = append(ctx.errors, gqlError{Message: "Could not resolve to a PullRequest with the provided global ID."})
				return errorStep("MergePullRequestPayload", stepMergePullPayload)
			}
			if _, err := mergePullRecord(found.repo, &found.repo.Pulls[found.index]); err != nil {
				ctx.errors = append(ctx.errors, gqlError{Message: "Pull request is not mergeable"})
				return errorStep("MergePullRequestPayload", stepMergePullPayload)
			}
			return &step{kind: stepMergePullPayload, typeName: "MergePullRequestPayload", value: repoPullValue{repo: found.repo, pull: found.repo.Pulls[found.index]}}
		}
		return nil
	case stepViewer:
		if name == "login" {
			return scalarString(viewer.Login)
		}
		return nil
	case stepUser:
		value := s.value.(userValue)
		if name == "login" {
			return scalarString(value.login)
		}
		if name == "id" {
			return scalarNumber(1000)
		}
		return nil
	case stepRepository:
		repo, _ := s.value.(*Repo)
		if repo == nil {
			return nil
		}
		switch name {
		case "id":
			return scalarString(repoGlobalId(repo.ID))
		case "databaseId":
			return scalarNumber(float64(repo.ID))
		case "name":
			return scalarString(repo.Name)
		case "nameWithOwner":
			return scalarString(repo.Owner + "/" + repo.Name)
		case "owner":
			return &step{kind: stepUser, typeName: "User", value: userValue{login: repo.Owner}}
		case "defaultBranchRef":
			return &step{kind: stepRef, typeName: "Ref", value: refValue{name: repo.DefaultBranch}}
		case "viewerPermission":
			return scalarString("ADMIN")
		case "hasIssuesEnabled":
			return scalarBool(true)
		case "issue":
			number := argInt(args, "number")
			var iss *issue
			for i := range repo.Issues {
				if repo.Issues[i].Number == number {
					iss = &repo.Issues[i]
					break
				}
			}
			if iss == nil {
				return &step{kind: stepIssue, typeName: "Issue", value: nil}
			}
			return &step{kind: stepIssue, typeName: "Issue", value: repoIssueValue{repo: repo, issue: *iss}}
		case "issues":
			return issuesConnection(repo, args)
		case "pullRequests":
			first := len(repo.Pulls)
			if f, ok := args["first"].(float64); ok && f > 0 {
				first = int(f)
			}
			if first > len(repo.Pulls) {
				first = len(repo.Pulls)
			}
			return &step{kind: stepPullConn, typeName: "PullRequestConnection", value: pullConnValue{repo: repo, nodes: repo.Pulls[:first]}}
		case "pullRequest":
			number := argInt(args, "number")
			var p *pull
			for i := range repo.Pulls {
				if repo.Pulls[i].Number == number {
					p = &repo.Pulls[i]
					break
				}
			}
			if p == nil {
				return &step{kind: stepPull, typeName: "PullRequest", value: nil}
			}
			return &step{kind: stepPull, typeName: "PullRequest", value: repoPullValue{repo: repo, pull: *p}}
		}
		return nil
	case stepIssue:
		if s.value == nil {
			return nil
		}
		value := s.value.(repoIssueValue)
		repo, iss := value.repo, value.issue
		switch name {
		case "id":
			return scalarString(issueGlobalId(repo.ID, iss.ID))
		case "number":
			return scalarNumber(float64(iss.Number))
		case "title":
			return scalarString(iss.Title)
		case "state":
			stateName := "CLOSED"
			if iss.State == "open" {
				stateName = "OPEN"
			}
			return scalarString(stateName)
		case "body":
			return scalarString(iss.Body)
		}
		return nil
	case stepIssueConn:
		value := s.value.(issueConnValue)
		switch name {
		case "nodes":
			nodes := make([]*step, 0, len(value.nodes))
			for _, iss := range value.nodes {
				nodes = append(nodes, &step{kind: stepIssue, typeName: "Issue", value: repoIssueValue{repo: value.repo, issue: iss}})
			}
			return &step{kind: stepList, typeName: "Issue", value: nodes}
		case "pageInfo":
			return &step{kind: stepPageInfo, typeName: "PageInfo", value: pageInfoValue{hasNext: value.hasNext, endCursor: value.endCursor}}
		}
		return nil
	case stepPageInfo:
		value := s.value.(pageInfoValue)
		switch name {
		case "hasNextPage":
			return scalarBool(value.hasNext)
		case "endCursor":
			if value.endCursor == nil {
				return &step{kind: stepScalar, typeName: "String", value: nil}
			}
			return scalarString(value.endCursor.(string))
		}
		return nil
	case stepRef:
		value := s.value.(refValue)
		switch name {
		case "name":
			return scalarString(value.name)
		case "id":
			return scalarString(base64.StdEncoding.EncodeToString([]byte("Ref:" + value.name)))
		}
		return nil
	case stepPullConn:
		value := s.value.(pullConnValue)
		if name == "nodes" {
			nodes := make([]*step, 0, len(value.nodes))
			for _, p := range value.nodes {
				nodes = append(nodes, &step{kind: stepPull, typeName: "PullRequest", value: repoPullValue{repo: value.repo, pull: p}})
			}
			return &step{kind: stepList, typeName: "PullRequest", value: nodes}
		}
		return nil
	case stepPull:
		if s.value == nil {
			return nil
		}
		value := s.value.(repoPullValue)
		repo, p := value.repo, value.pull
		switch name {
		case "id":
			return scalarString(pullGlobalId(repo.ID, p.ID))
		case "number":
			return scalarNumber(float64(p.Number))
		case "state":
			stateName := "CLOSED"
			if p.Merged {
				stateName = "MERGED"
			} else if p.State == "open" {
				stateName = "OPEN"
			}
			return scalarString(stateName)
		case "merged":
			return scalarBool(p.Merged)
		case "headRefName":
			return scalarString(p.HeadRef)
		case "baseRefName":
			return scalarString(p.BaseRef)
		case "mergeable":
			return scalarString("MERGEABLE")
		case "files":
			return pullFilesField(repo, p, args)
		}
		return nil
	case stepFilesConn:
		value := s.value.([]*step)
		if name == "nodes" {
			return &step{kind: stepList, typeName: "PullRequestChangedFile", value: value}
		}
		return nil
	case stepFile:
		value := s.value.(fileValue)
		switch name {
		case "path":
			return scalarString(value.path)
		case "additions":
			return scalarNumber(float64(value.additions))
		case "deletions":
			return scalarNumber(float64(value.deletions))
		}
		return nil
	case stepNode:
		value := s.value.(nodeValue)
		if name == "id" {
			return scalarString(value.nodeID)
		}
		return nil
	case stepGqlType:
		typeName := ""
		if t, ok := s.value.(string); ok {
			typeName = t
		}
		typeDef := gqlTypes[typeName]
		switch name {
		case "name":
			if typeName == "" {
				return &step{kind: stepScalar, typeName: "String", value: nil}
			}
			return scalarString(typeName)
		case "kind":
			return scalarString(typeDef.kind)
		case "fields":
			entries := make([]*step, 0, len(typeDef.fields))
			for _, fieldName := range typeDef.fields {
				entries = append(entries, &step{kind: stepNamed, typeName: "__Field", value: namedValue{name: fieldName}})
			}
			return &step{kind: stepList, typeName: "__Field", value: entries}
		case "enumValues":
			entries := make([]*step, 0, len(typeDef.enumValues))
			for _, entry := range typeDef.enumValues {
				entries = append(entries, &step{kind: stepNamed, typeName: "__EnumValue", value: namedValue{name: entry}})
			}
			return &step{kind: stepList, typeName: "__EnumValue", value: entries}
		}
		return nil
	case stepNamed:
		value := s.value.(namedValue)
		if name == "name" {
			return scalarString(value.name)
		}
		return nil
	case stepCreateIssuePayload:
		if name == "issue" {
			if s.value == nil {
				return &step{kind: stepIssue, typeName: "Issue", value: nil}
			}
			return &step{kind: stepIssue, typeName: "Issue", value: s.value.(repoIssueValue)}
		}
		return nil
	case stepCloseIssuePayload:
		if name == "issue" {
			if s.value == nil {
				return &step{kind: stepIssue, typeName: "Issue", value: nil}
			}
			return &step{kind: stepIssue, typeName: "Issue", value: s.value.(repoIssueValue)}
		}
		return nil
	case stepCreatePullPayload, stepMergePullPayload:
		if name == "pullRequest" {
			if s.value == nil {
				return &step{kind: stepPull, typeName: "PullRequest", value: nil}
			}
			return &step{kind: stepPull, typeName: "PullRequest", value: s.value.(repoPullValue)}
		}
		return nil
	case stepAddCommentPayload:
		if name == "commentEdge" {
			if s.value == nil {
				return &step{kind: stepCommentEdge, typeName: "IssueCommentEdge", value: nil}
			}
			return &step{kind: stepCommentEdge, typeName: "IssueCommentEdge", value: s.value.(commentValue)}
		}
		return nil
	case stepCommentEdge:
		if name == "node" {
			if s.value == nil {
				return &step{kind: stepComment, typeName: "IssueComment", value: nil}
			}
			value := s.value.(commentValue)
			return &step{kind: stepComment, typeName: "IssueComment", value: commentNodeValue{id: commentGlobalId(value.repo.ID, value.comment.ID), body: value.comment.Body}}
		}
		return nil
	case stepComment:
		if s.value == nil {
			return nil
		}
		value := s.value.(commentNodeValue)
		switch name {
		case "id":
			return scalarString(value.id)
		case "body":
			return scalarString(value.body)
		}
		return nil
	}
	return nil
}

func itemFromId(store *Store, id any) *itemFound {
	repoID, itemID, ok := decodeItemId(id)
	if !ok {
		return nil
	}
	repo := repoById(store, repoID)
	if repo == nil {
		return nil
	}
	for i := range repo.Issues {
		if repo.Issues[i].ID == itemID {
			return &itemFound{repo: repo, kind: "issue", index: i}
		}
	}
	for i := range repo.Pulls {
		if repo.Pulls[i].ID == itemID {
			return &itemFound{repo: repo, kind: "pull", index: i}
		}
	}
	return nil
}

type itemFound struct {
	repo  *Repo
	kind  string
	index int
}

func withFragment(s *step, fragType string) *step {
	if fragType == s.typeName {
		return s
	}
	if s.kind == stepNode && fragType == "Repository" {
		value := s.value.(nodeValue)
		if value.repo == nil {
			return nil
		}
		return &step{kind: stepRepository, typeName: "Repository", value: value.repo}
	}
	return nil
}

func resolveOne(s *step, selections []sel, ctx *gqlCtx) any {
	if s.kind == stepScalar {
		return s.value
	}
	if s.value == nil {
		return nil
	}
	if s.kind == stepList {
		items := s.value.([]*step)
		out := make([]any, 0, len(items))
		for _, item := range items {
			out = append(out, resolveOne(item, selections, ctx))
		}
		return out
	}
	out := map[string]any{}
	resolveStep(s, selections, out, ctx)
	return out
}

func writeValue(out map[string]any, key string, value *step, selections []sel, ctx *gqlCtx) {
	out[key] = resolveOne(value, selections, ctx)
}

func resolveStep(s *step, selections []sel, out map[string]any, ctx *gqlCtx) {
	for _, selItem := range selections {
		if selItem.kind == "frag" {
			if selItem.fragType == "" {
				resolveStep(s, selItem.selections, out, ctx)
				continue
			}
			concrete := withFragment(s, selItem.fragType)
			if concrete == nil {
				continue
			}
			resolveStep(concrete, selItem.selections, out, ctx)
			continue
		}
		args := map[string]any{}
		for _, arg := range selItem.args {
			args[arg.name] = valueOf(arg.value, ctx.vars)
		}
		fieldName := selItem.name
		next := fieldOf(s, fieldName, args, ctx)
		if next == nil {
			ctx.errors = append(ctx.errors, gqlError{Message: fmt.Sprintf("Cannot query field \"%s\" on type \"%s\".", fieldName, s.typeName)})
			continue
		}
		key := fieldName
		if selItem.alias != "" {
			key = selItem.alias
		}
		writeValue(out, key, next, selItem.selections, ctx)
	}
}

func execute(document string, variables map[string]any, store *Store) map[string]any {
	ops, parseErr := parseDocument(document)
	if parseErr != nil {
		return map[string]any{
			"data":   nil,
			"errors": []gqlError{{Message: "Cannot parse the operation document: " + parseErr.Error()}},
		}
	}
	if len(ops) == 0 {
		return map[string]any{
			"data":   nil,
			"errors": []gqlError{{Message: "Must provide an operation."}},
		}
	}
	ctx := &gqlCtx{store: store, vars: variables}
	data := map[string]any{}
	resolveStep(&step{kind: stepQuery, typeName: "Query", value: store}, ops[0].selections, data, ctx)
	return map[string]any{"data": data, "errors": ctx.errors}
}

// ---------------------------------------------------------------------------
// The HTTP entry point
// ---------------------------------------------------------------------------

func handleGraphql(res *response, store *Store, body []byte) {
	var parsed any
	if len(body) == 0 {
		parsed = nil
	} else if err := json.Unmarshal(body, &parsed); err != nil {
		sendError(res, 400, "Problems parsing JSON")
		return
	}
	if array, isArray := parsed.([]any); isArray {
		responses := []map[string]any{}
		for _, entry := range array {
			if entry == nil {
				responses = append(responses, map[string]any{
					"data":   nil,
					"errors": []gqlError{{Message: "Each batched operation must be an object."}},
				})
				continue
			}
			operation, ok := entry.(map[string]any)
			if !ok {
				responses = append(responses, map[string]any{
					"data":   nil,
					"errors": []gqlError{{Message: "Each batched operation must be an object."}},
				})
				continue
			}
			query := ""
			if q, isString := operation["query"].(string); isString {
				query = q
			}
			variables := map[string]any{}
			if v, isObj := operation["variables"].(map[string]any); isObj {
				variables = v
			}
			responses = append(responses, execute(query, variables, store))
		}
		sendJSON(res, 200, responses)
		return
	}
	operation, ok := parsed.(map[string]any)
	if !ok {
		sendError(res, 400, "Problems parsing JSON")
		return
	}
	query := ""
	if q, isString := operation["query"].(string); isString {
		query = q
	}
	variables := map[string]any{}
	if v, isObj := operation["variables"].(map[string]any); isObj {
		variables = v
	}
	sendJSON(res, 200, execute(query, variables, store))
}
