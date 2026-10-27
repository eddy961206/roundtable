"""Reject Gitea post bodies with leftovers seen on 2026-10-27: author draft lines,
local absolute paths, inline code emptied by shell quoting, attribution lines.

usage: glint.py <body.md> [...]   exit 0 clean, 1 findings, 2 usage/read error.
ORCH_LINT_EXTRA adds one more regex counted as a local path (e.g. a worktree prefix).
"""
import os
import re
import sys

RULES = [
    (r'push·PR|PR 게시|게시하지 않|병합하지 않았|로컬 초안|사용자가 별도로|사용자 후속 절차|외부 검수는 남',
     '작성자 초안 문구'),
    # C:\Users\.. C:/x  /c/Users/..  /home/u/  /Users/u/ — but not URL paths.
    (r'\b[A-Za-z]:[\\/][\w.$-]|(?<![\w.:/-])/[a-z]/[\w.-]+/|(?<![\w.:/-])/(home|Users)/[\w.-]+/',
     '로컬 절대 경로'),
    # `` alone (not part of a ``` fence) or a label followed by the gap a vanished value leaves.
    (r'(?<!`)``(?!`)|\b(head|base|커밋|commit|sha)\s{2,}\S', '빈 코드 조각(셸 따옴표로 값이 사라짐)'),
    (r'Generated with|Co-Authored-By|대신 작성', '대리 작성 표식'),
]


def lint(text, extra=None):
    """Returns [(reason, line_no, hit)] for every rule that matches, first hit per rule."""
    rules = list(RULES)
    if extra:
        rules.append((extra, '로컬 절대 경로(ORCH_LINT_EXTRA)'))
    found = []
    for pat, why in rules:
        m = re.search(pat, text)
        if m:
            found.append((why, text.count('\n', 0, m.start()) + 1, m.group(0)))
    return found


def main(argv):
    if not argv:
        print(__doc__.strip(), file=sys.stderr)
        return 2
    extra = os.environ.get('ORCH_LINT_EXTRA') or None
    status = 0
    for path in argv:
        try:
            with open(path, encoding='utf-8') as f:
                text = f.read()
        except OSError as e:
            print(f'LINT: {path}: {e}', file=sys.stderr)
            return 2
        if not text.strip():
            print(f'LINT: {path}:1: 빈 본문')
            status = 1
        for why, line, hit in lint(text, extra):
            print(f'LINT: {path}:{line}: {why}: {hit!r}')
            status = 1
    return status


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
