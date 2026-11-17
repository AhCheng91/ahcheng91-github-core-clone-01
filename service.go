// The operations every surface shares, and the JSON shapes GitHub answers with. REST, GraphQL and
// MCP all go through these views and records, so an issue created over one surface reads back the
// same over the others. Nothing here is surface-specific: no headers, no routing.

package main

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// The installed version string this forge reports; the REST surface carries its own.
const serviceInstalledVersion = "makefun-gh-forge/1.0"

// The full viewer object JSON surfaces serialize. The store's userRef carries only login and id;
// every user this forge ever stores is the viewer, so views render the full shape.
type userJSON struct {
	Login     string `json:"login"`
	ID        int    `json:"id"`
	Type      string `json:"type"`
	SiteAdmin bool   `json:"site_admin"`
}

// serviceUser is the shared `USER` constant service.ts exports.
var serviceUser = userJSON{Login: viewer.Login, ID: viewer.ID, Type: "User", SiteAdmin: false}

// The complete user object, from a stored userRef (always the viewer here).
func renderUser(u userRef) userJSON {
	return userJSON{Login: u.Login, ID: u.ID, Type: "User", SiteAdmin: false}
}

// ---------------------------------------------------------------------------
// JSON views

func issueState(iss issue) string { return iss.State }

type labelView struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
	Default     bool   `json:"default"`
}

func labelJSON(l label) labelView {
	return labelView{ID: l.ID, Name: l.Name, Color: l.Color, Description: l.Description, Default: false}
}

type issueView struct {
	ID        int         `json:"id"`
	NodeID    string      `json:"node_id"`
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

func issueJSON(repo *Repo, iss issue) issueView {
	labels := make([]labelView, 0, len(iss.Labels))
	for _, l := range iss.Labels {
		labels = append(labels, labelJSON(l))
	}
	return issueView{
		ID:        iss.ID,
		NodeID:    "I_" + strconv.Itoa(iss.ID),
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

type commentView struct {
	ID        int      `json:"id"`
	Body      string   `json:"body"`
	User      userJSON `json:"user"`
	CreatedAt string   `json:"created_at"`
	UpdatedAt string   `json:"updated_at"`
}

func commentJSON(comment issueComment) commentView {
	return commentView{
		ID:        comment.ID,
		Body:      comment.Body,
		User:      renderUser(comment.User),
		CreatedAt: comment.CreatedAt,
		UpdatedAt: comment.CreatedAt,
	}
}

type headBaseView struct {
	Ref   string   `json:"ref"`
	SHA   string   `json:"sha"`
	Label string   `json:"label"`
	User  userJSON `json:"user"`
}

type pullView struct {
	ID            int         `json:"id"`
	NodeID        string      `json:"node_id"`
	Number        int         `json:"number"`
	State         string      `json:"state"`
	Title         string      `json:"title"`
	Body          string      `json:"body"`
	User          userJSON    `json:"user"`
	Head          headBaseView `json:"head"`
	Base          headBaseView `json:"base"`
	MergeCommitSHA *string    `json:"merge_commit_sha"`
	Merged        bool        `json:"merged"`
	MergedAt      *string     `json:"merged_at"`
	Mergeable     bool        `json:"mergeable"`
	CreatedAt     string      `json:"created_at"`
	UpdatedAt     string      `json:"updated_at"`
	HTMLURL       string      `json:"html_url"`
}

func pullJSON(repo *Repo, p pull) pullView {
	return pullView{
		ID:     p.ID,
		NodeID: "PR_" + strconv.Itoa(p.ID),
		Number: p.Number,
		State:  p.State,
		Title:  p.Title,
		Body:   p.Body,
		User:   renderUser(p.User),
		Head: headBaseView{Ref: p.HeadRef, SHA: p.HeadSha, Label: repo.Owner + ":" + p.HeadRef, User: serviceUser},
		Base: headBaseView{Ref: p.BaseRef, SHA: p.BaseSha, Label: repo.Owner + ":" + p.BaseRef, User: serviceUser},
		MergeCommitSHA: p.MergeCommitSha,
		Merged:        p.Merged,
		MergedAt:      p.MergedAt,
		Mergeable:     !p.Merged,
		CreatedAt:     p.CreatedAt,
		UpdatedAt:     p.UpdatedAt,
		HTMLURL:       repo.Owner + "/" + repo.Name + "/pull/" + strconv.Itoa(p.Number),
	}
}

type repoOwnerView struct {
	Login string `json:"login"`
	ID    int    `json:"id"`
	Type  string `json:"type"`
}

type repoView struct {
	ID              int           `json:"id"`
	NodeID          string        `json:"node_id"`
	Name            string        `json:"name"`
	FullName        string        `json:"full_name"`
	Owner           repoOwnerView `json:"owner"`
	Private         bool          `json:"private"`
	Description     string        `json:"description"`
	Fork            bool          `json:"fork"`
	DefaultBranch   string        `json:"default_branch"`
	CreatedAt       string        `json:"created_at"`
	UpdatedAt       string        `json:"updated_at"`
	PushedAt        string        `json:"pushed_at"`
	Size            int           `json:"size"`
	OpenIssuesCount int           `json:"open_issues_count"`
	HasIssues       bool          `json:"has_issues"`
	HTMLURL         string        `json:"html_url"`
}

func repoJSON(repo *Repo) repoView {
	ownerID := 2000
	if repo.Owner == viewer.Login {
		ownerID = viewer.ID
	}
	open := 0
	for _, iss := range repo.Issues {
		if iss.State == "open" {
			open++
		}
	}
	return repoView{
		ID:              repo.ID,
		NodeID:          "R_" + strconv.Itoa(repo.ID),
		Name:            repo.Name,
		FullName:        repo.Owner + "/" + repo.Name,
		Owner:           repoOwnerView{Login: repo.Owner, ID: ownerID, Type: "User"},
		Private:         repo.Private,
		Description:     repo.Description,
		Fork:            false,
		DefaultBranch:   repo.DefaultBranch,
		CreatedAt:       repo.CreatedAt,
		UpdatedAt:       repo.UpdatedAt,
		PushedAt:        repo.UpdatedAt,
		Size:            0,
		OpenIssuesCount: open,
		HasIssues:       true,
		HTMLURL:         repo.Owner + "/" + repo.Name,
	}
}

type branchCommitView struct {
	SHA string `json:"sha"`
	URL string `json:"url"`
}

type branchView struct {
	Name      string           `json:"name"`
	Commit    branchCommitView `json:"commit"`
	Protected bool             `json:"protected"`
}

func branchJSON(repo *Repo, name, sha string) branchView {
	return branchView{
		Name:      name,
		Commit:    branchCommitView{SHA: sha, URL: repo.Owner + "/" + repo.Name + "/commits/" + sha},
		Protected: false,
	}
}

type commitAuthorView struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Date  string `json:"date"`
}

type commitTreeView struct {
	SHA string `json:"sha"`
}

type commitPartView struct {
	Message   string          `json:"message"`
	Author    commitAuthorView `json:"author"`
	Committer commitAuthorView `json:"committer"`
	Tree      commitTreeView   `json:"tree"`
}

type parentView struct {
	SHA string `json:"sha"`
}

type commitView struct {
	SHA     string         `json:"sha"`
	NodeID  string         `json:"node_id"`
	Commit  commitPartView `json:"commit"`
	Parents []parentView   `json:"parents"`
}

func commitJSON(repo *Repo, sha string) commitView {
	obj, ok := repo.Objects[sha]
	var fields commitFields
	if !ok {
		fields = commitFields{Tree: "", Parents: []string{}, Author: "", Committer: "", Message: ""}
	} else {
		fields = parseCommit(obj.Data)
	}
	parents := make([]parentView, 0, len(fields.Parents))
	for _, parent := range fields.Parents {
		parents = append(parents, parentView{SHA: parent})
	}
	author := commitAuthorView{Name: "Ada Iversen", Email: viewer.Email, Date: repo.CreatedAt}
	return commitView{
		SHA:     sha,
		NodeID:  sha,
		Commit:  commitPartView{Message: fields.Message, Author: author, Committer: author, Tree: commitTreeView{SHA: fields.Tree}},
		Parents: parents,
	}
}

// ---------------------------------------------------------------------------
// labels

func findLabel(repo *Repo, name string) (label, bool) {
	for _, l := range repo.Labels {
		if l.Name == name {
			return l, true
		}
	}
	return label{}, false
}

func addLabel(repo *Repo, name, color, description string) label {
	col := color
	if col == "" {
		col = "ededed"
	}
	l := label{ID: repo.NextLabel, Name: name, Color: col, Description: description}
	repo.NextLabel += 1
	repo.Labels = append(repo.Labels, l)
	return l
}

// ---------------------------------------------------------------------------
// issues and comments

type issueInput struct {
	Title  string
	Body   *string
	Labels []string
}

// Create an issue and append it to the repository. Returns an error carrying the exact message a
// surface should answer when the input is invalid.
func createIssueRecord(repo *Repo, input issueInput) (issue, error) {
	if input.Title == "" {
		return issue{}, errors.New("Issue title is required")
	}
	labels := []label{}
	for _, name := range input.Labels {
		l, ok := findLabel(repo, name)
		if !ok {
			return issue{}, errors.New("Validation Failed: label does not exist")
		}
		labels = append(labels, l)
	}
	when := stamp(repo)
	body := ""
	if input.Body != nil {
		body = *input.Body
	}
	iss := issue{
		ID:        100000 + repo.NextIssue*7,
		Number:    repo.NextIssue,
		Title:     input.Title,
		Body:      body,
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
	return iss, nil
}

func addComment(repo *Repo, iss *issue, body string) issueComment {
	comment := issueComment{
		ID:          repo.NextComment,
		IssueNumber: iss.Number,
		Body:        body,
		CreatedAt:   stamp(repo),
		User:        userRef{Login: viewer.Login, ID: viewer.ID},
	}
	repo.NextComment += 1
	repo.Comments = append(repo.Comments, comment)
	return comment
}

func closeIssue(repo *Repo, iss *issue) {
	iss.State = "closed"
	closedAt := stamp(repo)
	iss.ClosedAt = &closedAt
	iss.UpdatedAt = closedAt
}

// ---------------------------------------------------------------------------
// file diffs

// bufferLines splits blob bytes into lines; a nil blob or an empty file has no lines.
func bufferLines(data []byte) []string {
	if data == nil {
		return []string{}
	}
	text := string(data)
	if text == "" {
		return []string{}
	}
	return strings.Split(text, "\n")
}

// lineDelta counts added and deleted lines between two blobs by matching as many lines as possible
// and counting the rest. Exact match only, no textual similarity: deterministic.
func lineDelta(before, after []byte) (additions, deletions int) {
	oldLines := bufferLines(before)
	newLines := bufferLines(after)
	counts := map[string]int{}
	for _, text := range oldLines {
		counts[text]++
	}
	matched := 0
	for _, text := range newLines {
		if available := counts[text]; available > 0 {
			counts[text] = available - 1
			matched++
		}
	}
	return len(newLines) - matched, len(oldLines) - matched
}

type fileDiff struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Changes   int    `json:"changes"`
}

// diffFiles returns the changed files between two commits, in path order.
func diffFiles(repo *Repo, baseSha, headSha string) []fileDiff {
	before := filesAt(repo, baseSha)
	after := filesAt(repo, headSha)
	out := []fileDiff{}
	paths := map[string]bool{}
	for path := range before {
		paths[path] = true
	}
	for path := range after {
		paths[path] = true
	}
	sorted := make([]string, 0, len(paths))
	for path := range paths {
		sorted = append(sorted, path)
	}
	sort.Strings(sorted)
	for _, path := range sorted {
		oldSha, hasOld := before[path]
		newSha, hasNew := after[path]
		if hasOld && hasNew && oldSha == newSha {
			continue
		}
		switch {
		case !hasOld && hasNew:
			add, _ := lineDelta(nil, blobData(repo, newSha))
			out = append(out, fileDiff{Filename: path, Status: "added", Additions: add, Deletions: 0, Changes: add})
		case hasOld && !hasNew:
			_, del := lineDelta(blobData(repo, oldSha), nil)
			out = append(out, fileDiff{Filename: path, Status: "removed", Additions: 0, Deletions: del, Changes: del})
		default:
			add, del := lineDelta(blobData(repo, oldSha), blobData(repo, newSha))
			out = append(out, fileDiff{Filename: path, Status: "modified", Additions: add, Deletions: del, Changes: add + del})
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// pull requests

type pullInput struct {
	Title string
	Body  string
	Head  string
	Base  string
}

// createPullRecord opens a pull request between two branches of the repository (a `owner:head`
// head is treated as this repository's branch). Returns an error with the message a surface
// should answer when the branches are missing or identical.
func createPullRecord(repo *Repo, input pullInput) (pull, error) {
	head := input.Head
	if i := strings.Index(head, ":"); i >= 0 {
		head = head[i+1:]
	}
	base := input.Base
	if base == "" {
		base = repo.DefaultBranch
	}
	headSha, hasHead := repo.Refs[branchRef(head)]
	baseSha, hasBase := repo.Refs[branchRef(base)]
	if !hasHead || !hasBase {
		return pull{}, errors.New("Validation Failed: head or base branch does not exist")
	}
	if head == base {
		return pull{}, errors.New("Validation Failed: head and base are the same branch")
	}
	when := stamp(repo)
	p := pull{
		ID:             200000 + repo.NextPull*3,
		Number:         repo.NextPull,
		Title:          input.Title,
		Body:           input.Body,
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
	return p, nil
}

func findPull(repo *Repo, number int) (*pull, bool) {
	for i := range repo.Pulls {
		if repo.Pulls[i].Number == number {
			return &repo.Pulls[i], true
		}
	}
	return nil, false
}

var errNotMergeable = errors.New("not-mergeable")
var errNoBase = errors.New("no-base")

// mergePullMessage builds the squash commit message exactly as service.ts does.
func mergePullMessage(p *pull) string {
	if p.Title == "" {
		return fmt.Sprintf("Merge pull request #%d", p.Number)
	}
	return strings.TrimSpace(p.Title + "\n\n" + p.Body)
}

// mergePullRecord squash-merges a pull request onto its base branch: its files are replayed as one
// commit on top of the base tip. Returns the new commit id, or an error when the pull is already
// merged, its base branch is gone, or the base moved in a way that touches the same files.
func mergePullRecord(repo *Repo, p *pull) (string, error) {
	if p.Merged {
		return "", errNotMergeable
	}
	baseTip := refSha(repo, branchRef(p.BaseRef))
	if baseTip == "" {
		return "", errNoBase
	}
	headTip := refSha(repo, branchRef(p.HeadRef))
	if headTip == "" {
		headTip = p.HeadSha
	}
	baseCommit := peelShallow(repo.Objects, p.BaseSha)
	headCommit := peelShallow(repo.Objects, headTip)
	currentBase := peelShallow(repo.Objects, baseTip)
	if currentBase != baseCommit {
		pullPaths := map[string]bool{}
		for _, file := range diffFiles(repo, baseCommit, headCommit) {
			pullPaths[file.Filename] = true
		}
		for _, file := range diffFiles(repo, baseCommit, currentBase) {
			if pullPaths[file.Filename] {
				return "", errNotMergeable
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
			if content == nil {
				content = []byte{}
			}
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
	return commit, nil
}

// findRepo looks one repository up in the store (the spelling later surfaces use).
func findRepo(store *Store, owner, name string) (*Repo, bool) {
	return getRepo(store, owner, name)
}