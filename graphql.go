// GraphQL over POST /api/graphql: a small tokenizer, parser, and executor
// for the query/mutation surface the forge supports, plus introspection.
// Ported from graphql.ts — same fields, same resolvers, same clock ticks.
package main

import (
	"encoding/base64"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const gqlZeroOID = "0000000000000000000000000000000000000000"

// ---- AST ----

type gqlValueNode struct {
	kind   string // Variable, Int, Float, String, Boolean, Null, Enum, List, Object
	name   string
	num    float64
	str    string
	b      bool
	values []*gqlValueNode
	fields []gqlArg
}

type gqlArg struct {
	name  string
	value *gqlValueNode
}

type gqlField struct {
	name       string
	alias      string
	args       []gqlArg
	selections []gqlSelection
}

type gqlSelection struct {
	kind       string // Field, FragmentSpread, InlineFragment
	field      *gqlField
	fragName   string
	typeCond   string
	hasCond    bool
	selections []gqlSelection
}

type gqlOperation struct {
	operation   string // query | mutation
	name        string
	varDefs     []gqlVarDef
	selections  []gqlSelection
}

type gqlVarDef struct {
	name         string
	defaultValue *gqlValueNode
}

type gqlFragment struct {
	name        string
	typeCond    string
	selections  []gqlSelection
}

type gqlDocument struct {
	operations []*gqlOperation
	fragments  map[string]*gqlFragment
}

// ---- field tables ----

var gqlTypeFieldOrder = map[string][]string{
	"Query":                          {"viewer", "repository", "node", "__type"},
	"User":                           {"id", "login", "name", "email"},
	"Repository":                     {"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "description", "createdAt", "updatedAt", "isPrivate"},
	"Ref":                            {"name"},
	"Issue":                          {"id", "number", "title", "body", "state", "createdAt", "updatedAt", "author", "comments", "labels", "url", "closedAt"},
	"IssueConnection":                {"nodes", "pageInfo", "totalCount"},
	"PageInfo":                       {"hasNextPage", "endCursor"},
	"PullRequest":                    {"id", "number", "title", "body", "state", "merged", "mergeable", "headRefName", "baseRefName", "files", "headRef", "baseRef", "createdAt", "updatedAt", "closedAt", "mergeCommit"},
	"PullRequestConnection":          {"nodes", "pageInfo", "totalCount"},
	"PullRequestChangedFile":         {"path", "status", "additions", "deletions", "changes"},
	"PullRequestChangedFileConnection": {"nodes", "pageInfo", "totalCount"},
	"IssueComment":                   {"id", "body", "createdAt"},
	"IssueCommentConnection":         {"nodes", "pageInfo", "totalCount"},
	"CommentEdge":                    {"node", "cursor"},
	"Mutation":                       {"createIssue", "closeIssue", "createPullRequest", "mergePullRequest", "addComment"},
	"CreateIssuePayload":             {"issue", "clientMutationId"},
	"CloseIssuePayload":              {"issue", "clientMutationId"},
	"CreatePullRequestPayload":       {"pullRequest", "clientMutationId"},
	"MergePullRequestPayload":        {"pullRequest", "clientMutationId"},
	"AddCommentPayload":              {"commentEdge", "clientMutationId"},
	"__Type":                         {"fields", "enumValues", "name", "kind"},
	"__Field":                        {"name", "description"},
	"__EnumValue":                    {"name", "description"},
	"SearchType":                     {},
	"Label":                          {"id", "name", "color", "description"},
	"LabelConnection":                {"nodes", "pageInfo", "totalCount"},
}

var gqlTypeFieldSets = map[string]map[string]bool{}

func init() {
	for t, fields := range gqlTypeFieldOrder {
		set := map[string]bool{}
		for _, f := range fields {
			set[f] = true
		}
		gqlTypeFieldSets[t] = set
	}
}

var gqlSearchTypeEnum = []string{"ISSUE", "PULL_REQUEST", "DISCUSSION", "COMMIT", "REPOSITORY", "USER"}

// ---- ids / cursors ----

func gqlGlobalID(typeName, key string) string {
	return base64.StdEncoding.EncodeToString([]byte(typeName + ":" + key))
}

var gqlGlobalIDRe = regexp.MustCompile(`^([A-Za-z]+):(.+)$`)
var gqlCursorRe = regexp.MustCompile(`^cursor:(\d+)$`)

func gqlDecodeB64Lenient(s string) ([]byte, bool) {
	// Node's Buffer.from(s, 'base64') ignores characters outside the alphabet
	// and tolerates missing padding.
	var clean strings.Builder
	for _, c := range s {
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/' || c == '=' {
			clean.WriteRune(c)
		}
	}
	t := clean.String()
	for len(t)%4 != 0 {
		t += "="
	}
	b, err := base64.StdEncoding.DecodeString(t)
	if err != nil {
		return nil, false
	}
	return b, true
}

func gqlDecodeGlobalID(id any) (typ, key string, ok bool) {
	s, isStr := id.(string)
	if !isStr {
		return "", "", false
	}
	decoded, good := gqlDecodeB64Lenient(s)
	if !good {
		return "", "", false
	}
	m := gqlGlobalIDRe.FindStringSubmatch(string(decoded))
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

func gqlCursorFor(n int) string {
	return base64.StdEncoding.EncodeToString([]byte("cursor:" + strconv.Itoa(n)))
}

func gqlDecodeCursor(cursor any) (int, bool) {
	s, isStr := cursor.(string)
	if !isStr {
		return 0, false
	}
	decoded, good := gqlDecodeB64Lenient(s)
	if !good {
		return 0, false
	}
	m := gqlCursorRe.FindStringSubmatch(string(decoded))
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return n, true
}

// ---- value resolution ----

func gqlResolveValue(node *gqlValueNode, variables map[string]any) any {
	switch node.kind {
	case "Variable":
		return variables[node.name]
	case "Int", "Float":
		return node.num
	case "String":
		return node.str
	case "Boolean":
		return node.b
	case "Null":
		return nil
	case "Enum":
		return node.str
	case "List":
		out := make([]any, 0, len(node.values))
		for _, v := range node.values {
			out = append(out, gqlResolveValue(v, variables))
		}
		return out
	case "Object":
		out := map[string]any{}
		for _, f := range node.fields {
			out[f.name] = gqlResolveValue(f.value, variables)
		}
		return out
	}
	return nil
}

// ---- tokenizer ----

type gqlToken struct {
	kind  string
	value string
}

func gqlIsNameStart(ch byte) bool {
	return ch == '_' || ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z'
}

func gqlIsNameContinue(ch byte) bool {
	return gqlIsNameStart(ch) || ch >= '0' && ch <= '9'
}

func gqlIsDigit(ch byte) bool { return ch >= '0' && ch <= '9' }

func gqlTokenize(source string) ([]gqlToken, error) {
	var tokens []gqlToken
	i := 0
	length := len(source)
	for i < length {
		ch := source[i]
		if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' || ch == ',' || ch == 0xEF && i+2 < length && source[i+1] == 0xBB && source[i+2] == 0xBF {
			// BOM (\uFEFF) is 3 bytes in UTF-8; skip it as one unit.
			if ch == 0xEF {
				i += 3
			} else {
				i++
			}
			continue
		}
		if ch == '#' {
			i++
			for i < length && source[i] != '\n' && source[i] != '\r' {
				i++
			}
			continue
		}
		if strings.HasPrefix(source[i:], "...") {
			tokens = append(tokens, gqlToken{kind: "...", value: "..."})
			i += 3
			continue
		}
		switch ch {
		case '{', '}', '(', ')', ':', '!', '=', '$', '@', '[', ']':
			tokens = append(tokens, gqlToken{kind: string(ch), value: string(ch)})
			i++
			continue
		}
		if gqlIsNameStart(ch) {
			start := i
			i++
			for i < length && gqlIsNameContinue(source[i]) {
				i++
			}
			tokens = append(tokens, gqlToken{kind: "Name", value: source[start:i]})
			continue
		}
		if ch == '-' || gqlIsDigit(ch) {
			start := i
			if ch == '-' {
				i++
			}
			if i >= length || !gqlIsDigit(source[i]) {
				return nil, fmt.Errorf("Syntax Error: Invalid number at offset %d.", start)
			}
			for i < length && gqlIsDigit(source[i]) {
				i++
			}
			isFloat := false
			if i < length && source[i] == '.' {
				isFloat = true
				i++
				if i >= length || !gqlIsDigit(source[i]) {
					return nil, fmt.Errorf("Syntax Error: Invalid number at offset %d.", start)
				}
				for i < length && gqlIsDigit(source[i]) {
					i++
				}
			}
			if i < length && (source[i] == 'e' || source[i] == 'E') {
				isFloat = true
				i++
				if i < length && (source[i] == '+' || source[i] == '-') {
					i++
				}
				if i >= length || !gqlIsDigit(source[i]) {
					return nil, fmt.Errorf("Syntax Error: Invalid number at offset %d.", start)
				}
				for i < length && gqlIsDigit(source[i]) {
					i++
				}
			}
			kind := "Int"
			if isFloat {
				kind = "Float"
			}
			tokens = append(tokens, gqlToken{kind: kind, value: source[start:i]})
			continue
		}
		if ch == '"' {
			i++
			var sb strings.Builder
			closed := false
			for i < length {
				cur := source[i]
				i++
				if cur == '"' {
					closed = true
					break
				}
				if cur == '\\' {
					if i >= length {
						return nil, fmt.Errorf("Syntax Error: Unterminated string escape.")
					}
					esc := source[i]
					i++
					switch esc {
					case '"':
						sb.WriteByte('"')
					case '\\':
						sb.WriteByte('\\')
					case '/':
						sb.WriteByte('/')
					case 'n':
						sb.WriteByte('\n')
					case 'r':
						sb.WriteByte('\r')
					case 't':
						sb.WriteByte('\t')
					case 'b':
						sb.WriteByte('\b')
					case 'f':
						sb.WriteByte('\f')
					case 'u':
						if i+4 > length {
							return nil, fmt.Errorf("Syntax Error: Invalid unicode escape.")
						}
						hexStr := source[i : i+4]
						n, err := strconv.ParseInt(hexStr, 16, 32)
						if err != nil || !gqlIsHex4(hexStr) {
							return nil, fmt.Errorf("Syntax Error: Invalid unicode escape.")
						}
						sb.WriteRune(rune(n))
						i += 4
					default:
						return nil, fmt.Errorf("Syntax Error: Invalid string escape \\%c.", esc)
					}
					continue
				}
				if cur == '\n' || cur == '\r' {
					return nil, fmt.Errorf("Syntax Error: Unterminated string.")
				}
				sb.WriteByte(cur)
			}
			if !closed {
				return nil, fmt.Errorf("Syntax Error: Unterminated string.")
			}
			tokens = append(tokens, gqlToken{kind: "String", value: sb.String()})
			continue
		}
		return nil, fmt.Errorf("Syntax Error: Unexpected character %s.", strconv.Quote(string(ch)))
	}
	tokens = append(tokens, gqlToken{kind: "EOF", value: ""})
	return tokens, nil
}

func gqlIsHex4(s string) bool {
	if len(s) != 4 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// ---- parser ----

type gqlParser struct {
	tokens []gqlToken
	pos    int
}

func (p *gqlParser) peek(offset ...int) gqlToken {
	off := 0
	if len(offset) > 0 {
		off = offset[0]
	}
	if p.pos+off < len(p.tokens) {
		return p.tokens[p.pos+off]
	}
	return gqlToken{kind: "EOF", value: ""}
}

func (p *gqlParser) advance() gqlToken {
	tok := p.peek()
	if tok.kind != "EOF" {
		p.pos++
	}
	return tok
}

func (p *gqlParser) match(kind string) bool {
	if p.peek().kind == kind {
		p.advance()
		return true
	}
	return false
}

func (p *gqlParser) expect(kind string) (gqlToken, error) {
	tok := p.peek()
	if tok.kind != kind {
		return tok, fmt.Errorf("Syntax Error: Expected %q.", kind)
	}
	return p.advance(), nil
}

func (p *gqlParser) expectName() (string, error) {
	tok, err := p.expect("Name")
	if err != nil {
		return "", err
	}
	return tok.value, nil
}

func (p *gqlParser) expectKeyword(word string) error {
	tok, err := p.expect("Name")
	if err != nil {
		return err
	}
	if tok.value != word {
		return fmt.Errorf("Syntax Error: Expected %q.", word)
	}
	return nil
}

func (p *gqlParser) fail(msg string) error {
	return fmt.Errorf("Syntax Error: %s", msg)
}

func (p *gqlParser) parse() (*gqlDocument, error) {
	doc := &gqlDocument{fragments: map[string]*gqlFragment{}}
	for p.peek().kind != "EOF" {
		tok := p.peek()
		if tok.kind == "{" {
			sels, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			doc.operations = append(doc.operations, &gqlOperation{operation: "query", selections: sels})
			continue
		}
		if tok.kind == "Name" && (tok.value == "query" || tok.value == "mutation") {
			op, err := p.parseOperation()
			if err != nil {
				return nil, err
			}
			doc.operations = append(doc.operations, op)
			continue
		}
		if tok.kind == "Name" && tok.value == "fragment" {
			frag, err := p.parseFragment()
			if err != nil {
				return nil, err
			}
			doc.fragments[frag.name] = frag
			continue
		}
		return nil, p.fail("Expected query, mutation, fragment, or selection set.")
	}
	return doc, nil
}

func (p *gqlParser) parseOperation() (*gqlOperation, error) {
	opTok, err := p.expect("Name")
	if err != nil {
		return nil, err
	}
	operation := "query"
	if opTok.value == "mutation" {
		operation = "mutation"
	}
	var name string
	if p.peek().kind == "Name" {
		name, err = p.expectName()
		if err != nil {
			return nil, err
		}
	}
	if err := p.skipDirectives(); err != nil {
		return nil, err
	}
	var varDefs []gqlVarDef
	if p.match("(") {
		for p.peek().kind != ")" {
			if p.peek().kind == "EOF" {
				return nil, p.fail("Unterminated variable definitions.")
			}
			if _, err := p.expect("$"); err != nil {
				return nil, err
			}
			varName, err := p.expectName()
			if err != nil {
				return nil, err
			}
			if _, err := p.expect(":"); err != nil {
				return nil, err
			}
			if err := p.parseTypeReference(); err != nil {
				return nil, err
			}
			var defVal *gqlValueNode
			if p.match("=") {
				defVal, err = p.parseValue()
				if err != nil {
					return nil, err
				}
			}
			varDefs = append(varDefs, gqlVarDef{name: varName, defaultValue: defVal})
		}
		if _, err := p.expect(")"); err != nil {
			return nil, err
		}
	}
	if err := p.skipDirectives(); err != nil {
		return nil, err
	}
	sels, err := p.parseSelectionSet()
	if err != nil {
		return nil, err
	}
	return &gqlOperation{operation: operation, name: name, varDefs: varDefs, selections: sels}, nil
}

func (p *gqlParser) parseFragment() (*gqlFragment, error) {
	if err := p.expectKeyword("fragment"); err != nil {
		return nil, err
	}
	name, err := p.expectName()
	if err != nil {
		return nil, err
	}
	if err := p.expectKeyword("on"); err != nil {
		return nil, err
	}
	typeCond, err := p.expectName()
	if err != nil {
		return nil, err
	}
	sels, err := p.parseSelectionSet()
	if err != nil {
		return nil, err
	}
	return &gqlFragment{name: name, typeCond: typeCond, selections: sels}, nil
}

func (p *gqlParser) parseSelectionSet() ([]gqlSelection, error) {
	if _, err := p.expect("{"); err != nil {
		return nil, err
	}
	var sels []gqlSelection
	for p.peek().kind != "}" {
		if p.peek().kind == "EOF" {
			return nil, p.fail("Unterminated selection set.")
		}
		sel, err := p.parseSelection()
		if err != nil {
			return nil, err
		}
		sels = append(sels, sel)
	}
	if _, err := p.expect("}"); err != nil {
		return nil, err
	}
	return sels, nil
}

func (p *gqlParser) parseSelection() (gqlSelection, error) {
	if p.peek().kind == "..." {
		p.advance()
		if p.peek().kind == "Name" && p.peek().value == "on" {
			p.advance()
			typeCond, err := p.expectName()
			if err != nil {
				return gqlSelection{}, err
			}
			sels, err := p.parseSelectionSet()
			if err != nil {
				return gqlSelection{}, err
			}
			return gqlSelection{kind: "InlineFragment", typeCond: typeCond, hasCond: true, selections: sels}, nil
		}
		if p.peek().kind == "Name" {
			name, err := p.expectName()
			if err != nil {
				return gqlSelection{}, err
			}
			return gqlSelection{kind: "FragmentSpread", fragName: name}, nil
		}
		if p.peek().kind == "{" {
			sels, err := p.parseSelectionSet()
			if err != nil {
				return gqlSelection{}, err
			}
			return gqlSelection{kind: "InlineFragment", selections: sels}, nil
		}
		return gqlSelection{}, p.fail("Expected fragment name, type condition, or selection set.")
	}
	firstName, err := p.expectName()
	if err != nil {
		return gqlSelection{}, err
	}
	alias := ""
	name := firstName
	if p.match(":") {
		alias = firstName
		name, err = p.expectName()
		if err != nil {
			return gqlSelection{}, err
		}
	}
	var args []gqlArg
	if p.peek().kind == "(" {
		args, err = p.parseArguments()
		if err != nil {
			return gqlSelection{}, err
		}
	}
	if err := p.skipDirectives(); err != nil {
		return gqlSelection{}, err
	}
	var sels []gqlSelection
	if p.peek().kind == "{" {
		sels, err = p.parseSelectionSet()
		if err != nil {
			return gqlSelection{}, err
		}
	}
	return gqlSelection{kind: "Field", field: &gqlField{name: name, alias: alias, args: args, selections: sels}}, nil
}

func (p *gqlParser) parseArguments() ([]gqlArg, error) {
	if _, err := p.expect("("); err != nil {
		return nil, err
	}
	var args []gqlArg
	for p.peek().kind != ")" {
		if p.peek().kind == "EOF" {
			return nil, p.fail("Unterminated argument list.")
		}
		name, err := p.expectName()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(":"); err != nil {
			return nil, err
		}
		val, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		args = append(args, gqlArg{name: name, value: val})
	}
	if _, err := p.expect(")"); err != nil {
		return nil, err
	}
	return args, nil
}

func (p *gqlParser) skipDirectives() error {
	for p.peek().kind == "@" {
		p.advance()
		if _, err := p.expectName(); err != nil {
			return err
		}
		if p.peek().kind == "(" && p.peek(1).kind == "Name" {
			if _, err := p.parseArguments(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *gqlParser) parseTypeReference() error {
	if p.match("[") {
		if err := p.parseTypeReference(); err != nil {
			return err
		}
		if _, err := p.expect("]"); err != nil {
			return err
		}
	} else {
		if _, err := p.expectName(); err != nil {
			return err
		}
	}
	p.match("!")
	return nil
}

func (p *gqlParser) parseValue() (*gqlValueNode, error) {
	tok := p.peek()
	switch tok.kind {
	case "$":
		p.advance()
		name, err := p.expectName()
		if err != nil {
			return nil, err
		}
		return &gqlValueNode{kind: "Variable", name: name}, nil
	case "String":
		p.advance()
		return &gqlValueNode{kind: "String", str: tok.value}, nil
	case "Int":
		p.advance()
		n, _ := strconv.ParseFloat(tok.value, 64)
		return &gqlValueNode{kind: "Int", num: n}, nil
	case "Float":
		p.advance()
		n, _ := strconv.ParseFloat(tok.value, 64)
		return &gqlValueNode{kind: "Float", num: n}, nil
	case "Name":
		p.advance()
		switch tok.value {
		case "true":
			return &gqlValueNode{kind: "Boolean", b: true}, nil
		case "false":
			return &gqlValueNode{kind: "Boolean", b: false}, nil
		case "null":
			return &gqlValueNode{kind: "Null"}, nil
		}
		return &gqlValueNode{kind: "Enum", str: tok.value}, nil
	case "[":
		p.advance()
		var values []*gqlValueNode
		for p.peek().kind != "]" {
			if p.peek().kind == "EOF" {
				return nil, p.fail("Unterminated list value.")
			}
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			values = append(values, v)
		}
		if _, err := p.expect("]"); err != nil {
			return nil, err
		}
		return &gqlValueNode{kind: "List", values: values}, nil
	case "{":
		p.advance()
		var fields []gqlArg
		for p.peek().kind != "}" {
			if p.peek().kind == "EOF" {
				return nil, p.fail("Unterminated object value.")
			}
			name, err := p.expectName()
			if err != nil {
				return nil, err
			}
			if _, err := p.expect(":"); err != nil {
				return nil, err
			}
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			fields = append(fields, gqlArg{name: name, value: v})
		}
		if _, err := p.expect("}"); err != nil {
			return nil, err
		}
		return &gqlValueNode{kind: "Object", fields: fields}, nil
	}
	return nil, p.fail("Expected value.")
}

func gqlParseDocument(source string) (*gqlDocument, error) {
	tokens, err := gqlTokenize(source)
	if err != nil {
		return nil, err
	}
	return (&gqlParser{tokens: tokens}).parse()
}

// ---- executor ----

// Views: wrapper values the executor carries. typeNameOf reads the
// typename; resolveField switches on the parent type and reads the payload.
type gqlRepoView struct{ repo *Repo }
type gqlIssueView struct {
	repo  *Repo
	issue *Issue
}
type gqlPullView struct {
	repo *Repo
	pull *Pull
}
type gqlCommentView struct {
	repoID    int
	issueNum  int
	comment   *IssueComment
}
type gqlRecord struct {
	typename string
	fields   map[string]any
}

func (r *gqlRecord) get(name string) any {
	if r == nil {
		return nil
	}
	return r.fields[name]
}

type gqlError struct {
	message string
}

type gqlResult struct {
	data   map[string]any
	errors []gqlError
}

type gqlExecContext struct {
	forge     *Forge
	doc       *gqlDocument
	variables map[string]any
	errors    []gqlError
}

func gqlTypeNameOf(value any, fallback string) string {
	switch v := value.(type) {
	case *gqlRepoView:
		return "Repository"
	case *gqlIssueView:
		return "Issue"
	case *gqlPullView:
		return "PullRequest"
	case *gqlCommentView:
		return "IssueComment"
	case *gqlRecord:
		return v.typename
	}
	return fallback
}

func gqlIsKnownField(typeName, fieldName string) bool {
	set, ok := gqlTypeFieldSets[typeName]
	return ok && set[fieldName]
}

func gqlPrepareVariables(op *gqlOperation, variables map[string]any) map[string]any {
	prepared := map[string]any{}
	for k, v := range variables {
		prepared[k] = v
	}
	for _, def := range op.varDefs {
		if def.defaultValue != nil {
			if cur, ok := prepared[def.name]; !ok || cur == nil {
				prepared[def.name] = gqlResolveValue(def.defaultValue, prepared)
			}
		}
	}
	return prepared
}

func gqlPickOperation(doc *gqlDocument, operationName string) *gqlOperation {
	if len(doc.operations) == 0 {
		return nil
	}
	if operationName != "" {
		for _, op := range doc.operations {
			if op.name == operationName {
				return op
			}
		}
	}
	return doc.operations[0]
}

func gqlExecuteOperation(f *Forge, doc *gqlDocument, op *gqlOperation, variables map[string]any) gqlResult {
	prepared := gqlPrepareVariables(op, variables)
	ctx := &gqlExecContext{forge: f, doc: doc, variables: prepared}
	rootType := "Query"
	if op.operation == "mutation" {
		rootType = "Mutation"
	}
	root := &gqlRecord{typename: rootType, fields: map[string]any{}}
	data := gqlExecuteSelections(ctx, root, rootType, op.selections)
	if len(ctx.errors) > 0 {
		return gqlResult{data: nil, errors: ctx.errors}
	}
	return gqlResult{data: data}
}

func gqlExecuteSelections(ctx *gqlExecContext, value any, typeName string, sels []gqlSelection) map[string]any {
	currentType := gqlTypeNameOf(value, typeName)
	result := map[string]any{}
	for _, sel := range sels {
		switch sel.kind {
		case "Field":
			key, fieldValue := gqlExecuteField(ctx, value, currentType, sel.field)
			result[key] = fieldValue
		case "InlineFragment":
			if !sel.hasCond || sel.typeCond == currentType {
				for k, v := range gqlExecuteSelections(ctx, value, currentType, sel.selections) {
					result[k] = v
				}
			}
		case "FragmentSpread":
			if frag, ok := ctx.doc.fragments[sel.fragName]; ok && frag.typeCond == currentType {
				for k, v := range gqlExecuteSelections(ctx, value, currentType, frag.selections) {
					result[k] = v
				}
			}
		}
	}
	return result
}

func gqlExecuteField(ctx *gqlExecContext, parent any, parentType string, field *gqlField) (string, any) {
	key := field.name
	if field.alias != "" {
		key = field.alias
	}
	if field.name == "__typename" {
		return key, parentType
	}
	if !gqlIsKnownField(parentType, field.name) {
		ctx.errors = append(ctx.errors, gqlError{message: fmt.Sprintf("Cannot query field %q on type %q.", field.name, parentType)})
		return key, nil
	}
	resolved, typeName := gqlResolveField(ctx, parent, parentType, field)
	value := resolved
	if value == nil {
		return key, nil
	}
	if len(field.selections) == 0 {
		return key, value
	}
	if arr, ok := value.([]any); ok {
		out := make([]any, 0, len(arr))
		for _, item := range arr {
			out = append(out, gqlExecuteSelections(ctx, item, gqlTypeNameOf(item, typeName), field.selections))
		}
		return key, out
	}
	return key, gqlExecuteSelections(ctx, value, gqlTypeNameOf(value, typeName), field.selections)
}

// ---- argument helpers ----

func gqlArgValue(field *gqlField, name string, variables map[string]any) any {
	for _, arg := range field.args {
		if arg.name == name {
			return gqlResolveValue(arg.value, variables)
		}
	}
	return nil
}

func gqlStringArg(field *gqlField, name string, variables map[string]any) (string, bool) {
	v := gqlArgValue(field, name, variables)
	s, ok := v.(string)
	return s, ok
}

func gqlNumberArg(field *gqlField, name string, variables map[string]any) (int, bool) {
	v := gqlArgValue(field, name, variables)
	f, ok := v.(float64)
	if !ok || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return int(math.Trunc(f)), true
}

func gqlFirstArg(field *gqlField, variables map[string]any) int {
	v := gqlArgValue(field, "first", variables)
	if f, ok := v.(float64); ok && !math.IsNaN(f) && !math.IsInf(f, 0) {
		n := int(math.Floor(f))
		if n < 0 {
			return 0
		}
		return n
	}
	return 30
}

func gqlIsRecord(v any) bool {
	_, ok := v.(map[string]any)
	return ok
}

// ---- resolvers ----

func gqlResolveField(ctx *gqlExecContext, parent any, parentType string, field *gqlField) (any, string) {
	f := ctx.forge
	vars := ctx.variables
	name := field.name

	switch parentType {
	case "Query":
		switch name {
		case "viewer":
			return &gqlRecord{typename: "User", fields: map[string]any{}}, "User"
		case "repository":
			owner, _ := gqlStringArg(field, "owner", vars)
			repoName, hasName := gqlStringArg(field, "name", vars)
			if owner != ViewerLogin || !hasName {
				return nil, "Repository"
			}
			repo := f.GetRepo(owner, repoName)
			if repo == nil {
				return nil, "Repository"
			}
			return &gqlRepoView{repo: repo}, "Repository"
		case "node":
			typ, key, ok := gqlDecodeGlobalID(gqlArgValue(field, "id", vars))
			if !ok {
				return nil, ""
			}
			switch typ {
			case "Repository":
				repo := gqlFindRepoByKey(f, key)
				if repo == nil {
					return nil, "Repository"
				}
				return &gqlRepoView{repo: repo}, "Repository"
			case "Issue":
				repo, issue := gqlFindIssueByKey(f, key)
				if issue == nil {
					return nil, "Issue"
				}
				return &gqlIssueView{repo: repo, issue: issue}, "Issue"
			case "PullRequest":
				repo, pull := gqlFindPullByKey(f, key)
				if pull == nil {
					return nil, "PullRequest"
				}
				return &gqlPullView{repo: repo, pull: pull}, "PullRequest"
			}
			return nil, ""
		case "__type":
			typeName, ok := gqlStringArg(field, "name", vars)
			if !ok {
				return nil, "__Type"
			}
			intro := gqlBuildTypeIntrospection(typeName)
			if intro == nil {
				return nil, "__Type"
			}
			return intro, "__Type"
		}
		return nil, ""
	case "User":
		switch name {
		case "id":
			return gqlGlobalID("User", "1000"), ""
		case "login":
			return Viewer.Login, ""
		case "name":
			return Viewer.Name, ""
		case "email":
			return Viewer.Email, ""
		}
		return nil, ""
	case "Repository":
		view := parent.(*gqlRepoView)
		repo := view.repo
		switch name {
		case "id":
			return gqlGlobalID("Repository", strconv.Itoa(repo.ID)), ""
		case "databaseId":
			return repo.ID, ""
		case "name":
			return repo.Name, ""
		case "nameWithOwner":
			return repo.FullName, ""
		case "owner":
			return &gqlRecord{typename: "User", fields: map[string]any{}}, "User"
		case "defaultBranchRef":
			tip := f.RefOid(repo, "refs/heads/"+repo.DefaultBranch)
			if tip == gqlZeroOID {
				return nil, "Ref"
			}
			return &gqlRecord{typename: "Ref", fields: map[string]any{"name": repo.DefaultBranch}}, "Ref"
		case "viewerPermission":
			return "ADMIN", ""
		case "hasIssuesEnabled":
			return true, ""
		case "issue":
			number, ok := gqlNumberArg(field, "number", vars)
			if !ok {
				return nil, "Issue"
			}
			issue, exists := repo.Issues[number]
			if !exists {
				return nil, "Issue"
			}
			return &gqlIssueView{repo: repo, issue: issue}, "Issue"
		case "issues":
			return gqlMakeIssueConnection(ctx, repo, field), "IssueConnection"
		case "pullRequests":
			return gqlMakePullRequestConnection(ctx, repo, field), "PullRequestConnection"
		case "pullRequest":
			number, ok := gqlNumberArg(field, "number", vars)
			if !ok {
				return nil, "PullRequest"
			}
			pull, exists := repo.Pulls[number]
			if !exists {
				return nil, "PullRequest"
			}
			return &gqlPullView{repo: repo, pull: pull}, "PullRequest"
		case "description":
			return repo.Description, ""
		case "createdAt":
			return repo.CreatedAt, ""
		case "updatedAt":
			return repo.UpdatedAt, ""
		case "isPrivate":
			return repo.Private, ""
		}
		return nil, ""
	case "Ref":
		rec := parent.(*gqlRecord)
		if name == "name" {
			return rec.get("name"), ""
		}
		return nil, ""
	case "Issue":
		view := parent.(*gqlIssueView)
		repo, issue := view.repo, view.issue
		switch name {
		case "id":
			return gqlGlobalID("Issue", strconv.Itoa(repo.ID)+":"+strconv.Itoa(issue.Number)), ""
		case "number":
			return issue.Number, ""
		case "title":
			return issue.Title, ""
		case "body":
			return issue.Body, ""
		case "state":
			if issue.State == "open" {
				return "OPEN", ""
			}
			return "CLOSED", ""
		case "createdAt":
			return issue.CreatedAt, ""
		case "updatedAt":
			return issue.UpdatedAt, ""
		case "author":
			return &gqlRecord{typename: "User", fields: map[string]any{}}, "User"
		case "comments":
			return gqlMakeIssueCommentConnection(repo, issue), "IssueCommentConnection"
		case "labels":
			return gqlMakeLabelConnection(repo, issue), "LabelConnection"
		case "url":
			return nil, ""
		case "closedAt":
			if issue.ClosedAt == "" {
				return nil, ""
			}
			return issue.ClosedAt, ""
		}
		return nil, ""
	case "IssueConnection", "PullRequestConnection", "PullRequestChangedFileConnection", "IssueCommentConnection", "LabelConnection":
		rec := parent.(*gqlRecord)
		switch name {
		case "nodes":
			nodes, _ := rec.get("nodes").([]any)
			return nodes, gqlConnectionNodeType(rec.typename)
		case "pageInfo":
			return rec.get("pageInfo"), "PageInfo"
		case "totalCount":
			return rec.get("totalCount"), ""
		}
		return nil, ""
	case "PageInfo":
		rec := parent.(*gqlRecord)
		switch name {
		case "hasNextPage":
			return rec.get("hasNextPage"), ""
		case "endCursor":
			return rec.get("endCursor"), ""
		}
		return nil, ""
	case "PullRequest":
		view := parent.(*gqlPullView)
		repo, pull := view.repo, view.pull
		switch name {
		case "id":
			return gqlGlobalID("PullRequest", strconv.Itoa(repo.ID)+":"+strconv.Itoa(pull.Number)), ""
		case "number":
			return pull.Number, ""
		case "title":
			return pull.Title, ""
		case "body":
			return pull.Body, ""
		case "state":
			if pull.Merged {
				return "MERGED", ""
			}
			if pull.State == "closed" {
				return "CLOSED", ""
			}
			return "OPEN", ""
		case "merged":
			return pull.Merged, ""
		case "mergeable":
			if pull.Merged || pull.State == "closed" {
				return "UNKNOWN", ""
			}
			baseTip := f.RefOid(repo, "refs/heads/"+pull.BaseRef)
			headTip := f.RefOid(repo, "refs/heads/"+pull.HeadRef)
			if f.IsMergeable(repo, baseTip, headTip) {
				return "MERGEABLE", ""
			}
			return "CONFLICTING", ""
		case "headRefName":
			return pull.HeadRef, ""
		case "baseRefName":
			return pull.BaseRef, ""
		case "files":
			return gqlMakePullFileConnection(f, repo, pull, field, vars), "PullRequestChangedFileConnection"
		case "headRef":
			return &gqlRecord{typename: "Ref", fields: map[string]any{"name": pull.HeadRef}}, "Ref"
		case "baseRef":
			return &gqlRecord{typename: "Ref", fields: map[string]any{"name": pull.BaseRef}}, "Ref"
		case "createdAt":
			return pull.CreatedAt, ""
		case "updatedAt":
			return pull.UpdatedAt, ""
		case "closedAt":
			if pull.ClosedAt == "" {
				return nil, ""
			}
			return pull.ClosedAt, ""
		case "mergeCommit":
			return nil, ""
		}
		return nil, ""
	case "PullRequestChangedFile":
		rec := parent.(*gqlRecord)
		switch name {
		case "path", "status", "additions", "deletions", "changes":
			return rec.get(name), ""
		}
		return nil, ""
	case "IssueComment":
		view := parent.(*gqlCommentView)
		comment := view.comment
		switch name {
		case "id":
			return gqlGlobalID("IssueComment", strconv.Itoa(view.repoID)+":"+strconv.Itoa(view.issueNum)+":"+strconv.Itoa(comment.ID)), ""
		case "body":
			return comment.Body, ""
		case "createdAt":
			return comment.CreatedAt, ""
		}
		return nil, ""
	case "CommentEdge":
		rec := parent.(*gqlRecord)
		switch name {
		case "node":
			return rec.get("node"), "IssueComment"
		case "cursor":
			return rec.get("cursor"), ""
		}
		return nil, ""
	case "Label":
		rec := parent.(*gqlRecord)
		switch name {
		case "id", "name", "color", "description":
			return rec.get(name), ""
		}
		return nil, ""
	case "Mutation":
		switch name {
		case "createIssue":
			return gqlResolveCreateIssue(ctx, field)
		case "closeIssue":
			return gqlResolveCloseIssue(ctx, field)
		case "createPullRequest":
			return gqlResolveCreatePullRequest(ctx, field)
		case "mergePullRequest":
			return gqlResolveMergePullRequest(ctx, field)
		case "addComment":
			return gqlResolveAddComment(ctx, field)
		}
		return nil, ""
	case "CreateIssuePayload", "CloseIssuePayload":
		rec := parent.(*gqlRecord)
		switch name {
		case "issue":
			return rec.get("issue"), "Issue"
		case "clientMutationId":
			return rec.get("clientMutationId"), ""
		}
		return nil, ""
	case "CreatePullRequestPayload", "MergePullRequestPayload":
		rec := parent.(*gqlRecord)
		switch name {
		case "pullRequest":
			return rec.get("pullRequest"), "PullRequest"
		case "clientMutationId":
			return rec.get("clientMutationId"), ""
		}
		return nil, ""
	case "AddCommentPayload":
		rec := parent.(*gqlRecord)
		switch name {
		case "commentEdge":
			return rec.get("commentEdge"), "CommentEdge"
		case "clientMutationId":
			return rec.get("clientMutationId"), ""
		}
		return nil, ""
	case "__Type":
		rec := parent.(*gqlRecord)
		switch name {
		case "name":
			return rec.get("name"), ""
		case "kind":
			return rec.get("kind"), ""
		case "fields":
			return rec.get("fields"), "__Field"
		case "enumValues":
			return rec.get("enumValues"), "__EnumValue"
		}
		return nil, ""
	case "__Field", "__EnumValue":
		rec := parent.(*gqlRecord)
		switch name {
		case "name":
			return rec.get("name"), ""
		case "description":
			return rec.get("description"), ""
		}
		return nil, ""
	}
	return nil, ""
}

// ---- mutations ----

func gqlMutationInput(field *gqlField, variables map[string]any) map[string]any {
	v := gqlArgValue(field, "input", variables)
	if rec, ok := v.(map[string]any); ok {
		return rec
	}
	return map[string]any{}
}

func gqlMutationID(input map[string]any) any {
	v := input["clientMutationId"]
	if s, ok := v.(string); ok {
		return s
	}
	if v == nil {
		return nil
	}
	return nil
}

func gqlResolveCreateIssue(ctx *gqlExecContext, field *gqlField) (any, string) {
	input := gqlMutationInput(field, ctx.variables)
	repo := gqlResolveRepositoryInput(ctx.forge, input["repositoryId"])
	if repo == nil {
		ctx.errors = append(ctx.errors, gqlError{message: "Could not resolve repositoryId."})
		return nil, "CreateIssuePayload"
	}
	title, _ := input["title"].(string)
	body, _ := input["body"].(string)
	var labels []string
	if arr, ok := input["labels"].([]any); ok {
		for _, item := range arr {
			if s, ok := item.(string); ok {
				labels = append(labels, s)
			}
		}
	}
	issue := ctx.forge.CreateIssue(repo, title, body, labels)
	return &gqlRecord{typename: "CreateIssuePayload", fields: map[string]any{
		"issue":           &gqlIssueView{repo: repo, issue: issue},
		"clientMutationId": gqlMutationID(input),
	}}, "CreateIssuePayload"
}

func gqlResolveCloseIssue(ctx *gqlExecContext, field *gqlField) (any, string) {
	input := gqlMutationInput(field, ctx.variables)
	repo, issue := gqlResolveIssueInput(ctx.forge, input["issueId"])
	if issue == nil {
		ctx.errors = append(ctx.errors, gqlError{message: "Could not resolve issueId."})
		return nil, "CloseIssuePayload"
	}
	issue.State = "closed"
	issue.ClosedAt = ctx.forge.NowIso()
	issue.UpdatedAt = ctx.forge.NowIso()
	return &gqlRecord{typename: "CloseIssuePayload", fields: map[string]any{
		"issue":           &gqlIssueView{repo: repo, issue: issue},
		"clientMutationId": gqlMutationID(input),
	}}, "CloseIssuePayload"
}

func gqlResolveCreatePullRequest(ctx *gqlExecContext, field *gqlField) (any, string) {
	input := gqlMutationInput(field, ctx.variables)
	repo := gqlResolveRepositoryInput(ctx.forge, input["repositoryId"])
	if repo == nil {
		ctx.errors = append(ctx.errors, gqlError{message: "Could not resolve repositoryId."})
		return nil, "CreatePullRequestPayload"
	}
	title, _ := input["title"].(string)
	body, _ := input["body"].(string)
	baseRefName, _ := input["baseRefName"].(string)
	headRefName, _ := input["headRefName"].(string)
	pull := ctx.forge.CreatePull(repo, title, body, headRefName, baseRefName)
	if pull == nil {
		ctx.errors = append(ctx.errors, gqlError{message: "Invalid head or base."})
		return nil, "CreatePullRequestPayload"
	}
	return &gqlRecord{typename: "CreatePullRequestPayload", fields: map[string]any{
		"pullRequest":     &gqlPullView{repo: repo, pull: pull},
		"clientMutationId": gqlMutationID(input),
	}}, "CreatePullRequestPayload"
}

func gqlResolveMergePullRequest(ctx *gqlExecContext, field *gqlField) (any, string) {
	input := gqlMutationInput(field, ctx.variables)
	repo, pull := gqlResolvePullInput(ctx.forge, input["pullRequestId"])
	if pull == nil {
		ctx.errors = append(ctx.errors, gqlError{message: "Could not resolve pullRequestId."})
		return nil, "MergePullRequestPayload"
	}
	if pull.Merged || pull.State == "closed" {
		ctx.errors = append(ctx.errors, gqlError{message: "Pull request is already merged."})
		return nil, "MergePullRequestPayload"
	}
	baseTip := ctx.forge.RefOid(repo, "refs/heads/"+pull.BaseRef)
	headTip := ctx.forge.RefOid(repo, "refs/heads/"+pull.HeadRef)
	merged := ctx.forge.SquashMergeTree(repo, baseTip, headTip)
	if merged.Conflict {
		ctx.errors = append(ctx.errors, gqlError{message: "Merge conflict."})
		return nil, "MergePullRequestPayload"
	}
	epoch := ctx.forge.Stamp()
	commitText := "tree " + merged.TreeOID + "\n" +
		"parent " + baseTip + "\n" +
		"author ada iversen <adaiversen@example.invalid> " + strconv.FormatInt(epoch, 10) + " +0000\n" +
		"committer ada iversen <adaiversen@example.invalid> " + strconv.FormatInt(epoch, 10) + " +0000\n" +
		"\n" +
		pull.Title + " (#" + strconv.Itoa(pull.Number) + ")\n"
	squashOID := ctx.forge.PutObject(repo, "commit", []byte(commitText))
	ctx.forge.SetRef(repo, "refs/heads/"+pull.BaseRef, squashOID)
	pull.State = "closed"
	pull.Merged = true
	pull.MergedAt = ctx.forge.NowIso()
	pull.MergeCommitSha = squashOID
	pull.UpdatedAt = ctx.forge.NowIso()
	repo.UpdatedAt = ctx.forge.NowIso()
	return &gqlRecord{typename: "MergePullRequestPayload", fields: map[string]any{
		"pullRequest":     &gqlPullView{repo: repo, pull: pull},
		"clientMutationId": gqlMutationID(input),
	}}, "MergePullRequestPayload"
}

func gqlResolveAddComment(ctx *gqlExecContext, field *gqlField) (any, string) {
	input := gqlMutationInput(field, ctx.variables)
	repo, issue := gqlResolveIssueInput(ctx.forge, input["subjectId"])
	if issue == nil {
		ctx.errors = append(ctx.errors, gqlError{message: "Could not resolve subjectId."})
		return nil, "AddCommentPayload"
	}
	body, _ := input["body"].(string)
	stamp := ctx.forge.NowIso()
	repo.CommentSeq++
	comment := &IssueComment{ID: repo.CommentSeq, Body: body, User: Viewer, CreatedAt: stamp, UpdatedAt: stamp}
	issue.Comments = append(issue.Comments, comment)
	return &gqlRecord{typename: "AddCommentPayload", fields: map[string]any{
		"commentEdge": &gqlRecord{typename: "CommentEdge", fields: map[string]any{
			"node":   &gqlCommentView{repoID: repo.ID, issueNum: issue.Number, comment: comment},
			"cursor": gqlCursorFor(comment.ID),
		}},
		"clientMutationId": gqlMutationID(input),
	}}, "AddCommentPayload"
}

// ---- connections ----

func gqlMakeIssueConnection(ctx *gqlExecContext, repo *Repo, field *gqlField) *gqlRecord {
	vars := ctx.variables
	issues := make([]*Issue, 0, len(repo.Issues))
	for _, iss := range repo.Issues {
		issues = append(issues, iss)
	}
	sort.Slice(issues, func(i, j int) bool { return issues[i].Number < issues[j].Number })
	if statesValue, ok := gqlArgValue(field, "states", vars).([]any); ok {
		var states []string
		for _, v := range statesValue {
			if s, ok := v.(string); ok {
				states = append(states, s)
			}
		}
		filtered := issues[:0]
		for _, iss := range issues {
			st := "CLOSED"
			if iss.State == "open" {
				st = "OPEN"
			}
			for _, s := range states {
				if s == st {
					filtered = append(filtered, iss)
					break
				}
			}
		}
		issues = filtered
	}
	if orderBy, ok := gqlArgValue(field, "orderBy", vars).(map[string]any); ok {
		direction := ""
		if d, ok := orderBy["direction"].(string); ok {
			direction = strings.ToUpper(d)
		}
		if direction == "ASC" {
			sort.SliceStable(issues, func(i, j int) bool {
				if issues[i].CreatedAt != issues[j].CreatedAt {
					return issues[i].CreatedAt < issues[j].CreatedAt
				}
				return issues[i].Number < issues[j].Number
			})
		} else if direction == "DESC" {
			sort.SliceStable(issues, func(i, j int) bool {
				if issues[i].CreatedAt != issues[j].CreatedAt {
					return issues[i].CreatedAt > issues[j].CreatedAt
				}
				return issues[i].Number > issues[j].Number
			})
		}
	}
	totalCount := len(issues)
	if afterNumber, ok := gqlDecodeCursor(gqlArgValue(field, "after", vars)); ok {
		filtered := issues[:0]
		for _, iss := range issues {
			if iss.Number > afterNumber {
				filtered = append(filtered, iss)
			}
		}
		issues = filtered
	}
	first := gqlFirstArg(field, vars)
	if first > len(issues) {
		first = len(issues)
	}
	nodes := issues[:first]
	var last *Issue
	if len(nodes) > 0 {
		last = nodes[len(nodes)-1]
	}
	nodeVals := make([]any, 0, len(nodes))
	for _, iss := range nodes {
		nodeVals = append(nodeVals, &gqlIssueView{repo: repo, issue: iss})
	}
	var endCursor any
	if last != nil {
		endCursor = gqlCursorFor(last.Number)
	}
	return &gqlRecord{typename: "IssueConnection", fields: map[string]any{
		"nodes": nodeVals,
		"pageInfo": &gqlRecord{typename: "PageInfo", fields: map[string]any{
			"hasNextPage": len(issues) > len(nodes),
			"endCursor":   endCursor,
		}},
		"totalCount": totalCount,
	}}
}

func gqlMakePullRequestConnection(ctx *gqlExecContext, repo *Repo, field *gqlField) *gqlRecord {
	vars := ctx.variables
	pulls := make([]*Pull, 0, len(repo.Pulls))
	for _, p := range repo.Pulls {
		if p.State == "open" {
			pulls = append(pulls, p)
		}
	}
	sort.Slice(pulls, func(i, j int) bool { return pulls[i].Number > pulls[j].Number })
	totalCount := len(pulls)
	if afterNumber, ok := gqlDecodeCursor(gqlArgValue(field, "after", vars)); ok {
		filtered := pulls[:0]
		for _, p := range pulls {
			if p.Number < afterNumber {
				filtered = append(filtered, p)
			}
		}
		pulls = filtered
	}
	first := gqlFirstArg(field, vars)
	if first > len(pulls) {
		first = len(pulls)
	}
	nodes := pulls[:first]
	var last *Pull
	if len(nodes) > 0 {
		last = nodes[len(nodes)-1]
	}
	nodeVals := make([]any, 0, len(nodes))
	for _, p := range nodes {
		nodeVals = append(nodeVals, &gqlPullView{repo: repo, pull: p})
	}
	var endCursor any
	if last != nil {
		endCursor = gqlCursorFor(last.Number)
	}
	return &gqlRecord{typename: "PullRequestConnection", fields: map[string]any{
		"nodes": nodeVals,
		"pageInfo": &gqlRecord{typename: "PageInfo", fields: map[string]any{
			"hasNextPage": len(pulls) > len(nodes),
			"endCursor":   endCursor,
		}},
		"totalCount": totalCount,
	}}
}

func gqlMakeIssueCommentConnection(repo *Repo, issue *Issue) *gqlRecord {
	nodeVals := make([]any, 0, len(issue.Comments))
	for _, c := range issue.Comments {
		nodeVals = append(nodeVals, &gqlCommentView{repoID: repo.ID, issueNum: issue.Number, comment: c})
	}
	return &gqlRecord{typename: "IssueCommentConnection", fields: map[string]any{
		"nodes": nodeVals,
		"pageInfo": &gqlRecord{typename: "PageInfo", fields: map[string]any{
			"hasNextPage": false,
			"endCursor":   nil,
		}},
		"totalCount": len(nodeVals),
	}}
}

func gqlMakeLabelConnection(repo *Repo, issue *Issue) *gqlRecord {
	nodeVals := make([]any, 0, len(issue.Labels))
	for _, labelName := range issue.Labels {
		label, ok := repo.Labels[labelName]
		id := 0
		name := labelName
		color := ""
		desc := ""
		if ok {
			id, name, color, desc = label.ID, label.Name, label.Color, label.Description
		}
		nodeVals = append(nodeVals, &gqlRecord{typename: "Label", fields: map[string]any{
			"id": id, "name": name, "color": color, "description": desc,
		}})
	}
	return &gqlRecord{typename: "LabelConnection", fields: map[string]any{
		"nodes": nodeVals,
		"pageInfo": &gqlRecord{typename: "PageInfo", fields: map[string]any{
			"hasNextPage": false,
			"endCursor":   nil,
		}},
		"totalCount": len(nodeVals),
	}}
}

func gqlMakePullFileConnection(f *Forge, repo *Repo, pull *Pull, field *gqlField, vars map[string]any) *gqlRecord {
	baseTip := f.RefOid(repo, "refs/heads/"+pull.BaseRef)
	headTip := f.RefOid(repo, "refs/heads/"+pull.HeadRef)
	files := f.PullFiles(repo, baseTip, headTip)
	first := gqlFirstArg(field, vars)
	if first > len(files) {
		first = len(files)
	}
	nodeVals := make([]any, 0, first)
	for _, file := range files[:first] {
		nodeVals = append(nodeVals, &gqlRecord{typename: "PullRequestChangedFile", fields: map[string]any{
			"path": file.Filename, "status": strings.ToUpper(file.Status),
			"additions": file.Additions, "deletions": file.Deletions, "changes": file.Changes,
		}})
	}
	return &gqlRecord{typename: "PullRequestChangedFileConnection", fields: map[string]any{
		"nodes": nodeVals,
		"pageInfo": &gqlRecord{typename: "PageInfo", fields: map[string]any{
			"hasNextPage": false,
			"endCursor":   nil,
		}},
		"totalCount": len(files),
	}}
}

func gqlConnectionNodeType(typename string) string {
	switch typename {
	case "IssueConnection":
		return "Issue"
	case "PullRequestConnection":
		return "PullRequest"
	case "PullRequestChangedFileConnection":
		return "PullRequestChangedFile"
	case "IssueCommentConnection":
		return "IssueComment"
	case "LabelConnection":
		return "Label"
	}
	return ""
}

func gqlBuildTypeIntrospection(typeName string) *gqlRecord {
	fields, ok := gqlTypeFieldOrder[typeName]
	if !ok {
		return nil
	}
	isEnum := typeName == "SearchType"
	var fieldVals any
	var enumVals any
	if !isEnum {
		list := make([]any, 0, len(fields))
		for _, fn := range fields {
			list = append(list, &gqlRecord{typename: "__Field", fields: map[string]any{"name": fn, "description": nil}})
		}
		fieldVals = list
	}
	if isEnum {
		list := make([]any, 0, len(gqlSearchTypeEnum))
		for _, v := range gqlSearchTypeEnum {
			list = append(list, &gqlRecord{typename: "__EnumValue", fields: map[string]any{"name": v, "description": nil}})
		}
		enumVals = list
	}
	kind := "OBJECT"
	if isEnum {
		kind = "ENUM"
	}
	return &gqlRecord{typename: "__Type", fields: map[string]any{
		"name": typeName, "kind": kind, "fields": fieldVals, "enumValues": enumVals,
	}}
}

// ---- lookups by global id ----

var gqlKeyRe = regexp.MustCompile(`^(\d+):(\d+)$`)

func gqlFindRepoByKey(f *Forge, key string) *Repo {
	n, err := strconv.Atoi(key)
	if err != nil {
		return nil
	}
	return f.GetRepoByID(n)
}

func gqlFindIssueByKey(f *Forge, key string) (*Repo, *Issue) {
	m := gqlKeyRe.FindStringSubmatch(key)
	if m == nil {
		return nil, nil
	}
	repo := gqlFindRepoByKey(f, m[1])
	if repo == nil {
		return nil, nil
	}
	num, _ := strconv.Atoi(m[2])
	issue, ok := repo.Issues[num]
	if !ok {
		return nil, nil
	}
	return repo, issue
}

func gqlFindPullByKey(f *Forge, key string) (*Repo, *Pull) {
	m := gqlKeyRe.FindStringSubmatch(key)
	if m == nil {
		return nil, nil
	}
	repo := gqlFindRepoByKey(f, m[1])
	if repo == nil {
		return nil, nil
	}
	num, _ := strconv.Atoi(m[2])
	pull, ok := repo.Pulls[num]
	if !ok {
		return nil, nil
	}
	return repo, pull
}

func gqlResolveRepositoryInput(f *Forge, value any) *Repo {
	typ, key, ok := gqlDecodeGlobalID(value)
	if !ok || typ != "Repository" {
		return nil
	}
	return gqlFindRepoByKey(f, key)
}

func gqlResolveIssueInput(f *Forge, value any) (*Repo, *Issue) {
	typ, key, ok := gqlDecodeGlobalID(value)
	if !ok || typ != "Issue" {
		return nil, nil
	}
	return gqlFindIssueByKey(f, key)
}

func gqlResolvePullInput(f *Forge, value any) (*Repo, *Pull) {
	typ, key, ok := gqlDecodeGlobalID(value)
	if !ok || typ != "PullRequest" {
		return nil, nil
	}
	return gqlFindPullByKey(f, key)
}

// ---- entry point ----

func gqlInvalidOperation() gqlResult {
	return gqlResult{data: nil, errors: []gqlError{{message: "Invalid operation"}}}
}

func gqlExecuteBodyEntry(f *Forge, entry any) gqlResult {
	rec, ok := entry.(map[string]any)
	if !ok {
		return gqlInvalidOperation()
	}
	query, ok := rec["query"].(string)
	if !ok {
		return gqlInvalidOperation()
	}
	if vars, present := rec["variables"]; present {
		if _, ok := vars.(map[string]any); !ok {
			return gqlInvalidOperation()
		}
	}
	variables := map[string]any{}
	if vars, ok := rec["variables"].(map[string]any); ok {
		variables = vars
	}
	operationName := ""
	if s, ok := rec["operationName"].(string); ok {
		operationName = s
	}
	return gqlExecuteQuery(f, query, variables, operationName)
}

func gqlExecuteQuery(f *Forge, query string, variables map[string]any, operationName string) gqlResult {
	doc, err := gqlParseDocument(query)
	if err != nil {
		return gqlResult{data: nil, errors: []gqlError{{message: err.Error()}}}
	}
	op := gqlPickOperation(doc, operationName)
	if op == nil {
		return gqlInvalidOperation()
	}
	return gqlExecuteOperation(f, doc, op, variables)
}

// gqlToPublic converts a result to the wire shape: {data} or {data, errors}.
// The executor's internal values (views/records) are converted to plain
// maps first so the JSON encoder sees only plain data.
func gqlToPublic(result gqlResult) map[string]any {
	data := gqlPublicValue(result.data)
	if len(result.errors) > 0 {
		errs := make([]any, 0, len(result.errors))
		for _, e := range result.errors {
			errs = append(errs, map[string]any{"message": e.message})
		}
		return map[string]any{"data": data, "errors": errs}
	}
	return map[string]any{"data": data}
}

// gqlPublicValue converts executor values to plain JSON-ready values:
// views/records become their selected-field maps; scalars pass through.
func gqlPublicValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, val := range t {
			out[k] = gqlPublicValue(val)
		}
		return out
	case []any:
		out := make([]any, 0, len(t))
		for _, item := range t {
			out = append(out, gqlPublicValue(item))
		}
		return out
	case *gqlRecord:
		return gqlPublicValue(t.fields)
	case *gqlRepoView, *gqlIssueView, *gqlPullView, *gqlCommentView:
		// Views only appear inside already-selected results; they carry no
		// extra fields beyond what the executor selected.
		return nil
	}
	return v
}

// handleGraphQL answers one POST /api/graphql request.
func handleGraphQL(f *Forge, body any, bodyMalformed, authed bool) Response {
	if !authed {
		return errResp(401, "Requires authentication")
	}
	if bodyMalformed {
		return jsonResp(400, map[string]any{"errors": []any{map[string]any{"message": "Invalid JSON body"}}}, nil)
	}
	if arr, ok := body.([]any); ok {
		results := make([]any, 0, len(arr))
		for _, entry := range arr {
			results = append(results, gqlToPublic(gqlExecuteBodyEntry(f, entry)))
		}
		return jsonResp(200, results, nil)
	}
	if rec, ok := body.(map[string]any); ok {
		if query, ok := rec["query"].(string); ok {
			variables := map[string]any{}
			if vars, ok := rec["variables"].(map[string]any); ok {
				variables = vars
			}
			operationName := ""
			if s, ok := rec["operationName"].(string); ok {
				operationName = s
			}
			result := gqlExecuteQuery(f, query, variables, operationName)
			return jsonResp(200, gqlToPublic(result), nil)
		}
	}
	return jsonResp(400, map[string]any{"errors": []any{map[string]any{"message": "Must provide query string."}}}, nil)
}
