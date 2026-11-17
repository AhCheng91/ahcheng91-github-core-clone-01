// The shared http helpers every surface replies through: reading request bodies and sending JSON
// with the headers GitHub sends. Everything here stays on the hot path (2,000 issue reads, 200
// issue writes), so it is kept as light as possible and never logs.

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
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

/** The next reply id: `GH-` plus the counter in base 36, upper-cased. */
func nextRequestID() string {
	counter := requestCounter.Add(1)
	return "GH-" + strings.ToUpper(strconv.FormatUint(counter, 36))
}

/**
 * Read a request body into one buffer. A body longer than `limit` bytes is cut off at the limit
 * (the request is answered as if that were the whole body).
 */
func readBody(req *http.Request, limit ...int64) []byte {
	var cap int64 = defaultBodyLimit
	if len(limit) > 0 {
		cap = limit[0]
	}
	data, err := io.ReadAll(io.LimitReader(req.Body, cap+1))
	if err != nil {
		return nil
	}
	if int64(len(data)) > cap {
		data = data[:cap]
	}
	return data
}

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
 * Send `body` with its content type, its length and a fresh request id; any extra headers are
 * merged in first-come (they override nothing already set except by key). The caller passes the
 * exact content type: git and streamed surfaces need other values than JSON.
 */
func sendBuffer(res http.ResponseWriter, status int, body []byte, contentType string, extra ...map[string]string) {
	headers := res.Header()
	headers.Set("content-type", contentType)
	headers.Set("content-length", strconv.Itoa(len(body)))
	headers.Set("x-github-request-id", nextRequestID())
	for _, m := range extra {
		for key, value := range m {
			headers.Set(key, value)
		}
	}
	res.WriteHeader(status)
	if len(body) > 0 {
		_, _ = res.Write(body)
	}
}

/** Send an already-serialised JSON body (the fast path: no re-encoding on every read). */
func sendJSONText(res http.ResponseWriter, status int, text string, extra ...map[string]string) {
	sendBuffer(res, status, []byte(text), jsonType, extra...)
}

/** Send pre-serialised JSON bytes: the cached read path, no string round-trip. */
func sendJSONBytes(res http.ResponseWriter, status int, bytes []byte, extra ...map[string]string) {
	sendBuffer(res, status, bytes, jsonType, extra...)
}

/** Serialise and send a JSON body. */
func sendJSON(res http.ResponseWriter, status int, body any, extra ...map[string]string) {
	sendJSONText(res, status, string(marshalJSON(body)), extra...)
}

/** The error shape GitHub answers with: `{"message": "..."}` on REST. */
func sendError(res http.ResponseWriter, status int, message string, extra ...map[string]string) {
	sendJSON(res, status, struct {
		Message string `json:"message"`
	}{message}, extra...)
}

/** A 204 with no body (DELETE). */
func sendNoContent(res http.ResponseWriter) {
	res.Header().Set("x-github-request-id", nextRequestID())
	res.WriteHeader(204)
}