// GitHub REST v3: repositories, branches, commits, contents, labels, issues, comments and pull
// requests. Every reply is JSON with an X-GitHub-Request-Id; errors carry a `message`. This is a
// faithful port of src/rest.ts: same statuses, bodies, headers and the same stamp call order.

package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// The installed version REST reports (rest.ts's own constant, not service.ts's).
const restInstalledVersion = "makefun-forge/1.0"

// ---------------------------------------------------------------------------
// JSON views (the REST spellings: issues and pulls carry no node_id here)

type restIssueView struct {
	ID        int         `json:"id"`
	Number    int         `json:"number"`
	Title     string      `json:"title"`
	Body      string      `json:"body"`
	State     string      `json:"state"`
	Labels    []labelView `json:"labels"`
	User      userJSON    `json:"user"`
	Assignee  any         `json:"assignee"`
	Assignees []any       `json:"assignees"`
	Milestone any         `json:"milestone"`
	Comments  int         `json:"comments"`
	CreatedAt string      `json:"created_at"`
	UpdatedAt string      `json:"updated_at"`
	ClosedAt  *string     `json:"closed_at"`
	HTMLURL   string      `json:"html_url"`
	Locked    bool        `json:"locked"`
}

func restIssueJSON(repo *Repo, iss issue) restIssueView {
	labels := make([]labelView, 0, len(iss.Labels))
	for _, l := range iss.Labels {
		labels = append(labels, labelJSON(l))
	}
	return restIssueView{
		ID:        iss.ID,
		Number:    iss.Number,
		Title:     iss.Title,
		Body:      iss.Body,
		State:     iss.State,
		Labels:    labels,
		User:      renderUser(iss.User),
		Assignee:  nil,
		Assignees: []any{},
		Milestone: nil,
		Comments:  0,
		CreatedAt: iss.CreatedAt,
		UpdatedAt: iss.UpdatedAt,
		ClosedAt:  iss.ClosedAt,
		HTMLURL:   repo.Owner + "/" + repo.Name + "#" + strconv.Itoa(iss.Number),
		Locked:    false,
	}
}

type restPullView struct {
	ID             int          `json:"id"`
	Number         int          `json:"number"`
	State          string       `json:"state"`
	Title          string       `json:"title"`
	Body           string       `json:"body"`
	User           userJSON     `json:"user"`
	Head           headBaseView `json:"head"`
	Base           headBaseView `json:"base"`
	MergeCommitSHA *string      `json:"merge_commit_sha"`
	Merged         bool         `json:"merged"`
	MergedAt       *string      `json:"merged_at"`
	Mergeable      bool         `json:"mergeable"`
	CreatedAt      string       `json:"created_at"`
	UpdatedAt      string       `json:"updated_at"`
	HTMLURL        string       `json:"html_url"`
}

func restPullJSON(repo *Repo, p pull) restPullView {
	return restPullView{
		ID:     p.ID,
		Number: p.Number,
		State:  p.State,
		Title:  p.Title,
		Body:   p.Body,
		User:   renderUser(p.User),
		Head: headBaseView{Ref: p.HeadRef, SHA: p.HeadSha, Label: repo.Owner + ":" + p.HeadRef, User: serviceUser},
		Base: headBaseView{Ref: p.BaseRef, SHA: p.BaseSha, Label: repo.Owner + ":" + p.BaseRef, User: serviceUser},
		MergeCommitSHA: p.MergeCommitSha,
		Merged:         p.Merged,
		MergedAt:       p.MergedAt,
		Mergeable:      !p.Merged,
		CreatedAt:      p.CreatedAt,
		UpdatedAt:      p.UpdatedAt,
		HTMLURL:        repo.Owner + "/" + repo.Name + "/pull/" + strconv.Itoa(p.Number),
	}
}

type dirEntryView struct {
	Type string `json:"type"`
	Name string `json:"name"`
	Path string `json:"path"`
	SHA  string `json:"sha"`
	Size int    `json:"size"`
	Mode string `json:"mode"`
}

type contentEntryView struct {
	Type     string `json:"type"`
	Name     string `json:"name"`
	Path     string `json:"path"`
	SHA      string `json:"sha"`
	Size     int    `json:"size"`
	Encoding string `json:"encoding"`
	Content  string `json:"content"`
	Mode     string `json:"mode"`
}

type contentCommitView struct {
	SHA     string `json:"sha"`
	Message string `json:"message"`
	Author  struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	} `json:"author"`
}

type uploadContentView struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	SHA      string `json:"sha"`
	Size     int    `json:"size"`
	Encoding string `json:"encoding"`
	Content  string `json:"content"`
	Type     string `json:"type"`
}

type refView struct {
	Ref    string `json:"ref"`
	Object struct {
		SHA  string `json:"sha"`
		Type string `json:"type"`
		URL  string `json:"url,omitempty"`
	} `json:"object"`
}

// ---------------------------------------------------------------------------
// helpers

// numberParam mirrors Number.parseInt with a positive-only fallback.
func numberParam(query url.Values, name string, fallback int) int {
	raw := query.Get(name)
	if raw == "" {
		return fallback
	}
	value := 0
	digits := 0
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if c < '0' || c > '9' {
			break
		}
		value = value*10 + int(c-'0')
		digits++
	}
	if digits == 0 || value <= 0 {
		return fallback
	}
	return value
}

// pageItems slices `items` for one page, and returns the page and per-page numbers.
func pageSlice[T any](items []T, query url.Values) (slice []T, page, perPage int) {
	perPage = numberParam(query, "per_page", 30)
	if perPage > 100 {
		perPage = 100
	}
	page = numberParam(query, "page", 1)
	start := (page - 1) * perPage
	if start > len(items) {
		start = len(items)
	}
	end := start + perPage
	if end > len(items) {
		end = len(items)
	}
	return items[start:end], page, perPage
}

// linkHeader builds `Link: <...>; rel="next"` when another page exists, exactly as rest.ts does
// (per_page and page first, every other query parameter preserved).
func linkHeader(base, path string, query url.Values, page, perPage int, hasMore bool) map[string]string {
	if !hasMore {
		return nil
	}
	keys := make([]string, 0, len(query))
	for key := range query {
		if key != "per_page" && key != "page" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	parts := []string{}
	for _, key := range keys {
		for _, v := range query[key] {
			parts = append(parts, url.QueryEscape(key)+"="+url.QueryEscape(v))
		}
	}
	parts = append(parts, "per_page="+strconv.Itoa(perPage), "page="+strconv.Itoa(page+1))
	return map[string]string{"link": "<" + base + path + "?" + strings.Join(parts, "&") + ">; rel=\"next\""}
}

// parseJSONBody mirrors rest.ts's parseJsonBody: an empty body is an empty object, a malformed
// body, an array or a scalar is "not an object" (the caller answers 400).
func parseJSONBody(body []byte) (map[string]any, bool) {
	if len(body) == 0 {
		return map[string]any{}, true
	}
	var parsed any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, false
	}
	obj, ok := parsed.(map[string]any)
	if !ok {
		return nil, false
	}
	return obj, true
}

// jsonStr is `str()` in rest.ts: only strings pass through.
func jsonStr(value any) string {
	s, ok := value.(string)
	if !ok {
		return ""
	}
	return s
}

// nodeBase64 decodes a base64 string the way Buffer.from(text, 'base64') does: characters outside
// the alphabet and stray padding are ignored, partial trailing groups are dropped.
func nodeBase64(s string) []byte {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	values := make([]byte, 128)
	for i := range values {
		values[i] = 255
	}
	for i := 0; i < len(alphabet); i++ {
		values[alphabet[i]] = byte(i)
	}
	out := []byte{}
	acc := 0
	bits := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 128 || values[c] == 255 {
			continue
		}
		acc = (acc << 6) | int(values[c])
		bits += 6
		if bits >= 8 {
			bits -= 8
			out = append(out, byte((acc>>bits)&0xff))
		}
	}
	return out
}

// decodeContentBase64 decodes a contents-API body field: strict first, then the lenient Node form.
func decodeContentBase64(s string) []byte {
	if data, err := base64.StdEncoding.DecodeString(s); err == nil {
		return data
	}
	return nodeBase64(s)
}

// ---------------------------------------------------------------------------
// the router

// handleRest answers a `/api/v3` request. Returns false when no route matched (the caller sends
// the 404).
func handleRest(res http.ResponseWriter, store *Store, method string, segments []string, query url.Values, base string, body []byte) bool {
	if len(segments) == 0 {
		if method == "GET" {
			sendJSON(res, 200, struct {
				CurrentUserURL  string `json:"current_user_url"`
				RepositoriesURL string `json:"repositories_url"`
			}{"/api/v3/user", "/api/v3/user/repos"}, map[string]string{"x-oauth-scopes": "repo, read:org"})
			return true
		}
		return false
	}
	head := segments[0]
	if head == "meta" {
		sendJSON(res, 200, struct {
			InstalledVersion string `json:"installed_version"`
		}{restInstalledVersion})
		return true
	}
	if head == "user" {
		if len(segments) == 1 {
			if method != "GET" {
				return false
			}
			sendJSON(res, 200, struct {
				Login       string `json:"login"`
				ID          int    `json:"id"`
				Type        string `json:"type"`
				SiteAdmin   bool   `json:"site_admin"`
				Name        string `json:"name"`
				Email       string `json:"email"`
				PublicRepos int    `json:"public_repos"`
			}{viewer.Login, viewer.ID, "User", false, viewer.Name, viewer.Email, 0})
			return true
		}
		if segments[1] == "repos" {
			if len(segments) != 2 {
				return false
			}
			if method == "GET" {
				return restListRepos(res, store, query, base)
			}
			if method == "POST" {
				return restCreateRepo(res, store, body, base)
			}
			return false
		}
		return false
	}
	if head == "repos" && len(segments) >= 3 {
		owner := segments[1]
		name := segments[2]
		repo, ok := getRepo(store, owner, name)
		if !ok {
			sendError(res, 404, "Not Found")
			return true
		}
		return handleRepo(res, store, repo, method, segments[3:], query, base, body)
	}
	if head == "users" && len(segments) == 2 {
		sendJSON(res, 200, struct {
			Login     string `json:"login"`
			ID        int    `json:"id"`
			Type      string `json:"type"`
			SiteAdmin bool   `json:"site_admin"`
		}{segments[1], viewer.ID, "User", false})
		return true
	}
	return false
}

func restListRepos(res http.ResponseWriter, store *Store, query url.Values, base string) bool {
	all := []*Repo{}
	for _, key := range store.RepoNames {
		if repo, ok := store.Repos[key]; ok && repo.Owner == viewer.Login {
			all = append(all, repo)
		}
	}
	slice, page, perPage := pageSlice(all, query)
	extra := linkHeader(base, "/api/v3/user/repos", query, page, perPage, page*perPage < len(all))
	out := make([]repoView, 0, len(slice))
	for _, repo := range slice {
		out = append(out, repoJSON(repo))
	}
	if extra == nil {
		sendJSON(res, 200, out)
	} else {
		sendJSON(res, 200, out, extra)
	}
	return true
}

func restCreateRepo(res http.ResponseWriter, store *Store, body []byte, base string) bool {
	parsed, ok := parseJSONBody(body)
	if !ok {
		sendError(res, 400, "Problems parsing JSON")
		return true
	}
	name := jsonStr(parsed["name"])
	if name == "" {
		sendError(res, 422, "Repository name is required")
		return true
	}
	if _, exists := getRepo(store, viewer.Login, name); exists {
		sendError(res, 422, "Repository creation failed: name already exists")
		return true
	}
	repo := createRepo(store, viewer.Login, name, jsonStr(parsed["description"]), parsed["private"] == true)
	sendJSON(res, 201, repoJSON(repo), map[string]string{"location": base + "/api/v3/repos/" + repo.Owner + "/" + repo.Name})
	return true
}

func handleRepo(res http.ResponseWriter, store *Store, repo *Repo, method string, rest []string, query url.Values, base string, body []byte) bool {
	if len(rest) == 0 {
		if method == "GET" {
			sendJSON(res, 200, repoJSON(repo))
			return true
		}
		if method == "PATCH" {
			parsed, ok := parseJSONBody(body)
			if !ok {
				sendError(res, 400, "Problems parsing JSON")
				return true
			}
			if description, isString := parsed["description"].(string); isString {
				repo.Description = description
			}
			if newName, isString := parsed["name"].(string); isString && newName != "" && newName != repo.Name {
				if _, exists := getRepo(store, repo.Owner, newName); exists {
					sendError(res, 422, "Repository creation failed: name already exists")
					return true
				}
				oldKey := repoKey(repo.Owner, repo.Name)
				delete(store.Repos, oldKey)
				for i, existing := range store.RepoNames {
					if existing == oldKey {
						store.RepoNames[i] = repoKey(repo.Owner, newName)
						break
					}
				}
				repo.Name = newName
				store.Repos[repoKey(repo.Owner, repo.Name)] = repo
			}
			repo.UpdatedAt = stamp(repo)
			sendJSON(res, 200, repoJSON(repo))
			return true
		}
		if method == "DELETE" {
			deleteRepo(store, repo.Owner, repo.Name)
			sendNoContent(res)
			return true
		}
		return false
	}

	head := rest[0]
	if head == "branches" && len(rest) == 1 && method == "GET" {
		names := []string{}
		for ref := range repo.Refs {
			if strings.HasPrefix(ref, "refs/heads/") {
				names = append(names, ref[len("refs/heads/"):])
			}
		}
		sort.Strings(names)
		branches := make([]branchView, 0, len(names))
		for _, name := range names {
			branches = append(branches, branchJSON(repo, name, repo.Refs["refs/heads/"+name]))
		}
		sendJSON(res, 200, branches)
		return true
	}

	if head == "commits" && len(rest) == 1 && method == "GET" {
		wanted := query.Get("sha")
		if wanted == "" {
			wanted = ""
		}
		tip := resolveCommit(repo, wanted)
		out := []commitView{}
		current := tip
		seen := map[string]bool{}
		for current != "" && len(out) < 100 && !seen[current] {
			seen[current] = true
			obj, ok := repo.Objects[current]
			if !ok || obj.Type != typeCommit {
				break
			}
			out = append(out, commitJSON(repo, current))
			parents := parseCommit(obj.Data).Parents
			if len(parents) > 0 {
				current = parents[0]
			} else {
				current = ""
			}
		}
		sendJSON(res, 200, out)
		return true
	}

	if head == "git" && len(rest) >= 2 && rest[1] == "refs" {
		if len(rest) == 2 && method == "GET" {
			refs := []string{}
			for ref := range repo.Refs {
				refs = append(refs, ref)
			}
			sort.Strings(refs)
			out := make([]refView, 0, len(refs))
			for _, ref := range refs {
				view := refView{Ref: ref}
				view.Object.SHA = repo.Refs[ref]
				view.Object.Type = "commit"
				out = append(out, view)
			}
			sendJSON(res, 200, out)
			return true
		}
		if len(rest) == 2 && method == "POST" {
			parsed, ok := parseJSONBody(body)
			if !ok {
				sendError(res, 400, "Problems parsing JSON")
				return true
			}
			ref := jsonStr(parsed["ref"])
			sha := jsonStr(parsed["sha"])
			if !strings.HasPrefix(ref, "refs/") || !isHex40(sha) {
				sendError(res, 422, "Reference update failed")
				return true
			}
			if _, exists := repo.Objects[sha]; !exists {
				sendError(res, 422, "Object does not exist")
				return true
			}
			repo.Refs[ref] = sha
			view := refView{Ref: ref}
			view.Object.SHA = sha
			view.Object.Type = "commit"
			view.Object.URL = base + "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/git/refs"
			sendJSON(res, 201, view)
			return true
		}
		return false
	}

	if head == "contents" {
		return handleContents(res, repo, method, rest[1:], query, body)
	}
	if head == "labels" {
		return handleLabels(res, repo, method, rest[1:], body)
	}
	if head == "issues" {
		return handleIssues(res, repo, method, rest[1:], query, body)
	}
	if head == "pulls" {
		return handlePulls(res, repo, method, rest[1:], query, body)
	}
	return false
}

// ---------------------------------------------------------------------------
// contents

func handleContents(res http.ResponseWriter, repo *Repo, method string, pathParts []string, query url.Values, body []byte) bool {
	path := strings.Join(pathParts, "/")
	ref := query.Get("ref")
	if path == "" {
		sendError(res, 404, "Not Found")
		return true
	}
	if method == "GET" {
		commit := resolveCommit(repo, ref)
		tree := ""
		if commit != "" {
			tree = commitTree(repo, commit)
		}
		if commit == "" || tree == "" {
			sendError(res, 404, "Not Found")
			return true
		}
		parts := []string{}
		for _, part := range strings.Split(path, "/") {
			if part != "" {
				parts = append(parts, part)
			}
		}
		current := tree
		for i := 0; i < len(parts); i++ {
			var data []byte
			if obj, ok := repo.Objects[current]; ok {
				data = obj.Data
			}
			entries := parseTree(data)
			var entry *treeEntry
			for idx := range entries {
				if entries[idx].Name == parts[i] {
					entry = &entries[idx]
					break
				}
			}
			if entry == nil {
				sendError(res, 404, "Not Found")
				return true
			}
			if i == len(parts)-1 {
				if entry.Mode == "40000" {
					var childData []byte
					if obj, ok := repo.Objects[entry.SHA]; ok {
						childData = obj.Data
					}
					children := parseTree(childData)
					listing := make([]dirEntryView, 0, len(children))
					for _, child := range children {
						size := 0
						if child.Mode != "40000" {
							if obj, ok := repo.Objects[child.SHA]; ok {
								size = len(obj.Data)
							}
						}
						mode := child.Mode
						if child.Mode == "40000" {
							mode = "040000"
						}
						entryType := "file"
						if child.Mode == "40000" {
							entryType = "dir"
						}
						listing = append(listing, dirEntryView{Type: entryType, Name: child.Name, Path: path + "/" + child.Name, SHA: child.SHA, Size: size, Mode: mode})
					}
					sendJSON(res, 200, listing)
					return true
				}
				data := []byte{}
				if obj, ok := repo.Objects[entry.SHA]; ok {
					data = obj.Data
				}
				mode := "100644"
				if entry.Mode == "100755" {
					mode = "100755"
				}
				sendJSON(res, 200, contentEntryView{
					Type: "file", Name: entry.Name, Path: path, SHA: entry.SHA,
					Size: len(data), Encoding: "base64", Content: base64.StdEncoding.EncodeToString(data), Mode: mode,
				})
				return true
			}
			if entry.Mode != "40000" {
				sendError(res, 404, "Not Found")
				return true
			}
			current = entry.SHA
		}
		sendError(res, 404, "Not Found")
		return true
	}

	if method == "PUT" {
		parsed, ok := parseJSONBody(body)
		if !ok {
			sendError(res, 400, "Problems parsing JSON")
			return true
		}
		contentStr, isString := parsed["content"].(string)
		var content []byte
		if isString {
			content = decodeContentBase64(contentStr)
		}
		if !isString {
			sendError(res, 422, "Content is required")
			return true
		}
		branch := jsonStr(parsed["branch"])
		if branch == "" {
			branch = repo.DefaultBranch
		}
		currentSha := blobAt(repo, branch, path)
		supplied := jsonStr(parsed["sha"])
		if supplied != "" {
			if currentSha != supplied {
				sendError(res, 409, "sha does not match the current file")
				return true
			}
		} else if currentSha != "" {
			sendError(res, 409, "sha is required to update an existing file")
			return true
		}
		message := jsonStr(parsed["message"])
		commitMessage := message
		if commitMessage == "" {
			commitMessage = "Update " + path
		}
		commit, blobs := commitChanges(repo, branch, []fileChange{{Path: path, Content: content}}, commitMessage)
		blobSha := blobs[path]
		name := path
		if i := strings.LastIndex(name, "/"); i >= 0 {
			name = name[i+1:]
		}
		status := 200
		if currentSha == "" {
			status = 201
		}
		view := struct {
			Content uploadContentView  `json:"content"`
			Commit  contentCommitView  `json:"commit"`
		}{
			Content: uploadContentView{
				Name: name, Path: path, SHA: blobSha, Size: len(content),
				Encoding: "base64", Content: base64.StdEncoding.EncodeToString(content), Type: "file",
			},
			Commit: contentCommitView{SHA: commit, Message: message,
				Author: struct {
					Name  string `json:"name"`
					Email string `json:"email"`
				}{"Ada Iversen", viewer.Email}},
		}
		sendJSON(res, status, view)
		return true
	}

	if method == "DELETE" {
		parsed, ok := parseJSONBody(body)
		if !ok {
			sendError(res, 400, "Problems parsing JSON")
			return true
		}
		branch := jsonStr(parsed["branch"])
		if branch == "" {
			branch = repo.DefaultBranch
		}
		currentSha := blobAt(repo, branch, path)
		if currentSha == "" {
			sendError(res, 404, "Not Found")
			return true
		}
		supplied := jsonStr(parsed["sha"])
		if supplied != "" && supplied != currentSha {
			sendError(res, 409, "sha does not match the current file")
			return true
		}
		message := jsonStr(parsed["message"])
		commitMessage := message
		if commitMessage == "" {
			commitMessage = "Delete " + path
		}
		commit, _ := commitChanges(repo, branch, []fileChange{{Path: path, Content: nil}}, commitMessage)
		sendJSON(res, 200, struct {
			Content any `json:"content"`
			Commit  struct {
				SHA     string `json:"sha"`
				Message string `json:"message"`
			} `json:"commit"`
		}{nil, struct {
			SHA     string `json:"sha"`
			Message string `json:"message"`
		}{commit, message}})
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// labels, issues, comments

func handleLabels(res http.ResponseWriter, repo *Repo, method string, rest []string, body []byte) bool {
	if len(rest) == 0 {
		if method == "GET" {
			out := make([]labelView, 0, len(repo.Labels))
			for _, l := range repo.Labels {
				out = append(out, labelJSON(l))
			}
			sendJSON(res, 200, out)
			return true
		}
		if method == "POST" {
			parsed, ok := parseJSONBody(body)
			if !ok {
				sendError(res, 400, "Problems parsing JSON")
				return true
			}
			name := jsonStr(parsed["name"])
			if name == "" {
				sendError(res, 422, "Label name is required")
				return true
			}
			if _, exists := findLabel(repo, name); exists {
				sendJSON(res, 422, struct {
					Message string `json:"message"`
					Errors  []struct {
						Resource string `json:"resource"`
						Code     string `json:"code"`
						Field    string `json:"field"`
					} `json:"errors"`
				}{"Validation Failed", []struct {
					Resource string `json:"resource"`
					Code     string `json:"code"`
					Field    string `json:"field"`
				}{{"Label", "already_exists", "name"}}})
				return true
			}
			l := addLabel(repo, name, jsonStr(parsed["color"]), jsonStr(parsed["description"]))
			sendJSON(res, 201, labelJSON(l))
			return true
		}
		return false
	}
	name := strings.Join(rest, "/")
	if len(rest) >= 1 && method == "GET" {
		l, exists := findLabel(repo, name)
		if !exists {
			sendError(res, 404, "Not Found")
			return true
		}
		sendJSON(res, 200, labelJSON(l))
		return true
	}
	return false
}

func handleIssues(res http.ResponseWriter, repo *Repo, method string, rest []string, query url.Values, body []byte) bool {
	if len(rest) == 0 {
		if method == "GET" {
			state := query.Get("state")
			if state == "" {
				state = "open"
			}
			items := make([]issue, len(repo.Issues))
			copy(items, repo.Issues)
			if state != "all" {
				filtered := items[:0]
				for _, iss := range items {
					if iss.State == state {
						filtered = append(filtered, iss)
					}
				}
				items = filtered
			}
			sortBy := query.Get("sort")
			direction := 1
			if query.Get("direction") != "asc" {
				direction = -1
			}
			sort.SliceStable(items, func(i, j int) bool {
				cmp := items[i].Number - items[j].Number
				if sortBy == "updated" {
					cmp = strings.Compare(items[i].UpdatedAt, items[j].UpdatedAt)
				}
				return cmp*direction < 0
			})
			slice, _, _ := pageSlice(items, query)
			out := make([]restIssueView, 0, len(slice))
			for _, iss := range slice {
				out = append(out, restIssueJSON(repo, iss))
			}
			sendJSON(res, 200, out)
			return true
		}
		if method == "POST" {
			parsed, ok := parseJSONBody(body)
			if !ok {
				sendError(res, 400, "Problems parsing JSON")
				return true
			}
			title := jsonStr(parsed["title"])
			if title == "" {
				sendError(res, 422, "Issue title is required")
				return true
			}
			names := []string{}
			if list, isArray := parsed["labels"].([]any); isArray {
				for _, item := range list {
					if s, isString := item.(string); isString {
						names = append(names, s)
					}
				}
			}
			labels := []label{}
			for _, name := range names {
				l, exists := findLabel(repo, name)
				if !exists {
					sendError(res, 422, "Validation Failed: label does not exist")
					return true
				}
				labels = append(labels, l)
			}
			when := stamp(repo)
			iss := issue{
				ID:        100000 + repo.NextIssue*7,
				Number:    repo.NextIssue,
				Title:     title,
				Body:      jsonStr(parsed["body"]),
				State:     "open",
				Labels:    labels,
				CreatedAt: when,
				UpdatedAt: when,
				ClosedAt:  nil,
				User:      userRef{Login: viewer.Login, ID: viewer.ID},
			}
			repo.NextIssue += 1
			repo.Issues = append(repo.Issues, iss)
			repo.UpdatedAt = when
			sendJSON(res, 201, restIssueJSON(repo, iss))
			return true
		}
		return false
	}

	number, _ := strconv.Atoi(rest[0])
	var iss *issue
	for i := range repo.Issues {
		if repo.Issues[i].Number == number {
			iss = &repo.Issues[i]
			break
		}
	}
	if len(rest) == 1 {
		if iss == nil {
			sendError(res, 404, "Not Found")
			return true
		}
		if method == "GET" {
			sendJSON(res, 200, restIssueJSON(repo, *iss))
			return true
		}
		if method == "PATCH" {
			parsed, ok := parseJSONBody(body)
			if !ok {
				sendError(res, 400, "Problems parsing JSON")
				return true
			}
			if title, isString := parsed["title"].(string); isString {
				iss.Title = title
			}
			if b, isString := parsed["body"].(string); isString {
				iss.Body = b
			}
			if state, isString := parsed["state"].(string); isString && (state == "open" || state == "closed") {
				iss.State = state
				if state == "closed" {
					closedAt := stamp(repo)
					iss.ClosedAt = &closedAt
				} else {
					iss.ClosedAt = nil
				}
			}
			if list, isArray := parsed["labels"].([]any); isArray {
				wanted := []label{}
				for _, item := range list {
					name, isString := item.(string)
					if !isString {
						continue
					}
					l, exists := findLabel(repo, name)
					if !exists {
						sendError(res, 422, "Validation Failed: label does not exist")
						return true
					}
					wanted = append(wanted, l)
				}
				iss.Labels = wanted
			}
			updatedAt := stamp(repo)
			iss.UpdatedAt = updatedAt
			sendJSON(res, 200, restIssueJSON(repo, *iss))
			return true
		}
		return false
	}

	if rest[1] == "comments" {
		if iss == nil {
			sendError(res, 404, "Not Found")
			return true
		}
		if len(rest) == 2 && method == "GET" {
			out := []commentView{}
			for _, comment := range repo.Comments {
				if comment.IssueNumber == iss.Number {
					out = append(out, commentJSON(comment))
				}
			}
			sendJSON(res, 200, out)
			return true
		}
		if len(rest) == 2 && method == "POST" {
			parsed, ok := parseJSONBody(body)
			if !ok {
				sendError(res, 400, "Problems parsing JSON")
				return true
			}
			when := stamp(repo)
			comment := issueComment{
				ID:          repo.NextComment,
				IssueNumber: iss.Number,
				Body:        jsonStr(parsed["body"]),
				CreatedAt:   when,
				User:        userRef{Login: viewer.Login, ID: viewer.ID},
			}
			repo.NextComment += 1
			repo.Comments = append(repo.Comments, comment)
			sendJSON(res, 201, commentJSON(comment))
			return true
		}
		return false
	}
	return false
}

// ---------------------------------------------------------------------------
// pull requests

func headBranch(ref string) string {
	colon := strings.Index(ref, ":")
	if colon < 0 {
		return ref
	}
	return ref[colon+1:]
}

func handlePulls(res http.ResponseWriter, repo *Repo, method string, rest []string, query url.Values, body []byte) bool {
	if len(rest) == 0 {
		if method == "GET" {
			state := query.Get("state")
			if state == "" {
				state = "open"
			}
			items := repo.Pulls
			if state == "open" {
				filtered := []pull{}
				for _, p := range repo.Pulls {
					if p.State == "open" {
						filtered = append(filtered, p)
					}
				}
				items = filtered
			} else if state == "closed" {
				filtered := []pull{}
				for _, p := range repo.Pulls {
					if p.State == "closed" {
						filtered = append(filtered, p)
					}
				}
				items = filtered
			}
			slice, _, _ := pageSlice(items, query)
			out := make([]restPullView, 0, len(slice))
			for _, p := range slice {
				out = append(out, restPullJSON(repo, p))
			}
			sendJSON(res, 200, out)
			return true
		}
		if method == "POST" {
			parsed, ok := parseJSONBody(body)
			if !ok {
				sendError(res, 400, "Problems parsing JSON")
				return true
			}
			head := headBranch(jsonStr(parsed["head"]))
			base := jsonStr(parsed["base"])
			if base == "" {
				base = repo.DefaultBranch
			}
			headSha, hasHead := repo.Refs[branchRef(head)]
			baseSha, hasBase := repo.Refs[branchRef(base)]
			if !hasHead || !hasBase {
				sendError(res, 422, "Validation Failed: head or base branch does not exist")
				return true
			}
			if head == base {
				sendError(res, 422, "Validation Failed: head and base are the same branch")
				return true
			}
			when := stamp(repo)
			p := pull{
				ID:             200000 + repo.NextPull*3,
				Number:         repo.NextPull,
				Title:          jsonStr(parsed["title"]),
				Body:           jsonStr(parsed["body"]),
				State:          "open",
				HeadRef:        head,
				BaseRef:        base,
				HeadSha:        headSha,
				BaseSha:        baseSha,
				MergeCommitSha: nil,
				Merged:         false,
				MergedAt:       nil,
				CreatedAt:      when,
				UpdatedAt:      when,
				User:           userRef{Login: viewer.Login, ID: viewer.ID},
			}
			repo.NextPull += 1
			repo.Pulls = append(repo.Pulls, p)
			sendJSON(res, 201, restPullJSON(repo, p))
			return true
		}
		return false
	}

	number, _ := strconv.Atoi(rest[0])
	var p *pull
	for i := range repo.Pulls {
		if repo.Pulls[i].Number == number {
			p = &repo.Pulls[i]
			break
		}
	}
	if p == nil {
		sendError(res, 404, "Not Found")
		return true
	}
	if len(rest) == 1 {
		if method == "GET" {
			sendJSON(res, 200, restPullJSON(repo, *p))
			return true
		}
		if method == "PATCH" {
			parsed, ok := parseJSONBody(body)
			if !ok {
				sendError(res, 400, "Problems parsing JSON")
				return true
			}
			if title, isString := parsed["title"].(string); isString {
				p.Title = title
			}
			if b, isString := parsed["body"].(string); isString {
				p.Body = b
			}
			if state, isString := parsed["state"].(string); isString && (state == "open" || state == "closed") {
				p.State = state
			}
			updatedAt := stamp(repo)
			p.UpdatedAt = updatedAt
			sendJSON(res, 200, restPullJSON(repo, *p))
			return true
		}
		return false
	}
	if rest[1] == "files" && method == "GET" {
		headTip := refSha(repo, branchRef(p.HeadRef))
		if headTip == "" {
			headTip = p.HeadSha
		}
		files := diffFiles(repo, peelShallow(repo.Objects, p.BaseSha), peelShallow(repo.Objects, headTip))
		if files == nil {
			files = []fileDiff{}
		}
		sendJSON(res, 200, files)
		return true
	}
	if rest[1] == "merge" && method == "PUT" {
		return restMergePull(res, repo, p)
	}
	return false
}

func restMergePull(res http.ResponseWriter, repo *Repo, p *pull) bool {
	if p.Merged {
		sendError(res, 405, "Pull request is not mergeable")
		return true
	}
	baseTip := refSha(repo, branchRef(p.BaseRef))
	if baseTip == "" {
		sendError(res, 409, "Base branch is gone")
		return true
	}
	headTip := refSha(repo, branchRef(p.HeadRef))
	if headTip == "" {
		headTip = p.HeadSha
	}
	baseCommit := peelShallow(repo.Objects, p.BaseSha)
	headCommit := peelShallow(repo.Objects, headTip)
	if baseTip != peelShallow(repo.Objects, p.BaseSha) {
		pullPaths := map[string]bool{}
		for _, file := range diffFiles(repo, baseCommit, headCommit) {
			pullPaths[file.Filename] = true
		}
		for _, file := range diffFiles(repo, baseCommit, peelShallow(repo.Objects, baseTip)) {
			if pullPaths[file.Filename] {
				sendError(res, 405, "Pull Request is not mergeable")
				return true
			}
		}
	}
	changed := diffFiles(repo, baseCommit, headCommit)
	headFiles := filesAt(repo, headCommit)
	changes := make([]fileChange, 0, len(changed))
	for _, file := range changed {
		var content []byte
		if file.Status != "removed" {
			content = blobData(repo, headFiles[file.Filename])
		}
		changes = append(changes, fileChange{Path: file.Filename, Content: content})
	}
	message := mergePullMessage(p)
	commit, _ := commitChanges(repo, p.BaseRef, changes, message)
	mergedAt := stamp(repo)
	p.State = "closed"
	p.Merged = true
	p.MergedAt = &mergedAt
	p.MergeCommitSha = &commit
	p.UpdatedAt = mergedAt
	p.HeadSha = headTip
	sendJSON(res, 200, struct {
		SHA     string `json:"sha"`
		Merged  bool   `json:"merged"`
		Message string `json:"message"`
	}{commit, true, "Pull request successfully merged"})
	return true
}