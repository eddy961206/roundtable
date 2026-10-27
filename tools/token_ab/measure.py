"""Token and tool-call breakdown of roundtable rooms for A/B comparisons.

usage: measure.py ROOM[=LABEL] ...   (reads http://127.0.0.1:18787 and local CLI logs)

Per room: input (all), cached, uncached, output, turns, wall time (sum of turn
durations), model calls and tool calls by kind. "wait" counts calls that only
wait for a running command: Codex empty write_stdin polls, agy manage_task
status/list. Codex calls come from the rollout of each turn's session; agy and
Claude calls come from the room's trace events (one event per tool call).
"""
import collections, glob, json, os, re, sys, urllib.request

BASE = os.environ.get('RT_URL', 'http://127.0.0.1:18787')
HOME = os.path.expanduser('~')


def get(path):
    return json.load(urllib.request.urlopen(BASE + path, timeout=60))


def codex_kind(c):
    if 'write_stdin' in c and re.search(r'chars:\s*""', c):
        return 'wait'
    if 'apply_patch' in c:
        return 'edit'
    if re.search(r'go test|go vet|pytest|node --test|npm test', c):
        return 'test'
    return 'other'


def codex_calls(session_ids):
    """Model calls and tool calls of the given Codex sessions (whole rollout)."""
    calls, kinds, wait_in = 0, collections.Counter(), 0
    for sid in session_ids:
        for f in glob.glob(os.path.join(HOME, '.codex', 'sessions', '*', '*', '*', f'rollout-*{sid}.jsonl')):
            pending = None
            for line in open(f, encoding='utf-8'):
                try:
                    e = json.loads(line)
                except ValueError:
                    continue
                p = e.get('payload') or {}
                if p.get('type') == 'custom_tool_call':
                    pending = codex_kind(p.get('input', ''))
                    kinds[pending] += 1
                elif p.get('type') == 'token_count' and p.get('info'):
                    calls += 1
                    if pending == 'wait':
                        wait_in += (p['info'].get('last_token_usage') or {}).get('input_tokens', 0)
                    pending = None
    return calls, kinds, wait_in


def trace_kinds(room):
    kinds = collections.Counter()
    path = os.path.join(HOME, '.roundtable', 'rooms', room, 'events.jsonl')
    if not os.path.exists(path):
        return kinds
    for line in open(path, encoding='utf-8'):
        try:
            e = json.loads(line)['event']
        except (ValueError, KeyError):
            continue
        if e.get('kind') != 'tool_call':
            continue
        name, inp = e.get('name', ''), e.get('input', '')
        if name == 'manage_task' and re.search(r'"Action"\s*:\s*"(status|list)"', inp):
            kinds['wait'] += 1
        elif name in ('BashOutput', 'TaskOutput'):
            kinds['wait'] += 1
        elif name in ('edit_file', 'write_file', 'Edit', 'Write', 'MultiEdit'):
            kinds['edit'] += 1
        elif re.search(r'go test|go vet|pytest|node --test', inp):
            kinds['test'] += 1
        else:
            kinds['other'] += 1
    return kinds


def room_row(room):
    d = get(f'/api/rooms/{room}?limit=200')
    msgs = [m for m in d.get('messages') or [] if m.get('kind') == 'agent' and m.get('usage')]
    u = get(f'/api/rooms/{room}/usage')['totals']
    provs = {m.get('provider') for m in msgs}
    sessions = {m.get('session_id') for m in msgs if m.get('provider') == 'codex' and m.get('session_id')}
    row = dict(room=room, title=d['room']['title'], turns=u['turns'], errors=u['errors'],
               input=u['input_tokens'], cached=u['cached_tokens'], uncached=u['uncached_tokens'],
               output=u['output_tokens'], minutes=round(u['duration_ms'] / 60000, 1),
               max_ctx=u['max_context_tokens'])
    if 'codex' in provs:
        calls, kinds, wait_in = codex_calls(sessions)
        row.update(model_calls=calls, tools=dict(kinds), wait_input=wait_in)
    else:
        row.update(tools=dict(trace_kinds(room)))
    return row


if __name__ == '__main__':
    rows = []
    for arg in sys.argv[1:]:
        room, _, label = arg.partition('=')
        r = room_row(room)
        r['label'] = label or room
        rows.append(r)
    print(json.dumps(rows, ensure_ascii=False, indent=1))
