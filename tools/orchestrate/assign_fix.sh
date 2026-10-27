#!/bin/bash
# usage: assign_fix.sh <pr#> <review_id> <room> <branch> ["extra instruction"]
# Posts one reviewer review (fetched from Gitea, saved under $ORCH_STATE) to the writer
# room together with the shared writer rules; prints room:seq for rtwait.sh.
# Fails before posting if the room's workspace folder is gone, or if another running
# room occupies the branch's worktree (worktree_claims.py check).
# Needs ORCH_REPO and ORCH_WRITER_RULES (a rules file, see templates/).
. "$(dirname "$0")/env.sh"
set -o pipefail
[ $# -ge 4 ] && [ $# -le 5 ] || die "usage: assign_fix.sh <pr#> <review_id> <room> <branch> [\"extra instruction\"]"
pr=$1; rid=$2; room=$3; br=$4; extra=$5
case $pr$rid in ''|*[!0-9]*) die "pr# and review_id must be digits: $pr $rid";; esac
need ORCH_REPO ORCH_WRITER_RULES
[ -f "$ORCH_WRITER_RULES" ] || die "ORCH_WRITER_RULES file not found: $ORCH_WRITER_RULES"

ws=$(curl -sf "$ORCH_RT_URL/api/rooms/$room" | py -c "
import json,sys,os
r=json.load(sys.stdin); r=r.get('room',r); w=r['workspace']
print(w if os.path.isdir(w) else 'MISSING:'+w)") || { echo "cannot read room $room from $ORCH_RT_URL" >&2; exit 1; }
case $ws in MISSING:*) echo "room $room workspace gone (${ws#MISSING:}); repoint with rt update_room first" >&2; exit 1;; esac
# Refuse while another running room works in the branch's worktree.
py "$ORCH_DIR/worktree_claims.py" check --branch "$br" --room "$room" || exit 1

review="$(state_dir)/rv${rid}_pr${pr}.md"
"$ORCH_TEA" api "repos/$ORCH_REPO/pulls/$pr/reviews" | py -c "
import json,sys
rid,out=int(sys.argv[1]),sys.argv[2]
r=[x for x in json.load(sys.stdin) if x['id']==rid]
if not r: sys.exit('review %d not found on the PR' % rid)
open(out,'w',encoding='utf-8').write(r[0]['body'])" "$rid" "$review" || exit 1

{ echo "상대 개발자가 PR #$pr(브랜치 \`$br\`)에 리뷰 $rid를 남겼어(아래 원문). 네 worktree의 그 브랜치에서 고쳐 줘. 지적마다 반례 시험을 먼저 써서 현재 head에서 실패하는 것을 확인한 뒤 고쳐. 막지 않는 항목도 작으면 같이 처리하고, 미루는 것은 작업 기록에 이유와 함께 남겨. 전체 검사 뒤 같은 브랜치에 로컬 커밋만 해. 끝나면 지적별 결과 표와 새 head를 적어. $extra"
  echo; cat "$ORCH_WRITER_RULES"; echo; echo "--- 리뷰 $rid 원문 ---"; cat "$review"; } \
 | rt post --room "$room" --text - | py -c "import json,sys;print(sys.argv[1]+':'+str(json.load(sys.stdin)['seq']))" "$room"
