// The forge's in-memory store: every entity the four surfaces share, and the
// git object model underneath them. Ported from store.ts (TypeScript) — same
// semantics, same deterministic logical clock, same observable behaviour.
//
// One process, one store, no disk: the trial is a single run and the state
// export must be byte-identical across replays, so every timestamp comes from
// a deterministic logical clock (never time.Now()) and every id from a counter.
package main

import (
	"crypto/sha1"
	"encoding/hex"
	"sort"
	"strings"
	"time"
)

const ViewerLogin = "adaiversen"
const ViewerID = 1000
const ViewerName = "ada iversen"
const ViewerEmail = "adaiversen@example.invalid"

// The logical clock's epoch: 2025-01-01T00:00:00Z. Every created/updated stamp
// is this plus a tick.
const ClockEpochSec = 1735689600

const ZeroOID = "0000000000000000000000000000000000000000"

// A git object: its type and raw bytes. The id is sha1("<type> <len>\0" + data).
type GitObject struct {
	Type string // commit | tree | blob | tag
	Data []byte
}

func HashObject(typ string, data []byte) string {
	h := sha1.New()
	h.Write([]byte(typ + " " + itoa(len(data)) + "\x00"))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

type UserView struct {
	Login string `json:"login"`
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
	Type  string `json:"type"`
}

var Viewer = UserView{ID: ViewerID, Login: ViewerLogin, Name: ViewerName, Email: ViewerEmail, Type: "User"}

type Label struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

type IssueComment struct {
	ID        int      `json:"id"`
	Body      string   `json:"body"`
	User      UserView `json:"user"`
	CreatedAt string   `json:"created_at"`
	UpdatedAt string   `json:"updated_at"`
}

type Issue struct {
	ID        int
	Number    int
	Title     string
	Body      string
	State     string // open | closed
	Labels    []string
	User      UserView
	Comments  []*IssueComment
	CreatedAt string
	UpdatedAt string
	ClosedAt  string // "" = null
}

type PullFile struct {
	Filename  string
	Status    string // added | modified | removed
	Additions int
	Deletions int
	Changes   int
}

type Pull struct {
	ID             int
	Number         int
	Title          string
	Body           string
	State          string // open | closed
	Merged         bool
	HeadRef        string
	HeadSha        string
	BaseRef        string
	BaseSha        string
	User           UserView
	CreatedAt      string
	UpdatedAt      string
	ClosedAt       string // "" = null
	MergedAt       string // "" = null
	MergeCommitSha string // "" = null
	Files          []PullFile
}

type Repo struct {
	ID            int
	Owner         string
	Name          string
	FullName      string
	Description   string
	Private       bool
	DefaultBranch string
	CreatedAt     string
	UpdatedAt     string
	// refname -> commit oid (or ZERO for unborn).
	Refs    map[string]string
	Objects map[string]*GitObject
	Issues  map[int]*Issue
	Pulls   map[int]*Pull
	Labels  map[string]*Label
	// insertion order of labels (for list endpoints)
	LabelOrder []string
	// per-issue cached serialized JSON (R3 read cache)
	IssueJSONCache map[int]cachedIssueJSON
	IssueSeq   int
	PullSeq    int
	LabelSeq   int
	CommentSeq int
	// Bumped on every object/ref mutation; lets the git fetch cache invalidate exactly.
	Version int
}

// A flattened tree: path -> { oid, mode }.
type FlatTree map[string]FlatEntry

type FlatEntry struct {
	OID  string
	Mode string
}

type Forge struct {
	// owner/name -> repo. All repos belong to the viewer in this trial.
	Repos map[string]*Repo
	repoSeq int
	// Logical clock ticks: each stamp consumes one tick (1s).
	clock int64
	// Parsed commit parents, keyed by commit oid. Commits are immutable once
	// stored, so a cached parent list is valid forever.
	parentsCache map[string][]string
}

func NewForge() *Forge {
	return &Forge{Repos: make(map[string]*Repo), parentsCache: make(map[string][]string)}
}

// Next ISO timestamp from the logical clock.
func (f *Forge) NowIso() string {
	f.clock++
	return time.Unix(ClockEpochSec+f.clock, 0).UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

// Peek the current logical time without consuming a tick.
func (f *Forge) PeekIso() string {
	return time.Unix(ClockEpochSec+f.clock, 0).UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

// Stamp consumes one clock tick and returns its epoch seconds.
func (f *Forge) Stamp() int64 {
	f.clock++
	return ClockEpochSec + f.clock
}

// ---------------------------------------------------------- repos

func (f *Forge) CreateRepo(name, description string, isPrivate bool, defaultBranch string) *Repo {
	f.repoSeq++
	id := f.repoSeq
	stamp := f.NowIso()
	repo := &Repo{
		ID: id, Owner: ViewerLogin, Name: name, FullName: ViewerLogin + "/" + name,
		Description: description, Private: isPrivate, DefaultBranch: defaultBranch,
		CreatedAt: stamp, UpdatedAt: stamp,
		Refs: make(map[string]string), Objects: make(map[string]*GitObject),
		Issues: make(map[int]*Issue), Pulls: make(map[int]*Pull),
		Labels: make(map[string]*Label),
	}
	defaults := [][3]string{
		{"bug", "d73a4a", "Something isn't working"},
		{"enhancement", "a2eeef", "New feature or request"},
		{"documentation", "0075ca", "Improvements or additions to documentation"},
		{"question", "d876e3", "Further information is requested"},
		{"good first issue", "7057ff", "Good for newcomers"},
		{"help wanted", "008672", "Extra attention is needed"},
	}
	for _, d := range defaults {
		repo.LabelSeq++
		repo.Labels[d[0]] = &Label{ID: repo.LabelSeq, Name: d[0], Color: d[1], Description: d[2]}
		repo.LabelOrder = append(repo.LabelOrder, d[0])
	}
	f.Repos[repo.FullName] = repo
	return repo
}

func (f *Forge) GetRepo(owner, name string) *Repo {
	return f.Repos[owner+"/"+name]
}

// Find a repo by its numeric id (GraphQL databaseId).
func (f *Forge) GetRepoByID(id int) *Repo {
	for _, repo := range f.Repos {
		if repo.ID == id {
			return repo
		}
	}
	return nil
}

func (f *Forge) DeleteRepo(owner, name string) bool {
	_, ok := f.Repos[owner+"/"+name]
	if ok {
		delete(f.Repos, owner+"/"+name)
	}
	return ok
}

// Repos owned by the viewer, in creation order.
func (f *Forge) ViewerRepos() []*Repo {
	out := []*Repo{}
	for _, repo := range f.Repos {
		if repo.Owner == ViewerLogin {
			out = append(out, repo)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ---------------------------------------------------------- git objects

func (f *Forge) PutObject(repo *Repo, typ string, data []byte) string {
	oid := HashObject(typ, data)
	if _, ok := repo.Objects[oid]; !ok {
		repo.Objects[oid] = &GitObject{Type: typ, Data: data}
		repo.Version++
	}
	return oid
}

func (f *Forge) GetObject(repo *Repo, oid string) *GitObject {
	return repo.Objects[oid]
}

// The ref's current oid, or the zero oid when the ref does not exist.
func (f *Forge) RefOid(repo *Repo, ref string) string {
	if oid, ok := repo.Refs[ref]; ok {
		return oid
	}
	return ZeroOID
}

func (f *Forge) SetRef(repo *Repo, ref, oid string) {
	if repo.Refs[ref] != oid {
		repo.Refs[ref] = oid
		repo.Version++
	}
}

func (f *Forge) DeleteRef(repo *Repo, ref string) bool {
	_, ok := repo.Refs[ref]
	if ok {
		delete(repo.Refs, ref)
		repo.Version++
	}
	return ok
}

// HEAD's target branch ref. Always refs/heads/<default_branch> in this forge.
func (f *Forge) HeadRef(repo *Repo) string {
	return "refs/heads/" + repo.DefaultBranch
}

// The default branch's tip oid, or zero when unborn.
func (f *Forge) HeadOid(repo *Repo) string {
	return f.RefOid(repo, f.HeadRef(repo))
}

// ---------------------------------------------------------- trees

type TreeEntry struct {
	Mode string
	Name string
	OID  string
}

// Parse a tree object's raw bytes into entries.
func ParseTree(data []byte) []TreeEntry {
	entries := []TreeEntry{}
	off := 0
	for off < len(data) {
		space := indexByte(data, ' ', off)
		if space < 0 {
			break
		}
		mode := string(data[off:space])
		nul := indexByte(data, 0, space+1)
		if nul < 0 {
			break
		}
		name := string(data[space+1 : nul])
		if nul+21 > len(data) {
			break
		}
		oid := hex.EncodeToString(data[nul+1 : nul+21])
		entries = append(entries, TreeEntry{Mode: mode, Name: name, OID: oid})
		off = nul + 21
	}
	return entries
}

func indexByte(b []byte, c byte, from int) int {
	for i := from; i < len(b); i++ {
		if b[i] == c {
			return i
		}
	}
	return -1
}

// Build a tree object from entries (sorted git order: dirs sort as if named "name/").
func BuildTree(entries []TreeEntry) []byte {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool {
		an := sorted[i].Name
		if sorted[i].Mode == "40000" {
			an += "/"
		}
		bn := sorted[j].Name
		if sorted[j].Mode == "40000" {
			bn += "/"
		}
		return an < bn
	})
	var out []byte
	for _, e := range sorted {
		out = append(out, []byte(e.Mode+" "+e.Name+"\x00")...)
		raw, _ := hex.DecodeString(e.OID)
		out = append(out, raw...)
	}
	return out
}

// Flatten a tree (recursively) into path -> {oid, mode}. Missing tree => empty.
func (f *Forge) FlattenTree(repo *Repo, treeOid, prefix string) FlatTree {
	out := FlatTree{}
	obj := repo.Objects[treeOid]
	if obj == nil || obj.Type != "tree" {
		return out
	}
	for _, entry := range ParseTree(obj.Data) {
		path := entry.Name
		if prefix != "" {
			path = prefix + "/" + entry.Name
		}
		if entry.Mode == "40000" {
			for p, v := range f.FlattenTree(repo, entry.OID, path) {
				out[p] = v
			}
		} else {
			out[path] = FlatEntry{OID: entry.OID, Mode: entry.Mode}
		}
	}
	return out
}

// Build (and store) a tree from a flat path map. Directories are created as needed.
func (f *Forge) BuildTreeFromFlat(repo *Repo, flat FlatTree) string {
	var buildDir func(dir string) string
	buildDir = func(dir string) string {
		entries := []TreeEntry{}
		subdirs := map[string]bool{}
		var subdirNames []string
		for path, v := range flat {
			var rel string
			if dir == "" {
				rel = path
			} else if strings.HasPrefix(path, dir+"/") {
				rel = path[len(dir)+1:]
			} else {
				continue
			}
			slash := strings.Index(rel, "/")
			if slash < 0 {
				entries = append(entries, TreeEntry{Mode: v.Mode, Name: rel, OID: v.OID})
			} else {
				sub := rel[:slash]
				if !subdirs[sub] {
					subdirs[sub] = true
					subdirNames = append(subdirNames, sub)
				}
			}
		}
		for _, sub := range subdirNames {
			subDir := sub
			if dir != "" {
				subDir = dir + "/" + sub
			}
			subOid := buildDir(subDir)
			entries = append(entries, TreeEntry{Mode: "40000", Name: sub, OID: subOid})
		}
		data := BuildTree(entries)
		return f.PutObject(repo, "tree", data)
	}
	return buildDir("")
}

// The commit's tree oid, or '' when the commit is missing/unparseable.
func (f *Forge) CommitTree(repo *Repo, commitOid string) string {
	obj := repo.Objects[commitOid]
	if obj == nil || obj.Type != "commit" {
		return ""
	}
	text := string(obj.Data)
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "tree ") && len(line) == 45 {
			return line[5:]
		}
		if line == "" {
			break
		}
	}
	return ""
}

// The commit's parent oids, in order. Results are cached (commits are immutable).
func (f *Forge) CommitParents(repo *Repo, commitOid string) []string {
	if cached, ok := f.parentsCache[commitOid]; ok {
		return cached
	}
	out := []string{}
	obj := repo.Objects[commitOid]
	if obj != nil && obj.Type == "commit" {
		text := string(obj.Data)
		for _, line := range strings.Split(text, "\n") {
			if line == "" {
				break
			}
			if strings.HasPrefix(line, "parent ") && len(line) == 47 {
				out = append(out, line[7:])
			}
		}
	}
	f.parentsCache[commitOid] = out
	return out
}

// All ancestors of `tip` (inclusive), newest-first by BFS over parents.
func (f *Forge) CollectAncestors(repo *Repo, tip string) map[string]bool {
	seen := map[string]bool{}
	queue := []string{tip}
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if oid == ZeroOID || seen[oid] {
			continue
		}
		seen[oid] = true
		for _, p := range f.CommitParents(repo, oid) {
			queue = append(queue, p)
		}
	}
	return seen
}

// The merge-base: the newest commit reachable from both tips.
func (f *Forge) MergeBase(repo *Repo, a, b string) string {
	if a == ZeroOID || b == ZeroOID {
		return ZeroOID
	}
	aAnc := f.CollectAncestors(repo, a)
	seen := map[string]bool{}
	queue := []string{b}
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if oid == ZeroOID || seen[oid] {
			continue
		}
		seen[oid] = true
		if aAnc[oid] {
			return oid
		}
		for _, p := range f.CommitParents(repo, oid) {
			queue = append(queue, p)
		}
	}
	return ZeroOID
}

// Is `ancestor` reachable from `tip`? Walks from the tip and stops the moment
// the ancestor is found (O(1) in the common push case).
func (f *Forge) IsAncestor(repo *Repo, ancestor, tip string) bool {
	if ancestor == ZeroOID {
		return true
	}
	if ancestor == tip {
		return true
	}
	seen := map[string]bool{}
	queue := []string{tip}
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if oid == ZeroOID || seen[oid] {
			continue
		}
		seen[oid] = true
		for _, p := range f.CommitParents(repo, oid) {
			if p == ancestor {
				return true
			}
			queue = append(queue, p)
		}
	}
	return false
}

type FlatDiff struct {
	Path   string
	Status string // added | modified | removed
	OldOID string
	NewOID string
}

// Diff two flat trees.
func DiffFlat(oldTree, newTree FlatTree) []FlatDiff {
	paths := map[string]bool{}
	for p := range oldTree {
		paths[p] = true
	}
	for p := range newTree {
		paths[p] = true
	}
	var sorted []string
	for p := range paths {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)
	out := []FlatDiff{}
	for _, path := range sorted {
		o, hasO := oldTree[path]
		n, hasN := newTree[path]
		if hasO && !hasN {
			out = append(out, FlatDiff{Path: path, Status: "removed", OldOID: o.OID})
		} else if !hasO && hasN {
			out = append(out, FlatDiff{Path: path, Status: "added", NewOID: n.OID})
		} else if hasO && hasN && o.OID != n.OID {
			out = append(out, FlatDiff{Path: path, Status: "modified", OldOID: o.OID, NewOID: n.OID})
		}
	}
	return out
}

// Count added/deleted lines between two blob contents (naive line diff).
func LineCounts(oldContent, newContent []byte) (additions, deletions int) {
	splitLines := func(b []byte) []string {
		if b == nil {
			return []string{}
		}
		ls := strings.Split(string(b), "\n")
		if len(ls) > 0 && ls[len(ls)-1] == "" {
			ls = ls[:len(ls)-1]
		}
		return ls
	}
	a := splitLines(oldContent)
	b := splitLines(newContent)
	aSet := map[string]int{}
	for _, l := range a {
		aSet[l]++
	}
	bSet := map[string]int{}
	for _, l := range b {
		bSet[l]++
	}
	for l, n := range bSet {
		if d := n - aSet[l]; d > 0 {
			additions += d
		}
	}
	for l, n := range aSet {
		if d := n - bSet[l]; d > 0 {
			deletions += d
		}
	}
	return additions, deletions
}

// Pull files between base tip and head tip (diff of their trees).
func (f *Forge) PullFiles(repo *Repo, baseOid, headOid string) []PullFile {
	baseTree := f.FlattenTree(repo, f.CommitTree(repo, baseOid), "")
	headTree := f.FlattenTree(repo, f.CommitTree(repo, headOid), "")
	files := []PullFile{}
	for _, d := range DiffFlat(baseTree, headTree) {
		var oldData, newData []byte
		if d.OldOID != "" {
			if o := repo.Objects[d.OldOID]; o != nil {
				oldData = o.Data
			}
		}
		if d.NewOID != "" {
			if o := repo.Objects[d.NewOID]; o != nil {
				newData = o.Data
			}
		}
		add, del := LineCounts(oldData, newData)
		files = append(files, PullFile{Filename: d.Path, Status: d.Status, Additions: add, Deletions: del, Changes: add + del})
	}
	return files
}

type MergeResult struct {
	Conflict bool
	TreeOID  string
}

// 3-way merge for a squash merge: baseTip's tree + head's changes since merge-base.
func (f *Forge) SquashMergeTree(repo *Repo, baseOid, headOid string) MergeResult {
	base := f.MergeBase(repo, baseOid, headOid)
	baseTree := f.FlattenTree(repo, f.CommitTree(repo, base), "")
	curTree := f.FlattenTree(repo, f.CommitTree(repo, baseOid), "")
	headTree := f.FlattenTree(repo, f.CommitTree(repo, headOid), "")
	baseChanges := DiffFlat(baseTree, curTree)
	headChanges := DiffFlat(baseTree, headTree)
	baseChanged := map[string]FlatDiff{}
	for _, c := range baseChanges {
		baseChanged[c.Path] = c
	}
	for _, hc := range headChanges {
		if bc, ok := baseChanged[hc.Path]; ok && bc.NewOID != hc.NewOID {
			return MergeResult{Conflict: true}
		}
	}
	merged := FlatTree{}
	for p, v := range curTree {
		merged[p] = v
	}
	for _, hc := range headChanges {
		if hc.Status == "removed" {
			delete(merged, hc.Path)
		} else {
			if hv, ok := headTree[hc.Path]; ok {
				merged[hc.Path] = hv
			}
		}
	}
	treeOid := f.BuildTreeFromFlat(repo, merged)
	return MergeResult{Conflict: false, TreeOID: treeOid}
}

// Is the head mergeable into base (no 3-way conflict)?
func (f *Forge) IsMergeable(repo *Repo, baseOid, headOid string) bool {
	return !f.SquashMergeTree(repo, baseOid, headOid).Conflict
}

// ---------------------------------------------------------- issues / pulls

func (f *Forge) CreateIssue(repo *Repo, title, body string, labels []string) *Issue {
	repo.IssueSeq++
	stamp := f.NowIso()
	issue := &Issue{
		ID: repo.IssueSeq + 100000, Number: repo.IssueSeq,
		Title: title, Body: body, State: "open",
		Labels: append([]string{}, labels...),
		User: Viewer, Comments: []*IssueComment{},
		CreatedAt: stamp, UpdatedAt: stamp,
	}
	repo.Issues[issue.Number] = issue
	return issue
}

func (f *Forge) CreatePull(repo *Repo, title, body, headRef, baseRef string) *Pull {
	headOid := f.RefOid(repo, "refs/heads/"+headRef)
	baseOid := f.RefOid(repo, "refs/heads/"+baseRef)
	if headOid == ZeroOID || baseOid == ZeroOID {
		return nil
	}
	repo.PullSeq++
	stamp := f.NowIso()
	pull := &Pull{
		ID: repo.PullSeq + 200000, Number: repo.PullSeq,
		Title: title, Body: body, State: "open", Merged: false,
		HeadRef: headRef, HeadSha: headOid,
		BaseRef: baseRef, BaseSha: baseOid,
		User: Viewer,
		CreatedAt: stamp, UpdatedAt: stamp,
		Files: f.PullFiles(repo, baseOid, headOid),
	}
	repo.Pulls[pull.Number] = pull
	return pull
}
