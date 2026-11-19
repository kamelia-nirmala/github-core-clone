// MCP over Streamable HTTP: JSON-RPC 2.0 on POST /mcp.
// Ported from mcp.ts — same tools, same aliases, same clock ticks.
package main

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const mcpSessionID = "session-1"

var mcpSupportedVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}
const mcpLatestVersion = "2026-07-28"
const mcpServerName = "forge"
const mcpServerVersion = "1.0.0"

// ---- tool catalogue ----

type toolProperty struct {
	Type        string `json:"type"`
	Description string `json:"description"`
}

type toolSchema struct {
	Type       string                  `json:"type"`
	Properties map[string]toolProperty `json:"properties"`
	Required   []string                `json:"required"`
}

type toolDef struct {
	Name        string     `json:"name"`
	Description string     `json:"description"`
	InputSchema toolSchema `json:"inputSchema"`
}

type toolSpec struct {
	name        string
	description string
	props       [][3]string // name, type, description
	required    []string
}

var toolSpecs = []toolSpec{
	{"create_repo", "Create a repository owned by the viewer.", [][3]string{
		{"name", "string", "The repository name"},
		{"description", "string", "A short description of the repository"},
		{"private", "boolean", "Whether the repository is private"},
	}, []string{"name"}},
	{"get_repo", "Get a repository by owner and name.", [][3]string{
		{"owner", "string", "The repository owner"},
		{"repo", "string", "The repository name"},
	}, []string{"owner", "repo"}},
	{"create_issue", "Create an issue in a repository.", [][3]string{
		{"owner", "string", "The repository owner"},
		{"repo", "string", "The repository name"},
		{"title", "string", "The issue title"},
		{"body", "string", "The issue body"},
	}, []string{"owner", "repo", "title"}},
	{"list_issues", "List issues in a repository.", [][3]string{
		{"owner", "string", "The repository owner"},
		{"repo", "string", "The repository name"},
		{"state", "string", "open, closed or all"},
	}, []string{"owner", "repo"}},
	{"create_pull_request", "Create a pull request.", [][3]string{
		{"owner", "string", "The repository owner"},
		{"repo", "string", "The repository name"},
		{"title", "string", "The pull request title"},
		{"body", "string", "The pull request body"},
		{"head", "string", "The head branch name"},
		{"base", "string", "The base branch name"},
	}, []string{"owner", "repo", "title", "head", "base"}},
	{"merge_pull_request", "Merge a pull request with a squash merge.", [][3]string{
		{"owner", "string", "The repository owner"},
		{"repo", "string", "The repository name"},
		{"pull_number", "integer", "The pull request number"},
	}, []string{"owner", "repo", "pull_number"}},
}

func buildToolList() []toolDef {
	out := make([]toolDef, 0, len(toolSpecs))
	for _, spec := range toolSpecs {
		props := map[string]toolProperty{}
		for _, p := range spec.props {
			props[p[0]] = toolProperty{Type: p[1], Description: p[2]}
		}
		out = append(out, toolDef{
			Name: spec.name, Description: spec.description,
			InputSchema: toolSchema{Type: "object", Properties: props, Required: spec.required},
		})
	}
	return out
}

// ---- reply writers (session header, no x-github-request-id) ----

func mcpJSON(status int, payload any) Response {
	return Response{
		Status: status, ContentType: "application/json",
		Body: marshalJSON(payload),
		Extra: map[string]string{"mcp-session-id": mcpSessionID},
		NoRequestID: true,
	}
}

func mcpEmpty(status int) Response {
	return Response{
		Status: status,
		Extra:  map[string]string{"mcp-session-id": mcpSessionID},
		NoRequestID: true,
	}
}

func mcpResult(id any, result any) Response {
	return mcpJSON(200, map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func mcpRPCError(id any, code int, message string) Response {
	return mcpJSON(200, map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
}

func mcpProtocolError(status, code int, message string) Response {
	return mcpJSON(status, map[string]any{"jsonrpc": "2.0", "id": nil, "error": map[string]any{"code": code, "message": message}})
}

// ---- argument aliases ----

var mcpAliases = map[string][]string{
	"owner":       {"owner", "owner_login", "org", "user", "login"},
	"repo":        {"repo", "repository", "repository_name", "repo_name"},
	"title":       {"title", "issue_title", "pr_title", "subject"},
	"body":        {"body", "issue_body", "description", "content", "text"},
	"head":        {"head", "head_ref", "head_branch", "source_branch", "from_branch", "branch"},
	"base":        {"base", "base_ref", "base_branch", "target_branch", "into_branch"},
	"pullNumber":  {"pull_number", "pull_request_number", "pr_number", "number"},
	"name":        {"name", "repo", "repository"},
	"description": {"description", "about", "summary"},
	"isPrivate":   {"private", "is_private"},
	"state":       {"state", "status"},
}

func pickString(args map[string]any, keys []string) (string, bool) {
	for _, key := range keys {
		if v, ok := args[key]; ok {
			if s, ok := v.(string); ok {
				return s, true
			}
		}
	}
	return "", false
}

var intStringRe = regexp.MustCompile(`^-?\d+$`)

func pickInteger(args map[string]any, keys []string) (int, bool) {
	for _, key := range keys {
		v, ok := args[key]
		if !ok {
			continue
		}
		switch n := v.(type) {
		case float64:
			if n == float64(int(n)) {
				return int(n), true
			}
		case string:
			t := strings.TrimSpace(n)
			if t != "" && intStringRe.MatchString(t) {
				if i, err := strconv.Atoi(t); err == nil {
					return i, true
				}
			}
		}
	}
	return 0, false
}

func pickBoolean(args map[string]any, keys []string) (bool, bool) {
	for _, key := range keys {
		v, ok := args[key]
		if !ok {
			continue
		}
		switch b := v.(type) {
		case bool:
			return b, true
		case string:
			lowered := strings.ToLower(strings.TrimSpace(b))
			if lowered == "true" {
				return true, true
			}
			if lowered == "false" {
				return false, true
			}
		}
	}
	return false, false
}

func resolveOwnerRepo(args map[string]any) (owner, repo string, okOwner, okRepo bool) {
	owner, okOwner = pickString(args, mcpAliases["owner"])
	repo, okRepo = pickString(args, mcpAliases["repo"])
	if packed, isStr := args["full_name"].(string); isStr && (!okOwner || !okRepo) {
		slash := strings.Index(packed, "/")
		if slash > 0 && slash < len(packed)-1 {
			if !okOwner && packed[:slash] != "" {
				owner, okOwner = packed[:slash], true
			}
			if !okRepo && packed[slash+1:] != "" {
				repo, okRepo = packed[slash+1:], true
			}
		}
	}
	return owner, repo, okOwner, okRepo
}

func isNonEmptyString(v any) bool {
	s, ok := v.(string)
	return ok && strings.TrimSpace(s) != ""
}

// ---- views (MCP repoView keeps description as a string, not null) ----

type mcpRepoView struct {
	ID            int      `json:"id"`
	Name          string   `json:"name"`
	FullName      string   `json:"full_name"`
	Owner         OwnerRef `json:"owner"`
	Private       bool     `json:"private"`
	Description   string   `json:"description"`
	DefaultBranch string   `json:"default_branch"`
	HTMLURL       string   `json:"html_url"`
	CloneURL      string   `json:"clone_url"`
	CreatedAt     string   `json:"created_at"`
	UpdatedAt     string   `json:"updated_at"`
	PushedAt      string   `json:"pushed_at"`
	HasIssues     bool     `json:"has_issues"`
	HasProjects   bool     `json:"has_projects"`
	HasWiki       bool     `json:"has_wiki"`
}

func repoViewMCP(repo *Repo) mcpRepoView {
	return mcpRepoView{
		ID: repo.ID, Name: repo.Name, FullName: repo.FullName,
		Owner: OwnerRef{Login: Viewer.Login, ID: Viewer.ID, Type: Viewer.Type},
		Private: repo.Private, Description: repo.Description, DefaultBranch: repo.DefaultBranch,
		HTMLURL: "https://example.invalid/" + ViewerLogin + "/" + repo.Name,
		CloneURL: "https://example.invalid/" + ViewerLogin + "/" + repo.Name + ".git",
		CreatedAt: repo.CreatedAt, UpdatedAt: repo.UpdatedAt, PushedAt: repo.UpdatedAt,
		HasIssues: true, HasProjects: false, HasWiki: false,
	}
}

func toolResult(value any) map[string]any {
	return map[string]any{
		"content":           []map[string]any{{"type": "text", "text": string(marshalJSON(value))}},
		"structuredContent": value,
	}
}

func toolError(message string) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": message}},
		"isError": true,
	}
}

func resolveRepoForTool(f *Forge, args map[string]any) *Repo {
	owner, repo, okOwner, okRepo := resolveOwnerRepo(args)
	if !okOwner || !okRepo || strings.TrimSpace(owner) == "" || strings.TrimSpace(repo) == "" {
		return nil
	}
	if owner != ViewerLogin {
		return nil
	}
	return f.GetRepo(owner, repo)
}

// ---- tool handlers ----

var mcpValidRepoName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

func mcpCreateRepo(f *Forge, args map[string]any) map[string]any {
	name, ok := pickString(args, mcpAliases["name"])
	if !ok || strings.TrimSpace(name) == "" {
		return toolError("name is required")
	}
	if !mcpValidRepoName.MatchString(name) {
		return toolError("Invalid repository name")
	}
	if existing := f.GetRepo(ViewerLogin, name); existing != nil {
		return toolResult(repoViewMCP(existing))
	}
	description, _ := pickString(args, mcpAliases["description"])
	isPrivate, _ := pickBoolean(args, mcpAliases["isPrivate"])
	repo := f.CreateRepo(name, description, isPrivate, "main")
	return toolResult(repoViewMCP(repo))
}

func mcpGetRepo(f *Forge, args map[string]any) map[string]any {
	repo := resolveRepoForTool(f, args)
	if repo == nil {
		return toolError("Repository not found")
	}
	return toolResult(repoViewMCP(repo))
}

func mcpCreateIssue(f *Forge, args map[string]any) map[string]any {
	repo := resolveRepoForTool(f, args)
	if repo == nil {
		return toolError("Repository not found")
	}
	title, ok := pickString(args, mcpAliases["title"])
	if !ok || strings.TrimSpace(title) == "" {
		return toolError("title is required")
	}
	body, _ := pickString(args, mcpAliases["body"])
	issue := f.CreateIssue(repo, title, body, []string{})
	return toolResult(renderIssue(f, repo, issue))
}

func mcpListIssues(f *Forge, args map[string]any) map[string]any {
	repo := resolveRepoForTool(f, args)
	if repo == nil {
		return toolError("Repository not found")
	}
	state := "open"
	if asked, ok := pickString(args, mcpAliases["state"]); ok && (asked == "closed" || asked == "all") {
		state = asked
	}
	rows := []*Issue{}
	for _, issue := range repo.Issues {
		if state == "all" || issue.State == state {
			rows = append(rows, issue)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Number > rows[j].Number })
	views := make([]IssueView, 0, len(rows))
	for _, issue := range rows {
		views = append(views, renderIssue(f, repo, issue))
	}
	return toolResult(views)
}

func mcpCreatePull(f *Forge, args map[string]any) map[string]any {
	repo := resolveRepoForTool(f, args)
	if repo == nil {
		return toolError("Repository not found")
	}
	title, _ := pickString(args, mcpAliases["title"])
	head, _ := pickString(args, mcpAliases["head"])
	base, _ := pickString(args, mcpAliases["base"])
	if strings.TrimSpace(title) == "" || strings.TrimSpace(head) == "" || strings.TrimSpace(base) == "" {
		return toolError("title, head and base are required")
	}
	if _, ok := repo.Refs["refs/heads/"+head]; !ok {
		return toolError("Invalid head or base")
	}
	if _, ok := repo.Refs["refs/heads/"+base]; !ok {
		return toolError("Invalid head or base")
	}
	body, _ := pickString(args, mcpAliases["body"])
	pull := f.CreatePull(repo, title, body, head, base)
	if pull == nil {
		return toolError("Invalid head or base")
	}
	return toolResult(renderPull(f, repo, pull))
}

func mcpMergePull(f *Forge, args map[string]any) map[string]any {
	repo := resolveRepoForTool(f, args)
	if repo == nil {
		return toolError("Repository not found")
	}
	pullNumber, ok := pickInteger(args, mcpAliases["pullNumber"])
	if !ok {
		return toolError("pull_number is required")
	}
	pull, ok := repo.Pulls[pullNumber]
	if !ok {
		return toolError("Pull request not found")
	}
	if pull.Merged || pull.State == "closed" {
		return toolError("Pull request is already merged")
	}
	baseTip := f.RefOid(repo, "refs/heads/"+pull.BaseRef)
	headTip := f.RefOid(repo, "refs/heads/"+pull.HeadRef)
	merged := f.SquashMergeTree(repo, baseTip, headTip)
	if merged.Conflict {
		return toolError("Merge conflict")
	}
	// Four separate clock reads (load-bearing): commit stamp, merged_at,
	// updated_at, repo.updated_at.
	epoch := f.Stamp()
	ident := Viewer.Name + " <" + Viewer.Email + "> " + strconv.FormatInt(epoch, 10) + " +0000"
	commitText := "tree " + merged.TreeOID + "\n" +
		"parent " + baseTip + "\n" +
		"author " + ident + "\n" +
		"committer " + ident + "\n" +
		"\n" +
		pull.Title + " (#" + strconv.Itoa(pull.Number) + ")\n"
	squashOID := f.PutObject(repo, "commit", []byte(commitText))
	f.SetRef(repo, "refs/heads/"+pull.BaseRef, squashOID)
	pull.State = "closed"
	pull.Merged = true
	pull.MergedAt = f.NowIso()
	pull.MergeCommitSha = squashOID
	pull.UpdatedAt = f.NowIso()
	if pull.ClosedAt == "" {
		pull.ClosedAt = pull.UpdatedAt
	}
	repo.UpdatedAt = f.NowIso()
	return toolResult(renderPull(f, repo, pull))
}

var mcpToolHandlers = map[string]func(*Forge, map[string]any) map[string]any{
	"create_repo":         mcpCreateRepo,
	"get_repo":            mcpGetRepo,
	"create_issue":        mcpCreateIssue,
	"list_issues":         mcpListIssues,
	"create_pull_request": mcpCreatePull,
	"merge_pull_request":  mcpMergePull,
}

func dispatchToolCall(f *Forge, params map[string]any) map[string]any {
	name, ok := params["name"].(string)
	if !ok || name == "" {
		return nil
	}
	base := name
	if marker := strings.LastIndex(name, "__"); marker >= 0 {
		base = name[marker+2:]
	}
	handler, ok := mcpToolHandlers[base]
	if !ok {
		return nil
	}
	args := map[string]any{}
	if rec, ok := params["arguments"].(map[string]any); ok {
		args = rec
	}
	return handler(f, args)
}

// ---- handshake ----

func answerInitialize(id any, params map[string]any) Response {
	chosen := mcpLatestVersion
	if offered, ok := params["protocolVersion"].(string); ok {
		for _, v := range mcpSupportedVersions {
			if v == offered {
				chosen = offered
				break
			}
		}
	}
	return mcpResult(id, map[string]any{
		"protocolVersion": chosen,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": mcpServerName, "version": mcpServerVersion},
	})
}

func answerDiscover(id any) Response {
	info := map[string]any{"name": mcpServerName, "version": mcpServerVersion}
	versions := make([]string, len(mcpSupportedVersions))
	copy(versions, mcpSupportedVersions)
	return mcpResult(id, map[string]any{
		"supportedVersions": versions,
		"capabilities":      map[string]any{"tools": map[string]any{}},
		"serverInfo":        info,
		"_meta":             map[string]any{"serverInfo": info},
	})
}

// ---- entry point ----

// handleMCP answers one POST /mcp request. body is the raw request bytes.
func handleMCP(f *Forge, body []byte, authed bool) Response {
	parsed, ok := parseJSONBody(body)
	if !ok {
		return mcpProtocolError(400, -32700, "Parse error")
	}
	rec, ok := parsed.(map[string]any)
	if !ok {
		return mcpProtocolError(400, -32600, "Invalid Request")
	}
	method, ok := rec["method"].(string)
	if !ok || method == "" {
		return mcpProtocolError(400, -32600, "Invalid Request")
	}
	hasID := false
	var id any
	if v, present := rec["id"]; present && v != nil {
		hasID = true
		id = v
	}
	if !authed {
		var authID any
		if hasID {
			authID = id
		}
		return mcpJSON(401, map[string]any{
			"jsonrpc": "2.0",
			"id":      authID,
			"error":   map[string]any{"code": -32000, "message": "Requires authentication"},
		})
	}
	params := map[string]any{}
	if rec2, ok := rec["params"].(map[string]any); ok {
		params = rec2
	}
	if !hasID {
		return mcpEmpty(202)
	}
	switch method {
	case "server/discover":
		return answerDiscover(id)
	case "initialize":
		return answerInitialize(id, params)
	case "notifications/initialized":
		return mcpResult(id, map[string]any{})
	case "tools/list":
		return mcpResult(id, map[string]any{"tools": buildToolList()})
	case "tools/call":
		outcome := dispatchToolCall(f, params)
		if outcome == nil {
			requested := ""
			if s, ok := params["name"].(string); ok {
				requested = s
			}
			return mcpRPCError(id, -32602, "Unknown tool: "+requested)
		}
		return mcpResult(id, outcome)
	case "ping":
		return mcpResult(id, map[string]any{})
	default:
		return mcpRPCError(id, -32601, "Method not found: "+method)
	}
}

// parseJSONBody parses raw JSON; ok=false when it is not valid JSON.
func parseJSONBody(raw []byte) (any, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var v any
	if err := jsonUnmarshal(raw, &v); err != nil {
		return nil, false
	}
	return v, true
}
