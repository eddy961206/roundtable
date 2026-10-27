"""Find where open PRs collide before a merge makes it someone else's rebase.

usage: pr_overlap.py [--repo-dir DIR] [--prs 61,64] [--json] [--schema-regex RE]
                     [--integrate [--order 61,64] [--test-cmd CMD] [--test-timeout SEC]]

Reads open PRs of ORCH_REPO (tea), fetches their heads and bases into the private ref
namespace refs/orch/* of the local clone (ORCH_REPO_DIR or --repo-dir), then reports:
  - line conflicts per pair (git merge-tree --write-tree)
  - pairs touching the same file, and the same Markdown section (heading path)
  - schema number collisions: ORCH_SCHEMA_REGEX (group 1) read at each head; PRs that
    differ from the base and share one value
  - stacked branches: open PRs and local branches containing another open PR's head, one
    of its commits, or a tip it had before a rebase (reflogs of its branch)
  - --integrate: a temporary detached worktree on the base merges the PRs in order,
    then runs the test command (ORCH_INTEGRATE_TEST, default `go test ./...`); the
    worktree is always removed.
Never pushes, never moves a branch, never touches the remote.
"""
import argparse
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile

import orchlib

HEADING = re.compile(r'^(#{1,6})\s+(.*?)\s*#*\s*$')
FENCE = re.compile(r'^\s*(```|~~~)')
HUNK = re.compile(r'^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@')
PREAMBLE = '(머리말)'


def base_ref(branch):
    return 'refs/orch/base/' + branch


def pull_ref(number):
    return 'refs/orch/pull/%d' % number


def fetch(cfg, repo, bases, numbers):
    specs = ['+refs/heads/%s:%s' % (b, base_ref(b)) for b in sorted(set(bases))]
    specs += ['+refs/pull/%d/head:%s' % (n, pull_ref(n)) for n in sorted(set(numbers))]
    orchlib.git(repo, 'fetch', '-q', '--no-tags', cfg['ORCH_REMOTE'], *specs)


def rev(repo, ref):
    return orchlib.git(repo, 'rev-parse', '--verify', '-q', ref + '^{commit}').stdout.strip()


def is_ancestor(repo, a, b):
    return orchlib.git(repo, 'merge-base', '--is-ancestor', a, b, check=False).returncode == 0


def headings(text):
    """[(line_no, heading path)] of Markdown headings outside code fences."""
    out, stack, fenced = [], [], False
    for i, line in enumerate((text or '').splitlines(), 1):
        if FENCE.match(line):
            fenced = not fenced
            continue
        m = None if fenced else HEADING.match(line)
        if m:
            level = len(m.group(1))
            stack = [s for s in stack if s[0] < level] + [(level, '#' * level + ' ' + m.group(2))]
            out.append((i, ' > '.join(s[1] for s in stack)))
    return out


def section_at(heads, line):
    key = PREAMBLE
    for at, path in heads:
        if at > line:
            break
        key = path
    return key


def hunks_by_file(diff_text):
    """{path: [(old_start, old_count, new_start, new_count)]} from a -U0 diff."""
    out, cur, old_path = {}, None, None
    for line in diff_text.splitlines():
        if line.startswith('--- '):
            old_path = line[6:] if line.startswith('--- a/') else None
        elif line.startswith('+++ '):
            cur = line[6:] if line.startswith('+++ b/') else old_path
            out.setdefault(cur, [])
        elif cur is not None:
            m = HUNK.match(line)
            if m:
                a, b, c, d = m.groups()
                out[cur].append((int(a), 1 if b is None else int(b), int(c), 1 if d is None else int(d)))
    return out


def changes(repo, old, new, schema_re):
    """What new changed relative to old: files, (file, section) pairs, schema values."""
    q = ('-c', 'core.quotepath=false')
    files = [f for f in orchlib.git(repo, *q, 'diff', '--name-only', '-z', old, new).stdout.split('\0') if f]
    md = [f for f in files if f.lower().endswith(('.md', '.markdown'))]
    blobs = orchlib.read_blobs(repo, ['%s:%s' % (r, f) for f in md for r in (old, new)] + ['%s:%s' % (new, f) for f in files])
    sections = set()
    if md:
        diff = orchlib.git(repo, *q, 'diff', '-U0', '--no-color', '--no-renames', old, new, '--', *md).stdout
        for path, hunks in hunks_by_file(diff).items():
            old_h, new_h = headings(blobs.get('%s:%s' % (old, path))), headings(blobs.get('%s:%s' % (new, path)))
            for a, b, c, d in hunks:
                sections.update((path, section_at(old_h, n)) for n in range(a, a + b))
                sections.update((path, section_at(new_h, n)) for n in range(c, c + d))
    schema = {}
    for f in files:
        m = schema_re.search(blobs.get('%s:%s' % (new, f)) or '')
        if m:
            schema[f] = m.group(1)
    return {'files': set(files), 'sections': sections, 'schema': schema}


def analyze(cfg, repo, pulls, schema_re):
    infos = []
    for p in pulls:
        n, base = p['number'], p['base']['ref']
        head = rev(repo, pull_ref(n))
        if not head:
            raise SystemExit('PR #%d head was not fetched (%s)' % (n, pull_ref(n)))
        mb = orchlib.git(repo, 'merge-base', base_ref(base), head).stdout.strip()
        info = {'number': n, 'title': p.get('title', ''), 'branch': p['head']['ref'], 'head': head,
                'api_head': p['head'].get('sha', ''), 'base': base, 'merge_base': mb,
                'mergeable': p.get('mergeable')}
        info.update(changes(repo, mb, head, schema_re))
        infos.append(info)
    return infos


def merge_conflicts(repo, a, b):
    """Conflicted paths when merging commits a and b, [] when clean."""
    r = orchlib.git(repo, '-c', 'core.quotepath=false', 'merge-tree', '--write-tree', '--name-only', '--no-messages', a, b, check=False)
    if r.returncode not in (0, 1):
        raise SystemExit('git merge-tree failed: %s' % r.stderr.strip()[:300])
    return [l for l in r.stdout.splitlines()[1:] if l] if r.returncode == 1 else []


def pairs(repo, infos):
    out = []
    for i, a in enumerate(infos):
        for b in infos[i + 1:]:
            out.append({'a': a['number'], 'b': b['number'],
                        'conflicts': merge_conflicts(repo, a['head'], b['head']),
                        'files': sorted(a['files'] & b['files']),
                        'sections': sorted(a['sections'] & b['sections'])})
    return out


def schema_report(repo, infos, base, schema_re):
    """Rows {pr, file, value, base_value, clash: [other PRs with the same new value]}."""
    files = sorted({f for i in infos for f in i['schema']})
    base_vals = orchlib.read_blobs(repo, ['%s:%s' % (base_ref(base), f) for f in files])
    rows = []
    for i in infos:
        for f, v in sorted(i['schema'].items()):
            m = schema_re.search(base_vals.get('%s:%s' % (base_ref(base), f)) or '')
            bv = m.group(1) if m else ''
            if v == bv:
                continue
            rows.append({'pr': i['number'], 'file': f, 'value': v, 'base_value': bv})
    for r in rows:
        r['clash'] = [o['pr'] for o in rows if o is not r and o['file'] == r['file'] and o['value'] == r['value']]
    return rows


def range_roots(repo, head, *exclude):
    """Commits of `head ^exclude...` whose parents are all outside that range."""
    lines = orchlib.git(repo, 'rev-list', '--parents', head, *('^' + e for e in exclude)).stdout.split('\n')
    inside = {l.split()[0] for l in lines if l}
    return [l.split()[0] for l in lines if l and not set(l.split()[1:]) & inside]


def old_heads(repo, remote, branch):
    """Earlier tips of the PR branch from local reflogs (before rebases/amends)."""
    seen = []
    for ref in ('refs/heads/' + branch, 'refs/remotes/%s/%s' % (remote, branch)):
        for sha in orchlib.git(repo, 'reflog', 'show', '--format=%H', ref, '--', check=False).stdout.split():
            if sha not in seen:
                seen.append(sha)
    return seen


def stacked(repo, infos, base, remote):
    """Rows {branch, under, via}: open PRs and local branches built on another open PR —
    on its head, on one of its commits (forked mid-way), or on a tip the PR had before a
    rebase (from the reflogs of its branch)."""
    by_ref = {pull_ref(i['number']): i for i in infos}
    pr_heads = {(i['branch'], i['head']) for i in infos}
    scan = ['refs/heads'] + list(by_ref)
    rows = []
    for p in infos:
        if is_ancestor(repo, p['head'], base_ref(base)):
            continue
        marks = [(p['head'], 'head')]
        # P's own commits only: a PR stacked on Q also carries Q's commits.
        below = [q['head'] for q in infos if q is not p and is_ancestor(repo, q['head'], p['head'])]
        marks += [(c, '중간 커밋 %s' % c[:7]) for c in range_roots(repo, p['head'], base_ref(base), *below) if c != p['head']]
        marks += [(c, '재기반 전 head %s' % c[:7]) for c in old_heads(repo, remote, p['branch'])
                  if not is_ancestor(repo, c, base_ref(base)) and not is_ancestor(repo, c, p['head'])]
        own = {pull_ref(p['number']), 'refs/heads/' + p['branch']}
        seen = set()
        for commit, via in marks:
            refs = orchlib.git(repo, 'for-each-ref', '--contains', commit, '--format=%(refname) %(objectname)', *scan).stdout
            for line in filter(None, refs.split('\n')):
                ref, sha = line.split(' ', 1)
                if sha == p['head'] or ref in own or ref in seen:
                    continue
                seen.add(ref)
                if ref in by_ref:
                    o = by_ref[ref]
                    label = 'PR #%d (%s)' % (o['number'], o['branch'])
                else:
                    name = ref[len('refs/heads/'):]
                    if (name, sha) in pr_heads:
                        continue
                    label = 'local ' + name
                rows.append({'branch': label, 'under': p['number'], 'under_branch': p['branch'], 'via': via})
    return rows


def integrate(cfg, repo, infos, base, order, test_cmd, timeout):
    """Merges PRs in order on a temporary detached worktree, then runs test_cmd there."""
    by_num = {i['number']: i for i in infos}
    seq = [by_num[n] for n in order] if order else infos
    tmp = tempfile.mkdtemp(prefix='orch-integrate-')
    wt = os.path.join(tmp, 'wt')
    result = {'base': rev(repo, base_ref(base)), 'steps': [], 'test': None, 'worktree': wt, 'removed': False}
    try:
        orchlib.git(repo, 'worktree', 'add', '-q', '--detach', wt, base_ref(base))
        ident = ('-c', 'user.name=orchestrate', '-c', 'user.email=orchestrate@localhost', '-c', 'commit.gpgsign=false')
        for i in seq:
            r = orchlib.git(wt, *ident, 'merge', '--no-ff', '--no-edit', '--no-verify', '-q', i['head'], check=False)
            if r.returncode == 0:
                result['steps'].append({'pr': i['number'], 'merged': True, 'conflicts': []})
                continue
            bad = orchlib.git(wt, '-c', 'core.quotepath=false', 'diff', '--name-only', '--diff-filter=U').stdout.split()
            orchlib.git(wt, 'merge', '--abort', check=False)
            result['steps'].append({'pr': i['number'], 'merged': False, 'conflicts': bad or [r.stderr.strip()[:200]]})
        if test_cmd:
            try:
                t = subprocess.run(test_cmd, shell=True, cwd=wt, capture_output=True, text=True,
                                   encoding='utf-8', errors='replace', timeout=timeout)
                tail = (t.stdout + t.stderr).strip().splitlines()[-30:]
                result['test'] = {'cmd': test_cmd, 'exit': t.returncode, 'tail': tail}
            except subprocess.TimeoutExpired:
                result['test'] = {'cmd': test_cmd, 'exit': None, 'tail': ['timeout after %ds' % timeout]}
    finally:
        orchlib.git(repo, 'worktree', 'remove', '--force', wt, check=False)
        shutil.rmtree(tmp, ignore_errors=True)
        orchlib.git(repo, 'worktree', 'prune', check=False)
        result['removed'] = not os.path.exists(tmp)
    return result


def short(files, n=5):
    return ', '.join(files[:n]) + (' 외 %d개' % (len(files) - n) if len(files) > n else '')


def render(report):
    out = []
    infos = report['prs']
    out.append('## 열린 PR (%d개, base %s)\n' % (len(infos), report['base']))
    out.append(orchlib.md_table(['PR', '브랜치', 'head', '파일', 'mergeable', '비고'], [
        ('#%d' % i['number'], i['branch'], i['head'][:7], len(i['files']), i['mergeable'],
         '' if not i['api_head'] or i['api_head'] == i['head'] else 'API head %s와 다름' % i['api_head'][:7])
        for i in infos]))
    rows = [('#%d' % p['a'], '#%d' % p['b'], short(p['conflicts'])) for p in report['pairs'] if p['conflicts']]
    out.append('\n## 줄 충돌 (merge-tree)\n')
    out.append(orchlib.md_table(['PR', 'PR', '충돌 파일'], rows) if rows else '없음')
    rows = [('#%d' % p['a'], '#%d' % p['b'], len(p['files']), short(p['files'])) for p in report['pairs'] if p['files']]
    out.append('\n## 같은 파일\n')
    out.append(orchlib.md_table(['PR', 'PR', '수', '파일'], rows) if rows else '없음')
    rows = [('#%d' % p['a'], '#%d' % p['b'], f, s) for p in report['pairs'] for f, s in p['sections']]
    out.append('\n## 같은 Markdown 절\n')
    out.append(orchlib.md_table(['PR', 'PR', '파일', '절'], rows) if rows else '없음')
    rows = [('#%d' % r['pr'], r['file'], r['value'], r['base_value'] or '-',
             ', '.join('#%d' % c for c in r['clash']) or '-') for r in report['schema']]
    out.append('\n## 스키마 번호 (%s)\n' % report['schema_regex'])
    out.append(orchlib.md_table(['PR', '파일', '값', 'base', '같은 값을 쓰는 PR'], rows) if rows else 'base와 다른 값 없음')
    rows = [(r['branch'], '#%d (%s)' % (r['under'], r['under_branch']), r['via']) for r in report['stacked']]
    out.append('\n## 쌓인 브랜치\n')
    out.append(orchlib.md_table(['브랜치', '아래에 있는 열린 PR', '근거'], rows) if rows else '없음')
    it = report.get('integrate')
    if it:
        out.append('\n## 통합 시험 (base %s)\n' % it['base'][:7])
        out.append(orchlib.md_table(['순서', 'PR', '결과', '충돌 파일'], [
            (k + 1, '#%d' % s['pr'], '합침' if s['merged'] else '충돌로 건너뜀', short(s['conflicts']))
            for k, s in enumerate(it['steps'])]))
        t = it['test']
        if t:
            out.append('\n시험 `%s`: 종료 코드 %s\n\n```\n%s\n```' % (t['cmd'], t['exit'], '\n'.join(t['tail'][-15:])))
        out.append('\n임시 worktree 삭제: %s' % ('확인' if it['removed'] else '실패 ' + it['worktree']))
    return '\n'.join(out)


def to_json(report):
    def conv(o):
        if isinstance(o, set):
            return sorted(o)
        if isinstance(o, tuple):
            return list(o)
        raise TypeError(type(o))
    return json.dumps(report, ensure_ascii=False, indent=2, default=conv)


def build(cfg, repo, prs=None, schema_regex=None, do_integrate=False, order=None, test_cmd=None, timeout=1800):
    schema_regex = schema_regex or cfg['ORCH_SCHEMA_REGEX']
    schema_re = re.compile(schema_regex)
    pulls = orchlib.open_pulls(cfg)
    if prs:
        unknown = set(prs) - {p['number'] for p in pulls}
        if unknown:
            raise SystemExit('not open PRs: %s' % ', '.join('#%d' % n for n in sorted(unknown)))
        pulls = [p for p in pulls if p['number'] in prs]
    unknown = set(order or ()) - {p['number'] for p in pulls}
    if unknown:
        raise SystemExit('--order has PRs outside the report: %s' % ', '.join('#%d' % n for n in sorted(unknown)))
    base = cfg['ORCH_BASE']
    fetch(cfg, repo, [base] + [p['base']['ref'] for p in pulls], [p['number'] for p in pulls])
    infos = analyze(cfg, repo, pulls, schema_re)
    report = {'repo': cfg.get('ORCH_REPO'), 'base': base, 'base_head': rev(repo, base_ref(base)),
              'schema_regex': schema_regex, 'prs': infos, 'pairs': pairs(repo, infos),
              'schema': schema_report(repo, infos, base, schema_re), 'stacked': stacked(repo, infos, base, cfg['ORCH_REMOTE'])}
    if do_integrate:
        report['integrate'] = integrate(cfg, repo, infos, base, order, test_cmd, timeout)
    return report


def int_list(s):
    return [int(x) for x in s.split(',') if x.strip()]


def main(argv, cfg=None):
    cfg = cfg or orchlib.load_env()
    p = argparse.ArgumentParser(prog='pr_overlap.py')
    p.add_argument('--repo-dir', default=cfg.get('ORCH_REPO_DIR') or '.')
    p.add_argument('--prs', type=int_list)
    p.add_argument('--json', action='store_true')
    p.add_argument('--schema-regex')
    p.add_argument('--integrate', action='store_true')
    p.add_argument('--order', type=int_list)
    p.add_argument('--test-cmd', default=cfg.get('ORCH_INTEGRATE_TEST', 'go test ./...'))
    p.add_argument('--test-timeout', type=int, default=1800)
    a = p.parse_args(argv)
    report = build(cfg, a.repo_dir, a.prs, a.schema_regex, a.integrate, a.order, a.test_cmd, a.test_timeout)
    print(to_json(report) if a.json else render(report))
    return 0


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
