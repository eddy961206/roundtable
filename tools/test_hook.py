"""pre-push 훅 실행 시 자식 프로세스로 git 로컬 환경 변수가 전달되지 않는지 검사.

2026-11-06: .githooks/pre-push가 tools/check.py를 부를 때 GIT_DIR이 상속되어,
go test 내부의 임시 git 저장소 생성 코드가 실제 저장소의 .git/config를 오염시킨 결함 방지.
"""
import os
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent


def find_sh():
    s = shutil.which("sh")
    if s and "system32" not in s.lower():
        return s
    git = shutil.which("git")
    if git:
        git_path = Path(git)
        for c in (
            git_path.parent.parent / "bin" / "sh.exe",
            git_path.parent / "sh.exe",
            git_path.parent.parent / "usr" / "bin" / "sh.exe",
        ):
            if c.exists():
                return str(c)
    return None


SH = find_sh()


@unittest.skipIf(not SH, "sh executable not found")
class PrePushHookEnvTest(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp())
        self.repo = self.tmp / "repo"
        self.repo.mkdir()
        subprocess.run(["git", "init", "-q", str(self.repo)], check=True)
        self.tools = self.repo / "tools"
        self.tools.mkdir()

    def tearDown(self):
        shutil.rmtree(self.tmp, ignore_errors=True)

    def test_pre_push_strips_git_local_env_vars(self):
        # 훅이 실행할 mock tools/check.py: GIT_DIR 등 git 로컬 환경변수가 남아있는지 검사
        checker = (
            "import os, sys\n"
            "leaked = [v for v in ('GIT_DIR', 'GIT_WORK_TREE', 'GIT_INDEX_FILE', 'GIT_OBJECT_DIRECTORY') if v in os.environ]\n"
            "if leaked:\n"
            "    sys.stderr.write(f'FAIL: leaked git env vars: {leaked}\\n')\n"
            "    sys.exit(1)\n"
            "print('PASS: clean env')\n"
            "sys.exit(0)\n"
        )
        (self.tools / "check.py").write_text(checker, encoding="utf-8")

        hook_path = ROOT / ".githooks" / "pre-push"
        self.assertTrue(hook_path.exists(), f"hook not found: {hook_path}")

        # GIT_DIR이 주입된 git hook 실행 환경을 모사
        env = os.environ.copy()
        env["GIT_DIR"] = str(self.repo / ".git")
        env["GIT_WORK_TREE"] = str(self.repo)

        r = subprocess.run(
            [SH, str(hook_path)],
            cwd=str(self.repo),
            env=env,
            capture_output=True,
            text=True,
            encoding="utf-8",
        )
        self.assertEqual(r.returncode, 0, f"hook failed: stdout={r.stdout}, stderr={r.stderr}")
        self.assertIn("PASS: clean env", r.stdout)


if __name__ == "__main__":
    unittest.main()
