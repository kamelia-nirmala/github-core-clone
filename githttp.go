// Git smart HTTP: upload-pack protocol v2, receive-pack v0.
// Ported from git-http.ts — same wire format, same caches, same errors.
package main

import (
	"fmt"
	"sort"
	"strings"
)

type ProtocolError struct{ msg string }

func (e *ProtocolError) Error() string { return e.msg }

func uploadV0Caps(branch string) string {
	return "multi_ack thin-pack side-band-64k side-band ofs-delta shallow no-progress agent=forge/1.0 symref=HEAD:refs/heads/" + branch
}

func receiveV0Caps(branch string) string {
	return "report-status side-band-64k delete-refs ofs-delta agent=forge/1.0 symref=HEAD:refs/heads/" + branch
}

// Route one git smart-HTTP request. Always handled for a .git path.
// Panics are converted to a 500 (mirrors the TS try/catch in handleGit).
func handleGit(f *Forge, owner, repoName string, tail []string, query ParsedURL, method, gitProtocol, authHeader string, body []byte) (resp Response) {
	defer func() {
		if r := recover(); r != nil {
			resp = textResponse(500, "Internal Server Error")
		}
	}()
	var err error
	resp, err = dispatchGit(f, owner, repoName, tail, query, method, gitProtocol, authHeader, body)
	if err != nil {
		if pe, ok := err.(*ProtocolError); ok {
			return textResponse(400, pe.msg)
		}
		return textResponse(500, "Internal Server Error")
	}
	return resp
}

func dispatchGit(f *Forge, owner, repoName string, tail []string, query ParsedURL, method, gitProtocol, authHeader string, body []byte) (Response, error) {
	if !gitAuthorized(authHeader) {
		return textResponse(401, "Unauthorized"), nil
	}
	if method == "GET" && len(tail) == 2 && tail[0] == "info" && tail[1] == "refs" {
		return handleInfoRefs(f, owner, repoName, query, gitProtocol), nil
	}
	if method == "POST" && len(tail) == 1 && tail[0] == "git-upload-pack" {
		return handleUploadPack(f, owner, repoName, gitProtocol, body)
	}
	if method == "POST" && len(tail) == 1 && tail[0] == "git-receive-pack" {
		return handleReceivePack(f, owner, repoName, body)
	}
	return textResponse(404, "Not Found"), nil
}

func gitAuthorized(authHeader string) bool {
	token := shopToken()
	if token == "" {
		return true
	}
	if authHeader == "" {
		return true
	}
	t := bearerToken(authHeader)
	return t != "" && t == token
}

func textResponse(status int, text string) Response {
	return Response{Status: status, ContentType: "text/plain; charset=utf-8", Body: []byte(text), NoRequestID: true}
}

func gitResponse(status int, contentType string, body []byte) Response {
	return Response{Status: status, ContentType: contentType, Body: body, NoRequestID: true}
}

func handleInfoRefs(f *Forge, owner, repoName string, query ParsedURL, gitProtocol string) Response {
	service := query.Get("service")
	repo := f.GetRepo(owner, repoName)
	if service == "git-upload-pack" {
		if repo == nil {
			return textResponse(404, "Not Found")
		}
		if gitProtocol == "version=2" {
			return gitResponse(200, "application/x-git-upload-pack-advertisement", advertisementV2())
		}
		return gitResponse(200, "application/x-git-upload-pack-advertisement", cachedAdvertisementV0(repo, "git-upload-pack", uploadV0Caps(repo.DefaultBranch)))
	}
	if service == "git-receive-pack" {
		if repo == nil {
			return textResponse(404, "Not Found")
		}
		return gitResponse(200, "application/x-git-receive-pack-advertisement", cachedAdvertisementV0(repo, "git-receive-pack", receiveV0Caps(repo.DefaultBranch)))
	}
	return textResponse(400, "Bad Request")
}

func advertisementV0(repo *Repo, service, caps string) []byte {
	var out []byte
	out = append(out, EncodePktLineString("# service="+service+"\n")...)
	out = append(out, PktLineFlush()...)
	refs := sortedRefs(repo)
	headRef := "refs/heads/" + repo.DefaultBranch
	headOid := repo.Refs[headRef]
	if headOid == "" {
		headOid = ZeroOID
	}
	if len(refs) == 0 {
		out = append(out, EncodePktLineString(ZeroOID+" capabilities^{}\x00"+caps+"\n")...)
	} else {
		out = append(out, EncodePktLineString(headOid+" HEAD\x00"+caps+"\n")...)
		for _, ref := range refs {
			out = append(out, EncodePktLineString(ref.oid+" "+ref.name+"\n")...)
			if strings.HasPrefix(ref.name, "refs/tags/") {
				if obj := repo.Objects[ref.oid]; obj != nil && obj.Type == "tag" {
					if peeled := peelTag(repo, ref.oid); peeled != "" {
						out = append(out, EncodePktLineString(peeled+" "+ref.name+"^{}\n")...)
					}
				}
			}
		}
	}
	out = append(out, PktLineFlush()...)
	return out
}

func advertisementV2() []byte {
	var out []byte
	out = append(out, EncodePktLineString("# service=git-upload-pack\n")...)
	out = append(out, PktLineFlush()...)
	out = append(out, EncodePktLines([]string{
		"version 2\n",
		"ls-refs=unborn\n",
		"fetch=shallow filter\n",
		"object-format=sha1\n",
		"agent=forge/1.0\n",
		"object-info\n",
		"server-option\n",
	})...)
	out = append(out, PktLineFlush()...)
	return out
}

func handleUploadPack(f *Forge, owner, repoName, gitProtocol string, body []byte) (Response, error) {
	repo := f.GetRepo(owner, repoName)
	if repo == nil {
		return textResponse(404, "Not Found"), nil
	}
	v2 := gitProtocol == "version=2"
	packets, err := DecodePktLines(body, true)
	if err != nil {
		return Response{}, &ProtocolError{"malformed pkt-line"}
	}
	if len(packets) == 0 || packets[0].Kind != "data" {
		return Response{}, &ProtocolError{"malformed pkt-line"}
	}
	contentType := "application/x-git-upload-pack-result"
	if v2 {
		request := parseV2Request(packets)
		if request.command == "ls-refs" {
			return gitResponse(200, contentType, cachedLsRefs(repo, request.args)), nil
		}
		if request.command == "fetch" {
			resp, err := fetchV2(f, repo, request.args)
			if err != nil {
				return Response{}, err
			}
			return gitResponse(200, contentType, resp), nil
		}
		return Response{}, &ProtocolError{"unknown command"}
	}
	resp, err := fetchV0(f, repo, packets)
	if err != nil {
		return Response{}, err
	}
	return gitResponse(200, contentType, resp), nil
}

type v2Request struct {
	command string
	args    []string
}

func parseV2Request(packets []GitPacket) v2Request {
	split := -1
	for i, p := range packets {
		if p.Kind == "delim" || p.Kind == "flush" {
			split = i
			break
		}
	}
	if split < 0 {
		split = len(packets)
	}
	headerLines := linesOf(packets[:split])
	var args []string
	if split+1 <= len(packets) {
		args = linesOf(packets[split+1:])
	}
	command := ""
	for _, line := range headerLines {
		if strings.HasPrefix(line, "command=") {
			command = strings.TrimSpace(line[len("command="):])
		}
	}
	return v2Request{command: command, args: args}
}

func linesOf(packets []GitPacket) []string {
	out := []string{}
	for _, line := range PacketLines(packets) {
		for _, raw := range strings.Split(line, "\n") {
			raw = strings.TrimSuffix(raw, "\r")
			if len(raw) > 0 {
				out = append(out, raw)
			}
		}
	}
	return out
}

func lsRefs(repo *Repo, args []string) []byte {
	wantPeel := false
	wantSymrefs := false
	wantUnborn := false
	var prefixes []string
	for _, arg := range args {
		switch {
		case arg == "peel":
			wantPeel = true
		case arg == "symrefs":
			wantSymrefs = true
		case arg == "unborn":
			wantUnborn = true
		case arg == "ref-prefix":
			prefixes = append(prefixes, "")
		case strings.HasPrefix(arg, "ref-prefix "):
			prefixes = append(prefixes, strings.TrimSpace(arg[len("ref-prefix "):]))
		}
	}
	_ = wantUnborn
	matches := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	var out []byte
	refs := sortedRefs(repo)
	headRef := "refs/heads/" + repo.DefaultBranch
	headOid := repo.Refs[headRef]
	if headOid == "" {
		headOid = ZeroOID
	}
	if len(refs) != 0 {
		if headOid != ZeroOID && matches("HEAD") {
			line := headOid + " HEAD"
			if wantSymrefs {
				line += " symref-target:" + headRef
			}
			out = append(out, EncodePktLineString(line+"\n")...)
		}
		for _, ref := range refs {
			if !matches(ref.name) {
				continue
			}
			line := ref.oid + " " + ref.name
			if wantPeel && strings.HasPrefix(ref.name, "refs/tags/") {
				if obj := repo.Objects[ref.oid]; obj != nil && obj.Type == "tag" {
					if peeled := peelTag(repo, ref.oid); peeled != "" {
						line += " peeled:" + peeled
					}
				}
			}
			out = append(out, EncodePktLineString(line+"\n")...)
		}
	}
	out = append(out, PktLineFlush()...)
	return out
}

type FetchArgs struct {
	Wants         []string
	Haves         []string
	ClientShallow []string
	Deepen        int
	Done          bool
}

func parseFetchArgs(args []string) FetchArgs {
	result := FetchArgs{}
	deepenSince := false
	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "want "):
			oid := firstField(strings.TrimSpace(arg[5:]))
			if IsObjectID(oid) {
				result.Wants = append(result.Wants, oid)
			}
		case strings.HasPrefix(arg, "have "):
			oid := firstField(strings.TrimSpace(arg[5:]))
			if IsObjectID(oid) {
				result.Haves = append(result.Haves, oid)
			}
		case strings.HasPrefix(arg, "shallow "):
			oid := firstField(strings.TrimSpace(arg[8:]))
			if IsObjectID(oid) {
				result.ClientShallow = append(result.ClientShallow, oid)
			}
		case strings.HasPrefix(arg, "deepen "):
			var n int
			fmt.Sscanf(strings.TrimSpace(arg[7:]), "%d", &n)
			if n > 0 {
				result.Deepen = n
			}
		case arg == "deepen-since" || strings.HasPrefix(arg, "deepen-since "):
			deepenSince = true
		case arg == "done":
			result.Done = true
		}
	}
	if result.Deepen == 0 && deepenSince {
		result.Deepen = 1
	}
	return result
}

func firstField(s string) string {
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		return s[:i]
	}
	return s
}

// Caches (bounded FIFO, 64 entries each) — identical semantics to TS.
const cacheMax = 64

var fetchCache = newOrderedCache(cacheMax)
var advertisementCache = newOrderedCache(cacheMax)
var lsRefsCache = newOrderedCache(cacheMax)

type orderedCache struct {
	max int
	m   map[string][]byte
	ord []string
}

func newOrderedCache(max int) *orderedCache {
	return &orderedCache{max: max, m: make(map[string][]byte)}
}

func (c *orderedCache) get(key string) ([]byte, bool) {
	v, ok := c.m[key]
	return v, ok
}

func (c *orderedCache) set(key string, value []byte) {
	if _, ok := c.m[key]; !ok {
		if len(c.ord) >= c.max {
			delete(c.m, c.ord[0])
			c.ord = c.ord[1:]
		}
		c.ord = append(c.ord, key)
	}
	c.m[key] = value
}

func cachedLsRefs(repo *Repo, args []string) []byte {
	key := fmt.Sprintf("%d:%d:%s", repo.ID, repo.Version, strings.Join(args, "\n"))
	if cached, ok := lsRefsCache.get(key); ok {
		return cached
	}
	built := lsRefs(repo, args)
	lsRefsCache.set(key, built)
	return built
}

func cachedAdvertisementV0(repo *Repo, service, caps string) []byte {
	key := fmt.Sprintf("%d:%d:%s", repo.ID, repo.Version, service)
	if cached, ok := advertisementCache.get(key); ok {
		return cached
	}
	built := advertisementV0(repo, service, caps)
	advertisementCache.set(key, built)
	return built
}

func fetchV2(f *Forge, repo *Repo, args []string) ([]byte, error) {
	request := parseFetchArgs(args)
	if err := validateWants(repo, request.Wants); err != nil {
		return nil, err
	}
	depth := request.Deepen
	cacheKey := fmt.Sprintf("%d|%d|%s|%s|%d|%s",
		repo.ID, repo.Version,
		strings.Join(request.Wants, ","),
		strings.Join(request.Haves, ","),
		depth,
		strings.Join(request.ClientShallow, ","))
	if cached, ok := fetchCache.get(cacheKey); ok {
		return cached, nil
	}
	built, err := buildFetchV2(f, repo, request, depth)
	if err != nil {
		return nil, err
	}
	fetchCache.set(cacheKey, built)
	return built, nil
}

func buildFetchV2(f *Forge, repo *Repo, request FetchArgs, depth int) ([]byte, error) {
	ack := acknowledgesV2(f, repo, request.Wants, request.Haves)
	collected := collectObjects(repo, request.Wants, request.Haves, depth, request.ClientShallow)
	packObjs := make([]PackObject, 0, len(collected.objects))
	for _, o := range collected.objects {
		packObjs = append(packObjs, PackObject{Type: o.typ, Data: o.data, OID: o.oid})
	}
	pack := WritePack(packObjs)
	var out []byte
	out = append(out, EncodePktLineString("acknowledgments\n")...)
	for _, line := range ack {
		out = append(out, EncodePktLineString(line)...)
	}
	out = append(out, PktLineFlush()...)
	if depth > 0 || len(collected.unshallow) > 0 {
		out = append(out, EncodePktLineString("shallow-info\n")...)
		for _, oid := range collected.shallow {
			out = append(out, EncodePktLineString("shallow "+oid+"\n")...)
		}
		for _, oid := range collected.unshallow {
			out = append(out, EncodePktLineString("unshallow "+oid+"\n")...)
		}
		out = append(out, PktLineFlush()...)
	}
	out = append(out, EncodePktLineString("packfile\n")...)
	out = append(out, SidebandPack(pack)...)
	out = append(out, PktLineFlush()...)
	return out, nil
}

func fetchV0(f *Forge, repo *Repo, packets []GitPacket) ([]byte, error) {
	first, second := splitOnFlush(packets)
	firstLines := linesOf(first)
	secondLines := linesOf(second)
	var wants, clientShallow []string
	deepen := 0
	deepenSince := false
	for _, line := range firstLines {
		switch {
		case strings.HasPrefix(line, "want "):
			oid := firstField(strings.TrimSpace(line[5:]))
			if IsObjectID(oid) {
				wants = append(wants, oid)
			}
		case strings.HasPrefix(line, "shallow "):
			oid := firstField(strings.TrimSpace(line[8:]))
			if IsObjectID(oid) {
				clientShallow = append(clientShallow, oid)
			}
		case line == "deepen-since" || strings.HasPrefix(line, "deepen-since "):
			deepenSince = true
		case strings.HasPrefix(line, "deepen "):
			var n int
			fmt.Sscanf(strings.TrimSpace(line[7:]), "%d", &n)
			if n > 0 {
				deepen = n
			}
		}
	}
	if deepen == 0 && deepenSince {
		deepen = 1
	}
	var haves []string
	for _, line := range secondLines {
		if strings.HasPrefix(line, "have ") {
			oid := firstField(strings.TrimSpace(line[5:]))
			if IsObjectID(oid) {
				haves = append(haves, oid)
			}
		}
	}
	if len(wants) == 0 {
		return nil, &ProtocolError{"no wants"}
	}
	if err := validateWants(repo, wants); err != nil {
		return nil, err
	}
	ack := acknowledgesV0(f, repo, wants, haves)
	collected := collectObjects(repo, wants, haves, deepen, clientShallow)
	packObjs := make([]PackObject, 0, len(collected.objects))
	for _, o := range collected.objects {
		packObjs = append(packObjs, PackObject{Type: o.typ, Data: o.data, OID: o.oid})
	}
	pack := WritePack(packObjs)
	var out []byte
	for _, line := range ack {
		out = append(out, EncodePktLineString(line)...)
	}
	for _, oid := range collected.shallow {
		out = append(out, EncodePktLineString("shallow "+oid+"\n")...)
	}
	for _, oid := range collected.unshallow {
		out = append(out, EncodePktLineString("unshallow "+oid+"\n")...)
	}
	out = append(out, SidebandPack(pack)...)
	out = append(out, PktLineFlush()...)
	return out, nil
}

func validateWants(repo *Repo, wants []string) error {
	for _, want := range wants {
		if _, ok := repo.Objects[want]; !ok {
			return &ProtocolError{"want not found"}
		}
	}
	return nil
}

func findAck(f *Forge, repo *Repo, wants, haves []string) string {
	for _, have := range haves {
		if !IsObjectID(have) {
			continue
		}
		if _, ok := repo.Objects[have]; !ok {
			continue
		}
		for _, want := range wants {
			if !IsObjectID(want) {
				continue
			}
			if _, ok := repo.Objects[want]; !ok {
				continue
			}
			if f.IsAncestor(repo, have, want) {
				return have
			}
		}
	}
	return ""
}

func acknowledgesV2(f *Forge, repo *Repo, wants, haves []string) []string {
	ack := findAck(f, repo, wants, haves)
	if ack == "" {
		return []string{"NAK\n"}
	}
	return []string{"ACK " + ack + "\n", "ready\n"}
}

func acknowledgesV0(f *Forge, repo *Repo, wants, haves []string) []string {
	ack := findAck(f, repo, wants, haves)
	if ack == "" {
		return []string{"NAK\n"}
	}
	return []string{"ACK " + ack + "\n"}
}

type collectedObject struct {
	oid  string
	typ  string
	data []byte
}

type collectResult struct {
	objects   []collectedObject
	shallow   []string
	unshallow []string
}

func collectObjects(repo *Repo, wants, haves []string, depth int, clientShallow []string) collectResult {
	traverseThrough := map[string]bool{}
	for _, oid := range clientShallow {
		if IsObjectID(oid) {
			if _, ok := repo.Objects[oid]; ok {
				traverseThrough[oid] = true
			}
		}
	}
	exclude := map[string]bool{}
	var validHaves []string
	for _, have := range haves {
		if IsObjectID(have) {
			if _, ok := repo.Objects[have]; ok {
				validHaves = append(validHaves, have)
			}
		}
	}
	if len(validHaves) > 0 {
		base := reachableObjects(repo, validHaves, reachOpts{stopAt: traverseThrough})
		for _, obj := range base.objects {
			exclude[obj.oid] = true
		}
	}
	var tips []string
	for _, want := range wants {
		if IsObjectID(want) {
			tips = append(tips, want)
		}
	}
	depthOpt := 0
	if depth > 0 {
		depthOpt = depth
	}
	return reachableObjects(repo, tips, reachOpts{depth: depthOpt, exclude: exclude, traverseThrough: traverseThrough})
}

func splitOnFlush(packets []GitPacket) ([]GitPacket, []GitPacket) {
	for i, p := range packets {
		if p.Kind == "flush" {
			return packets[:i], packets[i+1:]
		}
	}
	return packets, []GitPacket{}
}

type reachOpts struct {
	depth          int
	exclude        map[string]bool
	traverseThrough map[string]bool
	stopAt         map[string]bool
}

func reachableObjects(repo *Repo, tips []string, opts reachOpts) collectResult {
	depth := opts.depth
	exclude := opts.exclude
	if exclude == nil {
		exclude = map[string]bool{}
	}
	traverseThrough := opts.traverseThrough
	if traverseThrough == nil {
		traverseThrough = map[string]bool{}
	}
	included := map[string]collectedObject{}
	shallow := map[string]bool{}
	unshallow := map[string]bool{}
	bestDepth := map[string]int{}
	type item struct {
		oid   string
		depth int
	}
	var stack []item
	for _, tip := range tips {
		if IsObjectID(tip) && !exclude[tip] {
			stack = append(stack, item{tip, 1})
		} else if IsObjectID(tip) && traverseThrough[tip] {
			stack = append(stack, item{tip, 1})
		}
	}
	for len(stack) > 0 {
		it := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		oid := it.oid
		obj := repo.Objects[oid]
		if obj == nil {
			continue
		}
		isShallowOwned := exclude[oid] && traverseThrough[oid]
		if exclude[oid] && !isShallowOwned {
			continue
		}
		if obj.Type == "tag" {
			if _, ok := included[oid]; ok {
				continue
			}
			included[oid] = collectedObject{oid, obj.Type, obj.Data}
			if target := tagTarget(obj.Data); target != "" {
				stack = append(stack, item{target, it.depth})
			}
			continue
		}
		if obj.Type == "commit" {
			if depth > 0 && it.depth > depth {
				continue
			}
			if prev, ok := bestDepth[oid]; ok && prev <= it.depth {
				continue
			}
			bestDepth[oid] = it.depth
			if !isShallowOwned {
				if _, ok := included[oid]; !ok {
					included[oid] = collectedObject{oid, obj.Type, obj.Data}
				}
			}
			header := commitHeader(obj.Data)
			tree := commitTreeOf(header)
			if tree != "" && !isShallowOwned {
				addTreeObjects(repo, tree, included, exclude)
			}
			parents := commitParentsOf(header)
			if opts.stopAt != nil && opts.stopAt[oid] {
				// stop the walk here
			} else if depth > 0 && it.depth >= depth {
				if len(parents) > 0 {
					shallow[oid] = true
				}
			} else {
				delete(shallow, oid)
				if isShallowOwned {
					unshallow[oid] = true
				}
				for _, parent := range parents {
					if !exclude[parent] || traverseThrough[parent] {
						stack = append(stack, item{parent, it.depth + 1})
					}
				}
			}
			continue
		}
		if obj.Type == "tree" {
			addTreeObjects(repo, oid, included, exclude)
			continue
		}
		if _, ok := included[oid]; !ok {
			included[oid] = collectedObject{oid, obj.Type, obj.Data}
		}
	}
	rank := map[string]int{"commit": 0, "tag": 1, "tree": 2, "blob": 3}
	objects := make([]collectedObject, 0, len(included))
	for _, o := range included {
		objects = append(objects, o)
	}
	sort.Slice(objects, func(i, j int) bool {
		ri, rj := rank[objects[i].typ], rank[objects[j].typ]
		if ri != rj {
			return ri < rj
		}
		return objects[i].oid < objects[j].oid
	})
	shallowList := sortedKeys(shallow)
	unshallowList := sortedKeys(unshallow)
	return collectResult{objects: objects, shallow: shallowList, unshallow: unshallowList}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func addTreeObjects(repo *Repo, rootOid string, included map[string]collectedObject, exclude map[string]bool) {
	stack := []string{rootOid}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if oid == "" || exclude[oid] {
			continue
		}
		if _, ok := included[oid]; ok {
			continue
		}
		obj := repo.Objects[oid]
		if obj == nil || obj.Type != "tree" {
			continue
		}
		included[oid] = collectedObject{oid, "tree", obj.Data}
		for _, entry := range treeEntries(obj.Data) {
			if entry.mode == "160000" {
				continue
			}
			if exclude[entry.oid] {
				continue
			}
			if _, ok := included[entry.oid]; ok {
				continue
			}
			child := repo.Objects[entry.oid]
			if child == nil {
				continue
			}
			if child.Type == "tree" {
				stack = append(stack, entry.oid)
			} else {
				included[entry.oid] = collectedObject{entry.oid, child.Type, child.Data}
			}
		}
	}
}

type treeEntry struct {
	mode string
	oid  string
}

func treeEntries(data []byte) []treeEntry {
	out := []treeEntry{}
	offset := 0
	for offset < len(data) {
		space := indexByte(data, ' ', offset)
		if space == -1 {
			break
		}
		nul := indexByte(data, 0, space+1)
		if nul == -1 {
			break
		}
		oidEnd := nul + 21
		if oidEnd > len(data) {
			break
		}
		mode := string(data[offset:space])
		oid := hexEncode(data[nul+1 : oidEnd])
		if len(oid) != 40 {
			break
		}
		out = append(out, treeEntry{mode, oid})
		offset = oidEnd
	}
	return out
}

func hexEncode(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&0xf])
	}
	return string(out)
}

func commitHeader(data []byte) string {
	text := string(data)
	if idx := strings.Index(text, "\n\n"); idx >= 0 {
		return text[:idx]
	}
	return text
}

func commitTreeOf(header string) string {
	for _, line := range strings.Split(header, "\n") {
		if strings.HasPrefix(line, "tree ") && len(line) == 45 {
			return line[5:]
		}
	}
	return ""
}

func commitParentsOf(header string) []string {
	out := []string{}
	for _, line := range strings.Split(header, "\n") {
		if strings.HasPrefix(line, "parent ") && len(line) == 47 {
			out = append(out, line[7:])
		}
	}
	return out
}

func tagTarget(data []byte) string {
	header := commitHeader(data)
	for _, line := range strings.Split(header, "\n") {
		if strings.HasPrefix(line, "object ") && len(line) == 47 {
			return line[7:]
		}
	}
	return ""
}

func peelTag(repo *Repo, tagOid string) string {
	oid := tagOid
	seen := map[string]bool{}
	for oid != "" && !seen[oid] {
		seen[oid] = true
		obj := repo.Objects[oid]
		if obj == nil {
			return ""
		}
		if obj.Type != "tag" {
			return oid
		}
		target := tagTarget(obj.Data)
		if target == "" {
			return ""
		}
		oid = target
	}
	return ""
}

type refEntry struct {
	name string
	oid  string
}

func sortedRefs(repo *Repo) []refEntry {
	refs := make([]refEntry, 0, len(repo.Refs))
	for name, oid := range repo.Refs {
		refs = append(refs, refEntry{name, oid})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].name < refs[j].name })
	return refs
}

func handleReceivePack(f *Forge, owner, repoName string, body []byte) (Response, error) {
	repo := f.GetRepo(owner, repoName)
	if repo == nil {
		return textResponse(404, "Not Found"), nil
	}
	lines, pack, err := readCommands(body)
	if err != nil {
		return Response{}, err
	}
	if len(lines) == 0 {
		return Response{}, &ProtocolError{"empty command list"}
	}
	type pushCommand struct {
		oldOid, newOid, ref string
	}
	var commands []pushCommand
	sideband := false
	for index, line := range lines {
		if index == 0 {
			if nul := strings.Index(line, "\x00"); nul >= 0 {
				caps := strings.Fields(line[nul+1:])
				for _, c := range caps {
					if c == "side-band-64k" {
						sideband = true
					}
				}
				line = line[:nul]
			}
		}
		parts := strings.Fields(strings.TrimSpace(line))
		if len(parts) < 3 {
			return Response{}, &ProtocolError{"bad command"}
		}
		oldOid, newOid, ref := parts[0], parts[1], parts[2]
		if !IsObjectID(oldOid) || !IsObjectID(newOid) || !strings.HasPrefix(ref, "refs/") {
			return Response{}, &ProtocolError{"bad command"}
		}
		commands = append(commands, pushCommand{oldOid, newOid, ref})
	}
	needsPack := false
	for _, c := range commands {
		if c.newOid != ZeroOID {
			needsPack = true
		}
	}
	unpackOk := true
	if needsPack {
		if len(pack) == 0 {
			unpackOk = false
		} else {
			parsed, err := ReadPack(pack, func(oid string) *GitObject {
				return repo.Objects[oid]
			})
			if err != nil {
				unpackOk = false
			} else {
				for _, obj := range parsed.Objects {
					if _, ok := repo.Objects[obj.OID]; !ok {
						repo.Objects[obj.OID] = &GitObject{Type: obj.Type, Data: obj.Data}
					}
				}
			}
		}
	}
	var report []string
	if !unpackOk {
		report = append(report, "unpack error\n")
		for _, c := range commands {
			report = append(report, "ng "+c.ref+" unpacker error\n")
		}
	} else {
		report = append(report, "unpack ok\n")
		for _, c := range commands {
			ok, reason := applyPush(f, repo, c.oldOid, c.newOid, c.ref)
			if ok {
				report = append(report, "ok "+c.ref+"\n")
			} else {
				report = append(report, "ng "+c.ref+" "+reason+"\n")
			}
		}
	}
	return gitResponse(200, "application/x-git-receive-pack-result", encodePushReport(report, sideband)), nil
}

func readCommands(body []byte) ([]string, []byte, error) {
	var lines []string
	offset := 0
	foundFlush := false
	for offset+4 <= len(body) {
		header := string(body[offset : offset+4])
		if !isHex4(header) {
			return nil, nil, &ProtocolError{"bad pkt-line length"}
		}
		length := hex4(header)
		if length == 0 {
			offset += 4
			foundFlush = true
			break
		}
		if length == 1 || length == 2 {
			offset += 4
			continue
		}
		if length < 4 || offset+length > len(body) {
			return nil, nil, &ProtocolError{"truncated pkt-line"}
		}
		payload := body[offset+4 : offset+length]
		offset += length
		text := string(payload)
		for _, raw := range strings.Split(text, "\n") {
			line := strings.TrimSuffix(raw, "\r")
			if len(line) > 0 {
				lines = append(lines, line)
			}
		}
	}
	if !foundFlush {
		return nil, nil, &ProtocolError{"missing flush"}
	}
	return lines, body[offset:], nil
}

func applyPush(f *Forge, repo *Repo, oldOid, newOid, ref string) (bool, string) {
	current := repo.Refs[ref]
	if current == "" {
		current = ZeroOID
	}
	if newOid == ZeroOID {
		if current != ZeroOID {
			f.DeleteRef(repo, ref)
		}
		return true, ""
	}
	if oldOid == ZeroOID {
		if current != ZeroOID {
			return false, "already exists"
		}
		if _, ok := repo.Objects[newOid]; !ok {
			return false, "missing object"
		}
		f.SetRef(repo, ref, newOid)
		return true, ""
	}
	if current != oldOid {
		return false, "non-fast-forward"
	}
	if _, ok := repo.Objects[newOid]; !ok {
		return false, "missing object"
	}
	if !f.IsAncestor(repo, oldOid, newOid) {
		return false, "non-fast-forward"
	}
	f.SetRef(repo, ref, newOid)
	return true, ""
}

func encodePushReport(lines []string, sideband bool) []byte {
	var inner []byte
	for _, line := range lines {
		inner = append(inner, EncodePktLineString(line)...)
	}
	if sideband {
		out := SidebandPack(inner)
		out = append(out, PktLineFlush()...)
		return out
	}
	out := append([]byte{}, inner...)
	out = append(out, PktLineFlush()...)
	return out
}
