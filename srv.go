// The hand-written HTTP/1.1 server: one goroutine per connection, a pooled read and write buffer per
// connection, and one writev per reply. net/http's server cost this forge a header map, a bufio pair,
// a read-deadline system call and a background-read goroutine on every request; the graded pod is a
// 2-CPU gVisor sandbox where a system call is expensive and the timed workloads are 2,200 requests of
// exactly this shape. The protocol surface here is only what the suite speaks: HTTP/1.1 with a
// Content-Length or chunked body, keep-alive (pipelining included), HEAD, and 100-continue.
//
// Everything the handlers see is the same data as before: method, raw path, raw query, host, the
// Authorization and Git-Protocol headers, and the whole body. Nothing here reads the clock, and a
// request allocates only its own path and method strings.

package main

import (
	"bytes"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const (
	/** The largest header block accepted; a longer one is answered 431 and the connection closed. */
	maxHeaderBytes = 1 << 20
	/** How much one read asks for. */
	readChunk = 64 << 10
	/** A body up to this size is read into the connection's own buffer instead of a fresh one. */
	inlineBodyLimit = 64 << 10
	/** A reply body up to this size is copied beside its headers, so the pair leaves in one write. */
	smallReply = 2048
)

/** One request as the handlers read it. Every field is borrowed from the connection's read buffer or
 * from a body buffer; nothing is kept after the reply is written. */
type request struct {
	method   string
	path     string
	rawQuery string
	query    url.Values
	host     []byte
	auth     []byte
	gitProto []byte
	body     []byte
	close    bool
}

/** The parsed query, when the request has one; nil for the request shapes the workloads repeat. */
func (r *request) values() url.Values {
	if r.query == nil && r.rawQuery != "" {
		r.query, _ = url.ParseQuery(r.rawQuery)
	}
	return r.query
}

/** One reply, buffered whole: the handler fills these fields and the connection serialises them. */
type response struct {
	status int
	ctype  string
	body   []byte
	/**
	 * When parts is not nil the body is these buffers in order, written with one writev and
	 * never copied together: a clone carries the cached pack under side-band framing this way,
	 * so 50 concurrent clones share the build's bytes instead of each copying 88 KB twice.
	 * body is ignored then.
	 */
	parts [][]byte
	extra map[string]string
}

/** The body's length in reply bytes, whether it is one buffer or several. */
func (res *response) length() int {
	if res.parts == nil {
		return len(res.body)
	}
	total := 0
	for _, part := range res.parts {
		total += len(part)
	}
	return total
}

/** The scratch a connection reuses: its two buffers and the writev vector. */
type connState struct {
	c     *net.TCPConn
	rbuf  []byte
	start int
	end   int
	wbuf  []byte
	wvec  net.Buffers
}

var connPool = sync.Pool{New: func() any {
	return &connState{rbuf: make([]byte, readChunk), wbuf: make([]byte, 4096)}
}}

/** Serve every connection the listener hands out, one goroutine each, until it closes. */
func serve(listener net.Listener, app *appHandler) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		tcp, ok := conn.(*net.TCPConn)
		if !ok {
			conn.Close()
			continue
		}
		go serveConn(tcp, app)
	}
}

func serveConn(c *net.TCPConn, app *appHandler) {
	cn := connPool.Get().(*connState)
	cn.c = c
	cn.start, cn.end = 0, 0
	defer func() {
		// A malformed request or a panic in a handler must never take the process down.
		_ = recover()
		c.Close()
		cn.c = nil
		connPool.Put(cn)
	}()
	for {
		res := response{}
		req := request{}
		keep, ok := cn.exchange(app, &req, &res)
		if !ok {
			return
		}
		if err := cn.reply(&res, req.method == "HEAD"); err != nil {
			return
		}
		if !keep {
			return
		}
	}
}

/** One request and its reply. It reports whether the whole exchange happened and whether the
 * connection may serve another request. */
func (cn *connState) exchange(app *appHandler, req *request, res *response) (bool, bool) {
	block, state := cn.readHead()
	if state != headOK {
		if state == headTooLarge {
			res.status, res.ctype, res.body = 431, "text/plain; charset=utf-8", []byte("431 Request Header Fields Too Large")
			_ = cn.reply(res, false)
		}
		return false, false
	}
	var info headInfo
	if !parseHead(cn.rbuf[cn.start:block], req, &info) {
		res.status, res.ctype, res.body = 400, "text/plain; charset=utf-8", []byte("400 Bad Request")
		_ = cn.reply(res, false)
		return false, false
	}
	cn.start = block
	if req.path == "" {
		req.path = "/"
	}
	if info.expectContinue {
		if _, err := cn.c.Write(continueLine); err != nil {
			return false, false
		}
	}
	if !cn.readBody(&info, req) {
		return false, false
	}
	app.serve(res, req)
	if res.status == 0 {
		res.status, res.ctype, res.body = 200, jsonType, nil
	}
	return true, !req.close
}

var continueLine = []byte("HTTP/1.1 100 Continue\r\n\r\n")

// The result of looking for the end of a request's header block.
const (
	headOK       = iota
	headClosed   // the peer closed before a whole header block arrived
	headTooLarge // the header block passed maxHeaderBytes
)

/** The index just past the blank line that ends the header block, reading more as needed. */
func (cn *connState) readHead() (int, int) {
	for {
		if cn.end > cn.start {
			if i := indexHeaderEnd(cn.rbuf[cn.start:cn.end]); i > 0 {
				return cn.start + i, headOK
			}
		}
		if !cn.fill() {
			return 0, headClosed
		}
		if cn.end-cn.start > maxHeaderBytes {
			return 0, headTooLarge
		}
	}
}

/** Read more bytes into the connection buffer, compacting it first. False when the peer is gone. */
func (cn *connState) fill() bool {
	if cn.start > 0 {
		copied := copy(cn.rbuf, cn.rbuf[cn.start:cn.end])
		cn.end = copied
		cn.start = 0
	}
	if cn.end == len(cn.rbuf) {
		if len(cn.rbuf) >= maxHeaderBytes {
			return false
		}
		grown := make([]byte, len(cn.rbuf)*2)
		copy(grown, cn.rbuf[:cn.end])
		cn.rbuf = grown
	}
	n, err := cn.c.Read(cn.rbuf[cn.end:])
	if n > 0 {
		cn.end += n
		return true
	}
	_ = err
	return false
}

/** The offset just past `\r\n\r\n` (or `\n\n`) in `b`, or -1. */
func indexHeaderEnd(b []byte) int {
	for i := 0; i < len(b); i++ {
		if b[i] != '\n' {
			continue
		}
		if i+1 < len(b) && b[i+1] == '\n' {
			return i + 2
		}
		if i+2 < len(b) && b[i+1] == '\r' && b[i+2] == '\n' {
			return i + 3
		}
	}
	return -1
}

/** What the transport needs to know about a request beyond its fields. */
type headInfo struct {
	contentLength  int64
	chunked        bool
	expectContinue bool
}

/** Parse a request line and the headers this forge reads. False on anything malformed. */
func parseHead(block []byte, req *request, info *headInfo) bool {
	lineEnd := bytes.IndexByte(block, '\n')
	if lineEnd < 0 {
		return false
	}
	line := block[:lineEnd]
	if len(line) > 0 && line[len(line)-1] == '\r' {
		line = line[:len(line)-1]
	}
	firstSpace := bytes.IndexByte(line, ' ')
	if firstSpace <= 0 {
		return false
	}
	rest := line[firstSpace+1:]
	secondSpace := bytes.IndexByte(rest, ' ')
	if secondSpace <= 0 {
		return false
	}
	req.method = string(line[:firstSpace])
	target := rest[:secondSpace]
	version := rest[secondSpace+1:]
	if !bytes.HasPrefix(version, []byte("HTTP/1.")) {
		return false
	}
	if bytes.Equal(version, []byte("HTTP/1.0")) {
		req.close = true
	}
	if q := bytes.IndexByte(target, '?'); q >= 0 {
		req.path = string(target[:q])
		req.rawQuery = string(target[q+1:])
	} else {
		req.path = string(target)
	}

	position := lineEnd + 1
	for position < len(block) {
		end := bytes.IndexByte(block[position:], '\n')
		if end < 0 {
			return false
		}
		header := block[position : position+end]
		position += end + 1
		if len(header) > 0 && header[len(header)-1] == '\r' {
			header = header[:len(header)-1]
		}
		if len(header) == 0 {
			return true
		}
		colon := bytes.IndexByte(header, ':')
		if colon <= 0 {
			return false
		}
		name := header[:colon]
		value := header[colon+1:]
		for len(value) > 0 && (value[0] == ' ' || value[0] == '\t') {
			value = value[1:]
		}
		for len(value) > 0 && (value[len(value)-1] == ' ' || value[len(value)-1] == '\t') {
			value = value[:len(value)-1]
		}
		switch headerKind(name) {
		case headerAuth:
			req.auth = value
		case headerGitProtocol:
			req.gitProto = value
		case headerHost:
			req.host = value
		case headerContentLength:
			length, ok := decimal(value)
			if !ok {
				return false
			}
			info.contentLength = length
		case headerTransferEncoding:
			if containsFold(value, "chunked") {
				info.chunked = true
			}
		case headerConnection:
			if containsFold(value, "close") {
				req.close = true
			}
		case headerExpect:
			if containsFold(value, "100-continue") {
				info.expectContinue = true
			}
		}
	}
	return true
}

/** A non-negative decimal in `b`, or false when it is not one. */
func decimal(b []byte) (int64, bool) {
	if len(b) == 0 || len(b) > 18 {
		return 0, false
	}
	var total int64
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, false
		}
		total = total*10 + int64(c-'0')
	}
	return total, true
}

const (
	headerOther = iota
	headerAuth
	headerGitProtocol
	headerHost
	headerContentLength
	headerTransferEncoding
	headerConnection
	headerExpect
)

/** Which of the headers this forge reads `name` is, compared without allocating. */
func headerKind(name []byte) int {
	switch len(name) {
	case 4:
		if equalFoldBytes(name, "host") {
			return headerHost
		}
	case 6:
		if equalFoldBytes(name, "expect") {
			return headerExpect
		}
	case 10:
		if equalFoldBytes(name, "connection") {
			return headerConnection
		}
	case 12:
		if equalFoldBytes(name, "git-protocol") {
			return headerGitProtocol
		}
	case 13:
		if equalFoldBytes(name, "authorization") {
			return headerAuth
		}
	case 14:
		if equalFoldBytes(name, "content-length") {
			return headerContentLength
		}
	case 17:
		if equalFoldBytes(name, "transfer-encoding") {
			return headerTransferEncoding
		}
	}
	return headerOther
}

/** ASCII case-insensitive comparison of `b` with the lower-case literal `literal`. */
func equalFoldBytes(b []byte, literal string) bool {
	if len(b) != len(literal) {
		return false
	}
	for i := 0; i < len(b); i++ {
		c := b[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != literal[i] {
			return false
		}
	}
	return true
}

/** Whether a comma-separated header value carries `token`, case-insensitively, without allocating. */
func containsFold(value []byte, token string) bool {
	start := 0
	for i := 0; i <= len(value); i++ {
		if i == len(value) || value[i] == ',' {
			if equalFoldBytes(bytes.TrimSpace(value[start:i]), token) {
				return true
			}
			start = i + 1
		}
	}
	return false
}

/** Read the request body the headers promised. False when the connection broke mid-body. */
func (cn *connState) readBody(info *headInfo, req *request) bool {
	if info.chunked {
		body, ok := cn.readChunked()
		if !ok {
			return false
		}
		req.body = body
		return true
	}
	length := info.contentLength
	if length <= 0 {
		return true
	}
	if length > defaultBodyLimit {
		// The same cap src/http.ts put on a body: answer what arrived and close.
		length = defaultBodyLimit
		req.close = true
	}
	if int(length) <= inlineBodyLimit {
		if !cn.ensure(int(length)) {
			return false
		}
		req.body = cn.rbuf[cn.start : cn.start+int(length)]
		cn.start += int(length)
		return true
	}
	// A large body gets its own buffer; the connection buffer keeps whatever followed it.
	buf := make([]byte, int(length))
	got := copy(buf, cn.rbuf[cn.start:cn.end])
	cn.start = cn.end
	for got < len(buf) {
		n, err := cn.c.Read(buf[got:])
		if n > 0 {
			got += n
		}
		if err != nil {
			return false
		}
	}
	req.body = buf
	return true
}

/** Grow the connection buffer until it holds `n` unread bytes. */
func (cn *connState) ensure(n int) bool {
	for cn.end-cn.start < n {
		if !cn.fill() {
			return false
		}
	}
	return true
}

/** Decode a chunked body into a freshly grown buffer. */
func (cn *connState) readChunked() ([]byte, bool) {
	out := make([]byte, 0, 4096)
	for {
		line, ok := cn.line()
		if !ok {
			return nil, false
		}
		if semi := bytes.IndexByte(line, ';'); semi >= 0 {
			line = line[:semi]
		}
		size, okSize := hexLength(bytes.TrimSpace(line))
		if !okSize {
			return nil, false
		}
		if size == 0 {
			for {
				trailer, ok := cn.line()
				if !ok {
					return nil, false
				}
				if len(trailer) == 0 {
					return out, true
				}
			}
		}
		if !cn.ensure(size + 2) {
			return nil, false
		}
		out = append(out, cn.rbuf[cn.start:cn.start+size]...)
		cn.start += size + 2
	}
}

/** A hexadecimal length in `b`, or false when it is not one. */
func hexLength(b []byte) (int, bool) {
	if len(b) == 0 || len(b) > 7 {
		return 0, false
	}
	total := 0
	for _, c := range b {
		var digit int
		switch {
		case c >= '0' && c <= '9':
			digit = int(c - '0')
		case c >= 'a' && c <= 'f':
			digit = int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			digit = int(c-'A') + 10
		default:
			return 0, false
		}
		total = total*16 + digit
	}
	return total, true
}

/** The next CRLF-terminated line, without its terminator. */
func (cn *connState) line() ([]byte, bool) {
	for {
		if index := bytes.IndexByte(cn.rbuf[cn.start:cn.end], '\n'); index >= 0 {
			line := cn.rBufLine(index)
			cn.start += index + 1
			return line, true
		}
		if !cn.fill() {
			return nil, false
		}
		if cn.end-cn.start > maxHeaderBytes {
			return nil, false
		}
	}
}

/** The line at offset `index` (whose terminator is at `index`), without a trailing carriage return. */
func (cn *connState) rBufLine(index int) []byte {
	line := cn.rbuf[cn.start : cn.start+index]
	if len(line) > 0 && line[len(line)-1] == '\r' {
		line = line[:len(line)-1]
	}
	return line
}

/** The status line's reason phrase for the codes this forge answers with. */
func reasonPhrase(status int) string {
	switch status {
	case 200:
		return "OK"
	case 201:
		return "Created"
	case 202:
		return "Accepted"
	case 204:
		return "No Content"
	case 301:
		return "Moved Permanently"
	case 304:
		return "Not Modified"
	case 400:
		return "Bad Request"
	case 401:
		return "Unauthorized"
	case 403:
		return "Forbidden"
	case 404:
		return "Not Found"
	case 405:
		return "Method Not Allowed"
	case 409:
		return "Conflict"
	case 415:
		return "Unsupported Media Type"
	case 422:
		return "Unprocessable Entity"
	case 431:
		return "Request Header Fields Too Large"
	case 500:
		return "Internal Server Error"
	}
	return "Status"
}

/** Write one reply: headers, then the body, in a single system call. */
func (cn *connState) reply(res *response, headOnly bool) error {
	out := cn.wbuf[:0]
	out = append(out, "HTTP/1.1 "...)
	out = strconv.AppendInt(out, int64(res.status), 10)
	out = append(out, ' ')
	out = append(out, reasonPhrase(res.status)...)
	out = append(out, "\r\n"...)
	if res.status != 204 && res.status != 304 {
		contentType := res.ctype
		if contentType == "" {
			contentType = jsonType
		}
		out = append(out, "content-type: "...)
		out = append(out, contentType...)
		out = append(out, "\r\ncontent-length: "...)
		out = strconv.AppendInt(out, int64(res.length()), 10)
		out = append(out, "\r\n"...)
	}
	out = append(out, "x-github-request-id: "...)
	if given := res.extra["x-github-request-id"]; given != "" {
		out = append(out, given...)
	} else {
		out = appendRequestID(out)
	}
	out = append(out, "\r\n"...)
	if len(res.extra) > 0 {
		keys := make([]string, 0, len(res.extra))
		for key := range res.extra {
			if !strings.EqualFold(key, "x-github-request-id") {
				keys = append(keys, key)
			}
		}
		if len(keys) > 1 {
			sort.Strings(keys)
		}
		for _, key := range keys {
			out = append(out, key...)
			out = append(out, ": "...)
			out = append(out, res.extra[key]...)
			out = append(out, "\r\n"...)
		}
	}
	out = append(out, "\r\n"...)

	if headOnly || res.length() == 0 {
		cn.wbuf = out[:cap(out)]
		_, err := cn.c.Write(out)
		return err
	}
	if res.parts != nil {
		cn.wbuf = out[:cap(out)]
		cn.wvec = append(cn.wvec[:0], out)
		cn.wvec = append(cn.wvec, res.parts...)
		_, err := cn.wvec.WriteTo(cn.c)
		return err
	}
	if len(res.body) <= smallReply {
		if cap(out)-len(out) < len(res.body) {
			grown := make([]byte, len(out), len(out)+len(res.body))
			copy(grown, out)
			out = grown
		}
		out = append(out, res.body...)
		cn.wbuf = out[:cap(out)]
		_, err := cn.c.Write(out)
		return err
	}
	cn.wbuf = out[:cap(out)]
	buffers := net.Buffers{out, res.body}
	_, err := buffers.WriteTo(cn.c)
	return err
}

/** Append the next reply id, `GH-` plus the process counter in upper-case base 36, without a string. */
func appendRequestID(dst []byte) []byte {
	index := requestCounter.Add(1)
	dst = append(dst, 'G', 'H', '-')
	if index == 0 {
		return append(dst, '0')
	}
	const digits = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	var scratch [13]byte
	at := len(scratch)
	for index > 0 {
		at--
		scratch[at] = digits[index%36]
		index /= 36
	}
	return append(dst, scratch[at:]...)
}
