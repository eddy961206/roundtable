"""Compare one long-lived review seat with a new seat per review, for a provider without fork.

usage: python agy_reuse.py <base_url> <rt_exe> <workspace> <out.json> [model] [effort]

Both arms review the same files of <workspace> in the same order, one file per request:
- reuse: one room, one seat, every review resumes its session.
- fresh: one new room per review.
The arms run at the same time; each request is awaited with `roundtable rt wait`.
Per reply the output keeps the stored usage and duration. agy reports input_tokens
without the cache and cache_read_tokens as the cached part.
"""
import json
import subprocess
import sys
import threading
import urllib.request

BASE, RT, WORKSPACE, OUT = sys.argv[1:5]
MODEL = sys.argv[5] if len(sys.argv) > 5 else 'gemini-4-argon'
EFFORT = sys.argv[6] if len(sys.argv) > 6 else 'high'
SEAT = {'provider': 'agy', 'model': MODEL, 'effort': EFFORT, 'label': '검수'}
FILES = ['internal/room/attachments.go', 'internal/room/decisions.go', 'internal/room/store.go',
         'internal/room/participants.go', 'internal/room/requests.go', 'internal/room/prompt.go']
ASK = '{f} 파일만 읽고 검수하세요. 확인한 사실 한 가지와 의심 지점 한 가지를 각각 한 줄(파일:줄 포함)로만 답하세요.'


def api(m, p, b=None):
    d = json.dumps(b).encode() if b is not None else None
    r = urllib.request.Request(BASE + p, data=d, method=m, headers={'Content-Type': 'application/json'})
    with urllib.request.urlopen(r, timeout=120) as x:
        body = x.read().decode()
        return json.loads(body) if body else None


def room(title):
    return api('POST', '/api/rooms', {'title': title, 'workspace': WORKSPACE, 'mode': 'read', 'participant_specs': [SEAT]})['id']


def ask(arm, i, rid):
    sent = api('POST', f'/api/rooms/{rid}/messages', {'text': ASK.format(f=FILES[i])})
    out = subprocess.run([RT, 'rt', '--url', BASE, 'wait', '--rooms', f'{rid}:{sent["seq"]}', '--timeout_seconds', '3600'],
                         capture_output=True, text=True, encoding='utf-8', check=True)
    waited = json.loads(out.stdout)
    if waited['timed_out']:
        raise SystemExit(f'timeout {rid}')
    m = [m for m in api('GET', f'/api/rooms/{rid}')['messages'] if m['kind'] == 'agent'][-1]
    print(arm, i + 1, m['status'], m.get('duration_ms'), flush=True)
    return {'arm': arm, 'turn': i + 1, 'file': FILES[i], 'room': rid, 'status': m['status'], 'error': m.get('error', ''),
            'duration_ms': m.get('duration_ms'), 'session_id': m.get('session_id'), 'usage': m.get('usage') or {},
            'text': m['text'][:300]}


def reuse(rows):
    rid = room('agy 이어 쓰기')
    for i in range(len(FILES)):
        rows.append(ask('reuse', i, rid))


def fresh(rows):
    for i in range(len(FILES)):
        rows.append(ask('fresh', i, room(f'agy 새 좌석 {i + 1}')))


def main():
    rows = {'reuse': [], 'fresh': []}
    threads = [threading.Thread(target=f, args=(rows[k],)) for k, f in (('reuse', reuse), ('fresh', fresh))]
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    json.dump({'seat': SEAT, 'files': FILES, 'rows': rows['reuse'] + rows['fresh']}, open(OUT, 'w', encoding='utf-8'),
              ensure_ascii=False, indent=1)
    print('saved', OUT)


main()
