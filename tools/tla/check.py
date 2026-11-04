"""Model-check RtWait.tla with TLC: the current rt wait must pass, and each
earlier version must still fail with the defect it shipped.

    set ROUNDTABLE_JAVA=D:\\Tools\\jdk-21.0.12.1+1\\bin\\java.exe
    set ROUNDTABLE_TLA2TOOLS=D:\\Tools\\tla\\tla2tools.jar
    python tools/tla/check.py

Java 11 or newer and tla2tools.jar (https://github.com/tlaplus/tlaplus/releases)
are required; the script stops when either is missing.
"""
import os
import subprocess
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))

# config -> the TLC line that must appear in its output
PASS = "Model checking completed. No error has been found."
EXPECT = {
    "RtWait_current.cfg": PASS,
    "RtWait_current_steer.cfg": PASS,
    # without publishList: a work step back to idle signaled nothing.
    "RtWait_current_nopublish.cfg": "Invariant SleepsOnlyWhenUnfinished is violated.",
    # b3a7b76: the steered turn's reply lies below after_seq.
    "RtWait_idle_steer.cfg": "Invariant SleepsOnlyWhenUnfinished is violated.",
    # b3a7b76: a work step (submit check, merge) did not count as active.
    "RtWait_idle_work.cfg": "Action property ReadyOnlyWhenIdle is violated.",
    # 5d6506c: a turn started between GET /api/rooms and the message read.
    "RtWait_replied.cfg": "Action property ReadyOnlyWhenIdle is violated.",
    # before 5d6506c: a notice pushed the reply out of max_chars (2026-11-04).
    "RtWait_window.cfg": "Invariant SleepsOnlyWhenUnfinished is violated.",
}


def main():
    sys.stdout.reconfigure(encoding="utf-8")
    java, jar = os.environ.get("ROUNDTABLE_JAVA"), os.environ.get("ROUNDTABLE_TLA2TOOLS")
    if not java or not jar:
        sys.exit("ROUNDTABLE_JAVA와 ROUNDTABLE_TLA2TOOLS를 지정해야 함 (이 파일 첫머리 참고)")
    failed = False
    for cfg, want in EXPECT.items():
        with tempfile.TemporaryDirectory() as meta:
            out = subprocess.run(
                [java, "-XX:+UseParallelGC", "-cp", jar, "tlc2.TLC", "-workers", "auto",
                 "-metadir", meta, "-config", cfg, "RtWait.tla"],
                cwd=HERE, capture_output=True, text=True, encoding="utf-8")
        text = out.stdout + out.stderr
        ok = want in text
        failed = failed or not ok
        states = next((l for l in text.splitlines() if "distinct states found" in l), "")
        print(f"{'통과' if ok else '실패'}: {cfg}: {want} {states}")
        if not ok:
            print(text)
    sys.exit(1 if failed else 0)


if __name__ == "__main__":
    main()
