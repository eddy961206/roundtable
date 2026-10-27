"""Counterexample tests for pr_overlap.py, after_merge.py and worktree_claims.py on a
throwaway git fixture: a bare origin with refs/pull/N/head, a local clone the tools read,
Gitea faked through orchlib.tea_api and roundtable through a local HTTP server.

run: python -m unittest discover -s tools/orchestrate -p "test_*.py"
"""
import contextlib
import io
import json
import os
import shutil
import subprocess
import sys
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import after_merge  # noqa: E402
import orchlib  # noqa: E402
import pr_overlap as po  # noqa: E402
import worktree_claims  # noqa: E402
from test_orchestrate import BASH, FAKE_RT, FAKE_TEA, fwd  # noqa: E402

DOC = '# Guide\n\n## A\na1\na2\na3\na4\na5\n\n## B\nb1\nb2\n\n```\n## not a heading\n```\nb3\n'


def g(cwd, *args):
    r = subprocess.run(['git', '-c', 'user.name=t', '-c', 'user.email=t@example.com', '-c', 'commit.gpgsign=false',
                        '-c', 'init.defaultBranch=main', *args], cwd=cwd, capture_output=True, text=True, encoding='utf-8')
    if r.returncode != 0:
        raise AssertionError('git %s: %s' % (' '.join(args), r.stderr))
    return r.stdout.strip()


def write(root, path, text):
    p = Path(root, path)
    p.parent.mkdir(parents=True, exist_ok=True)
    p.write_text(text, encoding='utf-8', newline='\n')


class FakeRT:
    """Minimal roundtable API: room list, room detail (workspace), message post."""

    def __init__(self):
        self.rooms, self.posts = {}, []
        state = self

        class H(BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def reply(self, obj):
                body = json.dumps(obj).encode()
                self.send_response(200)
                self.send_header('Content-Type', 'application/json')
                self.send_header('Content-Length', str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def do_GET(self):
                path = self.path.split('?')[0]
                if path == '/api/rooms':
                    return self.reply([{'id': k, 'title': 't-' + k, 'active': v['active']} for k, v in state.rooms.items()])
                rid = path.rsplit('/', 1)[-1]
                self.reply({'room': {'id': rid, 'workspace': state.rooms[rid]['workspace']}})

            def do_POST(self):
                n = int(self.headers['Content-Length'])
                rid = self.path.split('/')[3]
                state.posts.append((rid, json.loads(self.rfile.read(n))['text']))
                self.reply({'id': 'm%d' % len(state.posts), 'seq': 40 + len(state.posts)})

        self.server = ThreadingHTTPServer(('127.0.0.1', 0), H)
        self.url = 'http://127.0.0.1:%d' % self.server.server_address[1]
        threading.Thread(target=self.server.serve_forever, daemon=True).start()

    def room(self, rid, workspace, active):
        self.rooms[rid] = {'workspace': str(workspace), 'active': active}

    def close(self):
        self.server.shutdown()
        self.server.server_close()


class Fixture(unittest.TestCase):
    """main + PR #1..#4 and local branches:
    #1 feat/one   x.txt line2→one, doc A a1, schema 6
    #2 feat/two   x.txt line2→two (line conflict with #1), doc B b1, schema 6 (clash)
    #3 feat/three doc A a5 (same section as #1, no line conflict), schema.go comment only
    #4 feat/four  on top of #1, adds z.txt (stacked)
    local feat/local-stack on top of #2; local feat/one == #1 head."""

    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp())
        self.origin, work, self.repo = self.tmp / 'origin.git', self.tmp / 'work', self.tmp / 'local'
        g(self.tmp, 'init', '-q', '--bare', str(self.origin))
        g(self.tmp, 'init', '-q', str(work))
        write(work, 'store/schema.go', 'package store\n\n// a\n// b\n// c\n\nconst schemaVersion = 5\n')
        write(work, 'docs/guide.md', DOC)
        write(work, 'src/x.txt', 'line1\nline2\nline3\n')
        g(work, 'add', '-A'); g(work, 'commit', '-qm', 'base')
        heads = {}

        def branch(name, start, edits, msg):
            g(work, 'checkout', '-q', '-b', name, start)
            for path, old, new in edits:
                text = Path(work, path).read_text(encoding='utf-8') if Path(work, path).exists() else ''
                write(work, path, text.replace(old, new) if old else new)
            g(work, 'commit', '-qam', msg) if not any(o == '' for _, o, _ in edits) else (g(work, 'add', '-A'), g(work, 'commit', '-qm', msg))
            heads[name] = g(work, 'rev-parse', 'HEAD')

        schema6 = ('store/schema.go', 'Version = 5', 'Version = 6')
        branch('feat/one', 'main', [('src/x.txt', 'line2', 'one'), ('docs/guide.md', 'a1', 'a1 one'), schema6], 'one')
        branch('feat/two', 'main', [('src/x.txt', 'line2', 'two'), ('docs/guide.md', 'b1', 'b1 two'), schema6], 'two')
        branch('feat/three', 'main', [('docs/guide.md', 'a5', 'a5 three'), ('store/schema.go', '// a', '// a three')], 'three')
        branch('feat/four', 'feat/one', [('src/z.txt', '', 'z\n')], 'four')
        branch('feat/local-stack', 'feat/two', [('src/w.txt', '', 'w\n')], 'local')
        g(work, 'remote', 'add', 'origin', str(self.origin))
        g(work, 'push', '-q', 'origin', 'main', 'feat/one', 'feat/two', 'feat/three', 'feat/four')
        self.nums = {'feat/one': 1, 'feat/two': 2, 'feat/three': 3, 'feat/four': 4}
        for b, n in self.nums.items():
            g(self.origin, 'update-ref', 'refs/pull/%d/head' % n, heads[b])
        g(self.tmp, 'clone', '-q', str(self.origin), str(self.repo))
        g(self.repo, 'branch', 'feat/one', 'origin/feat/one')
        g(self.repo, 'fetch', '-q', str(work), 'feat/local-stack:feat/local-stack')
        self.work, self.heads = work, heads
        self.rt = FakeRT()
        self.cfg = orchlib.load_env({'ORCH_ENV': str(self.tmp / 'none.env'), 'HOME': str(self.tmp), 'ORCH_REPO': 'o/r',
                                     'ORCH_RT_URL': self.rt.url, 'ORCH_STATE': str(self.tmp / 'state'),
                                     'ORCH_BRANCH_ROOMS': str(self.tmp / 'branch_rooms.json')})
        self.pulls = {n: {'number': n, 'title': b, 'head': {'ref': b, 'sha': heads[b]}, 'base': {'ref': 'main'},
                          'mergeable': n != 2} for b, n in self.nums.items()}
        self.single = {}
        self._tea = orchlib.tea_api
        orchlib.tea_api = self.fake_tea

    def fake_tea(self, cfg, path):
        if 'pulls?state=open' in path:
            return [] if 'page=2' in path else list(self.pulls.values())
        return self.single[int(path.rsplit('/', 1)[-1])]

    def tearDown(self):
        orchlib.tea_api = self._tea
        self.rt.close()
        shutil.rmtree(self.tmp, ignore_errors=True)

    def refs(self, repo):
        return g(repo, 'for-each-ref', '--format=%(refname) %(objectname)', 'refs/heads', 'refs/remotes')


class PrOverlapTest(Fixture):
    def report(self, **kw):
        return po.build(self.cfg, str(self.repo), **kw)

    def pair(self, rep, a, b):
        return next(p for p in rep['pairs'] if (p['a'], p['b']) == (a, b))

    def test_line_conflict_pairs(self):
        rep = self.report()
        self.assertEqual(self.pair(rep, 1, 2)['conflicts'], ['src/x.txt'])
        self.assertEqual(self.pair(rep, 2, 4)['conflicts'], ['src/x.txt'])
        self.assertEqual(self.pair(rep, 1, 3)['conflicts'], [])
        self.assertEqual(self.pair(rep, 1, 4)['conflicts'], [])
        self.assertEqual(self.pair(rep, 1, 3)['files'], ['docs/guide.md', 'store/schema.go'])

    def test_same_section_without_line_conflict(self):
        p = self.pair(self.report(), 1, 3)
        self.assertEqual(p['conflicts'], [])  # git alone sees nothing here
        self.assertEqual(p['sections'], [('docs/guide.md', '# Guide > ## A')])
        self.assertEqual(self.pair(self.report(), 2, 3)['sections'], [])  # B vs A

    def test_headings_skip_code_fences(self):
        self.assertEqual([k for _, k in po.headings(DOC)], ['# Guide', '# Guide > ## A', '# Guide > ## B'])
        self.assertEqual(po.section_at(po.headings(DOC), 1), '# Guide')
        self.assertEqual(po.section_at(po.headings('x\n# T\n'), 1), po.PREAMBLE)

    def test_schema_clash(self):
        rows = {r['pr']: r for r in self.report()['schema']}
        self.assertEqual(sorted(rows), [1, 2, 4])
        self.assertEqual((rows[1]['value'], rows[1]['base_value']), ('6', '5'))
        self.assertEqual(rows[1]['clash'], [2, 4])
        self.assertNotIn(3, rows)

    def test_stacked_branches(self):
        rows = {(r['branch'], r['under']) for r in self.report()['stacked']}
        self.assertEqual(rows, {('PR #4 (feat/four)', 1), ('local feat/local-stack', 2)})

    def republish_one(self, new):
        g(self.work, 'push', '-q', '-f', 'origin', 'feat/one')
        g(self.origin, 'update-ref', 'refs/pull/1/head', new)
        self.pulls[1]['head']['sha'] = new

    def test_stack_on_pre_rebase_head(self):
        old = self.heads['feat/one']
        g(self.repo, 'branch', 'feat/old-stack', old)  # built on #1 before it was rewritten
        g(self.work, 'checkout', '-q', 'feat/one')
        g(self.work, 'commit', '-q', '--amend', '-m', 'one v2')
        self.republish_one(g(self.work, 'rev-parse', 'HEAD'))
        g(self.repo, 'fetch', '-q', 'origin')  # origin/feat/one's reflog keeps the old tip
        rows = {(r['branch'], r['under'], r['via']) for r in self.report()['stacked']}
        via = '재기반 전 head ' + old[:7]
        self.assertIn(('local feat/old-stack', 1, via), rows)
        self.assertIn(('PR #4 (feat/four)', 1, via), rows)

    def test_stack_on_mid_commit(self):
        first = self.heads['feat/one']
        g(self.work, 'checkout', '-q', 'feat/one')
        write(self.work, 'src/more.txt', 'more\n')
        g(self.work, 'add', '-A'); g(self.work, 'commit', '-qm', 'one more')
        self.republish_one(g(self.work, 'rev-parse', 'HEAD'))
        g(self.repo, 'branch', 'feat/mid', first)
        rows = {(r['branch'], r['under'], r['via']) for r in self.report()['stacked']}
        self.assertIn(('local feat/mid', 1, '중간 커밋 ' + first[:7]), rows)
        self.assertIn(('PR #4 (feat/four)', 1, '중간 커밋 ' + first[:7]), rows)

    def test_open_pr_already_in_base_is_not_a_stack(self):
        g(self.work, 'push', '-q', 'origin', 'feat/one:main')  # landed by fast-forward, PR still open
        rows = {(r['branch'], r['under']) for r in self.report()['stacked']}
        self.assertEqual(rows, {('local feat/local-stack', 2)})

    def test_render_and_json(self):
        rep = self.report()
        text = po.render(rep)
        for part in ('## 줄 충돌', '| #1 | #2 | src/x.txt |', '# Guide > ## A', '| #1 | store/schema.go | 6 | 5 | #2, #4 |',
                     '| PR #4 (feat/four) | #1 (feat/one) |'):
            self.assertIn(part, text)
        self.assertEqual(json.loads(po.to_json(rep))['pairs'][0]['a'], 1)

    def test_no_branch_or_remote_moves(self):
        before, origin_before = self.refs(self.repo), g(self.origin, 'for-each-ref')
        self.report(do_integrate=True, test_cmd='')
        self.assertEqual(self.refs(self.repo), before)
        self.assertEqual(g(self.origin, 'for-each-ref'), origin_before)

    def test_integrate_reports_and_removes_worktree(self):
        cmd = '"%s" -c "import os,sys; sys.exit(0 if os.path.exists(\'src/z.txt\') else 3)"' % sys.executable
        it = self.report(do_integrate=True, order=[1, 2, 3, 4], test_cmd=cmd)['integrate']
        self.assertEqual([(s['pr'], s['merged']) for s in it['steps']], [(1, True), (2, False), (3, True), (4, True)])
        self.assertEqual(it['steps'][1]['conflicts'], ['src/x.txt'])
        self.assertEqual(it['test']['exit'], 0)
        self.assertTrue(it['removed'])
        self.assertFalse(os.path.exists(it['worktree']))
        self.assertNotIn('orch-integrate', g(self.repo, 'worktree', 'list'))

    def test_integrate_cleans_up_after_failure(self):
        real = po.subprocess.run

        def boom(*a, **kw):
            if kw.get('shell'):
                raise RuntimeError('test command exploded')
            return real(*a, **kw)
        po.subprocess.run = boom
        try:
            with self.assertRaises(RuntimeError):
                self.report(do_integrate=True, test_cmd='anything')
        finally:
            po.subprocess.run = real
        self.assertNotIn('orch-integrate', g(self.repo, 'worktree', 'list'))

    def test_unknown_prs_rejected(self):
        with self.assertRaises(SystemExit):
            self.report(prs=[9])
        with self.assertRaises(SystemExit):
            self.report(do_integrate=True, order=[1, 9])


class AfterMergeTest(Fixture):
    def merge_one(self):
        g(self.work, 'checkout', '-q', 'main')
        g(self.work, 'merge', '-q', '--no-ff', '--no-edit', 'feat/one')
        g(self.work, 'push', '-q', 'origin', 'main')
        m = g(self.work, 'rev-parse', 'HEAD')
        self.single[1] = dict(self.pulls.pop(1), merged=True, merge_commit_sha=m)
        return m

    def test_affected_open_prs(self):
        self.merge_one()
        rep = after_merge.build(self.cfg, str(self.repo), 1)
        why = {r['pr'] or r['branch']: ' / '.join(r['reasons']) for r in rep['affected']}
        self.assertEqual(sorted(why, key=str), [2, 3, 4])
        self.assertIn('같은 파일: ', why[2])
        self.assertIn('스키마 store/schema.go: 병합 6, 이 PR 6', why[2])
        self.assertIn('새 main와 줄 충돌: src/x.txt', why[2])
        self.assertIn('Gitea mergeable=false', why[2])
        self.assertIn('같은 절: docs/guide.md / # Guide > ## A', why[3])
        self.assertNotIn('줄 충돌', why[3])
        self.assertIn('병합된 PR #1 위에 쌓임', why[4])

    def test_rebased_after_merge_is_not_stacked(self):
        self.merge_one()
        g(self.repo, 'fetch', '-q', 'origin')
        g(self.repo, 'branch', 'feat/after', 'origin/main')  # started from main after the merge
        g(self.work, 'checkout', '-q', 'feat/three')
        g(self.work, 'merge', '-q', '--no-edit', 'main')  # #3 brought up to date with main
        g(self.work, 'push', '-q', 'origin', 'feat/three')
        g(self.origin, 'update-ref', 'refs/pull/3/head', g(self.work, 'rev-parse', 'HEAD'))
        rep = after_merge.build(self.cfg, str(self.repo), 1)
        why = {r['pr'] or r['branch']: ' / '.join(r['reasons']) for r in rep['affected']}
        self.assertNotIn('feat/after', why)
        self.assertNotIn('쌓임', why.get(3, ''))
        self.assertIn('병합된 PR #1 위에 쌓임', why[4])

    def test_not_merged_rejected(self):
        self.single[2] = dict(self.pulls[2], merged=False)
        with self.assertRaises(SystemExit):
            after_merge.build(self.cfg, str(self.repo), 2)

    def assign(self, table, dry=False):
        Path(self.cfg['ORCH_BRANCH_ROOMS']).write_text(json.dumps(table), encoding='utf-8')
        rep = after_merge.build(self.cfg, str(self.repo), 1)
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            skipped = after_merge.assign(self.cfg, str(self.repo), rep, dry)
        return skipped, out.getvalue()

    def test_assign_posts_notice(self):
        self.merge_one()
        wt2 = self.tmp / 'wt-two'
        self.rt.room('r2', wt2, False)
        skipped, out = self.assign({'feat/two': {'room': 'r2', 'worktree': str(wt2)}})
        self.assertEqual(skipped, 0)
        self.assertIn('r2:41', out)
        room, text = self.rt.posts[0]
        self.assertEqual(room, 'r2')
        self.assertIn('PR #1(feat/one)', text)
        self.assertIn('- 새 main와 줄 충돌: src/x.txt', text)
        self.assertNotIn('${', text)

    def test_assign_refuses_busy_or_unknown_worktree(self):
        self.merge_one()
        wt2 = self.tmp / 'wt-two'
        self.rt.room('r2', wt2, False)
        self.rt.room('other', str(wt2) + os.sep, True)  # another running room works there
        skipped, out = self.assign({'feat/two': {'room': 'r2', 'worktree': str(wt2)}, 'feat/three': 'r3'})
        self.assertEqual(skipped, 2)
        self.assertIn('room other (t-other) is running there [workspace]', out)
        self.assertIn('SKIP feat/three: worktree unknown', out)
        self.assertEqual(self.rt.posts, [])


class ClaimsTest(Fixture):
    def run_main(self, *argv):
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            code = worktree_claims.main(list(argv), self.cfg)
        return code, out.getvalue()

    def test_workspace_match_refuses_and_names_room(self):
        wt = self.tmp / 'wt'
        self.rt.room('writer', wt, True)
        code, out = self.run_main('check', '--worktree', str(wt), '--room', 'reviewer')
        self.assertEqual(code, 1)
        self.assertIn('room writer (t-writer) is running there [workspace]', out)
        self.assertEqual(self.run_main('check', '--worktree', str(wt), '--room', 'writer')[0], 0)

    def test_idle_room_does_not_block(self):
        wt = self.tmp / 'wt'
        self.rt.room('writer', wt, False)
        self.assertEqual(self.run_main('check', '--worktree', str(wt))[0], 0)

    def test_claim_for_parent_workspace(self):
        wt = self.tmp / 'parent' / 'wt'
        self.rt.room('writer', self.tmp / 'parent', True)
        self.assertEqual(self.run_main('check', '--worktree', str(wt), '--room', 'reviewer')[0], 0)  # path alone says nothing
        self.assertEqual(self.run_main('claim', str(wt), '--room', 'writer', '--note', 'fix #64')[0], 0)
        code, out = self.run_main('check', '--worktree', str(wt), '--room', 'reviewer')
        self.assertEqual(code, 1)
        self.assertIn('[claim]', out)
        code, out = self.run_main('claim', str(wt), '--room', 'reviewer')
        self.assertEqual(code, 1)
        self.assertIn('claimed by room writer', out)
        self.assertEqual(self.run_main('release', str(wt), '--room', 'reviewer')[0], 1)
        self.assertEqual(self.run_main('release', str(wt), '--room', 'writer')[0], 0)
        self.assertEqual(self.run_main('check', '--worktree', str(wt), '--room', 'reviewer')[0], 0)

    def test_claim_refused_while_other_room_runs_there(self):
        wt = self.tmp / 'wt'
        self.rt.room('writer', wt, True)
        code, out = self.run_main('claim', str(wt), '--room', 'reviewer')
        self.assertEqual(code, 1)
        self.assertIn('room writer', out)

    def test_branch_rooms_mapping(self):
        wt = self.tmp / 'wt'
        self.rt.room('writer', self.tmp, True)
        Path(self.cfg['ORCH_BRANCH_ROOMS']).write_text(json.dumps({'feat/x': {'room': 'writer', 'worktree': str(wt)}}), encoding='utf-8')
        code, out = self.run_main('check', '--worktree', str(wt), '--room', 'reviewer')
        self.assertEqual(code, 1)
        self.assertIn('branch_rooms feat/x', out)

    def test_branch_resolves_through_worktree_list(self):
        wt = self.tmp / 'wt-three'
        g(self.repo, 'worktree', 'add', '-q', str(wt), '-b', 'feat/three', 'origin/feat/three')
        self.rt.room('writer', wt, True)
        cfg = dict(self.cfg, ORCH_REPO_DIR=str(self.repo))
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            self.assertEqual(worktree_claims.main(['check', '--branch', 'feat/three', '--room', 'rv'], cfg), 1)
            with self.assertRaises(SystemExit):
                worktree_claims.main(['check', '--branch', 'feat/missing'], cfg)

    def test_path_key(self):
        self.assertEqual(orchlib.path_key('/tmp/a/b/'), orchlib.path_key('/tmp/a/b'))
        if os.name == 'nt':
            self.assertEqual(orchlib.path_key('/d/Work/X'), orchlib.path_key('D:\\work\\x\\'))


@unittest.skipIf(not BASH, 'bash not found (set ORCH_BASH)')
class ShellClaimsTest(Fixture):
    def sh(self, script, *args, **env):
        for name, body in (('tea', FAKE_TEA), ('rt', FAKE_RT)):
            p = self.tmp / name
            p.write_text(body, encoding='utf-8', newline='\n')
        e = {k: v for k, v in os.environ.items() if not k.startswith('ORCH_')}
        e.update(ORCH_ENV=fwd(self.tmp / 'none.env'), ORCH_STATE=fwd(self.tmp / 'state'), ORCH_RT_URL=self.rt.url,
                 ORCH_TEA=fwd(self.tmp / 'tea'), ORCH_RT_BIN=fwd(self.tmp / 'rt'), ORCH_PYTHON=fwd(sys.executable),
                 ORCH_BRANCH_ROOMS=fwd(self.tmp / 'branch_rooms.json'), FAKE_LOG=fwd(self.tmp / 'calls.log'), **env)
        r = subprocess.run([BASH, fwd(HERE / script), *args], env=e, capture_output=True, text=True, encoding='utf-8')
        calls = (self.tmp / 'calls.log').read_text(encoding='utf-8') if (self.tmp / 'calls.log').exists() else ''
        return r, calls

    def test_queue_review_refuses_before_rebase(self):
        wt = self.tmp / 'wt-three'
        g(self.repo, 'worktree', 'add', '-q', str(wt), '-b', 'feat/three', 'origin/feat/three')
        tpl = self.tmp / 'review.md'
        tpl.write_text('review\n', encoding='utf-8')
        self.rt.room('writer', wt, True)
        head = g(wt, 'rev-parse', 'HEAD')
        r, calls = self.sh('queue_review.sh', 'L', fwd(wt), 'writer', 'reviewer', 'focus', ORCH_REVIEW_TEMPLATE=fwd(tpl))
        self.assertEqual(r.returncode, 1, r.stdout + r.stderr)
        self.assertIn('room writer', r.stdout)
        self.assertEqual(calls, '')
        self.assertEqual(g(wt, 'rev-parse', 'HEAD'), head)
        self.rt.rooms['writer']['active'] = False
        r, calls = self.sh('queue_review.sh', 'L', fwd(wt), 'writer', 'reviewer', 'focus', ORCH_REVIEW_TEMPLATE=fwd(tpl))
        self.assertIn('read --room writer', calls)  # check passed, the script went on

    def test_assign_fix_refuses_busy_room_workspace(self):
        wt = self.tmp / 'wt'
        wt.mkdir()
        rules = self.tmp / 'rules.md'
        rules.write_text('rules\n', encoding='utf-8')
        self.rt.room('writer', wt, False)
        self.rt.room('other', wt, True)
        r, calls = self.sh('assign_fix.sh', '64', '7', 'writer', 'feat/x', ORCH_REPO='o/r', ORCH_WRITER_RULES=fwd(rules))
        self.assertEqual(r.returncode, 1, r.stdout + r.stderr)
        self.assertIn('room other (t-other) is running there', r.stdout)
        self.assertEqual(calls, '')


if __name__ == '__main__':
    unittest.main()
