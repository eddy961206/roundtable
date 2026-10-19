"""Summarize two turns.py outputs (old vs new). usage: python summarize.py <old.json> <new.json>

Opus (Claude): per-turn new input = input_tokens + cache_creation_input_tokens;
total_cost_usd is cumulative per session, so the last value of each session is summed.
Argon (agy): context_tokens is the per-turn uncached input; cache_read_tokens is
cumulative per session, so its per-turn value is the difference from the previous turn."""
import json
import sys


def load(path):
    rows = json.load(open(path, encoding='utf-8'))['rows']
    opus = [r for r in rows if r['seat'] == 'Opus'][:8]
    argon = [r for r in rows if r['seat'] == 'Argon'][:8]
    cost = {r['session_id']: r['usage']['total_cost_usd'] for r in opus}
    cached, prev = [], (None, 0)
    for r in argon:
        c = r['usage']['cache_read_tokens']
        cached.append(c - prev[1] if prev[0] == r['session_id'] else c)
        prev = (r['session_id'], c)
    return {'opus_new_input': [r['usage']['cache_creation_input_tokens'] + r['usage']['input_tokens'] for r in opus],
            'opus_cost_usd': round(sum(cost.values()), 4), 'opus_sessions': len(cost),
            'argon_uncached': [r['usage']['context_tokens'] for r in argon], 'argon_cached': cached,
            'argon_sessions': len({r['session_id'] for r in argon}),
            'opus_ms': sum(r['duration_ms'] for r in opus), 'argon_ms': sum(r['duration_ms'] for r in argon)}


old, new = load(sys.argv[1]), load(sys.argv[2])
for k in old:
    print(f'{k:16} old={old[k]}  new={new[k]}')
    if isinstance(old[k], list):
        print(f'{"":16} sum old={sum(old[k])} new={sum(new[k])}')
