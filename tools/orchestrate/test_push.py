"""Posting after a push must wait for the push: gpost.sh pr and push_and_notify.sh
post nothing when the push fails or the remote/PR head is not the local head.
Real git against throwaway bare repositories; Gitea is a fake tea.

2026-10-27, VMS-next #66: a force push ended with "Authentication failed" but the
"pushed the new head" comment and re-review request went out anyway.
"""
import os
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
from test_orchestrate import BASH, fwd  # noqa: E402

FAKE_TEA = r'''#!/bin/bash
echo "$*" >> "$FAKE_LOG"
for a in "$@"; do case $a in @*) cat "${a#@}" >> "$FAKE_LOG.data";; esac; done
case "$*" in
  *issues/*/comments*) echo '{"id": 99}';;
  *requested_reviewers*) echo '{}';;
  *"repos/o/r/pulls --data"*) echo '{"number": 7}';;
  *pulls/*) echo "{\"head\": {\"sha\": \"${FAKE_PR_HEAD:-unset}\"}}";;
  *) echo '{}';;
esac
'''


def git(cwd, *args):
    r = subprocess.run(['git', '-c', 'user.name=t', '-c', 'user.email=t@example.com', '-c', 'commit.gpgsign=false',
                        '-c', 'init.defaultBranch=main', *args], cwd=cwd, capture_output=True, text=True, encoding='utf-8')
    if r.returncode != 0:
        raise AssertionError('git %s: %s' % (' '.join(args), r.stderr))
    return r.stdout.strip()


@unittest.skipIf(not BASH, 'bash not found (set ORCH_BASH)')
class PushThenPostTest(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp())
        self.origin, self.other, self.wt = self.tmp / 'origin.git', self.tmp / 'other.git', self.tmp / 'wt'
        git(self.tmp, 'init', '-q', '--bare', str(self.origin))
        git(self.tmp, 'init', '-q', '--bare', str(self.other))
        git(self.tmp, 'init', '-q', str(self.wt))
        (self.wt / 'a.txt').write_text('a\n', encoding='utf-8')
        git(self.wt, 'add', '-A'); git(self.wt, 'commit', '-qm', 'base')
        git(self.wt, 'remote', 'add', 'origin', fwd(self.origin))
        git(self.wt, 'push', '-q', 'origin', 'main')
        git(self.wt, 'checkout', '-q', '-b', 'feat/x')
        (self.wt / 'b.txt').write_text('b\n', encoding='utf-8')
        git(self.wt, 'add', '-A'); git(self.wt, 'commit', '-qm', 'work')
        self.head = git(self.wt, 'rev-parse', 'HEAD')
        tea = self.tmp / 'tea'
        tea.write_text(FAKE_TEA, encoding='utf-8', newline='\n')
        self.log = self.tmp / 'calls.log'
        self.body = self.tmp / 'body.md'
        self.body.write_text('새 head `{{HEAD}}`로 반영했습니다. 재리뷰 부탁드립니다.\n', encoding='utf-8')
        self.env = {k: v for k, v in os.environ.items() if not k.startswith('ORCH_')}
        self.env.update(ORCH_ENV=fwd(self.tmp / 'none.env'), ORCH_STATE=fwd(self.tmp / 'state'), ORCH_TEA=fwd(tea),
                        ORCH_PYTHON=fwd(sys.executable), ORCH_REPO='o/r', ORCH_REVIEWER='rev', FAKE_LOG=fwd(self.log),
                        ORCH_PR_HEAD_TRIES='1', ORCH_PR_HEAD_INTERVAL='0', FAKE_PR_HEAD=self.head)

    def tearDown(self):
        shutil.rmtree(self.tmp, ignore_errors=True)

    def sh(self, *args, **env):
        e = dict(self.env, **env)
        return subprocess.run([BASH, fwd(HERE / args[0]), *args[1:]], env=e, capture_output=True, text=True, encoding='utf-8')

    def calls(self):
        return self.log.read_text(encoding='utf-8') if self.log.exists() else ''

    def remote_head(self, repo):
        return subprocess.run(['git', '--git-dir', str(repo), 'rev-parse', '-q', '--verify', 'refs/heads/feat/x'],
                              capture_output=True, text=True).stdout.strip()

    # -- gpost.sh pr --------------------------------------------------------

    def test_gpost_pr_stops_when_push_fails(self):
        git(self.wt, 'remote', 'set-url', 'origin', fwd(self.tmp / 'missing.git'))
        r = self.sh('gpost.sh', 'pr', fwd(self.wt), 'feat/x', '제목', fwd(self.body))
        self.assertEqual(r.returncode, 1, r.stdout + r.stderr)
        self.assertEqual(self.calls(), '')

    def test_gpost_pr_stops_when_remote_head_differs(self):
        git(self.wt, 'config', 'remote.origin.pushurl', fwd(self.other))  # push "succeeds" elsewhere
        r = self.sh('gpost.sh', 'pr', fwd(self.wt), 'feat/x', '제목', fwd(self.body))
        self.assertEqual(r.returncode, 1, r.stdout + r.stderr)
        self.assertIn('remote head', r.stderr)
        self.assertEqual(self.calls(), '', 'no PR may be opened for a head the remote does not have')

    def test_gpost_pr_opens_after_verified_push(self):
        r = self.sh('gpost.sh', 'pr', fwd(self.wt), 'feat/x', '제목', fwd(self.body))
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertEqual(self.remote_head(self.origin), self.head)
        self.assertIn('PR 7', r.stdout)

    # -- push_and_notify.sh -----------------------------------------------

    def notify(self, *extra, **env):
        return self.sh('push_and_notify.sh', fwd(self.wt), 'feat/x', '12', fwd(self.body), *extra, **env)

    def test_notify_posts_nothing_when_push_fails(self):
        git(self.wt, 'remote', 'set-url', 'origin', fwd(self.tmp / 'missing.git'))
        r = self.notify()
        self.assertEqual(r.returncode, 1, r.stdout + r.stderr)
        self.assertIn('push failed', r.stderr)
        self.assertNotIn('comments', self.calls())
        self.assertNotIn('requested_reviewers', self.calls())

    def test_notify_posts_nothing_when_remote_head_differs(self):
        git(self.wt, 'config', 'remote.origin.pushurl', fwd(self.other))
        r = self.notify()
        self.assertEqual(r.returncode, 1, r.stdout + r.stderr)
        self.assertIn('remote head', r.stderr)
        self.assertNotIn('comments', self.calls())

    def test_notify_posts_nothing_when_pr_head_lags(self):
        r = self.notify(FAKE_PR_HEAD='0' * 40)
        self.assertEqual(r.returncode, 1, r.stdout + r.stderr)
        self.assertIn('PR #12 head', r.stderr)
        self.assertNotIn('comments', self.calls())

    def test_notify_force_rewrites_and_posts_verified_head(self):
        git(self.wt, 'push', '-q', 'origin', 'feat/x')
        git(self.wt, 'commit', '-q', '--amend', '-m', 'work v2')  # rebase-like rewrite
        head = git(self.wt, 'rev-parse', 'HEAD')
        r = self.notify(FAKE_PR_HEAD=head)
        self.assertEqual(r.returncode, 1, 'a rewrite needs --force: ' + r.stdout + r.stderr)
        self.assertNotIn('comments', self.calls())
        r = self.notify('--force', FAKE_PR_HEAD=head)
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertEqual(self.remote_head(self.origin), head)
        calls = self.calls()
        self.assertIn('repos/o/r/issues/12/comments', calls)
        self.assertIn('repos/o/r/pulls/12/requested_reviewers', calls)
        self.assertLess(calls.index('repos/o/r/pulls/12\n'), calls.index('issues/12/comments'), 'PR head checked before posting')
        posted = (self.tmp / 'calls.log.data').read_text(encoding='utf-8')
        self.assertIn(head[:7], posted, 'the comment names the verified head')
        self.assertNotIn('{{HEAD}}', posted)
        self.assertIn(head[:7], r.stdout)

    def test_notify_usage_and_lint_before_push(self):
        self.assertEqual(self.sh('push_and_notify.sh', fwd(self.wt), 'feat/x', 'x12', fwd(self.body)).returncode, 2)
        self.assertEqual(self.sh('push_and_notify.sh', fwd(self.wt), 'feat/x').returncode, 2)
        bad = self.tmp / 'bad.md'
        bad.write_text('Generated with x\n', encoding='utf-8')
        r = self.sh('push_and_notify.sh', fwd(self.wt), 'feat/x', '12', fwd(bad))
        self.assertEqual(r.returncode, 1)
        self.assertEqual(self.remote_head(self.origin), '', 'a body that fails lint stops before the push')
        git(self.wt, 'checkout', '-q', 'main')
        r = self.notify()
        self.assertEqual(r.returncode, 2, 'the worktree must have the branch checked out')


if __name__ == '__main__':
    unittest.main()
