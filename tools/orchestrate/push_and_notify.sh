#!/bin/bash
# usage: push_and_notify.sh <worktree> <branch> <pr#> <body.md> [--force]
# Pushes the branch, then posts the PR comment and re-requests review (ORCH_REVIEWER)
# only after: the push exited 0, the remote branch tip is the local HEAD, and the PR
# reports that head (polled ORCH_PR_HEAD_TRIES times, ORCH_PR_HEAD_INTERVAL seconds).
# --force uses --force-with-lease (after a rebase). {{HEAD}} in the body becomes the
# verified short head. The body passes glint.py before anything is pushed.
. "$(dirname "$0")/env.sh"
set -o pipefail
[ $# -eq 4 ] || [ $# -eq 5 ] || die "usage: push_and_notify.sh <worktree> <branch> <pr#> <body.md> [--force]"
wt=$1; br=$2; pr=$3; body=$4; force=$5
case $pr in ''|*[!0-9]*) die "pr# must be digits: $pr";; esac
[ -z "$force" ] || [ "$force" = "--force" ] || die "unknown option: $force"
need ORCH_REPO
[ -f "$body" ] || die "no body file: $body"
[ -d "$wt" ] || die "no worktree: $wt"
[ "$(git -C "$wt" branch --show-current)" = "$br" ] || die "worktree $wt is not on $br"
[ -n "$(git -C "$wt" status --short)" ] && die "dirty worktree $wt"
head=$(git -C "$wt" rev-parse HEAD) || exit 1

filled="$(state_dir)/notify.$$.md"; trap 'rm -f "$filled"' EXIT
py -c "import sys;open(sys.argv[2],'w',encoding='utf-8').write(open(sys.argv[1],encoding='utf-8').read().replace('{{HEAD}}',sys.argv[3]))" \
  "$body" "$filled" "${head:0:7}" || exit 1
py "$ORCH_DIR/glint.py" "$filled" || exit 1

opts=(-q origin "$br")
[ -n "$force" ] && opts=(-q --force-with-lease origin "$br")
git -C "$wt" -c credential.useHttpPath=false push "${opts[@]}" || { echo "push failed; nothing posted" >&2; exit 1; }
verify_pushed "$wt" "$br" || exit 1

sha=""
for i in $(seq 1 "${ORCH_PR_HEAD_TRIES:-10}"); do
  sha=$("$ORCH_TEA" api "repos/$ORCH_REPO/pulls/$pr" | py -c "import json,sys;print(json.load(sys.stdin)['head']['sha'])") || exit 1
  [ "$sha" = "$head" ] && break
  [ "$i" -lt "${ORCH_PR_HEAD_TRIES:-10}" ] && sleep "${ORCH_PR_HEAD_INTERVAL:-3}"
done
[ "$sha" = "$head" ] || { echo "PR #$pr head is ${sha:0:7}, pushed ${head:0:7}; nothing posted" >&2; exit 1; }

"$ORCH_DIR/gpost.sh" comment "$pr" "$filled" || exit 1
if [ -n "$ORCH_REVIEWER" ]; then
  py -c "import json,sys;print(json.dumps({'reviewers':[sys.argv[1]]}))" "$ORCH_REVIEWER" > "$filled.json"
  "$ORCH_TEA" api -X POST "repos/$ORCH_REPO/pulls/$pr/requested_reviewers" --data @"$filled.json" >/dev/null \
    || { rm -f "$filled.json"; echo "PR #$pr: comment posted, review request for $ORCH_REVIEWER failed" >&2; exit 1; }
  rm -f "$filled.json"
  echo "review requested: $ORCH_REVIEWER"
fi
echo "pushed ${head:0:7} to $br; PR #$pr shows it"
