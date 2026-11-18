// The forge's HTTP surface: one process, one store. The lifecycle lives in main.go and the transport
// in srv.go; this file only routes a parsed request to its surface (git, REST, GraphQL, MCP), runs the
// token checks, and turns a panic into a 500 JSON answer.

package main

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

/** The trial's bearer token, captured at startup exactly as app.ts does. */
var shopToken = os.Getenv("MAKEFUN_SHOP_TOKEN")

/** The exact Authorization value the token asks for, built once. */
var bearerAuth = []byte("Bearer " + shopToken)

var bearerScheme = []byte("Bearer ")

/** True when the request carries the trial's bearer token. */
func authorized(req *request) bool {
	if shopToken == "" {
		return len(req.auth) >= len(bearerScheme) && string(req.auth[:len(bearerScheme)]) == string(bearerScheme)
	}
	return string(req.auth) == string(bearerAuth)
}

type appHandler struct {
	store *Store
}

func newAppHandler() *appHandler {
	return &appHandler{store: createStore()}
}

/** Answer one request, turning a panic into a 500 JSON reply when nothing was decided yet. */
func (h *appHandler) serve(res *response, req *request) {
	defer func() {
		if r := recover(); r != nil {
			if res.status == 0 {
				sendError(res, 500, "Internal error: "+messageText(r))
			}
		}
	}()
	h.handle(res, req)
}

func messageText(v any) string {
	if err, ok := v.(error); ok {
		return err.Error()
	}
	return fmt.Sprint(v)
}

func (h *appHandler) handle(res *response, req *request) {
	method := req.method
	path := req.path
	query := req.values()

	// The env's boot probe: two calls, no token.
	if path == "/healthz" {
		sendJSON(res, 200, struct {
			Ok bool `json:"ok"`
		}{true})
		return
	}

	// Smart HTTP: /{owner}/{repo}.git/...
	gitIndex := strings.Index(path, ".git/")
	if gitIndex > 0 && strings.HasPrefix(path, "/") {
		if !authorized(req) {
			sendError(res, 401, "Requires authentication")
			return
		}
		body := readBody(req)
		ownerAndRepo := path[1:gitIndex]
		slash := strings.Index(ownerAndRepo, "/")
		if slash > 0 {
			owner := mustUnescape(ownerAndRepo[:slash])
			name := mustUnescape(ownerAndRepo[slash+1:])
			suffix := path[gitIndex+5:]
			handleGit(res, req, h.store, owner, name, suffix, query, body)
			return
		}
	}

	if path == "/mcp" || strings.HasPrefix(path, "/mcp/") {
		body := readBody(req)
		handleMcp(res, h.store, req, body)
		return
	}

	if path == "/api/graphql" {
		if !authorized(req) {
			sendJSON(res, 401, struct {
				Message string `json:"message"`
			}{"Requires authentication"})
			return
		}
		body := readBody(req)
		handleGraphql(res, h.store, body)
		return
	}

	if path == "/api/v3" || strings.HasPrefix(path, "/api/v3/") {
		if !authorized(req) {
			sendError(res, 401, "Requires authentication")
			return
		}
		body := readBody(req)
		rest := ""
		if path != "/api/v3" {
			rest = path[len("/api/v3/"):]
		}
		segments := []string{}
		if rest != "" {
			for _, part := range strings.Split(rest, "/") {
				segments = append(segments, mustUnescape(part))
			}
		}
		host := string(req.host)
		if host == "" {
			host = "127.0.0.1"
		}
		handled := handleRest(res, h.store, method, segments, query, "http://"+host, body)
		if !handled {
			sendError(res, 404, "Not Found")
		}
		return
	}

	sendError(res, 404, "Not Found")
}

/** decodeURIComponent without throwing: a malformed sequence panics, the handler turns that into a 500. */
func mustUnescape(s string) string {
	decoded, err := url.PathUnescape(s)
	if err != nil {
		panic("URIError: URI malformed")
	}
	return decoded
}
