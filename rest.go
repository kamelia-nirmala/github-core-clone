// REST v3 surface: repositories, contents/commits/refs, issues/pulls.
// Ported from rest-repos.ts, rest-contents.ts, rest-issues.ts — same routes,
// same gates order, same error messages, same clock-tick behaviour.
package main

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Response is what a handler returns; the server serializes it.
type Response struct {
	Status      int
	ContentType string
	Body        []byte
	Extra       map[string]string
	// Request-id handling: NoRequestID skips x-github-request-id entirely
	// (git + MCP replies); FixedReqID forces a literal value (the 204 quirk).
	NoRequestID    bool
	FixedReqID string
}

// RestCtx is everything a REST handler needs for one request.
type RestCtx struct {
	Method        string
	Segments      []string // below /api/v3
	Query         ParsedURL
	Body          any
	BodyMalformed bool
	Authed        bool
	Host          string
	Forge         *Forge
	ReqIDGen      *RequestIDGen
}

func jsonResp(status int, payload any, extra map[string]string) Response {
	return Response{Status: status, ContentType: "application/json", Body: marshalJSONLine(payload), Extra: extra}
}

func rawJSONResp(status int, text string, extra map[string]string) Response {
	return Response{Status: status, ContentType: "application/json", Body: []byte(text), Extra: extra}
}

func errResp(status int, message string) Response {
	return jsonResp(status, map[string]any{"message": message}, nil)
}

func deny(status int, message string) Response { return errResp(status, message) }

// bodyAsRecord: the parsed body as a plain object, or {} when it is not one.
func bodyAsRecord(ctx *RestCtx) map[string]any {
	if m, ok := ctx.Body.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func objectFields(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

func queryIntOr(raw string, fallback int) int {
	if raw == "" {
		return fallback
	}
	// Number(raw): rejects trailing junk and non-integers, like JS.
	f, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || f != math.Trunc(f) {
		return fallback
	}
	return int(f)
}

// parseNumberSegment: a path segment as a non-negative safe integer, else -1.
func parseNumberSegment(raw string) int {
	if raw == "" {
		return -1
	}
	for _, c := range raw {
		if c < '0' || c > '9' {
			return -1
		}
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return -1
	}
	return n
}

var validRepoName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
var hexSHA40 = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

// ---- repo payload shapes ----

type OwnerRef struct {
	Login string `json:"login"`
	ID    int    `json:"id"`
	Type  string `json:"type"`
}

type RepoPayload struct {
	ID            int       `json:"id"`
	Name          string    `json:"name"`
	FullName      string    `json:"full_name"`
	Owner         OwnerRef  `json:"owner"`
	Private       bool      `json:"private"`
	Description   *string   `json:"description"`
	DefaultBranch string    `json:"default_branch"`
	HTMLURL       string    `json:"html_url"`
	CloneURL      string    `json:"clone_url"`
	CreatedAt     string    `json:"created_at"`
	UpdatedAt     string    `json:"updated_at"`
	PushedAt      string    `json:"pushed_at"`
	HasIssues     bool      `json:"has_issues"`
	HasProjects   bool      `json:"has_projects"`
	HasWiki       bool      `json:"has_wiki"`
}

func repoPayload(repo *Repo) RepoPayload {
	var desc *string
	if repo.Description != "" {
		d := repo.Description
		desc = &d
	}
	return RepoPayload{
		ID: repo.ID, Name: repo.Name, FullName: repo.FullName,
		Owner: OwnerRef{Login: Viewer.Login, ID: Viewer.ID, Type: Viewer.Type},
		Private: repo.Private, Description: desc, DefaultBranch: repo.DefaultBranch,
		HTMLURL: "https://example.invalid/" + ViewerLogin + "/" + repo.Name,
		CloneURL: "https://example.invalid/" + ViewerLogin + "/" + repo.Name + ".git",
		CreatedAt: repo.CreatedAt, UpdatedAt: repo.UpdatedAt, PushedAt: repo.UpdatedAt,
		HasIssues: true, HasProjects: false, HasWiki: false,
	}
}

func pageParams(query ParsedURL) (perPage, page int) {
	perPage = 30
	if raw := query.Get("per_page"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			perPage = n
		}
	}
	if perPage < 1 {
		perPage = 1
	}
	if perPage > 100 {
		perPage = 100
	}
	page = 1
	if raw := query.Get("page"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			page = n
		}
	}
	if page < 1 {
		page = 1
	}
	return perPage, page
}

// repoRoutes: /api/v3 root, /meta, /user, /user/repos, /repos/{owner}/{repo}...
// Returns (handled, response). handled=false means "not mine".
func repoRoutes(ctx *RestCtx) (bool, Response) {
	if !ctx.Authed {
		return true, errResp(401, "Requires authentication")
	}
	if ctx.BodyMalformed {
		return true, errResp(400, "Invalid JSON body")
	}
	segs := ctx.Segments
	// API root
	if len(segs) == 0 || (len(segs) == 1 && segs[0] == "") {
		return true, jsonResp(200, map[string]any{
			"current_user_url": "/api/v3/user",
			"rate_limit_url":   "/api/v3/rate_limit",
			"user_url":         "/api/v3/users/{user}",
		}, map[string]string{"x-oauth-scopes": "repo, read:org"})
	}
	if len(segs) == 1 && segs[0] == "meta" {
		return true, jsonResp(200, map[string]any{
			"installed_version":                 "1.0.0",
			"verifiable_password_authentication": false,
		}, nil)
	}
	if len(segs) == 1 && segs[0] == "user" {
		return true, jsonResp(200, Viewer, nil)
	}
	if len(segs) == 2 && segs[0] == "user" && segs[1] == "repos" {
		if ctx.Method == "GET" {
			repos := ctx.Forge.ViewerRepos()
			payloads := make([]RepoPayload, 0, len(repos))
			for _, r := range repos {
				payloads = append(payloads, repoPayload(r))
			}
			perPage, page := pageParams(ctx.Query)
			start := (page - 1) * perPage
			end := start + perPage
			if start > len(payloads) {
				start = len(payloads)
			}
			if end > len(payloads) {
				end = len(payloads)
			}
			pageItems := payloads[start:end]
			var headers map[string]string
			if start+perPage < len(payloads) {
				headers = map[string]string{
					"Link": fmt.Sprintf("<http://%s/api/v3/user/repos?per_page=%d&page=%d>; rel=\"next\"", ctx.Host, perPage, page+1),
				}
			}
			return true, jsonResp(200, pageItems, headers)
		}
		if ctx.Method == "POST" {
			fields := objectFields(ctx.Body)
			name := ""
			if fields != nil {
				if s, ok := fields["name"].(string); ok {
					name = s
				}
			}
			if name == "" {
				return true, errResp(422, "Repository name is required")
			}
			if !validRepoName.MatchString(name) {
				return true, errResp(422, "Invalid repository name")
			}
			if ctx.Forge.GetRepo(ViewerLogin, name) != nil {
				return true, errResp(422, "Repository already exists")
			}
			description := ""
			if fields != nil {
				if s, ok := fields["description"].(string); ok {
					description = s
				}
			}
			isPrivate := false
			if fields != nil {
				if b, ok := fields["private"].(bool); ok && b {
					isPrivate = true
				}
			}
			defaultBranch := "main"
			if fields != nil {
				if s, ok := fields["default_branch"].(string); ok && s != "" {
					defaultBranch = s
				}
			}
			repo := ctx.Forge.CreateRepo(name, description, isPrivate, defaultBranch)
			return true, jsonResp(201, repoPayload(repo), nil)
		}
		return false, Response{}
	}
	if len(segs) < 1 || segs[0] != "repos" {
		return false, Response{}
	}
	if len(segs) < 3 {
		return false, Response{}
	}
	owner, name := segs[1], segs[2]
	if owner != ViewerLogin {
		return true, errResp(404, "Not Found")
	}
	repo := ctx.Forge.GetRepo(owner, name)
	rest := segs[3:]
	method := ctx.Method
	if len(rest) == 0 {
		if method == "GET" {
			if repo == nil {
				return true, errResp(404, "Not Found")
			}
			return true, jsonResp(200, repoPayload(repo), nil)
		}
		if method == "PATCH" {
			if repo == nil {
				return true, errResp(404, "Not Found")
			}
			fields := objectFields(ctx.Body)
			if fields != nil {
				if s, ok := fields["description"].(string); ok {
					repo.Description = s
				}
			}
			return true, jsonResp(200, repoPayload(repo), nil)
		}
		if method == "DELETE" {
			if repo == nil {
				return true, errResp(404, "Not Found")
			}
			ctx.Forge.DeleteRepo(owner, name)
			return true, Response{Status: 204, FixedReqID: "req-0"}
		}
		return false, Response{}
	}
	if len(rest) == 1 && rest[0] == "branches" {
		if method != "GET" {
			return false, Response{}
		}
		if repo == nil {
			return true, errResp(404, "Not Found")
		}
		return true, jsonResp(200, listBranches(repo), nil)
	}
	if len(rest) == 2 && rest[0] == "branches" {
		if method != "GET" {
			return false, Response{}
		}
		if repo == nil {
			return true, errResp(404, "Not Found")
		}
		branch := rest[1]
		oid, ok := repo.Refs["refs/heads/"+branch]
		if !ok {
			return true, errResp(404, "Not Found")
		}
		return true, jsonResp(200, map[string]any{
			"name": branch, "commit": map[string]any{"sha": oid},
		}, nil)
	}
	if len(rest) == 2 && rest[0] == "git" && rest[1] == "refs" {
		if method != "POST" {
			return false, Response{}
		}
		if repo == nil {
			return true, errResp(404, "Not Found")
		}
		fields := objectFields(ctx.Body)
		ref := ""
		if fields != nil {
			if s, ok := fields["ref"].(string); ok {
				ref = s
			}
		}
		if !strings.HasPrefix(ref, "refs/") {
			return true, errResp(422, "Invalid ref")
		}
		sha := ""
		if fields != nil {
			if s, ok := fields["sha"].(string); ok {
				sha = s
			}
		}
		if !hexSHA40.MatchString(sha) {
			return true, errResp(422, "Invalid sha")
		}
		if _, ok := repo.Objects[sha]; !ok {
			return true, errResp(422, "Invalid sha")
		}
		if _, ok := repo.Refs[ref]; ok {
			return true, errResp(422, "Reference already exists")
		}
		repo.Refs[ref] = sha
		return true, jsonResp(201, map[string]any{
			"ref": ref, "object": map[string]any{"sha": sha, "type": "commit"},
		}, nil)
	}
	if len(rest) == 4 && rest[0] == "git" && rest[1] == "refs" && (rest[2] == "heads" || rest[2] == "tags") {
		if method != "GET" {
			return false, Response{}
		}
		if repo == nil {
			return true, errResp(404, "Not Found")
		}
		kind, shortName := rest[2], rest[3]
		var ref string
		if kind == "heads" {
			ref = "refs/heads/" + shortName
		} else {
			ref = "refs/tags/" + shortName
		}
		oid, ok := repo.Refs[ref]
		if !ok {
			return true, errResp(404, "Not Found")
		}
		if kind == "heads" {
			return true, jsonResp(200, map[string]any{
				"ref": ref, "object": map[string]any{"sha": oid, "type": "commit"},
			}, nil)
		}
		typ := "commit"
		if obj := repo.Objects[oid]; obj != nil && obj.Type == "tag" {
			typ = "tag"
		}
		return true, jsonResp(200, map[string]any{
			"ref": ref, "object": map[string]any{"sha": oid, "type": typ},
		}, nil)
	}
	return false, Response{}
}

type branchView struct {
	Name      string `json:"name"`
	Commit    struct {
		Sha string `json:"sha"`
	} `json:"commit"`
	Protected bool `json:"protected"`
}

func listBranches(repo *Repo) []branchView {
	found := []branchView{}
	for ref, oid := range repo.Refs {
		if !strings.HasPrefix(ref, "refs/heads/") {
			continue
		}
		b := branchView{Name: ref[len("refs/heads/"):], Protected: false}
		b.Commit.Sha = oid
		found = append(found, b)
	}
	sort.Slice(found, func(i, j int) bool { return found[i].Name < found[j].Name })
	return found
}

// ---- content routes (commits, contents, refs) ----
// Matches rest-contents.ts dispatch exactly: shape first, then owner/repo.

func contentRoutes(ctx *RestCtx) (bool, Response) {
	if !ctx.Authed {
		return true, errResp(401, "Requires authentication")
	}
	if ctx.BodyMalformed {
		return true, errResp(400, "Invalid JSON body")
	}
	segs := ctx.Segments
	if len(segs) < 4 || segs[0] != "repos" {
		return false, Response{}
	}
	method := ctx.Method
	isCommits := len(segs) == 4 && segs[3] == "commits"
	isContents := segs[3] == "contents"
	isRefs := len(segs) == 7 && segs[3] == "git" && segs[4] == "refs" && (segs[5] == "heads" || segs[5] == "tags")
	if isCommits && method != "GET" {
		return false, Response{}
	}
	if isContents && method != "GET" && method != "PUT" && method != "DELETE" {
		return false, Response{}
	}
	if isRefs {
		readable := method == "GET"
		deletableBranch := method == "DELETE" && segs[5] == "heads"
		if !readable && !deletableBranch {
			return false, Response{}
		}
	}
	if !isCommits && !isContents && !isRefs {
		return false, Response{}
	}
	owner, name := segs[1], segs[2]
	if owner != ViewerLogin {
		return true, errResp(404, "Not Found")
	}
	repo := ctx.Forge.GetRepo(owner, name)
	if repo == nil {
		return true, errResp(404, "Not Found")
	}
	if isCommits {
		return true, handleCommits(ctx, repo)
	}
	if isContents {
		return handleContents(ctx, repo, owner, name, segs[4:])
	}
	kind := "heads"
	if segs[5] == "tags" {
		kind = "tags"
	}
	return handleRefs(ctx, repo, kind, segs[6])
}

type refObject struct {
	Sha  string `json:"sha"`
	Type string `json:"type"`
}

type refView struct {
	Ref    string    `json:"ref"`
	Object refObject `json:"object"`
}

// handleRefs: GET one ref / DELETE a branch. Matches TS handleRefs.
func handleRefs(ctx *RestCtx, repo *Repo, kind, branch string) (bool, Response) {
	full := "refs/" + kind + "/" + branch
	if ctx.Method == "DELETE" {
		removed := ctx.Forge.DeleteRef(repo, full)
		if !removed {
			return true, errResp(404, "Not Found")
		}
		return true, Response{Status: 204, FixedReqID: "req-0"}
	}
	oid := ctx.Forge.RefOid(repo, full)
	if oid == "" || oid == ZeroOID {
		return true, errResp(404, "Not Found")
	}
	typ := "commit"
	if kind == "tags" {
		if obj := repo.Objects[oid]; obj != nil && obj.Type == "tag" {
			typ = "tag"
		}
	}
	return true, jsonResp(200, refView{Ref: full, Object: refObject{oid, typ}}, nil)
}

// resolveTip: which commit a request is talking about. No ref means the
// default branch's tip. A 40-hex value is tried as a commit id first, then
// as a branch name. Anything that fails resolves to "" (caller answers 404).
func resolveTip(f *Forge, repo *Repo, ref string) string {
	if ref == "" {
		oid := f.RefOid(repo, "refs/heads/"+repo.DefaultBranch)
		if oid == "" || oid == ZeroOID {
			return ""
		}
		obj := repo.Objects[oid]
		if obj == nil || obj.Type != "commit" {
			return ""
		}
		return oid
	}
	if len(ref) == 40 && hexChars(ref) {
		if obj, ok := repo.Objects[ref]; ok && obj.Type == "commit" {
			return ref
		}
		// fall through to the branch lookup, like TS
	}
	oid := f.RefOid(repo, "refs/heads/"+ref)
	if oid != "" && oid != ZeroOID {
		if obj, ok := repo.Objects[oid]; ok && obj.Type == "commit" {
			return oid
		}
	}
	return ""
}

func hexChars(s string) bool {
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return len(s) > 0
}

// ---- commits ----

type commitAuthor struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Date  string `json:"date"`
}

type commitInner struct {
	Message   string       `json:"message"`
	Author    commitAuthor `json:"author"`
	Committer commitAuthor `json:"committer"`
}

type commitParent struct {
	Sha string `json:"sha"`
}

type CommitView struct {
	Sha       string         `json:"sha"`
	Commit    commitInner    `json:"commit"`
	Author    UserView       `json:"author"`
	Committer UserView       `json:"committer"`
	Parents   []commitParent `json:"parents"`
}

func commitPayload(repo *Repo, oid string) CommitView {
	obj := repo.Objects[oid]
	meta := parseCommit(obj.Data)
	when := formatTime(meta.Epoch)
	parents := make([]commitParent, 0, len(meta.Parents))
	for _, p := range meta.Parents {
		parents = append(parents, commitParent{Sha: p})
	}
	who := commitAuthor{Name: Viewer.Name, Email: Viewer.Email, Date: when}
	return CommitView{
		Sha: oid,
		Commit: commitInner{
			Message:   meta.Message,
			Author:    who,
			Committer: who,
		},
		Author: Viewer, Committer: Viewer, Parents: parents,
	}
}

// handleCommits: history walked depth-first from the tip (stack, parents
// pushed in reverse so the first parent is visited first). An unborn
// default branch answers with an empty list when no sha was asked for,
// and 404 when one was.
func handleCommits(ctx *RestCtx, repo *Repo) Response {
	rawSha := ctx.Query.Get("sha")
	sha := ""
	if rawSha != "" {
		sha = rawSha
	}
	tip := resolveTip(ctx.Forge, repo, sha)
	if tip == "" {
		if sha == "" {
			return jsonResp(200, []CommitView{}, nil)
		}
		return errResp(404, "Not Found")
	}
	seen := map[string]bool{}
	stack := []string{tip}
	out := []CommitView{}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj := repo.Objects[oid]
		if obj == nil || obj.Type != "commit" {
			continue
		}
		meta := parseCommit(obj.Data)
		out = append(out, commitPayload(repo, oid))
		for i := len(meta.Parents) - 1; i >= 0; i-- {
			stack = append(stack, meta.Parents[i])
		}
	}
	return jsonResp(200, out, nil)
}

// ---- contents ----

// mintCommit builds and stores a commit object on top of a tree, with the
// given parents and message, stamped from the logical clock (one tick).
func mintCommit(f *Forge, repo *Repo, treeOID string, parents []string, message string) string {
	epoch := f.Stamp()
	ident := Viewer.Name + " <" + Viewer.Email + "> " + strconv.FormatInt(epoch, 10) + " +0000"
	lines := []string{"tree " + treeOID}
	for _, p := range parents {
		lines = append(lines, "parent "+p)
	}
	lines = append(lines, "author "+ident, "committer "+ident, "", message)
	text := strings.Join(lines, "\n") + "\n"
	return f.PutObject(repo, "commit", []byte(text))
}

// treeOIDAtPath walks down a tree following one path segment per level and
// returns the tree id of the directory the whole path names, or "" when any
// level is missing or is a file.
func treeOIDAtPath(repo *Repo, root string, parts []string) string {
	current := root
	for _, part := range parts {
		obj := repo.Objects[current]
		if obj == nil || obj.Type != "tree" {
			return ""
		}
		var found *namedTreeEntry
		entries := parseTree(obj.Data)
		for i := range entries {
			if entries[i].Name == part {
				found = &entries[i]
				break
			}
		}
		if found == nil {
			return ""
		}
		if found.Mode != "40000" && found.Mode != "040000" {
			return ""
		}
		current = found.OID
	}
	return current
}

// listDirectory: one directory of a tree, entries sorted by name. Matches
// TS listDirectory (relative url, no download_url).
func listDirectory(repo *Repo, treeOID, prefix, owner, name string) []map[string]any {
	obj := repo.Objects[treeOID]
	if obj == nil || obj.Type != "tree" {
		return []map[string]any{}
	}
	entries := parseTree(obj.Data)
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		isDir := e.Mode == "40000" || e.Mode == "040000"
		full := e.Name
		if prefix != "" {
			full = prefix + "/" + e.Name
		}
		size := 0
		if !isDir {
			if blob := repo.Objects[e.OID]; blob != nil && blob.Type == "blob" {
				size = len(blob.Data)
			}
		}
		typ := "file"
		if isDir {
			typ = "dir"
		}
		out = append(out, map[string]any{
			"type": typ,
			"name": e.Name,
			"path": full,
			"sha":  e.OID,
			"size": size,
			"url":  "/api/v3/repos/" + owner + "/" + name + "/contents/" + full,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["name"].(string) < out[j]["name"].(string) })
	return out
}

// fileEntry: one file's contents payload. The base64 content carries a
// trailing newline and the url is relative, exactly like TS.
func fileEntry(repo *Repo, path, owner, name string, oid string, data []byte) map[string]any {
	return map[string]any{
		"type":     "file",
		"encoding": "base64",
		"content":  b64encode(data) + "\n",
		"sha":      oid,
		"size":     len(data),
		"name":     pathBase(path),
		"path":     path,
		"url":      "/api/v3/repos/" + owner + "/" + name + "/contents/" + path,
	}
}

func pathBase(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

func handleContents(ctx *RestCtx, repo *Repo, owner, name string, pathParts []string) (bool, Response) {
	path := strings.Join(pathParts, "/")
	method := ctx.Method

	// ---- Reading: a file, a directory, or the repository root. ----
	if method == "GET" {
		rawRef := ctx.Query.Get("ref")
		ref := ""
		if rawRef != "" {
			ref = rawRef
		}
		tip := resolveTip(ctx.Forge, repo, ref)
		if tip == "" {
			return true, errResp(404, "Not Found")
		}
		treeOID := ctx.Forge.CommitTree(repo, tip)
		if treeOID == "" {
			return true, errResp(404, "Not Found")
		}
		if len(pathParts) == 0 {
			return true, jsonResp(200, listDirectory(repo, treeOID, "", owner, name), nil)
		}
		flat := ctx.Forge.FlattenTree(repo, treeOID, "")
		if entry, ok := flat[path]; ok {
			var data []byte
			if blob := repo.Objects[entry.OID]; blob != nil && blob.Type == "blob" {
				data = blob.Data
			}
			return true, jsonResp(200, fileEntry(repo, path, owner, name, entry.OID, data), nil)
		}
		isDir := false
		for key := range flat {
			if strings.HasPrefix(key, path+"/") {
				isDir = true
				break
			}
		}
		if isDir {
			dirTreeOID := treeOIDAtPath(repo, treeOID, pathParts)
			if dirTreeOID != "" {
				return true, jsonResp(200, listDirectory(repo, dirTreeOID, path, owner, name), nil)
			}
		}
		return true, errResp(404, "Not Found")
	}

	// ---- Writing and deleting share the branch and its tip. ----
	fields := bodyAsRecord(ctx)
	branch := repo.DefaultBranch
	if s, ok := fields["branch"].(string); ok && s != "" {
		branch = s
	}
	tipRaw := ctx.Forge.RefOid(repo, "refs/heads/"+branch)
	tip := ""
	if tipRaw != "" && tipRaw != ZeroOID {
		tip = tipRaw
	}

	// ---- Writing: create a file, or replace one that is already there. ----
	if method == "PUT" {
		content, ok := fields["content"].(string)
		if !ok {
			return true, errResp(422, "content is required")
		}
		raw := b64decode(content) // lenient, like Node's Buffer.from
		message := "Update " + path
		if s, ok := fields["message"].(string); ok {
			message = s
		}
		blobOID := HashObject("blob", raw)
		flat := FlatTree{}
		if tip != "" {
			flat = ctx.Forge.FlattenTree(repo, ctx.Forge.CommitTree(repo, tip), "")
		}
		_, existed := flat[path]
		if shaVal, present := fields["sha"]; present {
			if shaStr, isStr := shaVal.(string); isStr {
				if !existed {
					return true, errResp(422, "sha does not match")
				}
				if flat[path].OID != shaStr {
					return true, errResp(409, "sha does not match")
				}
			}
		}
		flat[path] = FlatEntry{OID: blobOID, Mode: "100644"}
		ctx.Forge.PutObject(repo, "blob", raw)
		newTreeOID := ctx.Forge.BuildTreeFromFlat(repo, flat)
		parents := []string{}
		if tip != "" {
			parents = []string{tip}
		}
		commitOID := mintCommit(ctx.Forge, repo, newTreeOID, parents, message)
		ctx.Forge.SetRef(repo, "refs/heads/"+branch, commitOID)
		repo.UpdatedAt = ctx.Forge.NowIso()
		status := 201
		if existed {
			status = 200
		}
		return true, jsonResp(status, map[string]any{
			"content": map[string]any{
				"name": pathBase(path), "path": path, "sha": blobOID,
				"size": len(raw), "type": "file", "encoding": "base64",
			},
			"commit": map[string]any{"sha": commitOID, "message": message},
		}, nil)
	}

	// ---- Deleting: the file must exist, and the caller must name its id. ----
	shaVal, ok := fields["sha"].(string)
	if !ok {
		return true, errResp(422, "sha is required")
	}
	if tip == "" {
		return true, errResp(404, "Not Found")
	}
	message := "Delete " + path
	if s, ok := fields["message"].(string); ok {
		message = s
	}
	flat := ctx.Forge.FlattenTree(repo, ctx.Forge.CommitTree(repo, tip), "")
	existing, ok := flat[path]
	if !ok {
		return true, errResp(404, "Not Found")
	}
	if existing.OID != shaVal {
		return true, errResp(409, "sha does not match")
	}
	delete(flat, path)
	newTreeOID := ctx.Forge.BuildTreeFromFlat(repo, flat)
	commitOID := mintCommit(ctx.Forge, repo, newTreeOID, []string{tip}, message)
	ctx.Forge.SetRef(repo, "refs/heads/"+branch, commitOID)
	repo.UpdatedAt = ctx.Forge.NowIso()
	return true, jsonResp(200, map[string]any{
		"commit": map[string]any{"sha": commitOID, "message": message},
	}, nil)
}
