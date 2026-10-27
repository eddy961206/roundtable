#!/bin/bash
# usage: gpost.sh comment <issue#> <body.md>
#        gpost.sh pr <worktree> <branch> <title> <body.md>   (push + PR + reviewer request)
# Bodies always come from files (no inline shell quoting) and pass glint.py first.
# Needs ORCH_REPO (owner/name); ORCH_REVIEWER is requested on new PRs when set.
. "$(dirname "$0")/env.sh"
usage() { die "usage: gpost.sh comment <issue#> <body.md> | gpost.sh pr <worktree> <branch> <title> <body.md>"; }

# payload <out> <body.md> [key=value ...] writes {"body": <file text>, key: value...}.
payload() {
  py - "$@" <<'EOF'
import json, sys
out, body, *pairs = sys.argv[1:]
d = {'body': open(body, encoding='utf-8').read()}
d.update(p.split('=', 1) for p in pairs)
open(out, 'w', encoding='utf-8').write(json.dumps(d, ensure_ascii=False))
EOF
}
# field <name>: prints that field of the JSON on stdin, or the raw reply and fails.
field() { py -c "
import json,sys
raw=sys.stdin.read()
try: print(json.loads(raw)['$1'])
except Exception: sys.stderr.write('unexpected reply: '+raw[:500]+'\n'); sys.exit(1)"; }

case $1 in
comment)
  [ $# -eq 3 ] || usage
  case $2 in ''|*[!0-9]*) die "issue number must be digits: $2";; esac
  need ORCH_REPO
  py "$ORCH_DIR/glint.py" "$3" || exit 1
  tmp="$(state_dir)/gpost.$$.json"; trap 'rm -f "$tmp"' EXIT
  payload "$tmp" "$3" || exit 1
  id=$("$ORCH_TEA" api -X POST "repos/$ORCH_REPO/issues/$2/comments" --data @"$tmp" | field id) || exit 1
  echo "comment $id on #$2";;
pr)
  [ $# -eq 5 ] || usage
  wt=$2; br=$3; title=$4; body=$5
  need ORCH_REPO
  py "$ORCH_DIR/glint.py" "$body" || exit 1
  [ -d "$wt" ] || die "no worktree: $wt"
  [ -n "$(git -C "$wt" status --short)" ] && die "dirty worktree $wt"
  git -C "$wt" -c credential.useHttpPath=false push -q -u origin "$br" || exit 1
  tmp="$(state_dir)/gpost.$$.json"; trap 'rm -f "$tmp"' EXIT
  payload "$tmp" "$body" "title=$title" "head=$br" "base=$ORCH_BASE" || exit 1
  n=$("$ORCH_TEA" api -X POST "repos/$ORCH_REPO/pulls" --data @"$tmp" | field number) || exit 1
  echo "PR $n"
  if [ -n "$ORCH_REVIEWER" ]; then
    py -c "import json,sys;print(json.dumps({'reviewers':[sys.argv[1]]}))" "$ORCH_REVIEWER" > "$tmp"
    "$ORCH_TEA" api -X POST "repos/$ORCH_REPO/pulls/$n/requested_reviewers" --data @"$tmp" >/dev/null \
      || { echo "PR $n: reviewer request for $ORCH_REVIEWER failed" >&2; exit 1; }
    echo "review requested: $ORCH_REVIEWER"
  fi;;
*) usage;;
esac
