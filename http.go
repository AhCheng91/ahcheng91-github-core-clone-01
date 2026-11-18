// The shared reply helpers every surface replies through: JSON with the headers GitHub sends. The
// transport in srv.go owns the wire; a handler only fills a response, so these are plain struct
// assignments on the hot paths (2,000 issue reads, 200 issue writes) and never log.

package main

import (
	"bytes"
	"encoding/json"
	"sync/atomic"
)

const jsonType = "application/json; charset=utf-8"

/** The cap on one request body, the same 128 MiB src/http.ts uses. */
const defaultBodyLimit = 128 * 1024 * 1024

/**
 * A process-wide counter; request ids are unique per process and never read from the clock. Atomic
 * because concurrent clones answer concurrently (each reply carries its own id).
 */
var requestCounter = atomic.Uint64{}

/** Serialise `body` exactly as JSON.stringify does: no HTML escaping, no trailing newline. */
func marshalJSON(body any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(body); err != nil {
		return []byte("null")
	}
	out := buf.Bytes()
	if n := len(out); n > 0 && out[n-1] == '\n' {
		out = out[:n-1]
	}
	return out
}

/**
 * Answer `body` with its content type; any extra headers are merged in last and win, which is how
 * `location`, `link`, `x-oauth-scopes` and mcp's own request id reach the wire.
 */
func sendBuffer(res *response, status int, body []byte, contentType string, extra ...map[string]string) {
	res.status = status
	res.ctype = contentType
	res.body = body
	if len(extra) == 0 {
		return
	}
	if len(extra) == 1 {
		res.extra = extra[0]
		return
	}
	merged := make(map[string]string, len(extra)*2)
	for _, headers := range extra {
		for key, value := range headers {
			merged[key] = value
		}
	}
	res.extra = merged
}

/** The request body the transport already read (the cap and the truncation live there now). */
func readBody(req *request, limit ...int64) []byte {
	return req.body
}

/** Send an already-serialised JSON body (the fast path: no re-encoding on every read). */
func sendJSONText(res *response, status int, text string, extra ...map[string]string) {
	sendBuffer(res, status, []byte(text), jsonType, extra...)
}

/** Send pre-serialised JSON bytes: the cached read path, no string round-trip. */
func sendJSONBytes(res *response, status int, bytes []byte, extra ...map[string]string) {
	sendBuffer(res, status, bytes, jsonType, extra...)
}

/** Serialise and send a JSON body. */
func sendJSON(res *response, status int, body any, extra ...map[string]string) {
	sendJSONText(res, status, string(marshalJSON(body)), extra...)
}

/** The error shape GitHub answers with: `{"message": "..."}` on REST. */
func sendError(res *response, status int, message string, extra ...map[string]string) {
	sendJSON(res, status, struct {
		Message string `json:"message"`
	}{message}, extra...)
}

/** A 204 with no body (DELETE): the transport adds the request id and no content-length. */
func sendNoContent(res *response) {
	res.status = 204
}
