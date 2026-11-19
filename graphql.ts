import type { IncomingMessage, ServerResponse } from 'node:http';
import { Forge, VIEWER, type Repo, type Issue, type Pull } from './store.ts';
import { sendJson, sendError, isPlainObject } from './util.ts';

export interface GraphQLRequest {
  req: IncomingMessage;
  res: ServerResponse;
  forge: Forge;
  body: unknown;
  bodyMalformed: boolean;
  authed: boolean;
}

type ValueNode =
  | { kind: 'Variable'; name: string }
  | { kind: 'Int'; value: number }
  | { kind: 'Float'; value: number }
  | { kind: 'String'; value: string }
  | { kind: 'Boolean'; value: boolean }
  | { kind: 'Null' }
  | { kind: 'Enum'; value: string }
  | { kind: 'List'; values: ValueNode[] }
  | { kind: 'Object'; fields: Array<{ name: string; value: ValueNode }> };

interface FieldNode {
  kind: 'Field';
  name: string;
  alias: string | null;
  args: Array<{ name: string; value: ValueNode }>;
  selections: SelectionNode[];
}

interface FragmentSpreadNode {
  kind: 'FragmentSpread';
  name: string;
}

interface InlineFragmentNode {
  kind: 'InlineFragment';
  typeCondition: string | null;
  selections: SelectionNode[];
}

type SelectionNode = FieldNode | FragmentSpreadNode | InlineFragmentNode;

interface OperationNode {
  kind: 'Operation';
  operation: 'query' | 'mutation';
  name: string | null;
  variableDefinitions: Array<{ name: string; defaultValue: ValueNode | null }>;
  selections: SelectionNode[];
}

interface FragmentNode {
  kind: 'Fragment';
  name: string;
  typeCondition: string;
  selections: SelectionNode[];
}

interface DocumentNode {
  operations: OperationNode[];
  fragments: Map<string, FragmentNode>;
}

type TokenKind =
  | 'Name'
  | 'Int'
  | 'Float'
  | 'String'
  | '{'
  | '}'
  | '('
  | ')'
  | ':'
  | '!'
  | '='
  | '$'
  | '@'
  | '['
  | ']'
  | '...'
  | 'EOF';

interface Token {
  kind: TokenKind;
  value: string;
}

const ZERO = '0000000000000000000000000000000000000000';

const ISSUE_FIELDS = [
  'id',
  'number',
  'title',
  'body',
  'state',
  'createdAt',
  'updatedAt',
  'author',
  'comments',
  'labels',
  'url',
  'closedAt',
] as const;

const PULL_REQUEST_FIELDS = [
  'id',
  'number',
  'title',
  'body',
  'state',
  'merged',
  'mergeable',
  'headRefName',
  'baseRefName',
  'files',
  'headRef',
  'baseRef',
  'createdAt',
  'updatedAt',
  'closedAt',
  'mergeCommit',
] as const;

const REPOSITORY_FIELDS = [
  'id',
  'databaseId',
  'name',
  'nameWithOwner',
  'owner',
  'defaultBranchRef',
  'viewerPermission',
  'hasIssuesEnabled',
  'issue',
  'issues',
  'pullRequests',
  'pullRequest',
  'description',
  'createdAt',
  'updatedAt',
  'isPrivate',
] as const;

const QUERY_FIELDS = ['viewer', 'repository', 'node', '__type'] as const;
const USER_FIELDS = ['id', 'login', 'name', 'email'] as const;
const ISSUE_CONNECTION_FIELDS = ['nodes', 'pageInfo', 'totalCount'] as const;
const PULL_REQUEST_CONNECTION_FIELDS = ['nodes', 'pageInfo', 'totalCount'] as const;
const PAGE_INFO_FIELDS = ['hasNextPage', 'endCursor'] as const;
const SEARCH_TYPE_ENUM = ['ISSUE', 'PULL_REQUEST', 'DISCUSSION', 'COMMIT', 'REPOSITORY', 'USER'] as const;

const REF_FIELDS = ['name'] as const;
const PULL_REQUEST_CHANGED_FILE_FIELDS = ['path', 'status', 'additions', 'deletions', 'changes'] as const;
const ISSUE_COMMENT_FIELDS = ['id', 'body', 'createdAt'] as const;
const COMMENT_EDGE_FIELDS = ['node', 'cursor'] as const;

const MUTATION_FIELDS = [
  'createIssue',
  'closeIssue',
  'createPullRequest',
  'mergePullRequest',
  'addComment',
] as const;

const CREATE_ISSUE_PAYLOAD_FIELDS = ['issue', 'clientMutationId'] as const;
const CLOSE_ISSUE_PAYLOAD_FIELDS = ['issue', 'clientMutationId'] as const;
const CREATE_PULL_REQUEST_PAYLOAD_FIELDS = ['pullRequest', 'clientMutationId'] as const;
const MERGE_PULL_REQUEST_PAYLOAD_FIELDS = ['pullRequest', 'clientMutationId'] as const;
const ADD_COMMENT_PAYLOAD_FIELDS = ['commentEdge', 'clientMutationId'] as const;

const TYPE_FIELD_ORDER: Record<string, readonly string[]> = {
  Query: QUERY_FIELDS,
  User: USER_FIELDS,
  Repository: REPOSITORY_FIELDS,
  Ref: REF_FIELDS,
  Issue: ISSUE_FIELDS,
  IssueConnection: ISSUE_CONNECTION_FIELDS,
  PageInfo: PAGE_INFO_FIELDS,
  PullRequest: PULL_REQUEST_FIELDS,
  PullRequestConnection: PULL_REQUEST_CONNECTION_FIELDS,
  PullRequestChangedFile: PULL_REQUEST_CHANGED_FILE_FIELDS,
  PullRequestChangedFileConnection: ['nodes', 'pageInfo', 'totalCount'],
  IssueComment: ISSUE_COMMENT_FIELDS,
  IssueCommentConnection: ['nodes', 'pageInfo', 'totalCount'],
  CommentEdge: COMMENT_EDGE_FIELDS,
  Mutation: MUTATION_FIELDS,
  CreateIssuePayload: CREATE_ISSUE_PAYLOAD_FIELDS,
  CloseIssuePayload: CLOSE_ISSUE_PAYLOAD_FIELDS,
  CreatePullRequestPayload: CREATE_PULL_REQUEST_PAYLOAD_FIELDS,
  MergePullRequestPayload: MERGE_PULL_REQUEST_PAYLOAD_FIELDS,
  AddCommentPayload: ADD_COMMENT_PAYLOAD_FIELDS,
  __Type: ['fields', 'enumValues', 'name', 'kind'],
  __Field: ['name', 'description'],
  __EnumValue: ['name', 'description'],
  SearchType: [],
  Label: ['id', 'name', 'color', 'description'],
  LabelConnection: ['nodes', 'pageInfo', 'totalCount'],
};

const TYPE_FIELD_SETS = new Map<string, Set<string>>();
for (const [typeName, fields] of Object.entries(TYPE_FIELD_ORDER)) {
  TYPE_FIELD_SETS.set(typeName, new Set(fields));
}

interface GQLError {
  message: string;
}

interface GQLResult {
  data: Record<string, unknown> | null;
  errors: GQLError[];
}

interface RepoView {
  __typename: 'Repository';
  _repo: Repo;
}

interface IssueView {
  __typename: 'Issue';
  _repo: Repo;
  _issue: Issue;
}

interface PullView {
  __typename: 'PullRequest';
  _repo: Repo;
  _pull: Pull;
}

type IssueCommentData = Issue['comments'][number];
type PullFileData = Pull['files'][number];

interface CommentView {
  __typename: 'IssueComment';
  _repoId: number;
  _issueNumber: number;
  _comment: IssueCommentData;
}

interface Resolved {
  value: unknown;
  typeName: string | null;
}

interface ExecContext {
  forge: Forge;
  doc: DocumentNode;
  variables: Record<string, unknown>;
  errors: GQLError[];
}

interface IssueLocation {
  repo: Repo;
  issue: Issue;
}

interface PullLocation {
  repo: Repo;
  pull: Pull;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return isPlainObject(value);
}

function hasOwn(record: Record<string, unknown>, key: string): boolean {
  return Object.prototype.hasOwnProperty.call(record, key);
}

function globalId(typeName: string, key: string): string {
  return Buffer.from(`${typeName}:${key}`, 'utf8').toString('base64');
}

function decodeGlobalId(id: unknown): { type: string; key: string } | null {
  if (typeof id !== 'string') return null;
  try {
    const decoded = Buffer.from(id, 'base64').toString('utf8');
    const match = /^([A-Za-z]+):(.+)$/.exec(decoded);
    if (!match) return null;
    return { type: match[1], key: match[2] };
  } catch {
    return null;
  }
}

function cursorFor(n: number): string {
  return Buffer.from(`cursor:${n}`, 'utf8').toString('base64');
}

function decodeCursor(cursor: unknown): number | null {
  if (typeof cursor !== 'string') return null;
  try {
    const decoded = Buffer.from(cursor, 'base64').toString('utf8');
    const match = /^cursor:(\d+)$/.exec(decoded);
    if (!match) return null;
    return Number(match[1]);
  } catch {
    return null;
  }
}

function resolveValue(node: ValueNode, variables: Record<string, unknown>): unknown {
  switch (node.kind) {
    case 'Variable':
      return variables[node.name];
    case 'Int':
    case 'Float':
      return node.value;
    case 'String':
      return node.value;
    case 'Boolean':
      return node.value;
    case 'Null':
      return null;
    case 'Enum':
      return node.value;
    case 'List':
      return node.values.map((value) => resolveValue(value, variables));
    case 'Object': {
      const result: Record<string, unknown> = {};
      for (const field of node.fields) {
        result[field.name] = resolveValue(field.value, variables);
      }
      return result;
    }
  }
}

function isNameStart(ch: string | undefined): boolean {
  if (ch === undefined) return false;
  return ch === '_' || (ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z');
}

function isNameContinue(ch: string | undefined): boolean {
  if (ch === undefined) return false;
  return isNameStart(ch) || (ch >= '0' && ch <= '9');
}

function isDigit(ch: string | undefined): boolean {
  return ch !== undefined && ch >= '0' && ch <= '9';
}

function tokenize(source: string): Token[] {
  const tokens: Token[] = [];
  const length = source.length;
  let i = 0;

  while (i < length) {
    const ch = source[i]!;

    if (
      ch === ' ' ||
      ch === '\t' ||
      ch === '\n' ||
      ch === '\r' ||
      ch === ',' ||
      ch === '\uFEFF'
    ) {
      i += 1;
      continue;
    }

    if (ch === '#') {
      i += 1;
      while (i < length && source[i] !== '\n' && source[i] !== '\r') i += 1;
      continue;
    }

    if (source.startsWith('...', i)) {
      tokens.push({ kind: '...', value: '...' });
      i += 3;
      continue;
    }

    if (
      ch === '{' ||
      ch === '}' ||
      ch === '(' ||
      ch === ')' ||
      ch === ':' ||
      ch === '!' ||
      ch === '=' ||
      ch === '$' ||
      ch === '@' ||
      ch === '[' ||
      ch === ']'
    ) {
      tokens.push({ kind: ch as TokenKind, value: ch });
      i += 1;
      continue;
    }

    if (isNameStart(ch)) {
      const start = i;
      i += 1;
      while (i < length && isNameContinue(source[i])) i += 1;
      tokens.push({ kind: 'Name', value: source.slice(start, i) });
      continue;
    }

    if (ch === '-' || isDigit(ch)) {
      const start = i;
      if (ch === '-') i += 1;
      if (!isDigit(source[i])) {
        throw new Error(`Syntax Error: Invalid number at offset ${start}.`);
      }
      while (isDigit(source[i])) i += 1;

      let isFloat = false;
      if (source[i] === '.') {
        isFloat = true;
        i += 1;
        if (!isDigit(source[i])) {
          throw new Error(`Syntax Error: Invalid number at offset ${start}.`);
        }
        while (isDigit(source[i])) i += 1;
      }

      if (source[i] === 'e' || source[i] === 'E') {
        isFloat = true;
        i += 1;
        if (source[i] === '+' || source[i] === '-') i += 1;
        if (!isDigit(source[i])) {
          throw new Error(`Syntax Error: Invalid number at offset ${start}.`);
        }
        while (isDigit(source[i])) i += 1;
      }

      tokens.push({
        kind: isFloat ? 'Float' : 'Int',
        value: source.slice(start, i),
      });
      continue;
    }

    if (ch === '"') {
      i += 1;
      let value = '';
      let closed = false;

      while (i < length) {
        const current = source[i]!;
        i += 1;

        if (current === '"') {
          closed = true;
          break;
        }

        if (current === '\\') {
          if (i >= length) {
            throw new Error('Syntax Error: Unterminated string escape.');
          }
          const escaped = source[i]!;
          i += 1;
          switch (escaped) {
            case '"':
              value += '"';
              break;
            case '\\':
              value += '\\';
              break;
            case '/':
              value += '/';
              break;
            case 'n':
              value += '\n';
              break;
            case 'r':
              value += '\r';
              break;
            case 't':
              value += '\t';
              break;
            case 'b':
              value += '\b';
              break;
            case 'f':
              value += '\f';
              break;
            case 'u': {
              const hex = source.slice(i, i + 4);
              if (!/^[0-9a-fA-F]{4}$/.test(hex)) {
                throw new Error('Syntax Error: Invalid unicode escape.');
              }
              value += String.fromCharCode(parseInt(hex, 16));
              i += 4;
              break;
            }
            default:
              throw new Error(`Syntax Error: Invalid string escape \\${escaped}.`);
          }
          continue;
        }

        if (current === '\n' || current === '\r') {
          throw new Error('Syntax Error: Unterminated string.');
        }

        value += current;
      }

      if (!closed) {
        throw new Error('Syntax Error: Unterminated string.');
      }

      tokens.push({ kind: 'String', value });
      continue;
    }

    throw new Error(`Syntax Error: Unexpected character ${JSON.stringify(ch)}.`);
  }

  tokens.push({ kind: 'EOF', value: '' });
  return tokens;
}

class Parser {
  private readonly tokens: Token[];
  private pos = 0;

  constructor(tokens: Token[]) {
    this.tokens = tokens;
  }

  parse(): DocumentNode {
    const operations: OperationNode[] = [];
    const fragments = new Map<string, FragmentNode>();

    while (this.peek().kind !== 'EOF') {
      const token = this.peek();

      if (token.kind === '{') {
        operations.push({
          kind: 'Operation',
          operation: 'query',
          name: null,
          variableDefinitions: [],
          selections: this.parseSelectionSet(),
        });
        continue;
      }

      if (token.kind === 'Name' && (token.value === 'query' || token.value === 'mutation')) {
        operations.push(this.parseOperation());
        continue;
      }

      if (token.kind === 'Name' && token.value === 'fragment') {
        const fragment = this.parseFragment();
        fragments.set(fragment.name, fragment);
        continue;
      }

      this.fail('Expected query, mutation, fragment, or selection set.');
    }

    return { operations, fragments };
  }

  private parseOperation(): OperationNode {
    const operationToken = this.expect('Name');
    const operation = operationToken.value === 'mutation' ? 'mutation' : 'query';

    let name: string | null = null;
    if (this.peek().kind === 'Name') {
      name = this.expectName();
    }

    this.skipDirectives();

    const variableDefinitions: Array<{ name: string; defaultValue: ValueNode | null }> = [];

    if (this.match('(')) {
      while (this.peek().kind !== ')') {
        if (this.peek().kind === 'EOF') {
          this.fail('Unterminated variable definitions.');
        }
        this.expect('$');
        const variableName = this.expectName();
        this.expect(':');
        this.parseTypeReference();
        let defaultValue: ValueNode | null = null;
        if (this.match('=')) {
          defaultValue = this.parseValue();
        }
        variableDefinitions.push({ name: variableName, defaultValue });
      }
      this.expect(')');
    }

    this.skipDirectives();

    const selections = this.parseSelectionSet();

    return {
      kind: 'Operation',
      operation,
      name,
      variableDefinitions,
      selections,
    };
  }

  private parseFragment(): FragmentNode {
    this.expectKeyword('fragment');
    const name = this.expectName();
    this.expectKeyword('on');
    const typeCondition = this.expectName();
    const selections = this.parseSelectionSet();

    return {
      kind: 'Fragment',
      name,
      typeCondition,
      selections,
    };
  }

  private parseSelectionSet(): SelectionNode[] {
    this.expect('{');
    const selections: SelectionNode[] = [];

    while (this.peek().kind !== '}') {
      if (this.peek().kind === 'EOF') {
        this.fail('Unterminated selection set.');
      }
      selections.push(this.parseSelection());
    }

    this.expect('}');
    return selections;
  }

  private parseSelection(): SelectionNode {
    if (this.peek().kind === '...') {
      this.advance();

      if (this.peek().kind === 'Name' && this.peek().value === 'on') {
        this.advance();
        const typeCondition = this.expectName();
        const selections = this.parseSelectionSet();
        return { kind: 'InlineFragment', typeCondition, selections };
      }

      if (this.peek().kind === 'Name') {
        return { kind: 'FragmentSpread', name: this.expectName() };
      }

      if (this.peek().kind === '{') {
        const selections = this.parseSelectionSet();
        return { kind: 'InlineFragment', typeCondition: null, selections };
      }

      this.fail('Expected fragment name, type condition, or selection set.');
    }

    const firstName = this.expectName();
    let alias: string | null = null;
    let name = firstName;

    if (this.match(':')) {
      alias = firstName;
      name = this.expectName();
    }

    const args = this.peek().kind === '(' ? this.parseArguments() : [];
    this.skipDirectives();
    const selections = this.peek().kind === '{' ? this.parseSelectionSet() : [];

    return {
      kind: 'Field',
      name,
      alias,
      args,
      selections,
    };
  }

  private parseArguments(): Array<{ name: string; value: ValueNode }> {
    this.expect('(');
    const args: Array<{ name: string; value: ValueNode }> = [];

    while (this.peek().kind !== ')') {
      if (this.peek().kind === 'EOF') {
        this.fail('Unterminated argument list.');
      }
      const name = this.expectName();
      this.expect(':');
      args.push({ name, value: this.parseValue() });
    }

    this.expect(')');
    return args;
  }

  private skipDirectives(): void {
    while (this.peek().kind === '@') {
      this.advance();
      this.expectName();
      if (this.peek().kind === '(' && this.peek(1).kind === 'Name') {
        this.parseArguments();
      }
    }
  }

  private parseTypeReference(): void {
    if (this.match('[')) {
      this.parseTypeReference();
      this.expect(']');
    } else {
      this.expectName();
    }
    this.match('!');
  }

  private parseValue(): ValueNode {
    const token = this.peek();

    if (token.kind === '$') {
      this.advance();
      return { kind: 'Variable', name: this.expectName() };
    }

    if (token.kind === 'String') {
      this.advance();
      return { kind: 'String', value: token.value };
    }

    if (token.kind === 'Int') {
      this.advance();
      return { kind: 'Int', value: Number(token.value) };
    }

    if (token.kind === 'Float') {
      this.advance();
      return { kind: 'Float', value: Number(token.value) };
    }

    if (token.kind === 'Name') {
      this.advance();
      if (token.value === 'true') return { kind: 'Boolean', value: true };
      if (token.value === 'false') return { kind: 'Boolean', value: false };
      if (token.value === 'null') return { kind: 'Null' };
      return { kind: 'Enum', value: token.value };
    }

    if (token.kind === '[') {
      this.advance();
      const values: ValueNode[] = [];
      while (this.peek().kind !== ']') {
        if (this.peek().kind === 'EOF') {
          this.fail('Unterminated list value.');
        }
        values.push(this.parseValue());
      }
      this.expect(']');
      return { kind: 'List', values };
    }

    if (token.kind === '{') {
      this.advance();
      const fields: Array<{ name: string; value: ValueNode }> = [];
      while (this.peek().kind !== '}') {
        if (this.peek().kind === 'EOF') {
          this.fail('Unterminated object value.');
        }
        const name = this.expectName();
        this.expect(':');
        fields.push({ name, value: this.parseValue() });
      }
      this.expect('}');
      return { kind: 'Object', fields };
    }

    this.fail('Expected value.');
  }

  private expectKeyword(word: string): Token {
    const token = this.expect('Name');
    if (token.value !== word) {
      this.fail(`Expected "${word}".`);
    }
    return token;
  }

  private expectName(): string {
    return this.expect('Name').value;
  }

  private expect(kind: TokenKind): Token {
    const token = this.peek();
    if (token.kind !== kind) {
      this.fail(`Expected "${kind}".`);
    }
    return this.advance();
  }

  private match(kind: TokenKind): boolean {
    if (this.peek().kind === kind) {
      this.advance();
      return true;
    }
    return false;
  }

  private advance(): Token {
    const token = this.peek();
    if (token.kind !== 'EOF') {
      this.pos += 1;
    }
    return token;
  }

  private peek(offset = 0): Token {
    return this.tokens[this.pos + offset] ?? { kind: 'EOF', value: '' };
  }

  private fail(message: string): never {
    throw new Error(`Syntax Error: ${message}`);
  }
}

function parseDocumentSource(source: string): DocumentNode {
  return new Parser(tokenize(source)).parse();
}

function prepareVariables(
  operation: OperationNode,
  variables: Record<string, unknown>,
): Record<string, unknown> {
  const prepared: Record<string, unknown> = {};
  for (const [key, value] of Object.entries(variables)) {
    prepared[key] = value;
  }

  for (const definition of operation.variableDefinitions) {
    if (
      definition.defaultValue !== null &&
      (!hasOwn(prepared, definition.name) || prepared[definition.name] === undefined)
    ) {
      prepared[definition.name] = resolveValue(definition.defaultValue, prepared);
    }
  }

  return prepared;
}

function pickOperation(doc: DocumentNode, operationName: string | null): OperationNode | null {
  if (doc.operations.length === 0) return null;

  if (operationName !== null) {
    const found = doc.operations.find((operation) => operation.name === operationName);
    if (found) return found;
  }

  return doc.operations[0] ?? null;
}

function executeOperation(
  forge: Forge,
  doc: DocumentNode,
  operation: OperationNode,
  variables: Record<string, unknown>,
): GQLResult {
  const preparedVariables = prepareVariables(operation, variables);
  const errors: GQLError[] = [];
  const context: ExecContext = {
    forge,
    doc,
    variables: preparedVariables,
    errors,
  };

  const rootType = operation.operation === 'mutation' ? 'Mutation' : 'Query';
  const rootValue = { __typename: rootType };
  const data = executeSelections(context, rootValue, rootType, operation.selections);

  if (errors.length > 0) {
    return { data: null, errors };
  }

  return { data, errors: [] };
}

function executeSelections(
  context: ExecContext,
  value: unknown,
  typeName: string,
  selections: SelectionNode[],
): Record<string, unknown> {
  const currentType = typeNameOf(value, typeName);
  const result: Record<string, unknown> = {};

  for (const selection of selections) {
    if (selection.kind === 'Field') {
      const [key, fieldValue] = executeField(context, value, currentType, selection);
      result[key] = fieldValue;
      continue;
    }

    if (selection.kind === 'InlineFragment') {
      if (selection.typeCondition === null || selection.typeCondition === currentType) {
        Object.assign(
          result,
          executeSelections(context, value, currentType, selection.selections),
        );
      }
      continue;
    }

    const fragment = context.doc.fragments.get(selection.name);
    if (fragment && fragment.typeCondition === currentType) {
      Object.assign(
        result,
        executeSelections(context, value, currentType, fragment.selections),
      );
    }
  }

  return result;
}

function executeField(
  context: ExecContext,
  parent: unknown,
  parentType: string,
  field: FieldNode,
): [string, unknown] {
  const key = field.alias ?? field.name;

  if (field.name === '__typename') {
    return [key, parentType];
  }

  if (!isKnownField(parentType, field.name)) {
    context.errors.push({
      message: `Cannot query field "${field.name}" on type "${parentType}".`,
    });
    return [key, null];
  }

  const resolved = resolveField(context, parent, parentType, field);
  const value = resolved.value;

  if (value === null || value === undefined) {
    return [key, null];
  }

  if (field.selections.length === 0) {
    return [key, value];
  }

  if (Array.isArray(value)) {
    const elementType = typeNameOf(value, resolved.typeName);
    return [
      key,
      value.map((item) =>
        executeSelections(
          context,
          item,
          typeNameOf(item, resolved.typeName ?? elementType),
          field.selections,
        ),
      ),
    ];
  }

  return [
    key,
    executeSelections(
      context,
      value,
      typeNameOf(value, resolved.typeName),
      field.selections,
    ),
  ];
}

function typeNameOf(value: unknown, fallback: string | null): string {
  if (isRecord(value) && typeof value.__typename === 'string') {
    return value.__typename;
  }
  return fallback ?? '';
}

function isKnownField(typeName: string, fieldName: string): boolean {
  const fields = TYPE_FIELD_SETS.get(typeName);
  return fields !== undefined && fields.has(fieldName);
}

function resolveField(
  context: ExecContext,
  parent: unknown,
  parentType: string,
  field: FieldNode,
): Resolved {
  const forge = context.forge;
  const variables = context.variables;
  const name = field.name;

  switch (parentType) {
    case 'Query': {
      switch (name) {
        case 'viewer':
          return { value: VIEWER, typeName: 'User' };

        case 'repository': {
          const owner = stringArg(field, 'owner', variables);
          const repoName = stringArg(field, 'name', variables);
          if (owner !== VIEWER.login || repoName === undefined) {
            return { value: null, typeName: 'Repository' };
          }
          const repo = forge.getRepo(owner, repoName);
          return repo
            ? { value: repoView(repo), typeName: 'Repository' }
            : { value: null, typeName: 'Repository' };
        }

        case 'node': {
          const decoded = decodeGlobalId(argValue(field, 'id', variables));
          if (!decoded) return { value: null, typeName: null };

          if (decoded.type === 'Repository') {
            const repo = findRepoByKey(forge, decoded.key);
            return repo
              ? { value: repoView(repo), typeName: 'Repository' }
              : { value: null, typeName: 'Repository' };
          }

          if (decoded.type === 'Issue') {
            const location = findIssueByKey(forge, decoded.key);
            return location
              ? { value: issueView(location.repo, location.issue), typeName: 'Issue' }
              : { value: null, typeName: 'Issue' };
          }

          if (decoded.type === 'PullRequest') {
            const location = findPullByKey(forge, decoded.key);
            return location
              ? { value: pullView(location.repo, location.pull), typeName: 'PullRequest' }
              : { value: null, typeName: 'PullRequest' };
          }

          return { value: null, typeName: null };
        }

        case '__type': {
          const typeName = stringArg(field, 'name', variables);
          if (typeName === undefined) return { value: null, typeName: '__Type' };
          const introspection = buildTypeIntrospection(typeName);
          return introspection
            ? { value: introspection, typeName: '__Type' }
            : { value: null, typeName: '__Type' };
        }

        default:
          return { value: null, typeName: null };
      }
    }

    case 'User': {
      switch (name) {
        case 'id':
          return { value: globalId('User', '1000'), typeName: null };
        case 'login':
          return { value: VIEWER.login, typeName: null };
        case 'name':
          return { value: VIEWER.name, typeName: null };
        case 'email':
          return { value: VIEWER.email, typeName: null };
        default:
          return { value: null, typeName: null };
      }
    }

    case 'Repository': {
      const view = parent as RepoView;
      const repo = view._repo;

      switch (name) {
        case 'id':
          return { value: globalId('Repository', String(repo.id)), typeName: null };
        case 'databaseId':
          return { value: repo.id, typeName: null };
        case 'name':
          return { value: repo.name, typeName: null };
        case 'nameWithOwner':
          return { value: repo.full_name, typeName: null };
        case 'owner':
          return { value: VIEWER, typeName: 'User' };

        case 'defaultBranchRef': {
          const tip = forge.refOid(repo, `refs/heads/${repo.default_branch}`);
          if (tip === ZERO) return { value: null, typeName: 'Ref' };
          return {
            value: { __typename: 'Ref', name: repo.default_branch },
            typeName: 'Ref',
          };
        }

        case 'viewerPermission':
          return { value: 'ADMIN', typeName: null };
        case 'hasIssuesEnabled':
          return { value: true, typeName: null };

        case 'issue': {
          const number = numberArg(field, 'number', variables);
          const issue = number === null ? undefined : repo.issues.get(number);
          return issue
            ? { value: issueView(repo, issue), typeName: 'Issue' }
            : { value: null, typeName: 'Issue' };
        }

        case 'issues':
          return {
            value: makeIssueConnection(context, repo, field),
            typeName: 'IssueConnection',
          };

        case 'pullRequests':
          return {
            value: makePullRequestConnection(context, repo, field),
            typeName: 'PullRequestConnection',
          };

        case 'pullRequest': {
          const number = numberArg(field, 'number', variables);
          const pull = number === null ? undefined : repo.pulls.get(number);
          return pull
            ? { value: pullView(repo, pull), typeName: 'PullRequest' }
            : { value: null, typeName: 'PullRequest' };
        }

        case 'description':
          return { value: repo.description, typeName: null };
        case 'createdAt':
          return { value: repo.created_at, typeName: null };
        case 'updatedAt':
          return { value: repo.updated_at, typeName: null };
        case 'isPrivate':
          return { value: repo.private, typeName: null };

        default:
          return { value: null, typeName: null };
      }
    }

    case 'Ref': {
      const record = parent as Record<string, unknown>;
      if (name === 'name') return { value: record.name ?? null, typeName: null };
      return { value: null, typeName: null };
    }

    case 'Issue': {
      const view = parent as IssueView;
      const repo = view._repo;
      const issue = view._issue;

      switch (name) {
        case 'id':
          return {
            value: globalId('Issue', `${repo.id}:${issue.number}`),
            typeName: null,
          };
        case 'number':
          return { value: issue.number, typeName: null };
        case 'title':
          return { value: issue.title, typeName: null };
        case 'body':
          return { value: issue.body, typeName: null };
        case 'state':
          return { value: issueGraphQlState(issue), typeName: null };
        case 'createdAt':
          return { value: issue.created_at, typeName: null };
        case 'updatedAt':
          return { value: issue.updated_at, typeName: null };
        case 'author':
          return { value: issue.user ?? VIEWER, typeName: 'User' };
        case 'comments':
          return {
            value: makeIssueCommentConnection(repo, issue),
            typeName: 'IssueCommentConnection',
          };
        case 'labels':
          return {
            value: makeLabelConnection(repo, issue),
            typeName: 'LabelConnection',
          };
        case 'url':
          return { value: null, typeName: null };
        case 'closedAt':
          return { value: issue.closed_at, typeName: null };
        default:
          return { value: null, typeName: null };
      }
    }

    case 'IssueConnection':
    case 'PullRequestConnection':
    case 'PullRequestChangedFileConnection':
    case 'IssueCommentConnection':
    case 'LabelConnection': {
      const record = parent as Record<string, unknown>;
      switch (name) {
        case 'nodes':
          return {
            value: Array.isArray(record.nodes) ? record.nodes : [],
            typeName: connectionNodeType(record),
          };
        case 'pageInfo':
          return { value: record.pageInfo ?? null, typeName: 'PageInfo' };
        case 'totalCount':
          return { value: record.totalCount ?? null, typeName: null };
        default:
          return { value: null, typeName: null };
      }
    }

    case 'PageInfo': {
      const record = parent as Record<string, unknown>;
      switch (name) {
        case 'hasNextPage':
          return { value: record.hasNextPage ?? null, typeName: null };
        case 'endCursor':
          return { value: record.endCursor ?? null, typeName: null };
        default:
          return { value: null, typeName: null };
      }
    }

    case 'PullRequest': {
      const view = parent as PullView;
      const repo = view._repo;
      const pull = view._pull;

      switch (name) {
        case 'id':
          return {
            value: globalId('PullRequest', `${repo.id}:${pull.number}`),
            typeName: null,
          };
        case 'number':
          return { value: pull.number, typeName: null };
        case 'title':
          return { value: pull.title, typeName: null };
        case 'body':
          return { value: pull.body, typeName: null };
        case 'state':
          return { value: pullGraphQlState(pull), typeName: null };
        case 'merged':
          return { value: pull.merged, typeName: null };
        case 'mergeable':
          return { value: pullMergeable(forge, repo, pull), typeName: null };
        case 'headRefName':
          return { value: pull.head.ref, typeName: null };
        case 'baseRefName':
          return { value: pull.base.ref, typeName: null };
        case 'files':
          return {
            value: makePullFileConnection(forge, repo, pull, field, variables),
            typeName: 'PullRequestChangedFileConnection',
          };
        case 'headRef':
          return {
            value: { __typename: 'Ref', name: pull.head.ref },
            typeName: 'Ref',
          };
        case 'baseRef':
          return {
            value: { __typename: 'Ref', name: pull.base.ref },
            typeName: 'Ref',
          };
        case 'createdAt':
          return { value: pull.created_at, typeName: null };
        case 'updatedAt':
          return { value: pull.updated_at, typeName: null };
        case 'closedAt':
          return { value: pull.closed_at, typeName: null };
        case 'mergeCommit':
          return { value: null, typeName: null };
        default:
          return { value: null, typeName: null };
      }
    }

    case 'PullRequestChangedFile': {
      const record = parent as Record<string, unknown>;
      switch (name) {
        case 'path':
        case 'status':
        case 'additions':
        case 'deletions':
        case 'changes':
          return { value: record[name] ?? null, typeName: null };
        default:
          return { value: null, typeName: null };
      }
    }

    case 'IssueComment': {
      const view = parent as CommentView;
      const comment = view._comment;
      switch (name) {
        case 'id':
          return {
            value: globalId(
              'IssueComment',
              `${view._repoId}:${view._issueNumber}:${comment.id}`,
            ),
            typeName: null,
          };
        case 'body':
          return { value: comment.body, typeName: null };
        case 'createdAt':
          return { value: comment.created_at, typeName: null };
        default:
          return { value: null, typeName: null };
      }
    }

    case 'CommentEdge': {
      const record = parent as Record<string, unknown>;
      switch (name) {
        case 'node':
          return { value: record.node ?? null, typeName: 'IssueComment' };
        case 'cursor':
          return { value: record.cursor ?? null, typeName: null };
        default:
          return { value: null, typeName: null };
      }
    }

    case 'Label': {
      const record = parent as Record<string, unknown>;
      switch (name) {
        case 'id':
        case 'name':
        case 'color':
        case 'description':
          return { value: record[name] ?? null, typeName: null };
        default:
          return { value: null, typeName: null };
      }
    }

    case 'Mutation': {
      switch (name) {
        case 'createIssue':
          return resolveCreateIssue(context, field);
        case 'closeIssue':
          return resolveCloseIssue(context, field);
        case 'createPullRequest':
          return resolveCreatePullRequest(context, field);
        case 'mergePullRequest':
          return resolveMergePullRequest(context, field);
        case 'addComment':
          return resolveAddComment(context, field);
        default:
          return { value: null, typeName: null };
      }
    }

    case 'CreateIssuePayload':
    case 'CloseIssuePayload': {
      const record = parent as Record<string, unknown>;
      switch (name) {
        case 'issue':
          return { value: record.issue ?? null, typeName: 'Issue' };
        case 'clientMutationId':
          return { value: record.clientMutationId ?? null, typeName: null };
        default:
          return { value: null, typeName: null };
      }
    }

    case 'CreatePullRequestPayload':
    case 'MergePullRequestPayload': {
      const record = parent as Record<string, unknown>;
      switch (name) {
        case 'pullRequest':
          return { value: record.pullRequest ?? null, typeName: 'PullRequest' };
        case 'clientMutationId':
          return { value: record.clientMutationId ?? null, typeName: null };
        default:
          return { value: null, typeName: null };
      }
    }

    case 'AddCommentPayload': {
      const record = parent as Record<string, unknown>;
      switch (name) {
        case 'commentEdge':
          return { value: record.commentEdge ?? null, typeName: 'CommentEdge' };
        case 'clientMutationId':
          return { value: record.clientMutationId ?? null, typeName: null };
        default:
          return { value: null, typeName: null };
      }
    }

    case '__Type': {
      const record = parent as Record<string, unknown>;
      switch (name) {
        case 'name':
          return { value: record.name ?? null, typeName: null };
        case 'kind':
          return { value: record.kind ?? null, typeName: null };
        case 'fields':
          return { value: record.fields ?? null, typeName: '__Field' };
        case 'enumValues':
          return { value: record.enumValues ?? null, typeName: '__EnumValue' };
        default:
          return { value: null, typeName: null };
      }
    }

    case '__Field':
    case '__EnumValue': {
      const record = parent as Record<string, unknown>;
      switch (name) {
        case 'name':
          return { value: record.name ?? null, typeName: null };
        case 'description':
          return { value: record.description ?? null, typeName: null };
        default:
          return { value: null, typeName: null };
      }
    }

    default:
      return { value: null, typeName: null };
  }
}

function resolveCreateIssue(context: ExecContext, field: FieldNode): Resolved {
  const input = mutationInput(field, context.variables);
  const repo = resolveRepositoryInput(context.forge, input.repositoryId);

  if (!repo) {
    context.errors.push({ message: 'Could not resolve repositoryId.' });
    return { value: null, typeName: 'CreateIssuePayload' };
  }

  const title = typeof input.title === 'string' ? input.title : '';
  const body = typeof input.body === 'string' ? input.body : '';
  const labels = Array.isArray(input.labels)
    ? input.labels.filter((label): label is string => typeof label === 'string')
    : [];

  const issue = context.forge.createIssue(repo, title, body, labels);

  return {
    value: {
      __typename: 'CreateIssuePayload',
      issue: issueView(repo, issue),
      clientMutationId: mutationId(input),
    },
    typeName: 'CreateIssuePayload',
  };
}

function resolveCloseIssue(context: ExecContext, field: FieldNode): Resolved {
  const input = mutationInput(field, context.variables);
  const location = resolveIssueInput(context.forge, input.issueId);

  if (!location) {
    context.errors.push({ message: 'Could not resolve issueId.' });
    return { value: null, typeName: 'CloseIssuePayload' };
  }

  const { repo, issue } = location;
  issue.state = 'closed';
  issue.closed_at = context.forge.nowIso();
  issue.updated_at = context.forge.nowIso();

  return {
    value: {
      __typename: 'CloseIssuePayload',
      issue: issueView(repo, issue),
      clientMutationId: mutationId(input),
    },
    typeName: 'CloseIssuePayload',
  };
}

function resolveCreatePullRequest(context: ExecContext, field: FieldNode): Resolved {
  const input = mutationInput(field, context.variables);
  const repo = resolveRepositoryInput(context.forge, input.repositoryId);

  if (!repo) {
    context.errors.push({ message: 'Could not resolve repositoryId.' });
    return { value: null, typeName: 'CreatePullRequestPayload' };
  }

  const title = typeof input.title === 'string' ? input.title : '';
  const body = typeof input.body === 'string' ? input.body : '';
  const baseRefName = typeof input.baseRefName === 'string' ? input.baseRefName : '';
  const headRefName = typeof input.headRefName === 'string' ? input.headRefName : '';

  const pull = context.forge.createPull(repo, title, body, headRefName, baseRefName);

  if (!pull) {
    context.errors.push({ message: 'Invalid head or base.' });
    return { value: null, typeName: 'CreatePullRequestPayload' };
  }

  return {
    value: {
      __typename: 'CreatePullRequestPayload',
      pullRequest: pullView(repo, pull),
      clientMutationId: mutationId(input),
    },
    typeName: 'CreatePullRequestPayload',
  };
}

function resolveMergePullRequest(context: ExecContext, field: FieldNode): Resolved {
  const input = mutationInput(field, context.variables);
  const location = resolvePullInput(context.forge, input.pullRequestId);

  if (!location) {
    context.errors.push({ message: 'Could not resolve pullRequestId.' });
    return { value: null, typeName: 'MergePullRequestPayload' };
  }

  const { repo, pull } = location;

  if (pull.merged || pull.state === 'closed') {
    context.errors.push({ message: 'Pull request is already merged.' });
    return { value: null, typeName: 'MergePullRequestPayload' };
  }

  const baseTip = context.forge.refOid(repo, `refs/heads/${pull.base.ref}`);
  const headTip = context.forge.refOid(repo, `refs/heads/${pull.head.ref}`);
  const merged = context.forge.squashMergeTree(repo, baseTip, headTip);

  if (merged.conflict) {
    context.errors.push({ message: 'Merge conflict.' });
    return { value: null, typeName: 'MergePullRequestPayload' };
  }

  const epoch = Math.floor(new Date(context.forge.nowIso()).getTime() / 1000);
  const message = `${pull.title} (#${pull.number})`;
  const commitText = [
    `tree ${merged.treeOid}`,
    `parent ${baseTip}`,
    `author ada iversen <adaiversen@example.invalid> ${epoch} +0000`,
    `committer ada iversen <adaiversen@example.invalid> ${epoch} +0000`,
    '',
    `${message}\n`,
  ].join('\n');

  const squashOid = context.forge.putObject(
    repo,
    'commit',
    Buffer.from(commitText, 'utf8'),
  );

  context.forge.setRef(repo, `refs/heads/${pull.base.ref}`, squashOid);

  pull.state = 'closed';
  pull.merged = true;
  pull.merged_at = context.forge.nowIso();
  pull.merge_commit_sha = squashOid;
  pull.updated_at = context.forge.nowIso();
  repo.updated_at = context.forge.nowIso();

  return {
    value: {
      __typename: 'MergePullRequestPayload',
      pullRequest: pullView(repo, pull),
      clientMutationId: mutationId(input),
    },
    typeName: 'MergePullRequestPayload',
  };
}

function resolveAddComment(context: ExecContext, field: FieldNode): Resolved {
  const input = mutationInput(field, context.variables);
  const location = resolveIssueInput(context.forge, input.subjectId);

  if (!location) {
    context.errors.push({ message: 'Could not resolve subjectId.' });
    return { value: null, typeName: 'AddCommentPayload' };
  }

  const { repo, issue } = location;
  const body = typeof input.body === 'string' ? input.body : '';
  const stamp = context.forge.nowIso();

  repo.commentSeq += 1;

  const comment: IssueCommentData = {
    id: repo.commentSeq,
    body,
    user: VIEWER,
    created_at: stamp,
    updated_at: stamp,
  };

  issue.comments.push(comment);

  return {
    value: {
      __typename: 'AddCommentPayload',
      commentEdge: {
        __typename: 'CommentEdge',
        node: issueCommentView(repo.id, issue.number, comment),
        cursor: cursorFor(comment.id),
      },
      clientMutationId: mutationId(input),
    },
    typeName: 'AddCommentPayload',
  };
}

function mutationInput(
  field: FieldNode,
  variables: Record<string, unknown>,
): Record<string, unknown> {
  const value = argValue(field, 'input', variables);
  return isRecord(value) ? value : {};
}

function mutationId(input: Record<string, unknown>): string | null {
  const value = input.clientMutationId;
  return typeof value === 'string' || value === null ? value : null;
}

function argValue(
  field: FieldNode,
  name: string,
  variables: Record<string, unknown>,
): unknown {
  const found = field.args.find((arg) => arg.name === name);
  return found ? resolveValue(found.value, variables) : undefined;
}

function stringArg(
  field: FieldNode,
  name: string,
  variables: Record<string, unknown>,
): string | undefined {
  const value = argValue(field, name, variables);
  return typeof value === 'string' ? value : undefined;
}

function numberArg(
  field: FieldNode,
  name: string,
  variables: Record<string, unknown>,
): number | null {
  const value = argValue(field, name, variables);
  if (typeof value === 'number' && Number.isFinite(value)) {
    return Math.trunc(value);
  }
  return null;
}

function firstArg(field: FieldNode, variables: Record<string, unknown>): number {
  const value = argValue(field, 'first', variables);
  if (typeof value === 'number' && Number.isFinite(value)) {
    const n = Math.floor(value);
    return n < 0 ? 0 : n;
  }
  return 30;
}

function repoView(repo: Repo): RepoView {
  return { __typename: 'Repository', _repo: repo };
}

function issueView(repo: Repo, issue: Issue): IssueView {
  return { __typename: 'Issue', _repo: repo, _issue: issue };
}

function pullView(repo: Repo, pull: Pull): PullView {
  return { __typename: 'PullRequest', _repo: repo, _pull: pull };
}

function issueCommentView(
  repoId: number,
  issueNumber: number,
  comment: IssueCommentData,
): CommentView {
  return {
    __typename: 'IssueComment',
    _repoId: repoId,
    _issueNumber: issueNumber,
    _comment: comment,
  };
}

function issueGraphQlState(issue: Issue): 'OPEN' | 'CLOSED' {
  return issue.state === 'open' ? 'OPEN' : 'CLOSED';
}

function pullGraphQlState(pull: Pull): 'OPEN' | 'CLOSED' | 'MERGED' {
  if (pull.merged) return 'MERGED';
  if (pull.state === 'closed') return 'CLOSED';
  return 'OPEN';
}

function pullMergeable(forge: Forge, repo: Repo, pull: Pull): 'MERGEABLE' | 'CONFLICTING' | 'UNKNOWN' {
  if (pull.merged || pull.state === 'closed') return 'UNKNOWN';

  const baseTip = forge.refOid(repo, `refs/heads/${pull.base.ref}`);
  const headTip = forge.refOid(repo, `refs/heads/${pull.head.ref}`);

  return forge.isMergeable(repo, baseTip, headTip) ? 'MERGEABLE' : 'CONFLICTING';
}

function makeIssueConnection(
  context: ExecContext,
  repo: Repo,
  field: FieldNode,
): Record<string, unknown> {
  const variables = context.variables;
  let issues = Array.from(repo.issues.values()).sort((a, b) => a.number - b.number);

  const statesValue = argValue(field, 'states', variables);
  if (Array.isArray(statesValue)) {
    const states = statesValue.filter((value): value is string => typeof value === 'string');
    issues = issues.filter((issue) => states.includes(issueGraphQlState(issue)));
  }

  const orderBy = argValue(field, 'orderBy', variables);
  if (isRecord(orderBy)) {
    const direction = typeof orderBy.direction === 'string'
      ? orderBy.direction.toUpperCase()
      : '';

    if (direction === 'ASC') {
      issues.sort((a, b) => {
        const byDate = a.created_at.localeCompare(b.created_at);
        return byDate !== 0 ? byDate : a.number - b.number;
      });
    } else if (direction === 'DESC') {
      issues.sort((a, b) => {
        const byDate = b.created_at.localeCompare(a.created_at);
        return byDate !== 0 ? byDate : b.number - a.number;
      });
    }
  }

  const totalCount = issues.length;
  const afterValue = argValue(field, 'after', variables);
  const afterNumber = decodeCursor(afterValue);

  if (afterNumber !== null) {
    issues = issues.filter((issue) => issue.number > afterNumber);
  }

  const first = firstArg(field, variables);
  const nodes = issues.slice(0, first);
  const last = nodes.length > 0 ? nodes[nodes.length - 1]! : null;

  return {
    __typename: 'IssueConnection',
    nodes: nodes.map((issue) => issueView(repo, issue)),
    pageInfo: {
      __typename: 'PageInfo',
      hasNextPage: issues.length > nodes.length,
      endCursor: last === null ? null : cursorFor(last.number),
    },
    totalCount,
  };
}

function makePullRequestConnection(
  context: ExecContext,
  repo: Repo,
  field: FieldNode,
): Record<string, unknown> {
  const variables = context.variables;
  let pulls = Array.from(repo.pulls.values())
    .filter((pull) => pull.state === 'open')
    .sort((a, b) => b.number - a.number);

  const totalCount = pulls.length;
  const afterValue = argValue(field, 'after', variables);
  const afterNumber = decodeCursor(afterValue);

  if (afterNumber !== null) {
    pulls = pulls.filter((pull) => pull.number < afterNumber);
  }

  const first = firstArg(field, variables);
  const nodes = pulls.slice(0, first);
  const last = nodes.length > 0 ? nodes[nodes.length - 1]! : null;

  return {
    __typename: 'PullRequestConnection',
    nodes: nodes.map((pull) => pullView(repo, pull)),
    pageInfo: {
      __typename: 'PageInfo',
      hasNextPage: pulls.length > nodes.length,
      endCursor: last === null ? null : cursorFor(last.number),
    },
    totalCount,
  };
}

function makeIssueCommentConnection(
  repo: Repo,
  issue: Issue,
): Record<string, unknown> {
  const nodes = issue.comments.map((comment) =>
    issueCommentView(repo.id, issue.number, comment),
  );

  return {
    __typename: 'IssueCommentConnection',
    nodes,
    pageInfo: {
      __typename: 'PageInfo',
      hasNextPage: false,
      endCursor: null,
    },
    totalCount: nodes.length,
  };
}

function makeLabelConnection(repo: Repo, issue: Issue): Record<string, unknown> {
  const nodes = issue.labels.map((labelName) => {
    const label = repo.labels.get(labelName);
    return {
      __typename: 'Label',
      id: label ? label.id : 0,
      name: label ? label.name : labelName,
      color: label ? label.color : '',
      description: label ? label.description : '',
    };
  });

  return {
    __typename: 'LabelConnection',
    nodes,
    pageInfo: {
      __typename: 'PageInfo',
      hasNextPage: false,
      endCursor: null,
    },
    totalCount: nodes.length,
  };
}

function makePullFileConnection(
  forge: Forge,
  repo: Repo,
  pull: Pull,
  field: FieldNode,
  variables: Record<string, unknown>,
): Record<string, unknown> {
  const baseTip = forge.refOid(repo, `refs/heads/${pull.base.ref}`);
  const headTip = forge.refOid(repo, `refs/heads/${pull.head.ref}`);
  const files = forge.pullFiles(repo, baseTip, headTip);
  const first = firstArg(field, variables);

  const nodes = files.slice(0, first).map((file: PullFileData) => ({
    __typename: 'PullRequestChangedFile',
    path: file.filename,
    status: file.status.toUpperCase(),
    additions: file.additions,
    deletions: file.deletions,
    changes: file.changes,
  }));

  return {
    __typename: 'PullRequestChangedFileConnection',
    nodes,
    pageInfo: {
      __typename: 'PageInfo',
      hasNextPage: false,
      endCursor: null,
    },
    totalCount: files.length,
  };
}

function connectionNodeType(record: Record<string, unknown>): string {
  switch (record.__typename) {
    case 'IssueConnection':
      return 'Issue';
    case 'PullRequestConnection':
      return 'PullRequest';
    case 'PullRequestChangedFileConnection':
      return 'PullRequestChangedFile';
    case 'IssueCommentConnection':
      return 'IssueComment';
    case 'LabelConnection':
      return 'Label';
    default:
      return '';
  }
}

function buildTypeIntrospection(typeName: string): Record<string, unknown> | null {
  const fields = TYPE_FIELD_ORDER[typeName];
  if (fields === undefined) return null;

  const isEnum = typeName === 'SearchType';

  return {
    __typename: '__Type',
    name: typeName,
    kind: isEnum ? 'ENUM' : 'OBJECT',
    fields: isEnum
      ? null
      : fields.map((fieldName) => ({
          __typename: '__Field',
          name: fieldName,
          description: null,
        })),
    enumValues: isEnum
      ? SEARCH_TYPE_ENUM.map((value) => ({
          __typename: '__EnumValue',
          name: value,
          description: null,
        }))
      : null,
  };
}

function findRepoByKey(forge: Forge, key: string): Repo | null {
  const id = Number(key);
  if (!Number.isInteger(id)) return null;

  const byId = forge.getRepoById(id);
  if (byId) return byId;

  for (const repo of forge.repos.values()) {
    if (repo.id === id) return repo;
  }

  return null;
}

function findIssueByKey(forge: Forge, key: string): IssueLocation | null {
  const match = /^(\d+):(\d+)$/.exec(key);
  if (!match) return null;

  const repo = findRepoByKey(forge, match[1]!);
  if (!repo) return null;

  const issue = repo.issues.get(Number(match[2]));
  return issue ? { repo, issue } : null;
}

function findPullByKey(forge: Forge, key: string): PullLocation | null {
  const match = /^(\d+):(\d+)$/.exec(key);
  if (!match) return null;

  const repo = findRepoByKey(forge, match[1]!);
  if (!repo) return null;

  const pull = repo.pulls.get(Number(match[2]));
  return pull ? { repo, pull } : null;
}

function resolveRepositoryInput(forge: Forge, value: unknown): Repo | null {
  const decoded = decodeGlobalId(value);
  if (!decoded || decoded.type !== 'Repository') return null;
  return findRepoByKey(forge, decoded.key);
}

function resolveIssueInput(forge: Forge, value: unknown): IssueLocation | null {
  const decoded = decodeGlobalId(value);
  if (!decoded || decoded.type !== 'Issue') return null;
  return findIssueByKey(forge, decoded.key);
}

function resolvePullInput(forge: Forge, value: unknown): PullLocation | null {
  const decoded = decodeGlobalId(value);
  if (!decoded || decoded.type !== 'PullRequest') return null;
  return findPullByKey(forge, decoded.key);
}

function invalidOperationResult(): GQLResult {
  return {
    data: null,
    errors: [{ message: 'Invalid operation' }],
  };
}

function executeBodyEntry(forge: Forge, entry: unknown): GQLResult {
  if (!isRecord(entry) || typeof entry.query !== 'string') {
    return invalidOperationResult();
  }

  if (entry.variables !== undefined && !isRecord(entry.variables)) {
    return invalidOperationResult();
  }

  const variables = isRecord(entry.variables) ? entry.variables : {};
  const operationName = typeof entry.operationName === 'string'
    ? entry.operationName
    : null;

  return executeQuery(forge, entry.query, variables, operationName);
}

function executeQuery(
  forge: Forge,
  query: string,
  variables: Record<string, unknown>,
  operationName: string | null,
): GQLResult {
  try {
    const doc = parseDocumentSource(query);
    const operation = pickOperation(doc, operationName);

    if (!operation) {
      return invalidOperationResult();
    }

    return executeOperation(forge, doc, operation, variables);
  } catch (error) {
    const message = error instanceof Error ? error.message : 'Syntax Error: Invalid query.';
    return {
      data: null,
      errors: [{ message }],
    };
  }
}

function toPublicResult(result: GQLResult): Record<string, unknown> {
  if (result.errors.length > 0) {
    return {
      data: result.data,
      errors: result.errors,
    };
  }

  return { data: result.data };
}

export function handleGraphQL(g: GraphQLRequest): void {
  if (!g.authed) {
    sendError(g.res, 401, 'Requires authentication');
    return;
  }

  if (g.bodyMalformed) {
    sendJson(g.res, 400, {
      errors: [{ message: 'Invalid JSON body' }],
    });
    return;
  }

  if (Array.isArray(g.body)) {
    const results = g.body.map((entry) => toPublicResult(executeBodyEntry(g.forge, entry)));
    sendJson(g.res, 200, results);
    return;
  }

  if (isRecord(g.body) && typeof g.body.query === 'string') {
    const variables = isRecord(g.body.variables) ? g.body.variables : {};
    const operationName = typeof g.body.operationName === 'string'
      ? g.body.operationName
      : null;

    const result = executeQuery(g.forge, g.body.query, variables, operationName);
    sendJson(g.res, 200, toPublicResult(result));
    return;
  }

  sendJson(g.res, 400, {
    errors: [{ message: 'Must provide query string.' }],
  });
}