// Git's object model in one place: hashing, the tree and commit encodings, and the packfile
// reader and writer. Nothing here touches HTTP; rest.go, smart.go and store.go build on it.
//
// Everything is deterministic: a commit's bytes come from its tree, its parent, the caller's
// message and a caller-supplied timestamp, never from the clock. Byte-for-byte compatibility with
// the TypeScript reference matters: object hashes, tree entry ordering, commit headers and the
// pack checksum are all checked by the conformance suite.

package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

type objType string

const (
	typeBlob   objType = "blob"
	typeTree   objType = "tree"
	typeCommit objType = "commit"
	typeTag    objType = "tag"
)

const zeroOID = "0000000000000000000000000000000000000000"

type gitObj struct {
	Type objType
	Data []byte
}

/** The object table a repository owns: id -> bytes, with the type git stores beside them. */
type objectTable map[string]gitObj

func hashObject(t objType, data []byte) string {
	h := sha1.New()
	_, _ = h.Write([]byte(fmt.Sprintf("%s %d\x00", t, len(data))))
	_, _ = h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func putObject(table objectTable, t objType, data []byte) string {
	id := hashObject(t, data)
	if _, ok := table[id]; !ok {
		table[id] = gitObj{Type: t, Data: data}
	}
	return id
}

func getObject(table objectTable, id string) (gitObj, bool) {
	obj, ok := table[id]
	return obj, ok
}

// ---------------------------------------------------------------------------
// trees

type treeEntry struct {
	Mode string
	Name string
	SHA  string
}

/** Read a tree's entries. A tree is `<mode> <name>\0<20 raw bytes>` repeated. */
func parseTree(data []byte) []treeEntry {
	out := []treeEntry{}
	n := len(data)
	i := 0
	for i < n {
		space := bytes.IndexByte(data[i:], 0x20)
		if space < 0 {
			break
		}
		space += i
		nul := bytes.IndexByte(data[space+1:], 0x00)
		if nul < 0 {
			break
		}
		nul += space + 1
		end := nul + 21
		if end > n {
			end = n
		}
		mode := string(data[i:space])
		name := string(data[space+1 : nul])
		sha := hex.EncodeToString(data[nul+1 : end])
		out = append(out, treeEntry{Mode: mode, Name: name, SHA: sha})
		i = nul + 21
	}
	return out
}

func encodeTree(entries []treeEntry) []byte {
	sorted := append([]treeEntry(nil), entries...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return compareTreeEntries(sorted[i], sorted[j]) < 0
	})
	parts := make([][]byte, 0, len(sorted)*2)
	for _, entry := range sorted {
		parts = append(parts, []byte(entry.Mode+" "+entry.Name+"\x00"))
		sha, _ := hex.DecodeString(entry.SHA)
		parts = append(parts, sha)
	}
	return bytes.Join(parts, nil)
}

/** git's tree order: byte order of the name, with a directory's name compared as `name/`. */
func compareTreeEntries(a, b treeEntry) int {
	an := a.Name
	if a.Mode == "40000" {
		an += "/"
	}
	bn := b.Name
	if b.Mode == "40000" {
		bn += "/"
	}
	return strings.Compare(an, bn)
}

func writeTree(table objectTable, entries []treeEntry) string {
	return putObject(table, typeTree, encodeTree(entries))
}

/** Resolve `parts` inside a tree to the object id, or "" (null). An empty `parts` is the tree itself. */
func treeLookup(table objectTable, rootSha string, parts []string) (string, bool) {
	if rootSha == "" {
		return "", false
	}
	if len(parts) == 0 {
		return rootSha, true
	}
	current := rootSha
	for i := 0; i < len(parts); i++ {
		obj, ok := table[current]
		if !ok || obj.Type != typeTree {
			return "", false
		}
		var found *treeEntry
		entries := parseTree(obj.Data)
		for idx := range entries {
			if entries[idx].Name == parts[i] {
				found = &entries[idx]
				break
			}
		}
		if found == nil {
			return "", false
		}
		if i == len(parts)-1 {
			return found.SHA, true
		}
		if found.Mode != "40000" {
			return "", false
		}
		current = found.SHA
	}
	return "", false
}

/**
 * Write the tree that holds `parts` set to `sha` (or removed when `sha` is ""), keeping every
 * other entry of `rootSha`. Returns the new root tree id (an empty tree when the last file goes).
 */
func treeWith(table objectTable, rootSha string, parts []string, sha string) string {
	var entries []treeEntry
	if rootSha != "" {
		if obj, ok := table[rootSha]; ok {
			entries = parseTree(obj.Data)
		} else {
			entries = []treeEntry{}
		}
	}
	head := parts[0]
	rest := parts[1:]
	index := -1
	for i, entry := range entries {
		if entry.Name == head {
			index = i
			break
		}
	}
	if len(rest) == 0 {
		if index >= 0 {
			entries = append(entries[:index], entries[index+1:]...)
		}
		if sha != "" {
			entries = append(entries, treeEntry{Mode: "100644", Name: head, SHA: sha})
		}
		return writeTree(table, entries)
	}
	childSha := ""
	if index >= 0 && entries[index].Mode == "40000" {
		childSha = entries[index].SHA
	}
	nextChild := treeWith(table, childSha, rest, sha)
	if index >= 0 {
		entries = append(entries[:index], entries[index+1:]...)
	}
	// Keep an empty directory out of the tree, as git does.
	childData := table[nextChild].Data
	if len(parseTree(childData)) > 0 {
		entries = append(entries, treeEntry{Mode: "40000", Name: head, SHA: nextChild})
	}
	return writeTree(table, entries)
}

/** Every file path below a tree with its blob id, as a flat map. */
func flattenTree(table objectTable, rootSha string, prefix string) map[string]string {
	out := map[string]string{}
	if rootSha == "" {
		return out
	}
	obj, ok := table[rootSha]
	if !ok {
		return out
	}
	for _, entry := range parseTree(obj.Data) {
		path := entry.Name
		if prefix != "" {
			path = prefix + "/" + entry.Name
		}
		if entry.Mode == "40000" {
			for childPath, childSha := range flattenTree(table, entry.SHA, path) {
				out[childPath] = childSha
			}
		} else {
			out[path] = entry.SHA
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// commits and tags

type commitFields struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

func encodeCommit(fields commitFields) []byte {
	lines := []string{"tree " + fields.Tree}
	for _, parent := range fields.Parents {
		lines = append(lines, "parent "+parent)
	}
	lines = append(lines, "author "+fields.Author, "committer "+fields.Committer, "", fields.Message)
	return []byte(strings.Join(lines, "\n"))
}

func parseCommit(data []byte) commitFields {
	text := string(data)
	split := strings.Index(text, "\n\n")
	header := text
	message := ""
	if split >= 0 {
		header = text[:split]
		message = text[split+2:]
	}
	fields := commitFields{Parents: []string{}, Message: message}
	for _, line := range strings.Split(header, "\n") {
		if strings.HasPrefix(line, "tree ") {
			fields.Tree = line[5:]
		} else if strings.HasPrefix(line, "parent ") {
			fields.Parents = append(fields.Parents, line[7:])
		} else if strings.HasPrefix(line, "author ") {
			fields.Author = line[7:]
		} else if strings.HasPrefix(line, "committer ") {
			fields.Committer = line[10:]
		}
	}
	return fields
}

type tagFields struct {
	Object  string
	Type    string
	Tag     string
	Tagger  string
	Message string
}

func parseTag(data []byte) tagFields {
	text := string(data)
	split := strings.Index(text, "\n\n")
	header := text
	message := ""
	if split >= 0 {
		header = text[:split]
		message = text[split+2:]
	}
	fields := tagFields{Message: message}
	for _, line := range strings.Split(header, "\n") {
		if strings.HasPrefix(line, "object ") {
			fields.Object = line[7:]
		} else if strings.HasPrefix(line, "type ") {
			fields.Type = line[5:]
		} else if strings.HasPrefix(line, "tag ") {
			fields.Tag = line[4:]
		} else if strings.HasPrefix(line, "tagger ") {
			fields.Tagger = line[7:]
		}
	}
	return fields
}

/** The commit a tag points at, following nested tags; the object id itself for anything else. */
func peelShallow(table objectTable, id string) string {
	current := id
	for i := 0; i < 8; i++ {
		obj, ok := table[current]
		if !ok || obj.Type != typeTag {
			return current
		}
		current = parseTag(obj.Data).Object
	}
	return current
}

// peelToCommit is how later surfaces name the same operation.
func peelToCommit(table objectTable, id string) string {
	return peelShallow(table, id)
}

/** Walk a commit's first parents, newest first, stopping at a limit. */
func commitChain(table objectTable, tip string, limit int) []string {
	if limit <= 0 {
		limit = 1000
	}
	out := []string{}
	current := tip
	for current != "" && len(out) < limit {
		obj, ok := table[current]
		if !ok || obj.Type != typeCommit {
			break
		}
		out = append(out, current)
		if parents := parseCommit(obj.Data).Parents; len(parents) > 0 {
			current = parents[0]
		} else {
			current = ""
		}
	}
	return out
}

/** True when `ancestor` is reachable from `descendant` by parents (or is the same commit). */
func isAncestor(table objectTable, ancestor, descendant string) bool {
	if ancestor == descendant {
		return true
	}
	seen := map[string]bool{}
	stack := []string{descendant}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if id == ancestor {
			return true
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		obj, ok := table[id]
		if !ok || obj.Type != typeCommit {
			continue
		}
		for _, parent := range parseCommit(obj.Data).Parents {
			stack = append(stack, parent)
		}
	}
	return false
}

/** Every object reachable from the given roots (commits, their trees, tags). */
func reachable(table objectTable, roots []string) map[string]bool {
	seen := map[string]bool{}
	stack := append([]string(nil), roots...)
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[id] {
			continue
		}
		obj, ok := table[id]
		if !ok {
			continue
		}
		seen[id] = true
		if obj.Type == typeCommit {
			commit := parseCommit(obj.Data)
			stack = append(stack, commit.Tree)
			for _, parent := range commit.Parents {
				stack = append(stack, parent)
			}
		} else if obj.Type == typeTree {
			for _, entry := range parseTree(obj.Data) {
				stack = append(stack, entry.SHA)
			}
		} else if obj.Type == typeTag {
			stack = append(stack, parseTag(obj.Data).Object)
		}
	}
	return seen
}

// ---------------------------------------------------------------------------
// packfiles

type packEntry = gitObj

type readPackResult struct {
	Objects    map[string]gitObj
	DeltaCount int
}

/**
 * Read a version 2 packfile: header, entries (with OFS/REF deltas resolved against the pack or
 * `resolveBase`), and the SHA-1 trailer. Errors carry a message a caller can answer with.
 */
func readPack(pack []byte, resolveBase func(id string) (gitObj, bool)) (readPackResult, error) {
	if len(pack) < 32 || string(pack[0:4]) != "PACK" {
		return readPackResult{}, fmt.Errorf("not a packfile")
	}
	version := binary.BigEndian.Uint32(pack[4:8])
	if version != 2 && version != 3 {
		return readPackResult{}, fmt.Errorf("unsupported pack version %d", version)
	}
	count := binary.BigEndian.Uint32(pack[8:12])
	objects := map[string]gitObj{}
	byOffset := map[int]packEntry{}
	pos := 12
	deltaCount := 0
	for index := uint32(0); index < count; index++ {
		entryStart := pos
		var byteVal byte
		if pos < len(pack) {
			byteVal = pack[pos]
		}
		pos += 1
		typeCode := (byteVal >> 4) & 7
		size := int(byteVal & 15)
		shift := uint(4)
		for byteVal&0x80 != 0 {
			if pos < len(pack) {
				byteVal = pack[pos]
			} else {
				byteVal = 0
			}
			pos += 1
			size += int(byteVal&0x7f) << shift
			shift += 7
		}
		baseOffset := -1
		var baseID string
		if typeCode == 6 {
			if pos < len(pack) {
				byteVal = pack[pos]
			} else {
				byteVal = 0
			}
			pos += 1
			ofs := int(byteVal & 0x7f)
			for byteVal&0x80 != 0 {
				if pos < len(pack) {
					byteVal = pack[pos]
				} else {
					byteVal = 0
				}
				pos += 1
				ofs = (ofs+1)*128 + int(byteVal&0x7f)
			}
			baseOffset = entryStart - ofs
		} else if typeCode == 7 {
			baseID = hex.EncodeToString(pack[pos : pos+20])
			pos += 20
		}
		inflated, next, err := inflateAt(pack, pos)
		if err != nil {
			return readPackResult{}, err
		}
		pos = next
		if len(inflated) != size {
			return readPackResult{}, fmt.Errorf("pack entry %d inflated to %d bytes, header says %d", index, len(inflated), size)
		}
		var entryType objType
		var data []byte
		switch typeCode {
		case 1:
			entryType = typeCommit
			data = inflated
		case 2:
			entryType = typeTree
			data = inflated
		case 3:
			entryType = typeBlob
			data = inflated
		case 4:
			entryType = typeTag
			data = inflated
		case 6, 7:
			deltaCount += 1
			var base packEntry
			hasBase := false
			if baseOffset >= 0 {
				var ok bool
				base, ok = byOffset[baseOffset]
				if !ok {
					return readPackResult{}, fmt.Errorf("pack delta at %d names a base offset the pack does not hold", entryStart)
				}
				hasBase = true
			} else if baseID != "" {
				var ok bool
				base, ok = objects[baseID]
				if !ok && resolveBase != nil {
					base, ok = resolveBase(baseID)
				}
				if !ok {
					return readPackResult{}, fmt.Errorf("pack delta names a base object the server does not hold")
				}
				hasBase = true
			}
			if !hasBase {
				return readPackResult{}, fmt.Errorf("pack delta has no base")
			}
			var applyErr error
			data, applyErr = applyDelta(base.Data, inflated)
			if applyErr != nil {
				return readPackResult{}, applyErr
			}
			entryType = base.Type
		default:
			return readPackResult{}, fmt.Errorf("unsupported pack object type %d", typeCode)
		}
		id := hashObject(entryType, data)
		byOffset[entryStart] = packEntry{Type: entryType, Data: data}
		if _, ok := objects[id]; !ok {
			objects[id] = gitObj{Type: entryType, Data: data}
		}
	}
	expected := hex.EncodeToString(pack[len(pack)-20:])
	actualSum := sha1.Sum(pack[:len(pack)-20])
	actual := hex.EncodeToString(actualSum[:])
	if expected != actual {
		return readPackResult{}, fmt.Errorf("pack trailer does not match its bytes")
	}
	return readPackResult{Objects: objects, DeltaCount: deltaCount}, nil
}

/**
 * A reader that hands bytes to zlib exactly one at a time, so the count of consumed input bytes is
 * the precise end of the zlib stream (no buffering runs past it into the next pack entry).
 */
type countedReader struct {
	reader *bytes.Reader
	count  int
}

func (r *countedReader) ReadByte() (byte, error) {
	b, err := r.reader.ReadByte()
	if err == nil {
		r.count++
	}
	return b, err
}

func (r *countedReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	one := p[:1]
	n, err := r.reader.Read(one)
	if n > 0 {
		p[0] = one[0]
		r.count += n
	}
	return n, err
}

/** Inflate the zlib stream that starts at `pos`; `next` is the first byte after it. */
func inflateAt(pack []byte, pos int) ([]byte, int, error) {
	src := &countedReader{reader: bytes.NewReader(pack[pos:])}
	resetter, ok := getZlibReader().(zlib.Resetter)
	if !ok {
		zr, err := zlib.NewReader(src)
		if err != nil {
			return nil, 0, fmt.Errorf("zlib stream at %d did not inflate: %s", pos, err)
		}
		return inflateRead(zr, src, pos)
	}
	if err := resetter.Reset(src, nil); err != nil {
		return nil, 0, fmt.Errorf("zlib stream at %d did not inflate: %s", pos, err)
	}
	return inflateRead(resetter.(io.ReadCloser), src, pos)
}

func inflateRead(zr io.ReadCloser, src *countedReader, pos int) ([]byte, int, error) {
	defer putZlibReader(zr)
	data, readErr := io.ReadAll(zr)
	closeErr := zr.Close()
	if readErr != nil {
		return nil, 0, fmt.Errorf("zlib stream at %d did not inflate: %s", pos, readErr)
	}
	_ = closeErr
	consumed := src.count
	if consumed <= 0 {
		return nil, 0, fmt.Errorf("zlib stream at %d consumed no bytes", pos)
	}
	return data, pos + consumed, nil
}

/** Apply a git delta (the base-size, result-size, copy/insert opcode format) to a base object. */
func applyDelta(base []byte, delta []byte) ([]byte, error) {
	pos := 0
	shift := 0
	// Mirror JS: reading past the end yields 0 (undefined), never a crash.
	byteAt := func(p int) byte {
		if p < 0 || p >= len(delta) {
			return 0
		}
		return delta[p]
	}
	readSize := func() int {
		size := int(byteAt(pos) & 0x7f)
		pos += 1
		shift = 0
		for byteAt(pos-1)&0x80 != 0 {
			shift += 7
			size += int(byteAt(pos)&0x7f) << shift
			pos += 1
		}
		return size
	}
	baseSize := readSize()
	resultSize := readSize()
	if baseSize != len(base) {
		return nil, fmt.Errorf("delta base size does not match the base object")
	}
	out := make([]byte, resultSize)
	written := 0
	for pos < len(delta) {
		opcode := byteAt(pos)
		pos += 1
		if opcode&0x80 != 0 {
			offset := 0
			size := 0
			if opcode&0x01 != 0 {
				offset += int(byteAt(pos))
				pos += 1
			}
			if opcode&0x02 != 0 {
				offset += int(byteAt(pos)) * 256
				pos += 1
			}
			if opcode&0x04 != 0 {
				offset += int(byteAt(pos)) * 65536
				pos += 1
			}
			if opcode&0x08 != 0 {
				offset += int(byteAt(pos)) * 16777216
				pos += 1
			}
			if opcode&0x10 != 0 {
				size += int(byteAt(pos))
				pos += 1
			}
			if opcode&0x20 != 0 {
				size += int(byteAt(pos)) * 256
				pos += 1
			}
			if opcode&0x40 != 0 {
				size += int(byteAt(pos)) * 65536
				pos += 1
			}
			if size == 0 {
				size = 0x10000
			}
			copy(out[written:written+size], base[offset:offset+size])
			written += size
		} else if opcode != 0 {
			copy(out[written:written+int(opcode)], delta[pos:pos+int(opcode)])
			written += int(opcode)
			pos += int(opcode)
		} else {
			return nil, fmt.Errorf("delta holds a reserved opcode")
		}
	}
	if written != resultSize {
		return nil, fmt.Errorf("delta wrote %d bytes, header says %d", written, resultSize)
	}
	return out, nil
}

var typeCodeOf = map[objType]uint8{typeCommit: 1, typeTree: 2, typeBlob: 3, typeTag: 4}

// ---------------------------------------------------------------------------
// pooled compressors

/**
 * The pack path deflates every object (writePack) and inflates every received entry (readPack).
 * A fresh zlib writer allocates the deflater's window and history and a fresh reader allocates the
 * inflater's window and tables, so both are pooled and Reset per use. The output bytes never come
 * from the pooled buffers: a pack that shares a pooled buffer would be corrupted by the next use,
 * so every compressed entry is copied out and owned by the pack. Reset keeps the exact same
 * compression level and framing, so the bytes are identical to a fresh compressor.
 */

type zlibWriterCtx struct {
	zw  *zlib.Writer
	buf bytes.Buffer
}

var zlibWriterPool sync.Pool

func getZlibWriter() *zlibWriterCtx {
	entry, ok := zlibWriterPool.Get().(*zlibWriterCtx)
	if !ok || entry == nil {
		entry = &zlibWriterCtx{}
		entry.zw = zlib.NewWriter(&entry.buf)
	}
	entry.buf.Reset()
	entry.zw.Reset(&entry.buf)
	return entry
}

func putZlibWriter(entry *zlibWriterCtx) {
	zlibWriterPool.Put(entry)
}

var zlibReaderPool sync.Pool

/** A pooled zlib reader, reset for the stream at hand before use. */
func getZlibReader() io.ReadCloser {
	reader, ok := zlibReaderPool.Get().(io.ReadCloser)
	if !ok || reader == nil {
		reader, _ = zlib.NewReader(bytes.NewReader(nil))
	}
	return reader
}

func putZlibReader(reader io.ReadCloser) {
	zlibReaderPool.Put(reader)
}

/** Build a version 2 packfile from objects (no deltas: every entry stands alone). */
func writePack(objects []gitObj) []byte {
	parts := make([][]byte, 0, len(objects)*2+1)
	header := make([]byte, 12)
	copy(header[0:4], []byte("PACK"))
	binary.BigEndian.PutUint32(header[4:8], 2)
	binary.BigEndian.PutUint32(header[8:12], uint32(len(objects)))
	parts = append(parts, header)
	entry := getZlibWriter()
	for _, obj := range objects {
		typeCode := typeCodeOf[obj.Type]
		size := len(obj.Data)
		var encoded []uint8
		remaining := size
		first := (typeCode << 4) | uint8(remaining&15)
		remaining = remaining / 16
		for remaining > 0 {
			encoded = append(encoded, first|0x80)
			first = uint8(remaining & 0x7f)
			remaining = remaining / 128
		}
		encoded = append(encoded, first)
		parts = append(parts, encoded)
		_, _ = entry.zw.Write(obj.Data)
		_ = entry.zw.Close()
		compressed := make([]byte, entry.buf.Len())
		copy(compressed, entry.buf.Bytes())
		parts = append(parts, compressed)
		// Close leaves the writer closed (err is set); Reset clears that and
		// points it back at the emptied buffer for the next entry.
		entry.buf.Reset()
		entry.zw.Reset(&entry.buf)
	}
	putZlibWriter(entry)
	body := bytes.Join(parts, nil)
	sum := sha1.Sum(body)
	return append(body, sum[:]...)
}

// ---------------------------------------------------------------------------
// pack pkt-line helpers shared with smart.go

/** Encode one pkt-line; the control packets are `flush`, `delim` and `response-end`. */
func pktLine(payload string) []byte {
	body := []byte(payload)
	length := len(body) + 4
	head := []byte(fmt.Sprintf("%04x", length))
	return append(head, body...)
}

func pktLineBytes(payload []byte) []byte {
	length := len(payload) + 4
	head := []byte(fmt.Sprintf("%04x", length))
	return append(head, payload...)
}

var pktFlush = []byte("0000")
var pktDelim = []byte("0001")