// The forge's data lives here: the single authed user, every repository (its refs, git objects,
// issues, labels, comments and pulls) and the deterministic timestamps the whole trial agrees on.
// One process owns one store for the whole run; nothing here talks to the network. Every list is
// insertion-ordered (slices, plus maps only for lookup); no map order ever reaches a reply.

package main

import (
	"strconv"
	"strings"
	"time"
)

// The one user the trial's bearer token names (the suite's fixed scenario).
var viewer = struct {
	Login string
	ID    int
	Name  string
	Email string
}{"adaiversen", 1000, "ada iversen", "adaiversen@example.invalid"}

// The git identity every commit and tag this forge writes carries.
const identity = "Ada Iversen <adaiversen@example.invalid>"

/**
 * The zero point of the trial's clock: every generated timestamp counts seconds from
 * 2025-01-01T00:00:00Z. Determinism comes from this fixed base plus a per-repository tick,
 * never from the wall clock.
 */
const baseSeconds = 1_735_689_600

type label struct {
	ID          int
	Name        string
	Color       string
	Description string
}

type issueComment struct {
	ID          int
	IssueNumber int
	Body        string
	CreatedAt   string
	User        userRef
}

type issue struct {
	ID        int
	Number    int
	Title     string
	Body      string
	State     string
	Labels    []label
	CreatedAt string
	UpdatedAt string
	ClosedAt  *string
	User      userRef
}

type pull struct {
	ID             int
	Number         int
	Title          string
	Body           string
	State          string
	HeadRef        string
	BaseRef        string
	HeadSha        string
	BaseSha        string
	MergeCommitSha *string
	Merged         bool
	MergedAt       *string
	CreatedAt      string
	UpdatedAt      string
	User           userRef
}

type userRef struct {
	Login string
	ID    int
}

type repoStruct struct {
	ID            int
	Owner         string
	Name          string
	Description   string
	Private       bool
	DefaultBranch string
	CreatedAt     string
	UpdatedAt     string
	Objects       objectTable
	Refs          map[string]string
	Labels        []label
	Issues        []issue
	Comments      []issueComment
	Pulls         []pull
	NextIssue     int
	NextPull      int
	NextComment   int
	NextLabel     int
	Tick          int
}

type storeStruct struct {
	Repos      map[string]*repoStruct
	RepoNames  []string
	NextRepoID int
}

// Repo and Store are the public spellings later surfaces use.
type Repo = repoStruct
type Store = storeStruct

/** A fresh, empty store. Repository ids start at basis 5000. */
func createStore() *Store {
	return &Store{Repos: map[string]*Repo{}, RepoNames: []string{}, NextRepoID: 1}
}

/** The map key that identifies a repository (the same form GitHub's URL uses). */
func repoKey(owner, name string) string {
	return owner + "/" + name
}

func getRepo(store *Store, owner, name string) (*Repo, bool) {
	repo, ok := store.Repos[repoKey(owner, name)]
	return repo, ok
}

/**
 * A deterministic ISO timestamp: the base epoch plus one second per call on this repository.
 * Each read of time advances the repository's clock, so two runs stamp the same sequence.
 */
func stamp(repo *Repo) string {
	repo.Tick += 1
	return time.Unix(baseSeconds+int64(repo.Tick), 0).UTC().Format("2006-01-02T15:04:05Z")
}

func createRepo(store *Store, owner, name, description string, isPrivate bool) *Repo {
	repo := &Repo{
		ID:            5000 + store.NextRepoID,
		Owner:         owner,
		Name:          name,
		Description:   description,
		Private:       isPrivate,
		DefaultBranch: "main",
		Objects:       objectTable{},
		Refs:          map[string]string{},
		Labels:        []label{},
		Issues:        []issue{},
		Comments:      []issueComment{},
		Pulls:         []pull{},
		NextIssue:     1,
		NextPull:      1,
		NextComment:   1,
		NextLabel:     1,
	}
	store.NextRepoID += 1
	repo.CreatedAt = stamp(repo)
	repo.UpdatedAt = repo.CreatedAt
	store.Repos[repoKey(owner, name)] = repo
	store.RepoNames = append(store.RepoNames, repoKey(owner, name))
	return repo
}

/**
 * Remove a repository from the store. Returns true when it existed and was removed.
 * The same repository can be created again, taking a fresh id.
 */
func deleteRepo(store *Store, owner, name string) bool {
	key := repoKey(owner, name)
	if _, ok := store.Repos[key]; !ok {
		return false
	}
	delete(store.Repos, key)
	for i, existing := range store.RepoNames {
		if existing == key {
			store.RepoNames = append(store.RepoNames[:i], store.RepoNames[i+1:]...)
			break
		}
	}
	return true
}

/** The ref name of a branch. */
func branchRef(branch string) string {
	return "refs/heads/" + branch
}

/** The object id a ref currently names, or "" for an unborn ref. */
func refSha(repo *Repo, ref string) string {
	if sha, ok := repo.Refs[ref]; ok {
		return sha
	}
	return ""
}

/** True when `s` is a 40-hex object id. */
func isHex40(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

/**
 * The commit a ref or an object id names ("" means null):
 *   - "" means the default branch's tip ("" while the repository is unborn),
 *   - a refs/ name resolves as it is, falling back to the heads/ form,
 *   - a 40-hex id is peeled through any tags to a commit,
 *   - anything else is read as a branch name.
 */
func resolveCommit(repo *Repo, refOrSha string) string {
	if refOrSha == "" {
		return refSha(repo, branchRef(repo.DefaultBranch))
	}
	if strings.HasPrefix(refOrSha, "refs/") {
		if sha := refSha(repo, refOrSha); sha != "" {
			return sha
		}
		return refSha(repo, branchRef(strings.TrimPrefix(refOrSha, "refs/heads/")))
	}
	if isHex40(refOrSha) {
		return peelShallow(repo.Objects, refOrSha)
	}
	return refSha(repo, branchRef(refOrSha))
}

/** The root tree a commit names, or "" when the id is not a commit here. */
func commitTree(repo *Repo, commitSha string) string {
	obj, ok := repo.Objects[commitSha]
	if !ok || obj.Type != typeCommit {
		return ""
	}
	return parseCommit(obj.Data).Tree
}

/** One file change for `commitChanges`: `content` nil deletes the path. */
type fileChange struct {
	Path    string
	Content []byte
}

/**
 * Apply `changes` on top of `branch`'s current tree and write a commit on that ref. The parent
 * is the branch tip, or nothing for the repository's first commit. Returns the commit id and
 * every blob id written (keyed by path).
 */
func commitChanges(repo *Repo, branch string, changes []fileChange, message string) (string, map[string]string) {
	ref := branchRef(branch)
	parent := refSha(repo, ref)
	var parentTree string
	if parent != "" {
		parentTree = commitTree(repo, parent)
	}
	blobs := map[string]string{}
	tree := parentTree
	for _, change := range changes {
		var parts []string
		for _, part := range strings.Split(change.Path, "/") {
			if part != "" {
				parts = append(parts, part)
			}
		}
		blobSha := ""
		if change.Content != nil {
			blobSha = putObject(repo.Objects, typeBlob, change.Content)
			blobs[change.Path] = blobSha
		}
		tree = treeWith(repo.Objects, tree, parts, blobSha)
	}
	when := stamp(repo)
	commitTreeSha := tree
	if commitTreeSha == "" {
		commitTreeSha = writeTree(repo.Objects, []treeEntry{})
	}
	parents := []string{}
	if parent != "" {
		parents = []string{parent}
	}
	authorLine := identity + " " + strconv.Itoa(baseSeconds+repo.Tick) + " +0000"
	body := encodeCommit(commitFields{
		Tree:      commitTreeSha,
		Parents:   parents,
		Author:    authorLine,
		Committer: authorLine,
		Message:   message,
	})
	commit := putObject(repo.Objects, typeCommit, body)
	repo.Refs[ref] = commit
	repo.UpdatedAt = when
	return commit, blobs
}

/**
 * The blob id at `path` on a ref, or "" (absent). Anything that resolves to a non-blob
 * (a directory, a missing object) is reported as absent.
 */
func blobAt(repo *Repo, refOrSha, path string) string {
	commit := resolveCommit(repo, refOrSha)
	if commit == "" {
		return ""
	}
	var parts []string
	for _, part := range strings.Split(path, "/") {
		if part != "" {
			parts = append(parts, part)
		}
	}
	found, ok := treeLookup(repo.Objects, commitTree(repo, commit), parts)
	if !ok {
		return ""
	}
	obj, ok := repo.Objects[found]
	if !ok || obj.Type == typeBlob {
		return found
	}
	return ""
}

/** Every file path of a commit with its blob id, as a flat map. */
func filesAt(repo *Repo, commitSha string) map[string]string {
	if commitSha == "" {
		return map[string]string{}
	}
	return flattenTree(repo.Objects, commitTree(repo, commitSha), "")
}

/** Blob bytes, or nil when the id does not name a blob. */
func blobData(repo *Repo, sha string) []byte {
	obj, ok := repo.Objects[sha]
	if !ok || obj.Type != typeBlob {
		return nil
	}
	return obj.Data
}

/** Store one object of whatever type (used by the smart-HTTP push path). */
func storeObject(repo *Repo, t objType, data []byte) string {
	return putObject(repo.Objects, t, data)
}