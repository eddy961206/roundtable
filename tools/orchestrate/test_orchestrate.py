"""Small checks for glint.py, reviewer_check.py arguments and the shell scripts' argument
handling. Shell scripts run against fake tea/rt executables, so nothing touches Gitea or a
roundtable server.

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
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import glint  # noqa: E402
import reviewer_check  # noqa: E402


def fwd(p):
    return str(p).replace('\\', '/')


def find_bash():
    b = os.environ.get('ORCH_BASH') or shutil.which('bash')
    if b and os.name == 'nt' and 'system32' in b.lower():  # WSL launcher, not Git Bash
        git = shutil.which('git')
        b = None
        if git:
            for c in (Path(git).parent.parent / 'bin' / 'bash.exe', Path(git).parent / 'bash.exe'):
                if c.exists():
                    b = str(c)
    return b


BASH = find_bash()

FAKE_TEA = r'''#!/bin/bash
echo "$*" >> "$FAKE_LOG"
for a in "$@"; do case $a in @*) cat "${a#@}" > "$FAKE_LOG.data";; esac; done
case "$*" in
  *issues/*/comments*) echo '{"id": 99}';;
  */status*) echo '{"state": "success", "statuses": [{"context": "ci / linux", "status": "failure"}]}';;
  *pulls/*) echo '{"head": {"sha": "abcdef1234"}}';;
  *) echo '{}';;
esac
'''
FAKE_RT = r'''#!/bin/bash
echo "$*" >> "$FAKE_LOG"
echo '{"ready":[{"room":"r1","next_seq":9,"messages":[{"kind":"user","text":"q"},{"kind":"agent","status":"done","text":"the answer"}]}],"waiting":"r2:7"}'
'''


class GlintTest(unittest.TestCase):
    def reasons(self, text, extra=None):
        return [why for why, _, _ in glint.lint(text, extra)]

    def test_clean_body(self):
        text = ('## 변경\n- `internal/x.go`의 검사를 고쳤습니다.\n\n```go\nx := 1\n```\n'
                '근거: https://gitea.example.com/owner/repo/pulls/3/files\n')
        self.assertEqual(glint.lint(text), [])

    def test_each_rule(self):
        cases = {
            'push·PR은 사용자가 별도로 합니다': '작성자 초안 문구',
            '로컬 초안입니다': '작성자 초안 문구',
            r'파일 D:\work\repo\a.go 참고': '로컬 절대 경로',
            '파일 C:/Users/me/a.go 참고': '로컬 절대 경로',
            '위치 /c/Users/me/x 참고': '로컬 절대 경로',
            '위치 /home/me/x 참고': '로컬 절대 경로',
            '새 head  로 갱신': '빈 코드 조각(셸 따옴표로 값이 사라짐)',
            '브랜치 `` 에 커밋': '빈 코드 조각(셸 따옴표로 값이 사라짐)',
            '🤖 Generated with tool': '대리 작성 표식',
            'Co-Authored-By: x': '대리 작성 표식',
        }
        for text, why in cases.items():
            with self.subTest(text=text):
                self.assertEqual(self.reasons(text), [why])

    def test_line_number_and_extra(self):
        found = glint.lint('a\nb\n_worktrees/vmsnext-x\n', r'_worktrees[\\/]vmsnext-')
        self.assertEqual([(w, line) for w, line, _ in found], [('로컬 절대 경로(ORCH_LINT_EXTRA)', 3)])

    def test_main_exit_codes(self):
        with tempfile.TemporaryDirectory() as d:
            ok, bad, empty = (Path(d) / n for n in ('ok.md', 'bad.md', 'empty.md'))
            ok.write_text('정상 본문\n', encoding='utf-8')
            bad.write_text('Generated with x\n', encoding='utf-8')
            empty.write_text('  \n', encoding='utf-8')
            with contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
                self.assertEqual(glint.main([str(ok)]), 0)
                self.assertEqual(glint.main([str(ok), str(bad)]), 1)
                self.assertEqual(glint.main([str(empty)]), 1)
                self.assertEqual(glint.main([str(Path(d) / 'missing.md')]), 2)
                self.assertEqual(glint.main([]), 2)


class ReviewerArgsTest(unittest.TestCase):
    def test_defaults_from_env(self):
        a = reviewer_check.parse(['2026-10-27T00:00:00Z'], {'ORCH_REVIEWER': 'bob', 'ORCH_REPO': 'o/r'})
        self.assertEqual((a.user, a.repos), ('bob', ['o/r']))
        a = reviewer_check.parse(['t', '--repos', 'o/a,o/b'], {'ORCH_REVIEWER': 'bob', 'ORCH_WATCH_REPOS': 'o/x'})
        self.assertEqual(a.repos, ['o/a', 'o/b'])

    def test_rejects_missing_or_bad(self):
        for argv, env in ((['t'], {'ORCH_REPO': 'o/r'}), (['t'], {'ORCH_REVIEWER': 'bob'}),
                          (['t', '--repos', 'repo-without-owner'], {'ORCH_REVIEWER': 'bob'}), ([], {})):
            with self.subTest(argv=argv, env=env), self.assertRaises(SystemExit) as cm:
                with contextlib.redirect_stderr(io.StringIO()):
                    reviewer_check.parse(argv, env)
            self.assertEqual(cm.exception.code, 2)

    def test_approved_open_needs_head_approval(self):
        a = reviewer_check.parse(['--approved-open'], {'ORCH_REVIEWER': 'bob', 'ORCH_REPO': 'o/r'})
        self.assertTrue(a.approved_open)
        rv = lambda state, sha, at, user='bob': {'user': {'login': user}, 'state': state, 'commit_id': sha,
                                                 'submitted_at': at, 'id': at}
        pulls = [{'number': n, 'title': 't%d' % n, 'head': {'sha': 'h%d' % n}} for n in range(1, 6)]
        reviews = {
            1: [rv('REQUEST_CHANGES', 'old', '1'), rv('APPROVED', 'h1', '2'), rv('REQUEST_REVIEW', '', '3')],
            2: [rv('APPROVED', 'old', '1')],                          # approved an older head
            3: [rv('APPROVED', 'h3', '1'), rv('REQUEST_CHANGES', 'h3', '2')],
            4: [rv('APPROVED', 'h4', '1', user='eve')],               # someone else
            5: [rv('APPROVED', 'h5', '1'), rv('COMMENT', 'h5', '2')],  # a later comment keeps the verdict
        }
        get = lambda path: pulls if path.endswith('state=open&limit=50') else reviews[int(path.split('/')[-2])]
        got = [o[3] for o in reviewer_check.approved_open(['o/r'], 'bob', get)]
        self.assertEqual(got, [5, 1])


@unittest.skipIf(not BASH, 'bash not found (set ORCH_BASH)')
class ShellArgsTest(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp())
        self.log = self.tmp / 'calls.log'
        for name, body in (('tea', FAKE_TEA), ('rt', FAKE_RT)):
            p = self.tmp / name
            p.write_text(body, encoding='utf-8', newline='\n')
            p.chmod(0o755)
        self.env = {k: v for k, v in os.environ.items() if not k.startswith('ORCH_')}
        self.env.update(ORCH_ENV=fwd(self.tmp / 'no.env'), ORCH_STATE=fwd(self.tmp / 'state'),
                        ORCH_TEA=fwd(self.tmp / 'tea'), ORCH_RT_BIN=fwd(self.tmp / 'rt'),
                        ORCH_PYTHON=fwd(sys.executable), FAKE_LOG=fwd(self.log))

    def tearDown(self):
        shutil.rmtree(self.tmp, ignore_errors=True)

    def run_sh(self, script, *args, **env):
        e = dict(self.env, **env)
        return subprocess.run([BASH, fwd(HERE / script), *args], env=e, capture_output=True,
                              text=True, encoding='utf-8')

    def calls(self):
        return self.log.read_text(encoding='utf-8') if self.log.exists() else ''

    def test_usage_errors_exit_2_without_calls(self):
        cases = [
            ('rtwait.sh',), ('rtwait.sh', 'r1'), ('rtwait.sh', 'r1:5', 'r2'), ('rtwait.sh', 'r1:x'),
            ('gpost.sh',), ('gpost.sh', 'comment', '12'), ('gpost.sh', 'comment', 'x1', 'b.md'),
            ('gpost.sh', 'pr', 'wt', 'br', 'title'), ('gpost.sh', 'nope'),
            ('assign_fix.sh', '1', '2', 'room'), ('assign_fix.sh', 'x', '2', 'room', 'br'),
            ('queue_review.sh', 'label', 'wt', 'wr', 'ar'),
            ('ci_wait.sh',), ('ci_wait.sh', '12a'),
            ('reviewer_watch.sh',),
        ]
        for script, *args in cases:
            with self.subTest(args=[script, *args]):
                self.log.unlink(missing_ok=True)
                r = self.run_sh(script, *args, ORCH_REPO='o/r')
                self.assertEqual(r.returncode, 2, r.stdout + r.stderr)
                self.assertEqual(self.calls(), '')

    def test_required_settings(self):
        body = self.tmp / 'b.md'
        body.write_text('본문\n', encoding='utf-8')
        r = self.run_sh('gpost.sh', 'comment', '12', fwd(body))
        self.assertEqual(r.returncode, 2)
        self.assertIn('ORCH_REPO', r.stderr)
        r = self.run_sh('assign_fix.sh', '1', '2', 'room', 'br', ORCH_REPO='o/r')
        self.assertEqual(r.returncode, 2)
        self.assertIn('ORCH_WRITER_RULES', r.stderr)
        r = self.run_sh('queue_review.sh', 'l', 'wt', 'wr', 'ar', 'focus')
        self.assertIn('ORCH_REVIEW_TEMPLATE', r.stderr)
        self.assertEqual(self.calls(), '')

    def test_env_file_supplies_settings(self):
        envf = self.tmp / 'o.env'
        envf.write_text('ORCH_REPO=from/file\n', encoding='utf-8', newline='\n')
        body = self.tmp / 'b.md'
        body.write_text('본문\n', encoding='utf-8')
        r = self.run_sh('gpost.sh', 'comment', '12', fwd(body), ORCH_ENV=fwd(envf))
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn('repos/from/file/issues/12/comments', self.calls())
        r = self.run_sh('gpost.sh', 'comment', '12', fwd(body), ORCH_ENV=fwd(envf), ORCH_REPO='from/env')
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn('repos/from/env/issues/12/comments', self.calls())

    def test_gpost_comment_lint_blocks_post(self):
        body = self.tmp / 'b.md'
        body.write_text('Generated with x\n', encoding='utf-8')
        r = self.run_sh('gpost.sh', 'comment', '12', fwd(body), ORCH_REPO='o/r')
        self.assertEqual(r.returncode, 1)
        self.assertIn('대리 작성 표식', r.stdout)
        self.assertEqual(self.calls(), '')

    def test_gpost_comment_keeps_body_verbatim(self):
        text = '브랜치 `fix/a`의 head `abc1234`와 $HOME, "따옴표"를 그대로 둡니다.\n'
        body = self.tmp / 'b.md'
        body.write_text(text, encoding='utf-8')
        r = self.run_sh('gpost.sh', 'comment', '12', fwd(body), ORCH_REPO='o/r')
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn('comment 99 on #12', r.stdout)
        self.assertIn('api -X POST repos/o/r/issues/12/comments --data @', self.calls())
        self.assertEqual(json.loads((self.tmp / 'calls.log.data').read_text(encoding='utf-8')), {'body': text})

    def test_rtwait_joins_rooms_and_prints_answer(self):
        r = self.run_sh('rtwait.sh', 'r1:5', 'r2:7')
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn('wait --rooms r1:5,r2:7', self.calls())
        self.assertIn('READY r1 done next_seq 9', r.stdout)
        self.assertIn('the answer', r.stdout)
        self.assertIn('WAITING r2:7', r.stdout)

    def test_ci_wait_context_filter(self):
        r = self.run_sh('ci_wait.sh', '12', ORCH_REPO='o/r', ORCH_CI_CONTEXT='linux')
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn('#12 head=abcdef1 linux=failure', r.stdout)
        r = self.run_sh('ci_wait.sh', '12', ORCH_REPO='o/r')
        self.assertIn('ci=success', r.stdout)
        self.assertNotIn('merge', self.calls())


if __name__ == '__main__':
    unittest.main()
