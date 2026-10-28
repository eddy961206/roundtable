#!/bin/bash
# usage: ci_wait.sh <pr#>
# Waits until the CI status of the PR's current head leaves pending, then prints it.
# Never merges. Gitea has no push feed here, so this polls (ORCH_CI_INTERVAL seconds,
# ORCH_CI_TRIES times); run it as a background command.
# ORCH_CI_CONTEXT  substring of the status context to watch (e.g. linux); empty = combined state.
# A failed status prints the FAIL/panic lines of its job log (the job id ends target_url).
# Gitea reads go through tea_get: a failed or empty reply is retried, then the script stops
# with the cause instead of reading it as "no status".
. "$(dirname "$0")/env.sh"
set -o pipefail
[ $# -eq 1 ] || die "usage: ci_wait.sh <pr#>"
case $1 in ''|*[!0-9]*) die "pr# must be digits: $1";; esac
need ORCH_REPO
pr=$1; st=none; url=
for _ in $(seq 1 "${ORCH_CI_TRIES:-80}"); do
  j=$(tea_get "repos/$ORCH_REPO/pulls/$pr") || exit 1
  sha=$(py -c "import json,sys;print(json.load(sys.stdin)['head']['sha'])" <<<"$j") || exit 1
  j=$(tea_get "repos/$ORCH_REPO/commits/$sha/status") || exit 1
  read -r st url < <(py -c "
import json,sys
ctx=sys.argv[1]; d=json.load(sys.stdin); l=d.get('statuses') or []
if ctx: l=[x for x in l if ctx in x['context']]
st=(l[0]['status'] if ctx else d.get('state') or 'none') if l else 'none'
bad=[x for x in l if x['status'] in ('failure','error')]
print(st, (bad[0].get('target_url') or '-') if bad else '-')" "$ORCH_CI_CONTEXT" <<<"$j")
  url=${url%$'\r'}  # Python on Windows ends the line with CRLF; read keeps the CR
  [ -n "$st" ] || exit 1
  case $st in pending|none) sleep "${ORCH_CI_INTERVAL:-30}";; *) break;; esac
done
echo "#$pr head=${sha:0:7} ${ORCH_CI_CONTEXT:-ci}=$st"
job=${url##*/}
case $st:$job in
  failure:*[!0-9]*|error:*[!0-9]*|failure:|error:) ;;
  failure:*|error:*)
    echo "job $job log (FAIL/panic lines):"
    if log=$(tea_raw "repos/$ORCH_REPO/actions/jobs/$job/logs"); then
      py -c "
import re,sys
lines=[re.sub(r'^\S+Z ','',l.rstrip()) for l in sys.stdin]
hits=[l for l in lines if re.match(r'(--- FAIL|FAIL\s|panic:)', l)]
print('\n'.join('  '+l for l in hits[:40]) or '  (no FAIL/panic line; read the whole log)')" <<<"$log"
    else
      echo "  job log unavailable (cause above)" >&2
    fi;;
esac
case $st in pending|none) exit 3;; esac
