#!/bin/bash
# usage: reviewer_watch.sh <since ISO-8601> [reviewer_check.py options]
# Re-runs reviewer_check.py every ORCH_WATCH_INTERVAL seconds (default 300, ORCH_WATCH_TRIES
# times, default 24) and prints the first non-empty result. Polls; run it in the background.
. "$(dirname "$0")/env.sh"
[ $# -ge 1 ] || die "usage: reviewer_watch.sh <since ISO-8601> [--user LOGIN] [--repos owner/a,owner/b]"
export ORCH_TEA ORCH_REPO ORCH_REVIEWER ORCH_WATCH_REPOS
for _ in $(seq 1 "${ORCH_WATCH_TRIES:-24}"); do
  out=$(py "$ORCH_DIR/reviewer_check.py" "$@") || exit $?
  [ -n "$out" ] && { echo "$out"; exit 0; }
  sleep "${ORCH_WATCH_INTERVAL:-300}"
done
echo "no new activity since $1"
exit 3
