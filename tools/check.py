"""One entry point for the checks CONTRIBUTING.md asks for before pushing.

The checks run by what changed against the base branch (and in the working
tree), so a docs-only change does not wait for the Go suite. --all runs every
check. The first failing check stops the run with its exit code.

    python tools/check.py               # changes against origin/main
    python tools/check.py --all
    python tools/check.py --base origin/feat/x

.githooks/pre-push runs this script when a checkout opts in with
`git config core.hooksPath .githooks` (CONTRIBUTING.md).
"""
import argparse
import os
import shutil
import subprocess
import sys
import time

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
sys.path.insert(0, os.path.join(ROOT, "tools", "tla"))
import modelcheck as tla  # noqa: E402


def changed_files(base):
    """Paths changed on this branch since base, plus uncommitted and untracked ones."""
    def lines(*args):
        out = subprocess.run(["git", *args], cwd=ROOT, capture_output=True, text=True, encoding="utf-8")
        if out.returncode != 0:
            sys.exit(f"git {' '.join(args)} 실패: {out.stderr.strip()}")
        return [l for l in out.stdout.splitlines() if l.strip()]
    files = set(lines("diff", "--name-only", f"{base}...HEAD"))
    files |= set(lines("diff", "--name-only", "HEAD"))
    files |= set(lines("ls-files", "--others", "--exclude-standard"))
    return files


def plan(files, everything):
    """The checks for these changes, in the order to run them: cheap ones first."""
    go = everything or any(f.endswith((".go", "go.mod")) or f.startswith("web/") for f in files)
    js = everything or any(f.startswith("web/js/") for f in files)
    perf_js = everything or any(f.startswith(("web/js/", "tools/perf/")) and f.endswith((".js", ".mjs")) for f in files)
    orchestrate = everything or any(f.startswith("tools/orchestrate/") for f in files)
    # Guides and scripts whose `rt` examples TestDocumentedCommandsMatchTools reads.
    guides = any(f in ("README.md", "docs/AGENT_GUIDE.md", "skills/roundtable/SKILL.md") or f.startswith("tools/orchestrate/") for f in files)
    steps = []
    if go:
        steps.append(("gofmt", ["gofmt", "-l", "."], "gofmt"))
        steps.append(("go vet", ["go", "vet", "./..."], None))
    if js:
        steps.append(("modcheck", [sys.executable, "tools/modcheck.py", "web/js"], None))
    if perf_js and shutil.which("node"):
        tests = sorted(os.path.join("tools", "perf", f) for f in os.listdir(os.path.join(ROOT, "tools", "perf")) if f.endswith(".test.mjs"))
        steps.append(("node 화면 시험", ["node", "--test", *tests], None))
    if orchestrate:
        steps.append(("orchestrate 시험", [sys.executable, "-m", "unittest", "discover", "-s", "tools/orchestrate", "-p", "test_*.py"], None))
    if go:
        steps.append(("go test", ["go", "test", "./..."], None))
    elif guides:
        steps.append(("문서 rt 예시", ["go", "test", "./internal/rtmcp", "-run", "Documented"], None))
    # A spec reruns when it, its configs or the code it models changed.
    specs = [s for s in tla.specs() if everything or any(f.startswith("tools/tla/" + s) or f in tla.covers(s) for f in files)]
    if specs:
        steps.append(("모델 검사", [sys.executable, "tools/tla/modelcheck.py", *specs], "tla"))
    return steps


def run(name, cmd, mode):
    started = time.monotonic()
    print(f"== {name}: {' '.join(cmd)}", flush=True)
    out = subprocess.run(cmd, cwd=ROOT, capture_output=(mode == "gofmt"), text=True, encoding="utf-8")
    took = time.monotonic() - started
    if mode == "gofmt" and out.returncode == 0 and out.stdout.strip():
        print(out.stdout, end="")
        print(f"== {name}: 정렬되지 않은 파일이 있음 (gofmt -w로 고침, {took:.1f}s)")
        return 1
    if out.returncode != 0:
        if mode == "gofmt":
            print(out.stdout + out.stderr, end="")
        print(f"== {name}: 실패 (종료 코드 {out.returncode}, {took:.1f}s)")
        return out.returncode
    print(f"== {name}: 통과 ({took:.1f}s)", flush=True)
    return 0


def main():
    # A pipe on Windows defaults to the console code page; the messages are Korean.
    sys.stdout.reconfigure(encoding="utf-8")
    ap = argparse.ArgumentParser(description="CONTRIBUTING.md의 push 전 검사를 바뀐 영역에 맞춰 실행")
    ap.add_argument("--all", action="store_true", help="바뀐 영역과 상관없이 모든 검사")
    ap.add_argument("--base", default="origin/main", help="비교할 기준 브랜치 (기본 origin/main)")
    args = ap.parse_args()
    files = set() if args.all else changed_files(args.base)
    steps = plan(files, args.all)
    if not steps:
        print(f"{args.base} 대비 검사할 변경이 없음")
        return 0
    skipped = []
    for name, cmd, mode in steps:
        if mode == "tla" and not (os.environ.get("ROUNDTABLE_JAVA") and os.environ.get("ROUNDTABLE_TLA2TOOLS")):
            print(f"== {name}: 건너뜀 (ROUNDTABLE_JAVA·ROUNDTABLE_TLA2TOOLS 없음, tools/tla/modelcheck.py 첫머리 참고)")
            skipped.append(name)
            continue
        code = run(name, cmd, mode)
        if code:
            return code
    done = len(steps) - len(skipped)
    print(f"모든 검사 통과 ({done}개)" + (f", 건너뜀: {', '.join(skipped)}" if skipped else ""))
    return 0


if __name__ == "__main__":
    sys.exit(main())
