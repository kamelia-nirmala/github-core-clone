// Issues, comments, and pull requests over the REST v3 surface.
// Ported from rest-issues.ts — same routes, same gates, same clock ticks.
package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Precomputed JSON for the constant viewer object (S3 fast path).
var viewerJSON = string(marshalJSON(Viewer))

type cachedIssueJSON struct {
	Title     string
	Body      string
	State     string
	ClosedAt  string // "" = null
	UpdatedAt string
	Text      string
}

// nilIfEmpty converts a ""-means-null store string to a JSON null/string.
func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

type LabelView struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

type IssueView struct {
	ID        int         `json:"id"`
	Number    int         `json:"number"`
	Title     string      `json:"title"`
	Body      string      `json:"body"`
	State     string      `json:"state"`
	Labels    []LabelView `json:"labels"`
	User      UserView     `json:"user"`
	Assignee  *string     `json:"assignee"`
	Assignees []string    `json:"assignees"`
	CreatedAt string      `json:"created_at"`
	UpdatedAt string      `json:"updated_at"`
	ClosedAt  *string     `json:"closed_at"`
	HTMLURL   string      `json:"html_url"`
}

type CommentView struct {
	ID        int     `json:"id"`
	Body      string  `json:"body"`
	User      UserView `json:"user"`
	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
}

type PullRefView struct {
	Ref   string `json:"ref"`
	Sha   string `json:"sha"`
	Label string `json:"label"`
}

type PullView struct {
	ID             int         `json:"id"`
	Number         int         `json:"number"`
	Title          string      `json:"title"`
	Body           string      `json:"body"`
	State          string      `json:"state"`
	Merged         bool        `json:"merged"`
	Mergeable      bool        `json:"mergeable"`
	MergedAt       *string     `json:"merged_at"`
	MergeCommitSha *string     `json:"merge_commit_sha"`
	Head           PullRefView `json:"head"`
	Base           PullRefView `json:"base"`
	User           UserView     `json:"user"`
	CreatedAt      string      `json:"created_at"`
	UpdatedAt      string      `json:"updated_at"`
	ClosedAt       *string     `json:"closed_at"`
	HTMLURL        string      `json:"html_url"`
}

func renderLabel(l *Label) LabelView {
	return LabelView{ID: l.ID, Name: l.Name, Color: l.Color, Description: l.Description}
}

func renderIssue(f *Forge, repo *Repo, issue *Issue) IssueView {
	expanded := []LabelView{}
	for _, name := range issue.Labels {
		if found, ok := repo.Labels[name]; ok {
			expanded = append(expanded, renderLabel(found))
		}
	}
	return IssueView{
		ID: issue.ID, Number: issue.Number, Title: issue.Title, Body: issue.Body,
		State: issue.State, Labels: expanded, User: Viewer, Assignee: nil,
		Assignees: []string{}, CreatedAt: issue.CreatedAt, UpdatedAt: issue.UpdatedAt,
		ClosedAt: nilIfEmpty(issue.ClosedAt),
		HTMLURL: "https://example.invalid/" + ViewerLogin + "/" + repo.Name + "/issues/" + strconv.Itoa(issue.Number),
	}
}

func renderComment(c *IssueComment) CommentView {
	return CommentView{ID: c.ID, Body: c.Body, User: Viewer, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt}
}

func renderPull(f *Forge, repo *Repo, pull *Pull) PullView {
	baseTip := f.RefOid(repo, "refs/heads/"+pull.BaseRef)
	headTip := f.RefOid(repo, "refs/heads/"+pull.HeadRef)
	mergeable := false
	if pull.State == "open" {
		mergeable = f.IsMergeable(repo, baseTip, headTip)
	}
	return PullView{
		ID: pull.ID, Number: pull.Number, Title: pull.Title, Body: pull.Body,
		State: pull.State, Merged: pull.Merged, Mergeable: mergeable,
		MergedAt: nilIfEmpty(pull.MergedAt), MergeCommitSha: nilIfEmpty(pull.MergeCommitSha),
		Head: PullRefView{Ref: pull.HeadRef, Sha: headTip, Label: ViewerLogin + ":" + pull.HeadRef},
		Base: PullRefView{Ref: pull.BaseRef, Sha: baseTip, Label: ViewerLogin + ":" + pull.BaseRef},
		User: Viewer, CreatedAt: pull.CreatedAt, UpdatedAt: pull.UpdatedAt,
		ClosedAt: nilIfEmpty(pull.ClosedAt),
		HTMLURL: "https://example.invalid/" + ViewerLogin + "/" + repo.Name + "/pull/" + strconv.Itoa(pull.Number),
	}
}

// ---- routing ----

type endpointName string

const (
	epLabelsCreate         endpointName = "labels:create"
	epLabelsList           endpointName = "labels:list"
	epIssuesCreate         endpointName = "issues:create"
	epIssuesList           endpointName = "issues:list"
	epIssuesGet            endpointName = "issues:get"
	epIssuesPatch          endpointName = "issues:patch"
	epIssuesCommentsCreate endpointName = "issues:comments:create"
	epIssuesCommentsList   endpointName = "issues:comments:list"
	epPullsCreate          endpointName = "pulls:create"
	epPullsList            endpointName = "pulls:list"
	epPullsGet             endpointName = "pulls:get"
	epPullsFiles           endpointName = "pulls:files"
	epPullsMerge           endpointName = "pulls:merge"
)

type routeSpec struct {
	verb  string
	depth int
	kind  string
	tail  string
	name  endpointName
}

var routeTable = []routeSpec{
	{"POST", 4, "labels", "", epLabelsCreate},
	{"GET", 4, "labels", "", epLabelsList},
	{"POST", 4, "issues", "", epIssuesCreate},
	{"GET", 4, "issues", "", epIssuesList},
	{"GET", 5, "issues", "", epIssuesGet},
	{"PATCH", 5, "issues", "", epIssuesPatch},
	{"POST", 6, "issues", "comments", epIssuesCommentsCreate},
	{"GET", 6, "issues", "comments", epIssuesCommentsList},
	{"POST", 4, "pulls", "", epPullsCreate},
	{"GET", 4, "pulls", "", epPullsList},
	{"GET", 5, "pulls", "", epPullsGet},
	{"GET", 6, "pulls", "files", epPullsFiles},
	{"PUT", 6, "pulls", "merge", epPullsMerge},
}

func resolveRouteName(verb string, parts []string) endpointName {
	if len(parts) < 1 || parts[0] != "repos" || len(parts) < 4 {
		return ""
	}
	kind := parts[3]
	if kind == "issues" && len(parts) == 5 && verb == "GET" {
		return epIssuesGet
	}
	if kind == "issues" && len(parts) == 4 && verb == "POST" {
		return epIssuesCreate
	}
	for _, spec := range routeTable {
		if spec.verb != verb || spec.depth != len(parts) || spec.kind != kind {
			continue
		}
		if spec.tail != "" && (len(parts) < 6 || parts[5] != spec.tail) {
			continue
		}
		return spec.name
	}
	return ""
}

func parseStateFilter(raw string) string {
	if raw == "closed" {
		return "closed"
	}
	if raw == "all" {
		return "all"
	}
	return "open"
}

func orderIssues(entries []*Issue, field string, ascending bool) {
	sort.SliceStable(entries, func(i, j int) bool {
		li, rj := entries[i], entries[j]
		var ls, rs string
		if field == "updated" {
			ls, rs = li.UpdatedAt, rj.UpdatedAt
		} else {
			ls, rs = li.CreatedAt, rj.CreatedAt
		}
		cmp := 0
		if ls < rs {
			cmp = -1
		} else if ls > rs {
			cmp = 1
		} else if li.Number < rj.Number {
			cmp = -1
		} else if li.Number > rj.Number {
			cmp = 1
		}
		if ascending {
			return cmp < 0
		}
		return cmp > 0
	})
}

// slicePage: cut a page out of a list; next-page link from the current query.
func slicePage(ctx *RestCtx, total int) (start, end int, link string) {
	perPage, page := pageParams(ctx.Query)
	start = (page - 1) * perPage
	end = start + perPage
	if start > total {
		start = total
	}
	if end > total {
		end = total
	}
	if start+perPage < total {
		path := "/" + strings.Join(ctx.Segments, "/")
		link = fmt.Sprintf("<http://%s/api/v3%s?%s>; rel=\"next\"", ctx.Host, path, ctx.Query.SetPage(page+1))
	}
	return start, end, link
}

func linkHeader(link string) map[string]string {
	if link == "" {
		return nil
	}
	return map[string]string{"Link": link}
}

// issuePullRoutes: entry point. Returns (handled, response).
func issuePullRoutes(ctx *RestCtx) (bool, Response) {
	routeName := resolveRouteName(ctx.Method, ctx.Segments)
	if routeName == "" {
		return false, Response{}
	}
	if !ctx.Authed {
		return true, errResp(401, "Requires authentication")
	}
	if ctx.BodyMalformed {
		return true, errResp(400, "Invalid JSON body")
	}
	owner, repoName := ctx.Segments[1], ctx.Segments[2]
	if owner != ViewerLogin {
		return true, errResp(404, "Not Found")
	}
	repo := ctx.Forge.GetRepo(owner, repoName)
	if repo == nil {
		return true, errResp(404, "Not Found")
	}
	return true, dispatchIssuePull(ctx, repo, routeName)
}

func dispatchIssuePull(ctx *RestCtx, repo *Repo, name endpointName) Response {
	switch name {
	case epLabelsCreate:
		return createLabel(ctx, repo)
	case epLabelsList:
		return listLabels(ctx, repo)
	case epIssuesCreate:
		return createIssueH(ctx, repo)
	case epIssuesList:
		return listIssues(ctx, repo)
	case epIssuesGet:
		return getIssue(ctx, repo)
	case epIssuesPatch:
		return patchIssue(ctx, repo)
	case epIssuesCommentsCreate:
		return createCommentH(ctx, repo)
	case epIssuesCommentsList:
		return listComments(ctx, repo)
	case epPullsCreate:
		return createPullH(ctx, repo)
	case epPullsList:
		return listPulls(ctx, repo)
	case epPullsGet:
		return getPull(ctx, repo)
	case epPullsFiles:
		return listPullFiles(ctx, repo)
	case epPullsMerge:
		return mergePull(ctx, repo)
	}
	return errResp(404, "Not Found")
}

func createLabel(ctx *RestCtx, repo *Repo) Response {
	data := bodyAsRecord(ctx)
	wanted, _ := data["name"].(string)
	if wanted == "" {
		return errResp(422, "name is required")
	}
	if already, ok := repo.Labels[wanted]; ok {
		return jsonResp(201, renderLabel(already), nil)
	}
	repo.LabelSeq++
	color := "ededed"
	if s, ok := data["color"].(string); ok {
		color = s
	}
	desc := ""
	if s, ok := data["description"].(string); ok {
		desc = s
	}
	fresh := &Label{ID: repo.LabelSeq, Name: wanted, Color: color, Description: desc}
	repo.Labels[wanted] = fresh
	return jsonResp(201, renderLabel(fresh), nil)
}

func listLabels(ctx *RestCtx, repo *Repo) Response {
	rendered := make([]LabelView, 0, len(repo.Labels))
	// Map iteration is random in Go; labels must come out in insertion order.
	// The store keeps an insertion-ordered slice for this.
	for _, name := range repo.LabelOrder {
		if l, ok := repo.Labels[name]; ok {
			rendered = append(rendered, renderLabel(l))
		}
	}
	return jsonResp(200, rendered, nil)
}

func createIssueH(ctx *RestCtx, repo *Repo) Response {
	data := bodyAsRecord(ctx)
	title, _ := data["title"].(string)
	if title == "" {
		return errResp(422, "title is required")
	}
	chosen := []string{}
	if _, supplied := data["labels"]; supplied {
		rawLabels, ok := data["labels"].([]any)
		if !ok {
			return errResp(422, "Label does not exist")
		}
		for _, item := range rawLabels {
			s, ok := item.(string)
			if !ok {
				return errResp(422, "Label does not exist")
			}
			if _, exists := repo.Labels[s]; !exists {
				return errResp(422, "Label does not exist")
			}
			chosen = append(chosen, s)
		}
	}
	bodyText := ""
	if s, ok := data["body"].(string); ok {
		bodyText = s
	}
	issue := ctx.Forge.CreateIssue(repo, title, bodyText, chosen)
	if len(chosen) == 0 {
		// S3 fast path: build the response JSON directly (same key order as
		// renderIssue), skipping the intermediate object + full stringify.
		text := fmt.Sprintf("{\"id\":%d,\"number\":%d,\"title\":%s,\"body\":%s,\"state\":\"open\",\"labels\":[],\"user\":%s,\"assignee\":null,\"assignees\":[],\"created_at\":%s,\"updated_at\":%s,\"closed_at\":null,\"html_url\":%s}\n",
			issue.ID, issue.Number, jsonQuote(issue.Title), jsonQuote(issue.Body),
			viewerJSON, jsonQuote(issue.CreatedAt), jsonQuote(issue.UpdatedAt),
			jsonQuote("https://example.invalid/"+ViewerLogin+"/"+repo.Name+"/issues/"+strconv.Itoa(issue.Number)))
		return rawJSONResp(201, text, nil)
	}
	return jsonResp(201, renderIssue(ctx.Forge, repo, issue), nil)
}

func listIssues(ctx *RestCtx, repo *Repo) Response {
	wanted := parseStateFilter(ctx.Query.Get("state"))
	sortParam := ctx.Query.Get("sort")
	sortField := "created"
	if sortParam == "updated" {
		sortField = "updated"
	}
	ascending := ctx.Query.Get("direction") == "asc"
	rows := make([]*Issue, 0, len(repo.Issues))
	for _, iss := range repo.Issues {
		if wanted != "all" && iss.State != wanted {
			continue
		}
		rows = append(rows, iss)
	}
	orderIssues(rows, sortField, ascending)
	start, end, link := slicePage(ctx, len(rows))
	rendered := make([]IssueView, 0, end-start)
	for _, iss := range rows[start:end] {
		rendered = append(rendered, renderIssue(ctx.Forge, repo, iss))
	}
	return jsonResp(200, rendered, linkHeader(link))
}

func getIssue(ctx *RestCtx, repo *Repo) Response {
	num := parseNumberSegment(ctx.Segments[4])
	if num < 0 {
		return errResp(404, "Not Found")
	}
	issue, ok := repo.Issues[num]
	if !ok {
		return errResp(404, "Not Found")
	}
	if repo.IssueJSONCache == nil {
		repo.IssueJSONCache = map[int]cachedIssueJSON{}
	}
	if cached, ok := repo.IssueJSONCache[num]; ok &&
		cached.Title == issue.Title && cached.Body == issue.Body &&
		cached.State == issue.State && cached.ClosedAt == issue.ClosedAt &&
		cached.UpdatedAt == issue.UpdatedAt {
		return rawJSONResp(200, cached.Text, nil)
	}
	rendered := renderIssue(ctx.Forge, repo, issue)
	text := string(marshalJSONLine(rendered))
	repo.IssueJSONCache[num] = cachedIssueJSON{
		Title: issue.Title, Body: issue.Body, State: issue.State,
		ClosedAt: issue.ClosedAt, UpdatedAt: issue.UpdatedAt, Text: text,
	}
	return rawJSONResp(200, text, nil)
}

func patchIssue(ctx *RestCtx, repo *Repo) Response {
	num := parseNumberSegment(ctx.Segments[4])
	if num < 0 {
		return errResp(404, "Not Found")
	}
	issue, ok := repo.Issues[num]
	if !ok {
		return errResp(404, "Not Found")
	}
	data := bodyAsRecord(ctx)
	touched := false
	if nextState, ok := data["state"].(string); ok && (nextState == "open" || nextState == "closed") {
		issue.State = nextState
		if nextState == "closed" {
			issue.ClosedAt = ctx.Forge.NowIso()
		} else {
			issue.ClosedAt = ""
		}
		touched = true
	}
	if s, ok := data["title"].(string); ok {
		issue.Title = s
		touched = true
	}
	if s, ok := data["body"].(string); ok {
		issue.Body = s
		touched = true
	}
	if touched {
		issue.UpdatedAt = ctx.Forge.NowIso()
	}
	return jsonResp(200, renderIssue(ctx.Forge, repo, issue), nil)
}

func createCommentH(ctx *RestCtx, repo *Repo) Response {
	num := parseNumberSegment(ctx.Segments[4])
	if num < 0 {
		return errResp(404, "Not Found")
	}
	issue, ok := repo.Issues[num]
	if !ok {
		return errResp(404, "Not Found")
	}
	data := bodyAsRecord(ctx)
	text, _ := data["body"].(string)
	if text == "" {
		return errResp(422, "body is required")
	}
	repo.CommentSeq++
	stamp := ctx.Forge.NowIso()
	comment := &IssueComment{ID: repo.CommentSeq, Body: text, User: Viewer, CreatedAt: stamp, UpdatedAt: stamp}
	issue.Comments = append(issue.Comments, comment)
	return jsonResp(201, renderComment(comment), nil)
}

func listComments(ctx *RestCtx, repo *Repo) Response {
	num := parseNumberSegment(ctx.Segments[4])
	if num < 0 {
		return errResp(404, "Not Found")
	}
	issue, ok := repo.Issues[num]
	if !ok {
		return errResp(404, "Not Found")
	}
	rendered := make([]CommentView, 0, len(issue.Comments))
	for _, c := range issue.Comments {
		rendered = append(rendered, renderComment(c))
	}
	return jsonResp(200, rendered, nil)
}

func createPullH(ctx *RestCtx, repo *Repo) Response {
	data := bodyAsRecord(ctx)
	title, _ := data["title"].(string)
	head, _ := data["head"].(string)
	base, _ := data["base"].(string)
	if title == "" || head == "" || base == "" {
		return errResp(422, "title, head and base are required")
	}
	if _, ok := repo.Refs["refs/heads/"+head]; !ok {
		return errResp(422, "Invalid head or base")
	}
	if _, ok := repo.Refs["refs/heads/"+base]; !ok {
		return errResp(422, "Invalid head or base")
	}
	bodyText := ""
	if s, ok := data["body"].(string); ok {
		bodyText = s
	}
	pull := ctx.Forge.CreatePull(repo, title, bodyText, head, base)
	if pull == nil {
		return errResp(422, "Invalid head or base")
	}
	return jsonResp(201, renderPull(ctx.Forge, repo, pull), nil)
}

func listPulls(ctx *RestCtx, repo *Repo) Response {
	wanted := parseStateFilter(ctx.Query.Get("state"))
	rows := make([]*Pull, 0, len(repo.Pulls))
	for _, p := range repo.Pulls {
		if wanted != "all" && p.State != wanted {
			continue
		}
		rows = append(rows, p)
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Number > rows[j].Number })
	start, end, link := slicePage(ctx, len(rows))
	rendered := make([]PullView, 0, end-start)
	for _, p := range rows[start:end] {
		rendered = append(rendered, renderPull(ctx.Forge, repo, p))
	}
	return jsonResp(200, rendered, linkHeader(link))
}

func getPull(ctx *RestCtx, repo *Repo) Response {
	num := parseNumberSegment(ctx.Segments[4])
	if num < 0 {
		return errResp(404, "Not Found")
	}
	pull, ok := repo.Pulls[num]
	if !ok {
		return errResp(404, "Not Found")
	}
	return jsonResp(200, renderPull(ctx.Forge, repo, pull), nil)
}

type pullFileView struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Changes   int    `json:"changes"`
	Patch     string `json:"patch"`
}

func listPullFiles(ctx *RestCtx, repo *Repo) Response {
	num := parseNumberSegment(ctx.Segments[4])
	if num < 0 {
		return errResp(404, "Not Found")
	}
	pull, ok := repo.Pulls[num]
	if !ok {
		return errResp(404, "Not Found")
	}
	baseTip := ctx.Forge.RefOid(repo, "refs/heads/"+pull.BaseRef)
	headTip := ctx.Forge.RefOid(repo, "refs/heads/"+pull.HeadRef)
	changed := ctx.Forge.PullFiles(repo, baseTip, headTip)
	rendered := make([]pullFileView, 0, len(changed))
	for _, f := range changed {
		rendered = append(rendered, pullFileView{
			Filename: f.Filename, Status: f.Status, Additions: f.Additions,
			Deletions: f.Deletions, Changes: f.Changes, Patch: "",
		})
	}
	return jsonResp(200, rendered, nil)
}

type mergeResponse struct {
	Sha     string `json:"sha"`
	Merged  bool   `json:"merged"`
	Message string `json:"message"`
}

func mergePull(ctx *RestCtx, repo *Repo) Response {
	num := parseNumberSegment(ctx.Segments[4])
	if num < 0 {
		return errResp(404, "Not Found")
	}
	pull, ok := repo.Pulls[num]
	if !ok {
		return errResp(404, "Not Found")
	}
	data := bodyAsRecord(ctx)
	if methodRaw, present := data["merge_method"]; present {
		s, ok := methodRaw.(string)
		if !ok || (s != "squash" && s != "merge" && s != "rebase") {
			return errResp(422, "Invalid merge method")
		}
	}
	if pull.Merged || pull.State == "closed" {
		return errResp(405, "Pull request is already merged")
	}
	baseTip := ctx.Forge.RefOid(repo, "refs/heads/"+pull.BaseRef)
	headTip := ctx.Forge.RefOid(repo, "refs/heads/"+pull.HeadRef)
	merged := ctx.Forge.SquashMergeTree(repo, baseTip, headTip)
	if merged.Conflict {
		return errResp(405, "Merge conflict")
	}
	epoch := ctx.Forge.Stamp()
	stamp := formatTime(epoch)
	subject := pull.Title + " (#" + strconv.Itoa(pull.Number) + ")"
	ident := Viewer.Name + " <" + Viewer.Email + "> " + strconv.FormatInt(epoch, 10) + " +0000"
	commitText := "tree " + merged.TreeOID + "\n" +
		"parent " + baseTip + "\n" +
		"author " + ident + "\n" +
		"committer " + ident + "\n" +
		"\n" +
		subject + "\n"
	squashOID := ctx.Forge.PutObject(repo, "commit", []byte(commitText))
	ctx.Forge.SetRef(repo, "refs/heads/"+pull.BaseRef, squashOID)
	pull.State = "closed"
	pull.Merged = true
	pull.MergedAt = stamp
	pull.MergeCommitSha = squashOID
	pull.UpdatedAt = stamp
	repo.UpdatedAt = stamp
	return jsonResp(200, mergeResponse{Sha: squashOID, Merged: true, Message: "Pull request successfully merged"}, nil)
}
