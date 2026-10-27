#!/bin/bash
# usage: queue_review.sh <label> <worktree> <writer-room> <review-room> "<focus>"
# Rebases the worktree's branch onto origin/$ORCH_BASE, then queues a review request
# (ORCH_REVIEW_TEMPLATE) in the review room and prints "queued <label> room:seq".
# <worktree> is a directory, or a name under ORCH_WORKTREES.
# NOREBASE=1    keep the branch where it is (stacked on another open PR).
# PRIME=<file>  text sent before the request (e.g. the shared-context primer).
# ORCH_CONFLICT_HOOK  command run inside the worktree with the conflicted files as
#               arguments; it must resolve and `git add` them. Without it any conflict stops.
. "$(dirname "$0")/env.sh"
set -o pipefail
[ $# -eq 5 ] || die "usage: queue_review.sh <label> <worktree> <writer-room> <review-room> \"<focus>\""
label=$1; wt=$2; wr=$3; ar=$4; focus=$5
need ORCH_REVIEW_TEMPLATE
[ -f "$ORCH_REVIEW_TEMPLATE" ] || die "ORCH_REVIEW_TEMPLATE file not found: $ORCH_REVIEW_TEMPLATE"
[ -z "$PRIME" ] || [ -f "$PRIME" ] || die "PRIME file not found: $PRIME"
if [ -d "$wt" ]; then W=$wt; else need ORCH_WORKTREES; W=$ORCH_WORKTREES/$wt; fi
[ -d "$W" ] || die "no worktree: $W"

rt read --room "$wr" --after_seq 0 --max_chars 2000 \
 | py -c "import json,sys;m=json.load(sys.stdin)['messages'];print('writer last:', (m[-1]['text'] if m else '')[-300:])" || exit 1

cd "$W" || exit 1
git -c credential.useHttpPath=false fetch -q origin || exit 1
if [ -z "$NOREBASE" ] && ! git rebase -q "origin/$ORCH_BASE" >/dev/null 2>&1; then
  for _ in 1 2 3 4; do
    c=$(git diff --name-only --diff-filter=U)
    # Only a successful --continue breaks out, so no conflict here means rebase failed.
    [ -n "$c" ] || { echo "rebase onto origin/$ORCH_BASE failed in $W without conflicts (dirty tree?)" >&2; exit 2; }
    [ -n "$ORCH_CONFLICT_HOOK" ] || { echo "CONFLICT $c" >&2; exit 2; }
    # shellcheck disable=SC2086
    $ORCH_CONFLICT_HOOK $c || { echo "CONFLICT $c (hook failed)" >&2; exit 2; }
    GIT_EDITOR=true git rebase --continue >/dev/null 2>&1 && break
  done
  [ -z "$(git diff --name-only --diff-filter=U)" ] || { echo "CONFLICT $(git diff --name-only --diff-filter=U)" >&2; exit 2; }
fi
git log --oneline -1 | cat
br=$(git branch --show-current)

{ [ -n "$PRIME" ] && { cat "$PRIME"; echo; }
  echo "다음 검수 건: **$label** — worktree \`$(winpath "$W")\`(브랜치 $br). $focus 판정 줄은 '판정($label): 승인/수정 필요'로 써."
  echo; cat "$ORCH_REVIEW_TEMPLATE"; } \
 | rt post --room "$ar" --text - | py -c "import json,sys;print('queued',sys.argv[1],sys.argv[2]+':'+str(json.load(sys.stdin)['seq']))" "$label" "$ar"
