"""Model-check every TLA+ spec in tools/tla with TLC.

Each <Spec>_<case>.cfg runs <Spec>.tla and names the TLC result it must get:

    \\* expect: pass
    \\* expect: Invariant SleepsOnlyWhenUnfinished is violated.

A config for the current code expects pass. A config for an earlier version or
a deliberately broken variant expects the violation that version shipped, so
every property is seen failing at least once. Each spec lists the code it
models on a `\\* covers:` line; tools/check.py runs the specs whose covered
files changed. `\\* reach:` lines name states a passing config must reach
(for example `turn = "done"`), so a pass is never a model that cannot move.

    set ROUNDTABLE_JAVA=D:\\Tools\\jdk-21.0.12.1+1\\bin\\java.exe
    set ROUNDTABLE_TLA2TOOLS=D:\\Tools\\tla\\tla2tools.jar
    python tools/tla/modelcheck.py [Spec ...]

Java 11 or newer and tla2tools.jar (https://github.com/tlaplus/tlaplus/releases,
checked with v1.7.4) are required; the script stops when either is missing.
"""
import glob
import os
import re
import subprocess
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
PASS = "Model checking completed. No error has been found."


def header(path, key):
    """The values of `\\* <key>: ...` lines at the top of a spec or config."""
    values = []
    for line in open(path, encoding="utf-8"):
        m = re.match(r"\\\*\s*" + key + r":\s*(.+)$", line.strip())
        if m:
            values.append(m.group(1).strip())
    return values


def covers(spec):
    """Repo-relative paths a spec models."""
    return [p for line in header(os.path.join(HERE, spec + ".tla"), "covers") for p in line.split()]


def specs():
    return sorted(os.path.splitext(os.path.basename(p))[0] for p in glob.glob(os.path.join(HERE, "*.tla")))


def cases(spec):
    out = []
    for cfg in sorted(glob.glob(os.path.join(HERE, spec + "_*.cfg"))):
        expect = header(cfg, "expect")
        if len(expect) != 1:
            sys.exit(f"{os.path.basename(cfg)}: '\\* expect:' 줄이 정확히 하나여야 함")
        out.append((os.path.basename(cfg), PASS if expect[0] == "pass" else expect[0]))
    if not out:
        sys.exit(f"{spec}.tla: 설정 파일({spec}_*.cfg)이 없음")
    return out


def tlc(java, jar, cwd, cfg, spec):
    with tempfile.TemporaryDirectory() as meta:
        out = subprocess.run(
            [java, "-XX:+UseParallelGC", "-cp", jar, "tlc2.TLC", "-workers", "auto",
             "-metadir", meta, "-config", cfg, spec + ".tla"],
            cwd=cwd, capture_output=True, text=True, encoding="utf-8")
    return out.stdout + out.stderr


def unreached(java, jar, spec, cfg):
    """The reach: states a passing config never reaches. Each runs as an
    invariant that must fail, on a copy of the spec."""
    lines = open(os.path.join(HERE, spec + ".tla"), encoding="utf-8").read().rstrip().split("\n")
    end = max(i for i, l in enumerate(lines) if l.startswith("===="))
    config = open(os.path.join(HERE, cfg), encoding="utf-8").read()
    if "\nINVARIANTS " not in config:
        sys.exit(f"{cfg}: reach 검사는 한 줄짜리 'INVARIANTS ...' 줄에 불변식을 더함; 그 줄이 없음")
    missed = []
    for i, goal in enumerate(header(os.path.join(HERE, spec + ".tla"), "reach")):
        name = f"Unreached{i}"
        with tempfile.TemporaryDirectory() as tmp:
            open(os.path.join(tmp, spec + ".tla"), "w", encoding="utf-8").write("\n".join(lines[:end] + [f"{name} == ~({goal})", lines[end]]) + "\n")
            open(os.path.join(tmp, cfg), "w", encoding="utf-8").write(config.replace("INVARIANTS ", f"INVARIANTS {name} ", 1))
            if f"Invariant {name} is violated." not in tlc(java, jar, tmp, cfg, spec):
                missed.append(goal)
    return missed


def main():
    sys.stdout.reconfigure(encoding="utf-8")
    java, jar = os.environ.get("ROUNDTABLE_JAVA"), os.environ.get("ROUNDTABLE_TLA2TOOLS")
    if not java or not jar:
        sys.exit("ROUNDTABLE_JAVA와 ROUNDTABLE_TLA2TOOLS를 지정해야 함 (이 파일 첫머리 참고)")
    chosen = sys.argv[1:] or specs()
    unknown = set(chosen) - set(specs())
    if unknown:
        sys.exit(f"없는 명세: {', '.join(sorted(unknown))}")
    failed = False
    for spec in chosen:
        for cfg, want in cases(spec):
            text = tlc(java, jar, HERE, cfg, spec)
            ok = want in text
            states = next((l for l in reversed(text.splitlines()) if "distinct states found" in l), "")
            print(f"{'통과' if ok else '실패'}: {cfg}: {want} {states}", flush=True)
            if not ok:
                print(text)
            elif want == PASS:
                missed = unreached(java, jar, spec, cfg)
                if missed:
                    ok = False
                    print(f"실패: {cfg}: 도달하지 못한 상태(명세가 거기까지 움직이지 못함): {', '.join(missed)}", flush=True)
            failed = failed or not ok
    sys.exit(1 if failed else 0)


if __name__ == "__main__":
    main()
