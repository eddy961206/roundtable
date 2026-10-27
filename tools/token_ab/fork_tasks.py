"""Compare "a new room per task" with "fork every task from one base seat" on one roundtable server.

usage: python fork_tasks.py <base_url> <workspace> <out.json> <codex|claude> [fresh|fork|both]

Both arms do the same three small read-only tasks over the same shared documents
in <workspace> (a frozen copy of this repository):
- fresh: one new room per task; each task prompt first asks to read the documents.
- fork:  one base room reads the documents once, then each task room forks that seat.
`both` (default) runs the two arms at the same time so the account, model and time match.
Per agent reply the output keeps the usage the room stored, the duration and the
number of tool calls in its trace. Summarize with fork_summary.py.
"""
import json
import sys
import threading
import time
import urllib.request

BASE, WORKSPACE, OUT, MODEL = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4]
ARM = sys.argv[5] if len(sys.argv) > 5 else 'both'
ARMS = ['fresh', 'fork'] if ARM == 'both' else [ARM]
SEATS = {'codex': {'provider': 'codex', 'model': 'gpt-6.1-sol', 'effort': 'high', 'label': 'Sol'},
         'claude': {'provider': 'claude', 'model': 'claude-sonnet-5-5', 'effort': 'medium', 'label': 'Sonnet'}}
SEAT = SEATS[MODEL]

DOCS = ['README.md', 'docs/개선_구현계약_20261020.md', 'docs/역할카드_개선_계약.md',
        'docs/에이전트MCP_블라인드토론_결정카드_계약.md', 'docs/작업분배_적대적리뷰_계약.md', 'skills/roundtable/SKILL.md']
READ = ('공통 문서를 먼저 모두 끝까지 읽으세요: ' + ', '.join(DOCS) +
        '. 이 문서들의 계약이 이후 작업의 기준입니다.')
BASE_PROMPT = READ + ' 이번 턴에는 작업하지 말고, 문서를 다 읽었으면 "준비됨"만 답하세요.'
TASKS = [
    'docs/역할카드_개선_계약.md의 역할 카드 규칙 하나를 골라 internal/room/prompt.go 구현이 그 규칙을 지키는지 확인하세요. '
    '결론 한 문장과 근거(파일:줄) 두 개만 답하세요.',
    'skills/roundtable/SKILL.md의 rt read max_chars 설명이 internal/rtmcp 구현과 일치하는지 확인하세요. '
    '결론 한 문장과 근거(파일:줄) 두 개만 답하세요.',
    '다음 주장을 검수하세요: "좌석이 [[DECISION]] 표식으로 남긴 결정 카드는 곧바로 active가 되어 안내문에 들어간다." '
    '계약 문서와 internal/room 구현을 근거로 맞는지 틀린지 한 문장, 근거(파일:줄) 두 개만 답하세요.',
]


def api(m, p, b=None):
    d = json.dumps(b).encode() if b is not None else None
    r = urllib.request.Request(BASE + p, data=d, method=m, headers={'Content-Type': 'application/json'})
    with urllib.request.urlopen(r, timeout=120) as x:
        body = x.read().decode()
        return json.loads(body) if body else None


def wait(rid, limit=1800):
    t0 = time.time()
    time.sleep(3)
    while time.time() - t0 < limit:
        s = api('GET', f'/api/rooms/{rid}')
        if not s['queue'] and not any(m['status'] == 'streaming' for m in s['messages']):
            return s
        time.sleep(5)
    raise SystemExit(f'timeout {rid}')


def room(title, seat):
    return api('POST', '/api/rooms', {'title': title, 'workspace': WORKSPACE, 'mode': 'read', 'participant_specs': [seat]})


def ask(arm, step, r, text):
    rid = r['id']
    api('POST', f'/api/rooms/{rid}/messages', {'text': text})
    s = wait(rid)
    m = [m for m in s['messages'] if m['kind'] == 'agent'][-1]
    trace = api('GET', f'/api/rooms/{rid}/trace/{m["id"]}') or []
    tools = sum(1 for e in trace if e['kind'] == 'tool_call')
    print(arm, step, m['status'], m.get('duration_ms'), 'tools', tools, flush=True)
    return {'arm': arm, 'step': step, 'room': rid, 'status': m['status'], 'error': m.get('error', ''),
            'duration_ms': m.get('duration_ms'), 'session_id': m.get('session_id'), 'tool_calls': tools,
            'usage': m.get('usage') or {}, 'text': m['text'][:400]}


def fresh(rows):
    for i, task in enumerate(TASKS):
        rows.append(ask('fresh', f'task{i + 1}', room(f'A/B 새 방 {i + 1}', SEAT), READ + '\n\n작업: ' + task))


def fork(rows):
    base = room('A/B 기반', SEAT)
    rows.append(ask('fork', 'base', base, BASE_PROMPT))
    ref = {'room': base['id'], 'seat': base['participants'][0]['id']}
    for i, task in enumerate(TASKS):
        seat = {'provider': SEAT['provider'], 'label': SEAT['label'], 'fork_from': ref}
        rows.append(ask('fork', f'task{i + 1}', room(f'A/B 포크 {i + 1}', seat), '작업: ' + task))


def main():
    rows = {arm: [] for arm in ARMS}
    threads = [threading.Thread(target={'fresh': fresh, 'fork': fork}[arm], args=(rows[arm],)) for arm in ARMS]
    started = time.strftime('%Y-%m-%d %H:%M:%S')
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    out = {'model': SEAT, 'started': started, 'docs': DOCS, 'rows': [r for arm in ARMS for r in rows[arm]]}
    json.dump(out, open(OUT, 'w', encoding='utf-8'), ensure_ascii=False, indent=1)
    print('saved', OUT, len(out['rows']), 'rows')


main()
