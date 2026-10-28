"""Right after a PR lands: which open PRs and local branches now need attention.

usage: after_merge.py <merged_pr#> [--repo-dir DIR] [--json] [--schema-regex RE] [--assign [--dry-run]]

Compares what the merged PR changed (files, Markdown sections, schema values; from
merge-base(merge_commit^1, PR head) to the PR head) with every open PR, and reports per
open PR: shared files/sections/schema files, line conflicts against the new base
(git merge-tree), Gitea mergeable=false, and whether it is stacked on the merged head.
Local branches containing the merged head but not yet in the base are listed too.
Line conflicts are split into "docs" (every conflicted file is a document: .md, .markdown,
.rst, .txt) and "code": a docs-only conflict is cheaper to rebase by hand than through a
writer seat that would re-read a long session (2026-10-28 #79: two rows of ARCHITECTURE.md).
Assumes merge-commit or squash merges (merge_commit^1 is the base before the merge).

--assign sends a rebase notice (ORCH_REBASE_TEMPLATE, default templates/rebase-notice.md,
$-placeholders) to each affected branch's room from ORCH_BRANCH_ROOMS and prints room:seq.
Each send first runs the worktree occupancy check; a busy or unknown worktree is skipped
and makes the exit code 1. --dry-run prints the notices instead of sending.
"""
import argparse
import re
import string
import sys

import orchlib
import pr_overlap as po
import worktree_claims

DOC_EXT = ('.md', '.markdown', '.rst', '.txt')


def conflict_kind(files):
    """'docs' when every conflicted file is a document, 'code' otherwise, None when clean."""
    if not files:
        return None
    return 'docs' if all(f.lower().endswith(DOC_EXT) for f in files) else 'code'


def build(cfg, repo, number, schema_regex=None):
    schema_re = re.compile(schema_regex or cfg['ORCH_SCHEMA_REGEX'])
    orchlib.need(cfg, 'ORCH_REPO')
    mp = orchlib.tea_api(cfg, 'repos/%s/pulls/%d' % (cfg['ORCH_REPO'], number))
    if not mp.get('merged'):
        raise SystemExit('PR #%d is not merged' % number)
    base = mp['base']['ref']
    pulls = orchlib.open_pulls(cfg)
    po.fetch(cfg, repo, [base] + [p['base']['ref'] for p in pulls], [number] + [p['number'] for p in pulls])
    merge_commit = po.rev(repo, mp['merge_commit_sha'])
    if not merge_commit or not po.is_ancestor(repo, merge_commit, po.base_ref(base)):
        raise SystemExit('merge commit %s of #%d is not in fetched %s' % (mp['merge_commit_sha'], number, base))
    before = po.rev(repo, merge_commit + '^1')
    mhead = po.rev(repo, po.pull_ref(number))
    mb = orchlib.git(repo, 'merge-base', before, mhead).stdout.strip()
    merged = po.changes(repo, mb, mhead, schema_re)
    # Built on the PR branch itself, not on the base after the merge (which also
    # contains the PR head when it landed through a merge commit).
    built_on_pr = lambda sha: sha != mhead and po.is_ancestor(repo, mhead, sha) and not po.is_ancestor(repo, merge_commit, sha)
    infos = po.analyze(cfg, repo, pulls, schema_re)

    rows = []
    for i in infos:
        why = []
        if merged['files'] & i['files']:
            why.append('같은 파일: ' + po.short(sorted(merged['files'] & i['files'])))
        for f, s in sorted(merged['sections'] & i['sections']):
            why.append('같은 절: %s / %s' % (f, s))
        for f in sorted(set(merged['schema']) & set(i['schema'])):
            why.append('스키마 %s: 병합 %s, 이 PR %s' % (f, merged['schema'][f], i['schema'][f]))
        bad = po.merge_conflicts(repo, po.base_ref(base), i['head'])
        kind = conflict_kind(bad)
        if bad:
            why.append('새 %s와 줄 충돌: %s' % (base, po.short(bad)))
        if i['mergeable'] is False:
            why.append('Gitea mergeable=false')
        if built_on_pr(i['head']):
            why.append('병합된 PR #%d 위에 쌓임' % number)
        if why:
            rows.append({'pr': i['number'], 'branch': i['branch'], 'reasons': why, 'conflict': kind, 'conflict_files': bad})
    pr_heads = {(i['branch'], i['head']) for i in infos}
    refs = orchlib.git(repo, 'for-each-ref', '--contains', mhead, '--format=%(refname:short) %(objectname)', 'refs/heads').stdout
    for line in filter(None, refs.split('\n')):
        name, sha = line.split(' ', 1)
        if (name, sha) in pr_heads or not built_on_pr(sha):
            continue
        rows.append({'pr': None, 'branch': name, 'reasons': ['병합된 PR #%d 위에 쌓인 로컬 브랜치' % number], 'conflict': None, 'conflict_files': []})
    return {'merged': {'number': number, 'title': mp.get('title', ''), 'branch': mp['head']['ref'], 'head': mhead,
                       'merge_commit': merge_commit, 'files': merged['files'], 'sections': merged['sections'],
                       'schema': merged['schema']},
            'base': base, 'base_head': po.rev(repo, po.base_ref(base)), 'affected': rows}


def render(report):
    m = report['merged']
    out = ['## PR #%d 병합 영향 (%s → %s %s)\n' % (m['number'], m['branch'], report['base'], report['base_head'][:7])]
    if not report['affected']:
        out.append('영향받는 열린 PR·로컬 브랜치 없음')
        return '\n'.join(out)
    label = {'docs': '문서 줄만', 'code': '코드 포함', None: '-'}
    out.append(orchlib.md_table(['대상', '브랜치', '줄 충돌', '이유'], [
        ('#%d' % r['pr'] if r['pr'] else '로컬', r['branch'], label[r['conflict']], '; '.join(r['reasons']))
        for r in report['affected']]))
    for kind, title in (('docs', '문서 줄 충돌만(작성 좌석 대신 직접 재기반하면 싸다)'), ('code', '코드가 섞인 줄 충돌(작성 좌석에 재기반 배정)')):
        hit = [r for r in report['affected'] if r['conflict'] == kind]
        if hit:
            out.append('\n%s: %s' % (title, ', '.join(('#%d' % r['pr'] if r['pr'] else r['branch']) + ' (' + po.short(r['conflict_files'], 3) + ')' for r in hit)))
    return '\n'.join(out)


def assign(cfg, repo, report, dry_run=False):
    """Sends rebase notices; returns the number of affected mapped branches left unsent."""
    table = orchlib.branch_rooms(cfg)
    tpl_path = cfg.get('ORCH_REBASE_TEMPLATE') or str(orchlib.ORCH_DIR / 'templates' / 'rebase-notice.md')
    with open(orchlib.native_path(tpl_path), encoding='utf-8') as f:
        tpl = string.Template(f.read())
    m, skipped = report['merged'], 0
    for r in report['affected']:
        e = table.get(r['branch'])
        if not e:
            print('no room for %s (ORCH_BRANCH_ROOMS)' % r['branch'])
            continue
        wt = e['worktree'] or orchlib.branch_worktree(repo, r['branch'])
        if not wt:
            print('SKIP %s: worktree unknown (not checked out in %s, none in ORCH_BRANCH_ROOMS)' % (r['branch'], repo))
            skipped += 1
            continue
        busy = worktree_claims.occupants(cfg, wt, e['room'])
        if busy:
            worktree_claims.report_busy(wt, busy)
            print('SKIP %s: not sent to room %s' % (r['branch'], e['room']))
            skipped += 1
            continue
        text = tpl.safe_substitute(
            merged_pr='#%d' % m['number'], merged_title=m['title'], merged_head=m['head'][:7],
            base=report['base'], base_head=report['base_head'][:7], branch=r['branch'], worktree=wt,
            pr='PR #%d' % r['pr'] if r['pr'] else '로컬 브랜치', reasons='\n'.join('- ' + x for x in r['reasons']))
        if dry_run:
            print('--- %s → room %s\n%s' % (r['branch'], e['room'], text))
            continue
        print('%s:%d' % (e['room'], orchlib.post_room(cfg, e['room'], text)))
    return skipped


def main(argv, cfg=None):
    cfg = cfg or orchlib.load_env()
    p = argparse.ArgumentParser(prog='after_merge.py')
    p.add_argument('merged_pr', type=int)
    p.add_argument('--repo-dir', default=cfg.get('ORCH_REPO_DIR') or '.')
    p.add_argument('--json', action='store_true')
    p.add_argument('--schema-regex')
    p.add_argument('--assign', action='store_true')
    p.add_argument('--dry-run', action='store_true')
    a = p.parse_args(argv)
    report = build(cfg, a.repo_dir, a.merged_pr, a.schema_regex)
    print(po.to_json(report) if a.json else render(report))
    if a.assign:
        return 1 if assign(cfg, a.repo_dir, report, a.dry_run) else 0
    return 0


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
