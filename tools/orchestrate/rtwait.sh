#!/bin/bash
# usage: rtwait.sh room:after_seq [room:after_seq ...]
# Blocks (server SSE, no polling) until one or more rooms answer after their seq,
# then prints each ready room's status and the tail of its last seat answer.
# after_seq is required: without it rt wait counts only replies newer than the wait
# start, so an answer that already landed is never seen and the wait runs to timeout.
. "$(dirname "$0")/env.sh"
set -o pipefail
[ $# -gt 0 ] || die "usage: rtwait.sh room:after_seq [room:after_seq ...]"
for a in "$@"; do
  [[ $a =~ ^[^:,[:space:]]+:[0-9]+$ ]] || die "need room:after_seq (seq from rt post), got: $a"
done
rooms=$(IFS=,; echo "$*")
summary=()
[ -n "$ORCH_WAIT_SUMMARY" ] && summary=(--summary true)
rt wait --rooms "$rooms" --timeout_seconds "${ORCH_WAIT_TIMEOUT:-3600}" --max_chars "${ORCH_WAIT_MAX_CHARS:-4000}" "${summary[@]}" \
 | py -c "
import json,sys
tail=int(sys.argv[1])
d=json.load(sys.stdin)
if d.get('timed_out'): print('TIMEOUT')
for r in d.get('ready') or []:
    ms=[m for m in r.get('messages',[]) if m.get('kind')!='user']
    m=ms[-1] if ms else {}
    print('READY', r.get('room'), m.get('status'), 'next_seq', r.get('next_seq'), (m.get('error') or '')[:200])
    print((m.get('text') or '')[-tail:])
    print('-----')
print('WAITING', d.get('waiting') or '')
" "${ORCH_WAIT_TAIL:-1500}"
