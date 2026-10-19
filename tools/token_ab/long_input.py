"""Long-input A/B: one Argon seat, one message between the old 24KB command-line
limit and the 60KB history clip, secret in the middle. usage: long_input.py <base> <label> <out> <reps>"""
import json
import sys
import time
import urllib.request

BASE, LABEL, OUT, REPS = sys.argv[1], sys.argv[2], sys.argv[3], int(sys.argv[4])


def api(m, p, b=None):
    d = json.dumps(b).encode() if b is not None else None
    r = urllib.request.Request(BASE + p, data=d, method=m, headers={'Content-Type': 'application/json'})
    with urllib.request.urlopen(r, timeout=120) as x:
        body = x.read().decode()
        return json.loads(body) if body else None


def wait(rid):
    time.sleep(3)
    for _ in range(300):
        s = api('GET', f'/api/rooms/{rid}')
        if not s['queue'] and not any(m['status'] == 'streaming' for m in s['messages']):
            return s
        time.sleep(4)
    raise SystemExit('timeout')


filler = '이 문장은 길이 측정을 위한 채움 문장이며 의미가 없다. ' * 10 + '\n'   # ~0.9KB
codes = ['HERON-7319', 'MAPLE-2804', 'OTTER-5561']
results = []
for i in range(REPS):
    text = ('아래 자료 가운데에 숨은 비밀 코드만 그대로 답해. 다른 말은 쓰지 마.\n\n' + filler * 24
            + f'\n비밀 코드: {codes[i]}\n' + filler * 24 + '\n끝.')
    r = api('POST', '/api/rooms', {'title': f'A/B 긴 입력 ({LABEL} {i + 1})', 'workspace': '', 'mode': 'chat', 'note': '',
                                   'max_chain': 4, 'discussion_rounds': 3, 'persona': False,
                                   'participant_specs': [{'provider': 'agy', 'model': 'gemini-4-argon', 'effort': 'low', 'label': 'Argon', 'count': 1}]})
    rid = r['id']
    api('POST', f'/api/rooms/{rid}/messages', {'text': text})
    s = wait(rid)
    m = [x for x in s['messages'] if x['kind'] == 'agent'][-1]
    tr = api('GET', f"/api/rooms/{rid}/trace/{m['id']}")
    events = tr if isinstance(tr, list) else tr.get('events', [])
    tools = [e.get('name') for e in events if e.get('kind') == 'tool_call']
    results.append({'bytes': len(text.encode()), 'code': codes[i], 'correct': codes[i] in m['text'], 'answer': m['text'][:120],
                    'status': m['status'], 'duration_ms': m.get('duration_ms'), 'usage': m.get('usage') or {}, 'tool_calls': tools})
    print(LABEL, i + 1, results[-1]['correct'], results[-1]['usage'].get('input_tokens'), tools, flush=True)
json.dump(results, open(OUT, 'w', encoding='utf-8'), ensure_ascii=False, indent=1)
