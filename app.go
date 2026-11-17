// The forge's HTTP surface: one process, one store. The lifecycle lives in main.go; this file only
// answers requests. The four surfaces (git, REST, GraphQL, MCP) are filled in by later steps; here
// they answer 404 JSON so the router, the auth checks and the error handling are already real.

package main

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
)

/** The trial's bearer token, captured at startup exactly as app.ts does. */
var shopToken = os.Getenv("MAKEFUN_SHOP_TOKEN")

/** True when the request carries the trial's bearer token. */
func authorized(req *http.Request) bool {
	header := req.Header.Get("Authorization")
	if shopToken == "" {
		return strings.HasPrefix(header, "Bearer ")
	}
	return header == "Bearer "+shopToken
}

/** The path and query of a request URL; never throws. */
func parseURL(u *url.URL) (string, url.Values) {
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	return path, u.Query()
}

type appHandler struct {
	store *Store
}

func newAppHandler() *appHandler {
	return &appHandler{store: createStore()}
}

/** Tracks whether the status line has been written, so a panic can still answer a 500. */
type responseWriter struct {
	http.ResponseWriter
	wroteHeader bool
}

func (w *responseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.wroteHeader = true
		w.ResponseWriter.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

func (h *appHandler) ServeHTTP(res http.ResponseWriter, req *http.Request) {
	w := &responseWriter{ResponseWriter: res}
	defer func() {
		if r := recover(); r != nil {
			// A panic never crashes the process: answer 500 JSON when nothing was sent yet.
			if !w.wroteHeader {
				sendError(w, 500, "Internal error: "+messageText(r))
			}
		}
	}()
	h.handle(w, req)
}

func messageText(v any) string {
	if err, ok := v.(error); ok {
		return err.Error()
	}
	return fmt.Sprint(v)
}

func (h *appHandler) handle(res http.ResponseWriter, req *http.Request) {
	method := req.Method
	path, query := parseURL(req.URL)

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
		handleMcp(res, h.store, body)
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
		host := req.Host
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

// ---------------------------------------------------------------------------
// the four surfaces; this step they answer 404 JSON, later steps fill them in.

func handleMcp(res http.ResponseWriter, _ *Store, _ []byte) {
	sendError(res, 404, "Not Found")
}
