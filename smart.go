// The git smart-HTTP surface: protocol v2 (capability advertisement, ls-refs, fetch) and protocol v0
// (the upload-pack advertisement and the receive-pack advertisement, plus push). The packfile writer
// and reader and the object helpers in gitobj.go do all the heavy lifting; this file only speaks the
// pkt-line dialect on top of them.

package main

import (
	"bytes"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

/** The agent string this forge offers where git lets a server name itself. */
const agent = "agent=makefun-forge/1.0"

const uploadAdvertType = "application/x-git-upload-pack-advertisement"
const uploadResultType = "application/x-git-upload-pack-result"
const receiveAdvertType = "application/x-git-receive-pack-advertisement"
const receiveResultType = "application/x-git-receive-pack-result"

// The kinds a decoded pkt-line frame can have.
const (
	frameData = iota
	frameFlush
	frameDelim
	frameEnd
)

/** One decoded pkt-line frame: a data frame carrying payload bytes, or a control frame. */
type packetFrame struct {
	kind    int
	payload []byte
}

/**
 * Decode a buffer as a stream of pkt-lines. The scan stops as soon as the bytes no longer look like
 * framing (a push's packfile riding after the command list, or garbage); whatever was not consumed
 * comes back whole as `rest`.
 */
func splitPackets(buf []byte) (frames []packetFrame, rest []byte, ended bool) {
	frames = []packetFrame{}
	cursor := 0
	for cursor+4 <= len(buf) {
		size, err := strconv.ParseInt(string(buf[cursor:cursor+4]), 16, 32)
		if err != nil {
			break
		}
		if size == 0 {
			frames = append(frames, packetFrame{kind: frameFlush})
			cursor += 4
			continue
		}
		if size == 1 {
			frames = append(frames, packetFrame{kind: frameDelim})
			cursor += 4
			continue
		}
		if size == 2 {
			frames = append(frames, packetFrame{kind: frameEnd})
			cursor += 4
			continue
		}
		if cursor+int(size) > len(buf) {
			break
		}
		var payload []byte
		if size >= 4 {
			payload = buf[cursor+4 : cursor+int(size)]
		}
		frames = append(frames, packetFrame{kind: frameData, payload: payload})
		cursor += int(size)
		if size < 4 {
			break
		}
	}
	return frames, buf[cursor:], cursor >= len(buf)
}

/** The text a data frame carries, its trailing newline removed; empty for control frames. */
func frameText(frame packetFrame) string {
	if frame.kind != frameData {
		return ""
	}
	return strings.TrimSuffix(string(frame.payload), "\n")
}

/** Emit `payload` as band-1 sideband pkt-lines (≤ `max` payload bytes each) and close with a flush. */
func sidebandPack(payload []byte, max int) []byte {
	out := []byte{}
	for at := 0; at < len(payload); at += max {
		end := at + max
		if end > len(payload) {
			end = len(payload)
		}
		out = append(out, pktLineBytes(append([]byte{1}, payload[at:end]...))...)
	}
	out = append(out, pktFlush...)
	return out
}

/** The repository's refs ordered by name, each as a `{ name, sha }` record. */
type sortedRef struct {
	name string
	sha  string
}

func sortedRefs(repo *Repo) []sortedRef {
	records := make([]sortedRef, 0, len(repo.Refs))
	for name, sha := range repo.Refs {
		records = append(records, sortedRef{name: name, sha: sha})
	}
	sort.Slice(records, func(i, j int) bool { return records[i].name < records[j].name })
	return records
}

/** Follow a tag object (and nested tags) to the object it names; "" when the id is not a tag. */
func unwindTag(repo *Repo, oid string) string {
	object, ok := repo.Objects[oid]
	if !ok || object.Type != typeTag {
		return ""
	}
	target := parseTag(object.Data).Object
	for hop := 0; hop < 8; hop += 1 {
		next, ok := repo.Objects[target]
		if !ok || next.Type != typeTag {
			return target
		}
		target = parseTag(next.Data).Object
	}
	return target
}

// ---------------------------------------------------------------------------
// the request router
// ---------------------------------------------------------------------------

func handleGit(res *response, req *request, store *Store, owner string, name string, suffix string, query url.Values, body []byte) {
	repo, ok := getRepo(store, owner, name)
	if !ok {
		sendError(res, 404, "Not Found")
		return
	}
	service := query.Get("service")

	if suffix == "info/refs" {
		if service == "git-receive-pack" {
			sendBuffer(res, 200, receiveAdvert(repo), receiveAdvertType)
			return
		}
		if service == "git-upload-pack" {
			asksForV2 := strings.Contains(string(req.gitProto), "version=2")
			if asksForV2 {
				sendBuffer(res, 200, uploadAdvertV2(), uploadAdvertType)
			} else {
				sendBuffer(res, 200, uploadAdvertV0(repo), uploadAdvertType)
			}
			return
		}
		sendError(res, 400, "Unknown service")
		return
	}

	if suffix == "git-upload-pack" && req.method == "POST" {
		head, framed := runUploadPack(repo, body)
		if framed != nil {
			res.status, res.ctype, res.parts = 200, uploadResultType, [][]byte{head, framed}
			return
		}
		sendBuffer(res, 200, head, uploadResultType)
		return
	}

	if suffix == "git-receive-pack" && req.method == "POST" {
		sendBuffer(res, 200, runReceivePack(repo, body), receiveResultType)
		return
	}

	sendError(res, 404, "Not Found")
}

// ---------------------------------------------------------------------------
// advertisements
// ---------------------------------------------------------------------------

/** The capability list advertised to a protocol v2 client. */
func v2CapabilityList() []string {
	return []string{"version 2", "agent=git/makefun-forge-1.0", "ls-refs=unborn", "fetch=shallow wait-for-done", "server-option", "object-format=sha1"}
}

/** The protocol v2 advertisement: the service banner, the capability lines, then a flush. */
func uploadAdvertV2() []byte {
	return uploadAdvertV2Bytes
}

/** The v2 advertisement is the same bytes for every repository, so it is built once. */
var uploadAdvertV2Bytes = buildUploadAdvertV2()

func buildUploadAdvertV2() []byte {
	out := [][]byte{pktLine("# service=git-upload-pack\n"), pktFlush}
	for _, capability := range v2CapabilityList() {
		out = append(out, pktLine(capability+"\n"))
	}
	out = append(out, pktFlush)
	return bytes.Join(out, nil)
}

/** The protocol v0 upload-pack advertisement: every ref with the capability set on the first line. */
func uploadAdvertV0(repo *Repo) []byte {
	caps := []string{"multi_ack_detailed", "side-band-64k", "thin-pack", "ofs-delta", "shallow", "no-progress", "object-format=sha1", agent, "symref=HEAD:refs/heads/main"}
	out := [][]byte{pktLine("# service=git-upload-pack\n"), pktFlush}
	refs := sortedRefs(repo)
	if len(refs) == 0 {
		out = append(out, pktLine(zeroOID+" capabilities^{}\x00"+strings.Join(caps, " ")+"\n"))
	} else {
		headOID, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]
		if !ok {
			headOID = refs[0].sha
		}
		out = append(out, pktLine(headOID+" HEAD\x00"+strings.Join(caps, " ")+"\n"))
		for _, ref := range refs {
			out = append(out, pktLine(ref.sha+" "+ref.name+"\n"))
		}
	}
	out = append(out, pktFlush)
	return bytes.Join(out, nil)
}

/** The receive-pack advertisement: capabilities ride on the first ref, then plain ref lines. */
func receiveAdvert(repo *Repo) []byte {
	caps := []string{"report-status", "side-band-64k", "delete-refs", "ofs-delta", "quiet", "object-format=sha1", agent}
	out := [][]byte{pktLine("# service=git-receive-pack\n"), pktFlush}
	refs := sortedRefs(repo)
	if len(refs) == 0 {
		out = append(out, pktLine(zeroOID+" capabilities^{}\x00"+strings.Join(caps, " ")+"\n"))
	} else {
		for index, ref := range refs {
			var text string
			if index == 0 {
				text = ref.sha + " " + ref.name + "\x00" + strings.Join(caps, " ") + "\n"
			} else {
				text = ref.sha + " " + ref.name + "\n"
			}
			out = append(out, pktLine(text))
		}
	}
	out = append(out, pktFlush)
	return bytes.Join(out, nil)
}

// ---------------------------------------------------------------------------
// upload-pack: ls-refs and fetch
// ---------------------------------------------------------------------------

/** Dispatch one POSTed upload-pack command. Anything unknown gets a bare flush. */
func runUploadPack(repo *Repo, body []byte) ([]byte, []byte) {
	frames, _, _ := splitPackets(body)
	var first packetFrame
	if len(frames) > 0 {
		first = frames[0]
	}
	command := frameText(first)
	if command == "command=ls-refs" {
		return listRefs(repo, frames), nil
	}
	if command == "command=fetch" {
		return doFetch(repo, frames)
	}
	return pktFlush, nil
}

/** The v2 ls-refs reply: HEAD with its symref, then each matching ref, honouring peel/symrefs/unborn. */
func listRefs(repo *Repo, frames []packetFrame) []byte {
	argumentsOpen := false
	askPeel := false
	askSymrefs := false
	askUnborn := false
	prefixes := []string{}
	for _, frame := range frames[1:] {
		if frame.kind == frameDelim {
			argumentsOpen = true
			continue
		}
		if frame.kind != frameData || !argumentsOpen {
			continue
		}
		text := frameText(frame)
		if text == "peel" {
			askPeel = true
		} else if text == "symrefs" {
			askSymrefs = true
		} else if text == "unborn" {
			askUnborn = true
		} else if strings.HasPrefix(text, "ref-prefix ") {
			prefixes = append(prefixes, text[len("ref-prefix "):])
		}
	}
	matchesPrefix := func(refName string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, prefix := range prefixes {
			if strings.HasPrefix(refName, prefix) {
				return true
			}
		}
		return false
	}
	out := [][]byte{}
	headOID, hasHead := repo.Refs["refs/heads/"+repo.DefaultBranch]
	if matchesPrefix("HEAD") {
		if hasHead {
			suffix := ""
			if askSymrefs {
				suffix = " symref-target:refs/heads/" + repo.DefaultBranch
			}
			out = append(out, pktLine(headOID+" HEAD"+suffix+"\n"))
		} else if askUnborn {
			out = append(out, pktLine("unborn HEAD symref-target:refs/heads/"+repo.DefaultBranch+"\n"))
		}
	}
	for _, ref := range sortedRefs(repo) {
		if !matchesPrefix(ref.name) {
			continue
		}
		text := ref.sha + " " + ref.name
		if askPeel {
			if target := unwindTag(repo, ref.sha); target != "" {
				text += " peeled:" + target
			}
		}
		out = append(out, pktLine(text+"\n"))
	}
	out = append(out, pktFlush)
	return bytes.Join(out, nil)
}

/** The named arguments a fetch command carries. */
type fetchArguments struct {
	wants    []string
	haves    []string
	depth    int
	hasDepth bool
	shallow  []string
	done     bool
}

/** Walk a fetch command's frames and pull out its arguments from after the delim. */
func readFetchArguments(frames []packetFrame) fetchArguments {
	var arguments fetchArguments
	readingArguments := false
	for _, frame := range frames {
		if frame.kind == frameDelim {
			readingArguments = true
			continue
		}
		if frame.kind != frameData || !readingArguments {
			continue
		}
		text := frameText(frame)
		if strings.HasPrefix(text, "want ") {
			arguments.wants = append(arguments.wants, text[5:])
		} else if strings.HasPrefix(text, "have ") {
			arguments.haves = append(arguments.haves, text[5:])
		} else if strings.HasPrefix(text, "shallow ") {
			arguments.shallow = append(arguments.shallow, text[8:])
		} else if strings.HasPrefix(text, "deepen ") {
			arguments.hasDepth = true
			if parsed, err := strconv.Atoi(strings.TrimSpace(text[7:])); err == nil {
				arguments.depth = parsed
			}
		} else if text == "done" {
			arguments.done = true
		}
	}
	return arguments
}

/** Walk an id through tags until a commit is found; "" when a non-tag blocks the path. */
func targetCommit(repo *Repo, id string) string {
	current := id
	for hop := 0; hop < 8; hop += 1 {
		object, ok := repo.Objects[current]
		if !ok {
			return ""
		}
		if object.Type == typeCommit {
			return current
		}
		if object.Type != typeTag {
			return ""
		}
		current = parseTag(object.Data).Object
	}
	return ""
}

/**
 * Every object reachable from the given roots, in the exact DFS order the reference uses (a stack
 * popped from the end). The order is deterministic so the packfile bytes are reproducible.
 */
func reachableOrdered(table objectTable, roots []string) []string {
	seen := map[string]bool{}
	order := []string{}
	stack := append([]string(nil), roots...)
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[id] {
			continue
		}
		object, ok := table[id]
		if !ok {
			continue
		}
		seen[id] = true
		order = append(order, id)
		if object.Type == typeCommit {
			commit := parseCommit(object.Data)
			stack = append(stack, commit.Tree)
			for _, parent := range commit.Parents {
				stack = append(stack, parent)
			}
		} else if object.Type == typeTree {
			for _, entry := range parseTree(object.Data) {
				stack = append(stack, entry.SHA)
			}
		} else if object.Type == typeTag {
			stack = append(stack, parseTag(object.Data).Object)
		}
	}
	return order
}

/**
 * Choose the objects a fetch sends. Wants are walked down to `depth` (their chain is shallow-cut and
 * reported as a boundary); whatever the client's haves already cover is set aside, then every wanted
 * commit's tree and its reachable contents are gathered.
 */
func chooseFetchObjects(repo *Repo, argument fetchArguments) (objects []gitObj, shallow []string, unshallow []string) {
	table := repo.Objects
	depth := 0
	if argument.hasDepth && argument.depth > 0 {
		depth = argument.depth
	}
	commits := []string{}
	frontier := map[string]bool{}
	frontierOrder := []string{}
	touched := map[string]bool{}
	extra := []string{}
	var descend func(oid string, level int)
	descend = func(oid string, level int) {
		if touched[oid] {
			return
		}
		object, ok := table[oid]
		if !ok {
			return
		}
		touched[oid] = true
		commitID := oid
		if object.Type == typeTag {
			extra = append(extra, oid)
			target := targetCommit(repo, oid)
			if target == "" {
				return
			}
			commitID = target
			if touched[commitID] {
				return
			}
		}
		commit, ok := table[commitID]
		if !ok || commit.Type != typeCommit {
			return
		}
		touched[commitID] = true
		commits = append(commits, commitID)
		parents := parseCommit(commit.Data).Parents
		if len(parents) == 0 {
			return
		}
		if depth != 0 && level >= depth {
			if !frontier[commitID] {
				frontier[commitID] = true
				frontierOrder = append(frontierOrder, commitID)
			}
			return
		}
		for _, parent := range parents {
			descend(parent, level+1)
		}
	}
	for _, want := range argument.wants {
		descend(want, 1)
	}

	// Mark every object the client states it holds, so it is not shipped again. The client's shallow
	// set stops the walk there: those commits count as held, but their ancestors do not.
	held := map[string]bool{}
	if len(argument.haves) > 0 {
		stops := map[string]bool{}
		for _, id := range argument.shallow {
			stops[id] = true
		}
		pending := append([]string(nil), argument.haves...)
		for len(pending) > 0 {
			id := pending[len(pending)-1]
			pending = pending[:len(pending)-1]
			if held[id] {
				continue
			}
			object, ok := table[id]
			if !ok {
				continue
			}
			held[id] = true
			if object.Type == typeCommit {
				commit := parseCommit(object.Data)
				pending = append(pending, commit.Tree)
				if !stops[id] {
					for _, parent := range commit.Parents {
						pending = append(pending, parent)
					}
				}
			} else if object.Type == typeTree {
				for _, entry := range parseTree(object.Data) {
					pending = append(pending, entry.SHA)
				}
			} else if object.Type == typeTag {
				pending = append(pending, parseTag(object.Data).Object)
			}
		}
	}

	toSend := map[string]bool{}
	order := []string{}
	addToSend := func(id string) {
		if !toSend[id] {
			toSend[id] = true
			order = append(order, id)
		}
	}
	for _, id := range extra {
		addToSend(id)
	}
	for _, commitID := range commits {
		addToSend(commitID)
		commit, ok := table[commitID]
		if !ok {
			continue
		}
		for _, id := range reachableOrdered(table, []string{parseCommit(commit.Data).Tree}) {
			addToSend(id)
		}
	}
	for _, id := range order {
		if held[id] {
			continue
		}
		object, ok := table[id]
		if !ok {
			continue
		}
		objects = append(objects, object)
	}
	argShallow := map[string]bool{}
	for _, id := range argument.shallow {
		argShallow[id] = true
	}
	for _, oid := range frontierOrder {
		if !argShallow[oid] {
			shallow = append(shallow, oid)
		}
	}
	for _, oid := range argument.shallow {
		if !frontier[oid] {
			unshallow = append(unshallow, oid)
		}
	}
	return objects, shallow, unshallow
}

/** Answer a fetch: the packfile section (sideband-wrapped) after any shallow-info boundary move. */
/**
 * A fetch's reply: the pkt-lines that precede the pack, and the pack already under band-1
 * side-band framing as the cache built it. The connection writes the two as separate buffers in
 * one writev, so no clone copies its pack.
 */
func doFetch(repo *Repo, frames []packetFrame) ([]byte, []byte) {
	argument := readFetchArguments(frames)
	if len(argument.wants) == 0 {
		return pktFlush, nil
	}
	objects, shallow, unshallow := chooseFetchObjects(repo, argument)
	entry := repo.cachedPack(fetchPackCacheKey(argument), objects)
	out := [][]byte{}
	if len(shallow) > 0 || len(unshallow) > 0 {
		out = append(out, pktLine("shallow-info\n"))
		for _, oid := range shallow {
			out = append(out, pktLine("shallow "+oid+"\n"))
		}
		for _, oid := range unshallow {
			out = append(out, pktLine("unshallow "+oid+"\n"))
		}
		out = append(out, pktDelim)
	}
	out = append(out, pktLine("packfile\n"))
	return bytes.Join(out, nil), entry.framed
}

/**
 * The cache key of a fetch's pack: everything that can change the pack bytes is the same request
 * here. The pack lives on the repository, so the repository identity is implicit; the depth and the
 * sorted want / shallow / have lists are all that distinguish two fetches' packs. Sorted lists are
 * used because the client presents them in arbitrary order and the pack does not depend on it.
 */
func fetchPackCacheKey(argument fetchArguments) string {
	wants := append([]string(nil), argument.wants...)
	sort.Strings(wants)
	haves := append([]string(nil), argument.haves...)
	sort.Strings(haves)
	shallow := append([]string(nil), argument.shallow...)
	sort.Strings(shallow)
	var b strings.Builder
	b.WriteString(strconv.Itoa(argument.depth))
	b.WriteByte('\x00')
	for _, id := range wants {
		b.WriteString(id)
		b.WriteByte('\x00')
	}
	b.WriteByte('|')
	for _, id := range shallow {
		b.WriteString(id)
		b.WriteByte('\x00')
	}
	b.WriteByte('|')
	for _, id := range haves {
		b.WriteString(id)
		b.WriteByte('\x00')
	}
	return b.String()
}

/** One cached pack: its bytes, and the same pack already under band-1 side-band framing. */
type packReply struct {
	pack   []byte
	framed []byte
}

/** One in-flight pack build whose bytes the other 49 identical clones await. */
type packBuild struct {
	done  chan struct{}
	entry *packReply
}

/**
 * Return the cached pack for a key, building it once even when many requests miss at the same
 * time. The framing is built with it, so a clone writes the cached bytes instead of copying its
 * pack into its own reply. Both slices are immutable and must never be written to.
 */
func (repo *Repo) cachedPack(key string, objects []gitObj) *packReply {
	repo.PackMu.Lock()
	if repo.PackCache == nil {
		repo.PackCache = map[string]*packReply{}
	}
	if cached, ok := repo.PackCache[key]; ok {
		repo.PackMu.Unlock()
		return cached
	}
	if pending, ok := repo.PackPending[key]; ok {
		repo.PackMu.Unlock()
		<-pending.done
		return pending.entry
	}
	build := &packBuild{done: make(chan struct{})}
	if repo.PackPending == nil {
		repo.PackPending = map[string]*packBuild{}
	}
	repo.PackPending[key] = build
	repo.PackMu.Unlock()

	pack := writePack(objects)
	build.entry = &packReply{pack: pack, framed: sidebandPack(pack, 65500)}
	close(build.done)

	repo.PackMu.Lock()
	if repo.PackPending[key] == build {
		delete(repo.PackPending, key)
	}
	if repo.PackCache[key] == nil {
		repo.PackCache[key] = build.entry
	}
	repo.PackMu.Unlock()
	return build.entry
}

// ---------------------------------------------------------------------------
// receive-pack: the push
// ---------------------------------------------------------------------------

/** One command from a push command list. */
type pushRef struct {
	oldOID string
	newOID string
	ref    string
}

/** Parse the command list, take the pack off the tail, store it and report per-ref verdicts. */
func runReceivePack(repo *Repo, body []byte) []byte {
	frames, rest, _ := splitPackets(body)
	commands := []pushRef{}
	capabilitySet := map[string]bool{}
	for _, frame := range frames {
		if frame.kind != frameData {
			continue
		}
		raw := string(frame.payload)
		nul := strings.IndexByte(raw, 0)
		primary := raw
		if nul >= 0 {
			primary = raw[:nul]
			for _, capability := range strings.Split(strings.TrimSuffix(raw[nul+1:], "\n"), " ") {
				if capability != "" {
					capabilitySet[capability] = true
				}
			}
		}
		bodyless := strings.TrimSuffix(primary, "\n")
		parts := strings.Split(bodyless, " ")
		if len(parts) < 3 {
			continue
		}
		commands = append(commands, pushRef{oldOID: parts[0], newOID: parts[1], ref: strings.Join(parts[2:], " ")})
	}

	unpacked := "ok"
	if len(rest) > 0 {
		opened, err := readPack(rest, nil)
		if err != nil {
			unpacked = err.Error()
		} else {
			for oid, object := range opened.Objects {
				if _, ok := repo.Objects[oid]; !ok {
					repo.Objects[oid] = object
				}
			}
		}
	}

	reportLines := [][]byte{pktLine("unpack " + unpacked + "\n")}
	if unpacked == "ok" {
		for _, command := range commands {
			current, present := repo.Refs[command.ref]
			if !present {
				current = zeroOID
			}
			refusal := ""
			refused := false
			if command.newOID == zeroOID {
				delete(repo.Refs, command.ref)
			} else if current != command.oldOID {
				refused, refusal = true, "non-fast-forward"
			} else if current != zeroOID && !isAncestor(repo.Objects, current, command.newOID) {
				refused, refusal = true, "non-fast-forward"
			} else if _, has := repo.Objects[command.newOID]; !has {
				refused, refusal = true, "missing object"
			} else {
				repo.Refs[command.ref] = command.newOID
				if strings.HasPrefix(command.ref, "refs/heads/") {
					if _, has := repo.Refs["refs/heads/"+repo.DefaultBranch]; !has {
						repo.DefaultBranch = strings.TrimPrefix(command.ref, "refs/heads/")
					}
				}
			}
			if refused {
				reportLines = append(reportLines, pktLine("ng "+command.ref+" "+refusal+"\n"))
			} else {
				reportLines = append(reportLines, pktLine("ok "+command.ref+"\n"))
			}
		}
	}
	reportLines = append(reportLines, pktFlush)
	report := bytes.Join(reportLines, nil)
	if capabilitySet["side-band-64k"] {
		framed := [][]byte{}
		for at := 0; at < len(report); at += 32768 {
			end := at + 32768
			if end > len(report) {
				end = len(report)
			}
			framed = append(framed, pktLineBytes(append([]byte{1}, report[at:end]...)))
		}
		framed = append(framed, pktFlush)
		return bytes.Join(framed, nil)
	}
	return report
}