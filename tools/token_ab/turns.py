"""Run the same scripted conversation against one roundtable server and record per-turn usage.

usage: python turns.py <base_url> <label> <out.json> [short]

Two seats (Opus, Argon), the same room note, 8 broadcast questions, both efforts
changed low -> medium before question 7. Without `short`, a long message targeted
at Argon follows. Run against two servers (old and new binary, separate -data) at once.
"""
import json
import sys
import time
import urllib.request

BASE, LABEL, OUT = sys.argv[1], sys.argv[2], sys.argv[3]


def api(m, p, b=None):
    d = json.dumps(b).encode() if b is not None else None
    r = urllib.request.Request(BASE + p, data=d, method=m, headers={'Content-Type': 'application/json'})
    with urllib.request.urlopen(r, timeout=120) as x:
        body = x.read().decode()
        return json.loads(body) if body else None


def snap(rid):
    return api('GET', f'/api/rooms/{rid}')


def busy(s):
    return bool(s['queue']) or any(m['status'] == 'streaming' for m in s['messages'])


def wait(rid, limit=900):
    t0 = time.time()
    time.sleep(3)
    while time.time() - t0 < limit:
        s = snap(rid)
        if not busy(s):
            return s
        time.sleep(4)
    raise SystemExit('timeout')


NOTE = ('이 방은 사내 백엔드 팀의 설계 메모를 검토하는 자리다. 결정은 사람이 내린다. 답은 한국어로 짧게. '
        '용어: SLA는 서비스 수준 약속, RPO는 허용 데이터 손실 시간, RTO는 복구 목표 시간이다. '
        '전제: 서비스는 단일 리전, 하루 요청 2천만 건, 피크는 평시의 3배, 데이터베이스는 기본·대기 2대. '
        '금지: 근거 없는 수치 단정, 확인하지 않은 사실을 확인했다고 말하기. '
        '형식: 질문에 한두 문장으로 답하고, 동의하지 않으면 이유를 한 문장 덧붙인다. ') * 3

QUESTIONS = [
    'RPO 5분을 지키려면 백업만으로 충분한가?',
    '피크 대비 용량은 평시의 몇 배로 잡는 게 무난한가?',
    '대기 DB로 읽기 부하를 나누면 생기는 위험 하나만 말해줘.',
    'SLA 99.9%면 한 달 허용 중단 시간은 대략 얼마인가?',
    '단일 리전 구성의 가장 큰 약점 하나는?',
    '지금까지 나온 답 중 동의하지 않는 것이 있으면 하나만 짚어줘.',
    '추론 수준을 바꾼 뒤 첫 질문: RTO 30분 목표에 가장 중요한 준비 하나는?',
    '마지막 질문: 오늘 논의를 한 문장으로 요약해줘.',
]


def long_text():
    filler = '이 문장은 길이 측정을 위한 채움 문장이며 내용상 의미가 없다. ' * 30 + '\n'
    return ('아래 긴 자료의 가운데에 숨은 비밀 코드만 그대로 답해. 다른 말은 쓰지 마.\n\n'
            + filler * 45 + '\n비밀 코드: HERON-7319\n' + filler * 45 + '\n끝.')


def main():
    r = api('POST', '/api/rooms', {'title': f'A/B 토큰 측정 ({LABEL})', 'workspace': '', 'mode': 'chat', 'note': NOTE,
                                   'max_chain': 4, 'discussion_rounds': 3, 'persona': False,
                                   'participant_specs': [
                                       {'provider': 'claude', 'model': 'claude-opus-5-5', 'effort': 'low', 'label': 'Opus', 'count': 1},
                                       {'provider': 'agy', 'model': 'gemini-4-argon', 'effort': 'low', 'label': 'Argon', 'count': 1}]})
    rid = r['id']
    seats = {p['label']: p['id'] for p in r['participants']}
    marks = []
    for i, q in enumerate(QUESTIONS):
        if i == 6:
            api('PATCH', f'/api/rooms/{rid}', {'participants': [{'id': seats['Opus'], 'effort': 'medium'}, {'id': seats['Argon'], 'effort': 'medium'}]})
            marks.append(('effort_changed_before_turn', 7))
        api('POST', f'/api/rooms/{rid}/messages', {'text': f'질문 {i + 1}: {q}'})
        wait(rid)
        print(LABEL, 'turn', i + 1, 'done', flush=True)
    long = long_text()
    s = snap(rid)
    if len(sys.argv) < 5:
        api('POST', f'/api/rooms/{rid}/messages', {'text': long, 'target': seats['Argon']})
        s = wait(rid, limit=1200)
        print(LABEL, 'long done', flush=True)
    rows = []
    for m in s['messages']:
        if m['kind'] != 'agent':
            continue
        rows.append({'seat': [k for k, v in seats.items() if v == m['author']][0], 'seq': m['seq'], 'status': m['status'],
                     'error': m.get('error', ''), 'duration_ms': m.get('duration_ms'), 'effort': m.get('effort'),
                     'session_id': m.get('session_id'), 'usage': m.get('usage') or {}, 'text': m['text'][:200]})
    json.dump({'label': LABEL, 'room': rid, 'long_bytes': len(long.encode()), 'rows': rows, 'marks': marks},
              open(OUT, 'w', encoding='utf-8'), ensure_ascii=False, indent=1)
    print(LABEL, 'saved', OUT, len(rows), 'rows')


main()
