#!/bin/bash
# usage: ci_wait.sh <pr#>
# Waits until the CI status of the PR's current head leaves pending, then prints it.
# Never merges. Gitea has no push feed here, so this polls (ORCH_CI_INTERVAL seconds,
# ORCH_CI_TRIES times); run it as a background command.
# ORCH_CI_CONTEXT  substring of the status context to watch (e.g. linux); empty = combined state.
. "$(dirname "$0")/env.sh"
set -o pipefail
[ $# -eq 1 ] || die "usage: ci_wait.sh <pr#>"
case $1 in ''|*[!0-9]*) die "pr# must be digits: $1";; esac
need ORCH_REPO
pr=$1; st=none
for _ in $(seq 1 "${ORCH_CI_TRIES:-80}"); do
  sha=$("$ORCH_TEA" api "repos/$ORCH_REPO/pulls/$pr" | py -c "import json,sys;print(json.load(sys.stdin)['head']['sha'])") || exit 1
  st=$("$ORCH_TEA" api "repos/$ORCH_REPO/commits/$sha/status" | py -c "
import json,sys
ctx=sys.argv[1]; d=json.load(sys.stdin)
if not ctx: print(d.get('state') or 'none' if d.get('statuses') else 'none'); sys.exit()
l=[x['status'] for x in d.get('statuses') or [] if ctx in x['context']]
print(l[0] if l else 'none')" "$ORCH_CI_CONTEXT") || exit 1
  case $st in pending|none) sleep "${ORCH_CI_INTERVAL:-30}";; *) break;; esac
done
echo "#$pr head=${sha:0:7} ${ORCH_CI_CONTEXT:-ci}=$st"
case $st in pending|none) exit 3;; esac
