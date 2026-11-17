// The Model Context Protocol endpoint: JSON-RPC 2.0 over the Streamable HTTP transport, revision
// 2026-07-28 (`server/discover`) and the older handshake (`initialize`), with tools that act on the
// same store REST reads. Everything here is deterministic and allocation-light: the tools list is
// serialised once at startup so two tools/list calls answer byte-identical bodies, and no response
// ever depends on a map's iteration order (lookup keys come from the request or sorted maps).

package main

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
)

// JSON-RPC error codes the transport answers with.
const (
	mcpParseError     = -32700
	mcpInvalidRequest = -32600
	mcpMethodNotFound = -32601
	mcpInvalidParams  = -32602
)

// The preferred revision and the full list a discover answer names, newest first.
const mcpPreferredRevision = "2026-07-28"

var mcpSupportedVersions = []string{mcpPreferredRevision, "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

// The identity every initialize and discover answer carries.
type mcpServerInfoVal struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

var mcpServerInfo = mcpServerInfoVal{Name: "makefun-gh-forge", Version: "1.0.0"}

// The capabilities advertised on every handshake answer: the tools capability, and nothing else.
var mcpCapabilities = map[string]any{"tools": map[string]any{"listChanged": false}}

// ---------------------------------------------------------------------------
// the tool table, built once

type mcpSchema struct {
	Type                 string         `json:"type"`
	Properties           map[string]any `json:"properties"`
	Required             []string       `json:"required"`
	AdditionalProperties bool           `json:"additionalProperties"`
}

type mcpTool struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	InputSchema mcpSchema `json:"inputSchema"`
}

var mcpStrProp = map[string]any{"type": "string"}

// The tools (all eight src/mcp.ts defines; the required six are the first six).
func mcpBuildTools() []mcpTool {
	ownerRepoProps := map[string]any{"owner": mcpStrProp, "repo": mcpStrProp, "full_name": mcpStrProp}
	issueNumberProps := map[string]any{"owner": mcpStrProp, "repo": mcpStrProp, "full_name": mcpStrProp, "issue_number": map[string]any{"type": "number"}}
	return []mcpTool{
		{
			Name:        "create_repo",
			Description: "Create a repository owned by the authenticated user. Returns the repository object.",
			InputSchema: mcpSchema{Type: "object", Properties: map[string]any{"name": mcpStrProp, "description": mcpStrProp, "private": map[string]any{"type": "boolean"}}, Required: []string{"name"}, AdditionalProperties: true},
		},
		{
			Name:        "get_repo",
			Description: "Read one repository by its owner and name (or full_name \"owner/repo\").",
			InputSchema: mcpSchema{Type: "object", Properties: ownerRepoProps, Required: []string{}, AdditionalProperties: true},
		},
		{
			Name:        "create_issue",
			Description: "Open an issue in a repository. Returns the issue object with its number.",
			InputSchema: mcpSchema{Type: "object", Properties: map[string]any{"owner": mcpStrProp, "repo": mcpStrProp, "full_name": mcpStrProp, "title": mcpStrProp, "body": mcpStrProp, "labels": map[string]any{"type": "array", "items": mcpStrProp}}, Required: []string{"title"}, AdditionalProperties: true},
		},
		{
			Name:        "list_issues",
			Description: "List the issues of a repository, optionally filtered by state (open, closed or all).",
			InputSchema: mcpSchema{Type: "object", Properties: map[string]any{"owner": mcpStrProp, "repo": mcpStrProp, "full_name": mcpStrProp, "state": mcpStrProp}, Required: []string{}, AdditionalProperties: true},
		},
		{
			Name:        "create_pull_request",
			Description: "Open a pull request from a head branch onto a base branch of a repository.",
			InputSchema: mcpSchema{Type: "object", Properties: map[string]any{"owner": mcpStrProp, "repo": mcpStrProp, "full_name": mcpStrProp, "title": mcpStrProp, "body": mcpStrProp, "head": mcpStrProp, "base": mcpStrProp}, Required: []string{"title", "head"}, AdditionalProperties: true},
		},
		{
			Name:        "merge_pull_request",
			Description: "Squash-merge a pull request and land its commit on the base branch.",
			InputSchema: mcpSchema{Type: "object", Properties: map[string]any{"owner": mcpStrProp, "repo": mcpStrProp, "full_name": mcpStrProp, "pull_number": map[string]any{"type": "number"}, "number": map[string]any{"type": "number"}}, Required: []string{}, AdditionalProperties: true},
		},
		{
			Name:        "get_issue",
			Description: "Read one issue of a repository by its number.",
			InputSchema: mcpSchema{Type: "object", Properties: issueNumberProps, Required: []string{}, AdditionalProperties: true},
		},
		{
			Name:        "add_comment",
			Description: "Add a comment to an issue and return it.",
			InputSchema: mcpSchema{Type: "object", Properties: map[string]any{"owner": mcpStrProp, "repo": mcpStrProp, "full_name": mcpStrProp, "issue_number": map[string]any{"type": "number"}, "body": mcpStrProp}, Required: []string{"body"}, AdditionalProperties: true},
		},
	}
}

// The tool table and its serialised result part, both built once.
var mcpTools = mcpBuildTools()
var mcpToolsListJSON = func() string {
	b := marshalJSON(struct {
		Tools []mcpTool `json:"tools"`
	}{mcpTools})
	return string(b)
}()

// ---------------------------------------------------------------------------
// JSON-RPC shape

// The discover result a modern server answers with, field order as src/mcp.ts builds it.
type mcpDiscoverResult struct {
	SupportedVersions []string         `json:"supportedVersions"`
	Capabilities      map[string]any   `json:"capabilities"`
	ServerInfo        mcpServerInfoVal `json:"serverInfo"`
	Instructions      string           `json:"instructions"`
}

func mcpDiscoverResultJSON() string {
	b := marshalJSON(mcpDiscoverResult{
		SupportedVersions: mcpSupportedVersions,
		Capabilities:      mcpCapabilities,
		ServerInfo:        mcpServerInfo,
		Instructions:      "A GitHub-compatible forge: repositories, issues and pull requests.",
	})
	return string(b)
}

// The initialize result: the revision the request chose (or the preferred one), tools and identity.
type mcpInitializeResult struct {
	ProtocolVersion string           `json:"protocolVersion"`
	Capabilities    map[string]any   `json:"capabilities"`
	ServerInfo      mcpServerInfoVal `json:"serverInfo"`
}

func mcpInitializeResultJSON(revision string) string {
	b := marshalJSON(mcpInitializeResult{ProtocolVersion: revision, Capabilities: mcpCapabilities, ServerInfo: mcpServerInfo})
	return string(b)
}

// The two tool result shapes: a success carries structed content, a tool error only its text.
type mcpAddCommentResult struct {
	ID        int    `json:"id"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
}

type mcpMergeResult struct {
	Merged bool   `json:"merged"`
	SHA    string `json:"sha"`
	Number int    `json:"number"`
	State  string `json:"state"`
}

// One JSON-RPC request -> one answer. `answered` false means a notification (202, no body).
type mcpOutcome struct {
	status   int
	body     string
	answered bool
}

/** Send an id the way JSON.stringify does: what the request carried, or null when it carried none. */
func mcpEncodeID(id any) string {
	b, err := json.Marshal(id)
	if err != nil || b == nil {
		return "null"
	}
	return string(b)
}

func mcpRpcResult(id any, resultJSON string) mcpOutcome {
	return mcpOutcome{200, `{"jsonrpc":"2.0","id":` + mcpEncodeID(id) + `,"result":` + resultJSON + `}`, true}
}

func mcpRpcErrorBody(id any, code int, message string) string {
	text, _ := json.Marshal(message)
	return `{"jsonrpc":"2.0","id":` + mcpEncodeID(id) + `,"error":{"code":` + strconv.Itoa(code) + `,"message":` + string(text) + `}}`
}

func mcpRpcError(id any, code int, message string) mcpOutcome {
	return mcpOutcome{200, mcpRpcErrorBody(id, code, message), true}
}

// A tool success: the value's JSON in a text block and again as structured content.
func mcpToolResult(id any, valueJSON string) mcpOutcome {
	text, _ := json.Marshal(valueJSON)
	body := `{"content":[{"type":"text","text":` + string(text) + `}],"structuredContent":` + valueJSON + `,"isError":false}`
	return mcpRpcResult(id, body)
}

// A tool execution error: only the message, flagged with isError, never a JSON-RPC error.
func mcpToolError(id any, message string) mcpOutcome {
	text, _ := json.Marshal(message)
	body := `{"content":[{"type":"text","text":` + string(text) + `}],"isError":true}`
	return mcpRpcResult(id, body)
}

// ---------------------------------------------------------------------------
// argument helpers (src/mcp.ts, translated field for field)

/** A request field that must be a string; anything else is "". */
func mcpStr(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	return ""
}

/** `text()`: a string passes, a number or boolean is String()ed, everything else is "". */
func mcpText(value any) string {
	switch t := value.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	}
	return ""
}

/** The request's params object, or {}. */
func mcpParamsOf(obj map[string]any) map[string]any {
	if m, ok := obj["params"].(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

/** A tools/call's arguments: params.arguments when it is an object, else {}. */
func mcpArgumentsOf(params map[string]any) map[string]any {
	if m, ok := params["arguments"].(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

/** The owner and repository a tool call names: explicit owner+repo, else full_name "owner/repo". */
func mcpRepoOf(store *Store, args map[string]any) *Repo {
	owner := mcpText(args["owner"])
	if owner == "" {
		owner = mcpText(args["owner_login"])
	}
	name := mcpText(args["repo"])
	if name == "" {
		name = mcpText(args["repo_name"])
	}
	if name == "" {
		name = mcpText(args["repository"])
	}
	if owner != "" && name != "" {
		repo, _ := getRepo(store, owner, name)
		return repo
	}
	full := mcpText(args["full_name"])
	if full == "" {
		full = mcpText(args["repository_full_name"])
	}
	if full == "" {
		full = mcpText(args["name_with_owner"])
	}
	if slash := strings.Index(full, "/"); slash > 0 {
		repo, _ := getRepo(store, full[:slash], full[slash+1:])
		return repo
	}
	if name != "" {
		repo, _ := getRepo(store, viewer.Login, name)
		return repo
	}
	return nil
}

/** JS Number.parseInt(uint), base 10: a leading sign, then digits until a non-digit. */
func mcpParseInt10(s string) (int64, bool) {
	i := 0
	neg := false
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		neg = s[i] == '-'
		i++
	}
	var n int64
	anyDigit := false
	for ; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			break
		}
		anyDigit = true
		n = n*10 + int64(c-'0')
	}
	if !anyDigit {
		return 0, false
	}
	if neg {
		n = -n
	}
	return n, true
}

/** The number a tool call names: a number or a parseable string, else NaN. */
func mcpNumberArg(args map[string]any, names ...string) float64 {
	for _, listName := range names {
		value, ok := args[listName]
		if !ok {
			continue
		}
		if f, isNumber := value.(float64); isNumber {
			return f
		}
		if s, isString := value.(string); isString && s != "" {
			if n, ok := mcpParseInt10(s); ok {
				return float64(n)
			}
		}
	}
	return math.NaN()
}

/** One value marshalled exactly as JSON.stringify would send it. */
func mcpJSONText(value any) string {
	return string(marshalJSON(value))
}

// ---------------------------------------------------------------------------
// the tools

func mcpCallTool(store *Store, id any, name string, args map[string]any) mcpOutcome {
	switch name {
	case "create_repo":
		repoName := mcpText(args["name"])
		if repoName == "" {
			repoName = mcpText(args["repo"])
		}
		if repoName == "" {
			return mcpRpcError(id, mcpInvalidParams, "create_repo needs a name")
		}
		if existing, ok := getRepo(store, viewer.Login, repoName); ok {
			return mcpToolResult(id, mcpJSONText(repoJSON(existing)))
		}
		repo := createRepo(store, viewer.Login, repoName, mcpText(args["description"]), args["private"] == true)
		return mcpToolResult(id, mcpJSONText(repoJSON(repo)))
	case "get_repo":
		repo := mcpRepoOf(store, args)
		if repo == nil {
			return mcpToolError(id, "repository not found")
		}
		return mcpToolResult(id, mcpJSONText(repoJSON(repo)))
	case "create_issue":
		repo := mcpRepoOf(store, args)
		if repo == nil {
			return mcpToolError(id, "repository not found")
		}
		labels := []string{}
		if raw, ok := args["labels"].([]any); ok {
			for _, label := range raw {
				if s, isString := label.(string); isString {
					labels = append(labels, s)
				}
			}
		}
		body := mcpText(args["body"])
		made, err := createIssueRecord(repo, issueInput{Title: mcpText(args["title"]), Body: &body, Labels: labels})
		if err != nil {
			return mcpToolError(id, err.Error())
		}
		return mcpToolResult(id, mcpJSONText(issueJSON(repo, made)))
	case "list_issues":
		repo := mcpRepoOf(store, args)
		if repo == nil {
			return mcpToolError(id, "repository not found")
		}
		state := mcpText(args["state"])
		if state == "" {
			state = "open"
		}
		issues := []issueView{}
		for _, iss := range repo.Issues {
			if state == "all" || iss.State == state {
				issues = append(issues, issueJSON(repo, iss))
			}
		}
		return mcpToolResult(id, mcpJSONText(issues))
	case "get_issue":
		repo := mcpRepoOf(store, args)
		if repo == nil {
			return mcpToolError(id, "repository not found")
		}
		number := mcpNumberArg(args, "issue_number", "number", "issueNumber")
		for _, iss := range repo.Issues {
			if float64(iss.Number) == number {
				return mcpToolResult(id, mcpJSONText(issueJSON(repo, iss)))
			}
		}
		return mcpToolError(id, "issue not found")
	case "add_comment":
		repo := mcpRepoOf(store, args)
		if repo == nil {
			return mcpToolError(id, "repository not found")
		}
		number := mcpNumberArg(args, "issue_number", "number", "issueNumber")
		var target *issue
		for i := range repo.Issues {
			if float64(repo.Issues[i].Number) == number {
				target = &repo.Issues[i]
				break
			}
		}
		if target == nil {
			return mcpToolError(id, "issue not found")
		}
		comment := addComment(repo, target, mcpText(args["body"]))
		return mcpToolResult(id, mcpJSONText(mcpAddCommentResult{ID: comment.ID, Body: comment.Body, CreatedAt: comment.CreatedAt}))
	case "create_pull_request":
		repo := mcpRepoOf(store, args)
		if repo == nil {
			return mcpToolError(id, "repository not found")
		}
		head := mcpText(args["head"])
		if head == "" {
			head = mcpText(args["head_ref"])
		}
		if head == "" {
			return mcpRpcError(id, mcpInvalidParams, "create_pull_request needs a head branch")
		}
		base := mcpText(args["base"])
		if base == "" {
			base = mcpText(args["base_ref"])
		}
		made, err := createPullRecord(repo, pullInput{Title: mcpText(args["title"]), Body: mcpText(args["body"]), Head: head, Base: base})
		if err != nil {
			return mcpToolError(id, err.Error())
		}
		return mcpToolResult(id, mcpJSONText(pullJSON(repo, made)))
	case "merge_pull_request":
		repo := mcpRepoOf(store, args)
		if repo == nil {
			return mcpToolError(id, "repository not found")
		}
		number := mcpNumberArg(args, "pull_number", "number", "pullNumber", "pull_request_number")
		if math.IsNaN(number) {
			return mcpRpcError(id, mcpInvalidParams, "merge_pull_request needs a pull_number")
		}
		pull, ok := findPull(repo, int(number))
		if !ok {
			return mcpToolError(id, "pull request not found")
		}
		sha, err := mergePullRecord(repo, pull)
		if err != nil {
			if err == errNoBase {
				return mcpToolError(id, "base branch is gone")
			}
			return mcpToolError(id, "pull request is not mergeable")
		}
		return mcpToolResult(id, mcpJSONText(mcpMergeResult{Merged: true, SHA: sha, Number: pull.Number, State: pull.State}))
	default:
		return mcpRpcError(id, mcpInvalidParams, "unknown tool: "+name)
	}
}

// ---------------------------------------------------------------------------
// one JSON-RPC request -> one answer

func mcpHandleRpc(store *Store, obj map[string]any, authorized bool) mcpOutcome {
	id := any(nil)
	if v, ok := obj["id"]; ok && v != nil {
		id = v
	}
	method := mcpStr(obj["method"])
	if method == "" {
		return mcpRpcError(id, mcpInvalidRequest, "missing method")
	}
	params := mcpParamsOf(obj)
	switch method {
	case "server/discover":
		return mcpRpcResult(id, mcpDiscoverResultJSON())
	case "initialize":
		revision := mcpPreferredRevision
		if pv := mcpStr(params["protocolVersion"]); pv != "" {
			revision = pv
		}
		return mcpRpcResult(id, mcpInitializeResultJSON(revision))
	case "notifications/initialized", "notifications/cancelled":
		return mcpOutcome{answered: false}
	case "ping":
		return mcpRpcResult(id, "{}")
	case "tools/list":
		if v, ok := obj["id"]; !ok || v == nil {
			return mcpOutcome{answered: false}
		}
		return mcpRpcResult(id, mcpToolsListJSON)
	case "tools/call":
		if !authorized {
			return mcpOutcome{401, mcpRpcErrorBody(id, mcpInvalidRequest, "unauthorized"), true}
		}
		rawName := mcpStr(params["name"])
		name := rawName
		if last := strings.LastIndex(rawName, "__"); last >= 0 {
			name = rawName[last+2:]
		}
		var known *mcpTool
		for i := range mcpTools {
			if mcpTools[i].Name == name || mcpTools[i].Name == strings.ToLower(name) {
				known = &mcpTools[i]
				break
			}
		}
		if known == nil {
			return mcpRpcError(id, mcpMethodNotFound, "unknown tool: "+rawName)
		}
		args := mcpArgumentsOf(params)
		for _, key := range known.InputSchema.Required {
			if _, present := args[key]; !present {
				return mcpRpcError(id, mcpInvalidParams, "missing required argument: "+key)
			}
		}
		return mcpCallTool(store, id, known.Name, args)
	default:
		return mcpRpcError(id, mcpMethodNotFound, "unknown method: "+method)
	}
}

// ---------------------------------------------------------------------------
// the transport

/** The id the notification answers carry, the same literal src/mcp.ts uses. */
const mcpRequestID = "GH-MCP"

func mcpSendParseError(res http.ResponseWriter) {
	sendJSONText(res, 400, `{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"Parse error"}}`)
}

/** A notification (or a batch with nothing to answer): 202, id header only, no body. */
func mcpSendNoContent(res http.ResponseWriter) {
	res.Header().Set("x-github-request-id", mcpRequestID)
	res.WriteHeader(202)
}

func handleMcp(res http.ResponseWriter, store *Store, req *http.Request, body []byte) {
	authorized := strings.HasPrefix(req.Header.Get("Authorization"), "Bearer ")
	var parsed any
	if err := json.Unmarshal(body, &parsed); err != nil {
		mcpSendParseError(res)
		return
	}
	if parsed == nil {
		mcpSendParseError(res)
		return
	}
	if arr, isArray := parsed.([]any); isArray {
		answers := make([]string, 0, len(arr))
		for _, item := range arr {
			obj, ok := item.(map[string]any)
			if !ok {
				answers = append(answers, mcpRpcErrorBody(nil, mcpInvalidRequest, "invalid request"))
				continue
			}
			out := mcpHandleRpc(store, obj, authorized)
			if out.answered {
				answers = append(answers, out.body)
			}
		}
		if len(answers) == 0 {
			mcpSendNoContent(res)
			return
		}
		sendJSONText(res, 200, "["+strings.Join(answers, ",")+"]")
		return
	}
	obj, ok := parsed.(map[string]any)
	if !ok {
		mcpSendParseError(res)
		return
	}
	method := mcpStr(obj["method"])
	id := any(nil)
	if v, ok := obj["id"]; ok && v != nil {
		id = v
	}
	// The tools/call without a token is the one 401 the transport owes the suite.
	if method == "tools/call" && !authorized {
		sendJSONText(res, 401, mcpRpcErrorBody(id, mcpInvalidRequest, "unauthorized"))
		return
	}
	out := mcpHandleRpc(store, obj, authorized)
	if !out.answered {
		mcpSendNoContent(res)
		return
	}
	sendJSONText(res, out.status, out.body)
}