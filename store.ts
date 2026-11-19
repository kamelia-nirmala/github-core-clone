// The forge's in-memory store: every entity the four surfaces share, and the git object
// model underneath them. One process, one store, no disk: the trial is a single run and
// the state export must be byte-identical across replays, so every timestamp comes from a
// deterministic logical clock (never Date.now()) and every id from a counter.
//
// Viewer: the token names the scenario's viewer (wire.ts buildScenario, fixed seed):
// login "adaiversen", id 1000. The server hardcodes it; the token only gates auth.

import { createHash } from 'node:crypto';

export const VIEWER_LOGIN = 'adaiversen';
export const VIEWER_ID = 1000;
export const VIEWER_NAME = 'ada iversen';
export const VIEWER_EMAIL = 'adaiversen@example.invalid';

/** The logical clock's epoch: 2025-01-01T00:00:00Z. Every created/updated stamp is this plus a tick. */
export const CLOCK_EPOCH_SEC = 1_735_689_600;

/** A git object: its type and raw bytes. The id is sha1("<type> <len>\0" + data). */
export interface GitObject {
  readonly type: 'commit' | 'tree' | 'blob' | 'tag';
  readonly data: Buffer;
}

export type GitObjectType = GitObject['type'];

export function hashObject(type: GitObjectType, data: Buffer): string {
  const header = Buffer.from(`${type} ${data.length}\0`, 'utf8');
  return createHash('sha1').update(header).update(data).digest('hex');
}

export interface UserView {
  readonly id: number;
  readonly login: string;
  readonly name: string;
  readonly email: string;
  readonly type: 'User';
}

export const VIEWER: UserView = Object.freeze({
  id: VIEWER_ID,
  login: VIEWER_LOGIN,
  name: VIEWER_NAME,
  email: VIEWER_EMAIL,
  type: 'User',
});

export interface Label {
  readonly id: number;
  readonly name: string;
  readonly color: string;
  readonly description: string;
}

export interface IssueComment {
  readonly id: number;
  readonly body: string;
  readonly user: UserView;
  readonly created_at: string;
  readonly updated_at: string;
}

export interface Issue {
  readonly id: number;
  readonly number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  labels: string[];
  readonly user: UserView;
  comments: IssueComment[];
  readonly created_at: string;
  updated_at: string;
  closed_at: string | null;
}

export interface PullFile {
  readonly filename: string;
  readonly status: 'added' | 'modified' | 'removed';
  readonly additions: number;
  readonly deletions: number;
  readonly changes: number;
}

export interface Pull {
  readonly id: number;
  readonly number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  merged: boolean;
  readonly head: { ref: string; sha: string; label: string };
  readonly base: { ref: string; sha: string; label: string };
  readonly user: UserView;
  readonly created_at: string;
  updated_at: string;
  closed_at: string | null;
  merged_at: string | null;
  merge_commit_sha: string | null;
  /** Files changed between base and head at creation time (recomputed on read from live tips). */
  files: PullFile[];
}

export interface Repo {
  readonly id: number;
  readonly owner: string;
  readonly name: string;
  readonly full_name: string;
  description: string;
  private: boolean;
  readonly default_branch: string;
  readonly created_at: string;
  updated_at: string;
  /** refname -> commit oid (or ZERO for unborn). Includes refs/heads/*, refs/tags/*, HEAD is virtual. */
  readonly refs: Map<string, string>;
  /** oid -> object. */
  readonly objects: Map<string, GitObject>;
  readonly issues: Map<number, Issue>;
  readonly pulls: Map<number, Pull>;
  readonly labels: Map<string, Label>;
  issueSeq: number;
  pullSeq: number;
  labelSeq: number;
  commentSeq: number;
  /** Bumped on every object/ref mutation; lets the git fetch cache invalidate exactly. */
  version: number;
}

/** A flattened tree: path -> { oid, mode }. */
export type FlatTree = Map<string, { oid: string; mode: string }>;

export class Forge {
  /** owner/name -> repo. All repos belong to the viewer in this trial. */
  readonly repos = new Map<string, Repo>();
  private repoSeq = 1;
  /** Logical clock ticks: each stamp consumes one tick (1s). */
  private clock = 0;
  /** Global id counters for issues/pulls/comments are per-repo (GitHub numbers are per-repo). */

  /** Next ISO timestamp from the logical clock. */
  nowIso(): string {
    this.clock += 1;
    return new Date((CLOCK_EPOCH_SEC + this.clock) * 1000).toISOString();
  }

  /** Peek the current logical time without consuming a tick. */
  peekIso(): string {
    return new Date((CLOCK_EPOCH_SEC + this.clock) * 1000).toISOString();
  }

  // ---------------------------------------------------------- repos

  createRepo(name: string, description: string, isPrivate: boolean, defaultBranch: string): Repo {
    const id = this.repoSeq++;
    const stamp = this.nowIso();
    const repo: Repo = {
      id,
      owner: VIEWER_LOGIN,
      name,
      full_name: `${VIEWER_LOGIN}/${name}`,
      description,
      private: isPrivate,
      default_branch: defaultBranch,
      created_at: stamp,
      updated_at: stamp,
      refs: new Map(),
      objects: new Map(),
      issues: new Map(),
      pulls: new Map(),
      labels: new Map(),
      issueSeq: 0,
      pullSeq: 0,
      labelSeq: 0,
      commentSeq: 0,
      version: 0,
    };
    // Default labels, as GitHub ships them.
    const defaults: Array<[string, string, string]> = [
      ['bug', 'd73a4a', "Something isn't working"],
      ['enhancement', 'a2eeef', 'New feature or request'],
      ['documentation', '0075ca', 'Improvements or additions to documentation'],
      ['question', 'd876e3', 'Further information is requested'],
      ['good first issue', '7057ff', 'Good for newcomers'],
      ['help wanted', '008672', 'Extra attention is needed'],
    ];
    for (const [lname, color, desc] of defaults) {
      repo.labelSeq += 1;
      repo.labels.set(lname, { id: repo.labelSeq, name: lname, color, description: desc });
    }
    this.repos.set(repo.full_name, repo);
    return repo;
  }

  getRepo(owner: string, name: string): Repo | undefined {
    return this.repos.get(`${owner}/${name}`);
  }

  /** Find a repo by its numeric id (GraphQL databaseId). */
  getRepoById(id: number): Repo | undefined {
    for (const repo of this.repos.values()) {
      if (repo.id === id) return repo;
    }
    return undefined;
  }

  deleteRepo(owner: string, name: string): boolean {
    return this.repos.delete(`${owner}/${name}`);
  }

  /** Repos owned by the viewer, in creation order. */
  viewerRepos(): Repo[] {
    return [...this.repos.values()].filter((r) => r.owner === VIEWER_LOGIN);
  }

  // ---------------------------------------------------------- git objects

  putObject(repo: Repo, type: GitObjectType, data: Buffer): string {
    const oid = hashObject(type, data);
    if (!repo.objects.has(oid)) {
      repo.objects.set(oid, { type, data });
      repo.version += 1;
    }
    return oid;
  }

  getObject(repo: Repo, oid: string): GitObject | undefined {
    return repo.objects.get(oid);
  }

  /** The ref's current oid, or the zero oid when the ref does not exist. */
  refOid(repo: Repo, ref: string): string {
    return repo.refs.get(ref) ?? '0'.repeat(40);
  }

  setRef(repo: Repo, ref: string, oid: string): void {
    if (repo.refs.get(ref) !== oid) {
      repo.refs.set(ref, oid);
      repo.version += 1;
    }
  }

  deleteRef(repo: Repo, ref: string): boolean {
    const deleted = repo.refs.delete(ref);
    if (deleted) repo.version += 1;
    return deleted;
  }

  /** HEAD's target branch ref. Always refs/heads/<default_branch> in this forge. */
  headRef(repo: Repo): string {
    return `refs/heads/${repo.default_branch}`;
  }

  /** The default branch's tip oid, or zero when unborn. */
  headOid(repo: Repo): string {
    return this.refOid(repo, this.headRef(repo));
  }

  // ---------------------------------------------------------- trees

  /** Parse a tree object's raw bytes into entries. */
  parseTree(data: Buffer): Array<{ mode: string; name: string; oid: string }> {
    const entries: Array<{ mode: string; name: string; oid: string }> = [];
    let off = 0;
    while (off < data.length) {
      const space = data.indexOf(0x20, off);
      if (space < 0) break;
      const mode = data.subarray(off, space).toString('utf8');
      const nul = data.indexOf(0x00, space + 1);
      if (nul < 0) break;
      const name = data.subarray(space + 1, nul).toString('utf8');
      const oid = data.subarray(nul + 1, nul + 21).toString('hex');
      entries.push({ mode, name, oid });
      off = nul + 21;
    }
    return entries;
  }

  /** Build a tree object from entries (sorted git order: dirs sort as if named "name/"). */
  buildTree(entries: Array<{ mode: string; name: string; oid: string }>): Buffer {
    const sorted = [...entries].sort((a, b) => {
      const an = a.mode === '40000' ? `${a.name}/` : a.name;
      const bn = b.mode === '40000' ? `${b.name}/` : b.name;
      return an < bn ? -1 : an > bn ? 1 : 0;
    });
    const parts: Buffer[] = [];
    for (const e of sorted) {
      parts.push(Buffer.from(`${e.mode} ${e.name}\0`, 'utf8'));
      parts.push(Buffer.from(e.oid, 'hex'));
    }
    return Buffer.concat(parts);
  }

  /** Flatten a tree (recursively) into path -> {oid, mode}. Missing tree => empty. */
  flattenTree(repo: Repo, treeOid: string, prefix = ''): FlatTree {
    const out: FlatTree = new Map();
    const obj = repo.objects.get(treeOid);
    if (!obj || obj.type !== 'tree') return out;
    for (const entry of this.parseTree(obj.data)) {
      const path = prefix === '' ? entry.name : `${prefix}/${entry.name}`;
      if (entry.mode === '40000') {
        for (const [p, v] of this.flattenTree(repo, entry.oid, path)) out.set(p, v);
      } else {
        out.set(path, { oid: entry.oid, mode: entry.mode });
      }
    }
    return out;
  }

  /** Build (and store) a tree from a flat path map. Directories are created as needed. */
  buildTreeFromFlat(repo: Repo, flat: FlatTree): string {
    // Group paths by directory.
    const buildDir = (dir: string): string => {
      const entries: Array<{ mode: string; name: string; oid: string }> = [];
      const subdirs = new Set<string>();
      for (const [path, v] of flat) {
        const rel = dir === '' ? path : path.startsWith(`${dir}/`) ? path.slice(dir.length + 1) : null;
        if (rel === null) continue;
        const slash = rel.indexOf('/');
        if (slash < 0) {
          entries.push({ mode: v.mode, name: rel, oid: v.oid });
        } else {
          subdirs.add(rel.slice(0, slash));
        }
      }
      for (const sub of subdirs) {
        const subOid = buildDir(dir === '' ? sub : `${dir}/${sub}`);
        entries.push({ mode: '40000', name: sub, oid: subOid });
      }
      const data = this.buildTree(entries);
      return this.putObject(repo, 'tree', data);
    };
    return buildDir('');
  }

  /** The commit's tree oid, or '' when the commit is missing/unparseable. */
  commitTree(repo: Repo, commitOid: string): string {
    const obj = repo.objects.get(commitOid);
    if (!obj || obj.type !== 'commit') return '';
    const text = obj.data.toString('utf8');
    const m = /^tree ([0-9a-f]{40})$/m.exec(text);
    return m ? (m[1] as string) : '';
  }

  /** The commit's parent oids, in order. */
  commitParents(repo: Repo, commitOid: string): string[] {
    const obj = repo.objects.get(commitOid);
    if (!obj || obj.type !== 'commit') return [];
    const text = obj.data.toString('utf8');
    const out: string[] = [];
    for (const m of text.matchAll(/^parent ([0-9a-f]{40})$/gm)) out.push(m[1] as string);
    return out;
  }

  /** All ancestors of `tip` (inclusive), newest-first by BFS over parents. */
  collectAncestors(repo: Repo, tip: string): Set<string> {
    const seen = new Set<string>();
    const queue = [tip];
    while (queue.length > 0) {
      const oid = queue.pop() as string;
      if (oid === '0'.repeat(40) || seen.has(oid)) continue;
      seen.add(oid);
      for (const p of this.commitParents(repo, oid)) queue.push(p);
    }
    return seen;
  }

  /** The merge-base: the newest commit reachable from both tips (by first-parent walk order). */
  mergeBase(repo: Repo, a: string, b: string): string {
    if (a === '0'.repeat(40) || b === '0'.repeat(40)) return '0'.repeat(40);
    const aAnc = this.collectAncestors(repo, a);
    // Walk b's history newest-first; the first commit also in a's set is the base.
    const seen = new Set<string>();
    const queue = [b];
    while (queue.length > 0) {
      const oid = queue.shift() as string;
      if (oid === '0'.repeat(40) || seen.has(oid)) continue;
      seen.add(oid);
      if (aAnc.has(oid)) return oid;
      for (const p of this.commitParents(repo, oid)) queue.push(p);
    }
    return '0'.repeat(40);
  }

  /** Is `ancestor` reachable from `tip`? (fast-forward check). */
  isAncestor(repo: Repo, ancestor: string, tip: string): boolean {
    if (ancestor === '0'.repeat(40)) return true;
    return this.collectAncestors(repo, tip).has(ancestor);
  }

  /** Diff two flat trees: [{path, status, oldOid, newOid}]. */
  diffFlat(oldTree: FlatTree, newTree: FlatTree): Array<{ path: string; status: 'added' | 'modified' | 'removed'; oldOid: string | null; newOid: string | null }> {
    const out: Array<{ path: string; status: 'added' | 'modified' | 'removed'; oldOid: string | null; newOid: string | null }> = [];
    const paths = new Set([...oldTree.keys(), ...newTree.keys()]);
    for (const path of [...paths].sort()) {
      const o = oldTree.get(path);
      const n = newTree.get(path);
      if (o && !n) out.push({ path, status: 'removed', oldOid: o.oid, newOid: null });
      else if (!o && n) out.push({ path, status: 'added', oldOid: null, newOid: n.oid });
      else if (o && n && o.oid !== n.oid) out.push({ path, status: 'modified', oldOid: o.oid, newOid: n.oid });
    }
    return out;
  }

  /** Count added/deleted lines between two blob contents (naive line diff). */
  lineCounts(oldContent: Buffer | null, newContent: Buffer | null): { additions: number; deletions: number } {
    const oldLines = oldContent === null ? [] : oldContent.toString('utf8').split('\n');
    const newLines = newContent === null ? [] : newContent.toString('utf8').split('\n');
    // Drop the trailing empty element from a final newline for counting.
    const trim = (ls: string[]): string[] => (ls.length > 0 && ls[ls.length - 1] === '' ? ls.slice(0, -1) : ls);
    const a = trim(oldLines);
    const b = trim(newLines);
    const aSet = new Map<string, number>();
    for (const l of a) aSet.set(l, (aSet.get(l) ?? 0) + 1);
    const bSet = new Map<string, number>();
    for (const l of b) bSet.set(l, (bSet.get(l) ?? 0) + 1);
    let additions = 0;
    let deletions = 0;
    for (const [l, n] of bSet) additions += Math.max(0, n - (aSet.get(l) ?? 0));
    for (const [l, n] of aSet) deletions += Math.max(0, n - (bSet.get(l) ?? 0));
    return { additions, deletions };
  }

  /** Pull files between base tip and head tip (diff of their trees). */
  pullFiles(repo: Repo, baseOid: string, headOid: string): PullFile[] {
    const baseTree = this.flattenTree(repo, this.commitTree(repo, baseOid));
    const headTree = this.flattenTree(repo, this.commitTree(repo, headOid));
    const files: PullFile[] = [];
    for (const d of this.diffFlat(baseTree, headTree)) {
      const oldObj = d.oldOid ? repo.objects.get(d.oldOid) : undefined;
      const newObj = d.newOid ? repo.objects.get(d.newOid) : undefined;
      const counts = this.lineCounts(oldObj?.data ?? null, newObj?.data ?? null);
      files.push({
        filename: d.path,
        status: d.status,
        additions: counts.additions,
        deletions: counts.deletions,
        changes: counts.additions + counts.deletions,
      });
    }
    return files;
  }

  /**
   * 3-way merge for a squash merge: baseTip's tree + head's changes since merge-base.
   * Returns { conflict: boolean, treeOid: string } — treeOid is the merged tree (stored) when no conflict.
   */
  squashMergeTree(repo: Repo, baseOid: string, headOid: string): { conflict: boolean; treeOid: string } {
    const base = this.mergeBase(repo, baseOid, headOid);
    const baseTree = this.flattenTree(repo, this.commitTree(repo, base));
    const curTree = this.flattenTree(repo, this.commitTree(repo, baseOid));
    const headTree = this.flattenTree(repo, this.commitTree(repo, headOid));
    const baseChanges = this.diffFlat(baseTree, curTree);
    const headChanges = this.diffFlat(baseTree, headTree);
    const baseChanged = new Map(baseChanges.map((c) => [c.path, c]));
    for (const hc of headChanges) {
      const bc = baseChanged.get(hc.path);
      if (bc && bc.newOid !== hc.newOid) return { conflict: true, treeOid: '' };
    }
    // Apply head's changes onto the current tree.
    const merged = new Map(curTree);
    for (const hc of headChanges) {
      if (hc.status === 'removed') merged.delete(hc.path);
      else {
        const hv = headTree.get(hc.path);
        if (hv) merged.set(hc.path, hv);
      }
    }
    const treeOid = this.buildTreeFromFlat(repo, merged);
    return { conflict: false, treeOid };
  }

  /** Is the head mergeable into base (no 3-way conflict)? */
  isMergeable(repo: Repo, baseOid: string, headOid: string): boolean {
    return !this.squashMergeTree(repo, baseOid, headOid).conflict;
  }

  // ---------------------------------------------------------- issues / pulls

  createIssue(repo: Repo, title: string, body: string, labels: string[]): Issue {
    repo.issueSeq += 1;
    const stamp = this.nowIso();
    const issue: Issue = {
      id: repo.issueSeq + 100000,
      number: repo.issueSeq,
      title,
      body,
      state: 'open',
      labels: [...labels],
      user: VIEWER,
      comments: [],
      created_at: stamp,
      updated_at: stamp,
      closed_at: null,
    };
    repo.issues.set(issue.number, issue);
    return issue;
  }

  createPull(repo: Repo, title: string, body: string, headRef: string, baseRef: string): Pull | null {
    const headOid = this.refOid(repo, `refs/heads/${headRef}`);
    const baseOid = this.refOid(repo, `refs/heads/${baseRef}`);
    if (headOid === '0'.repeat(40) || baseOid === '0'.repeat(40)) return null;
    repo.pullSeq += 1;
    const stamp = this.nowIso();
    const pull: Pull = {
      id: repo.pullSeq + 200000,
      number: repo.pullSeq,
      title,
      body,
      state: 'open',
      merged: false,
      head: { ref: headRef, sha: headOid, label: `${VIEWER_LOGIN}:${headRef}` },
      base: { ref: baseRef, sha: baseOid, label: `${VIEWER_LOGIN}:${baseRef}` },
      user: VIEWER,
      created_at: stamp,
      updated_at: stamp,
      closed_at: null,
      merged_at: null,
      merge_commit_sha: null,
      files: this.pullFiles(repo, baseOid, headOid),
    };
    repo.pulls.set(pull.number, pull);
    return pull;
  }
}
