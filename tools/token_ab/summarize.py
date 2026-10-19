"""Summarize two turns.py outputs (old vs new).
usage: python summarize.py <old.json>[@cumulative] <new.json>[@cumulative]

Mark a file @cumulative when it came from a build before the agy per-turn usage
fix (1c5a9d9 and earlier): those builds stored agy's conversation totals.

Opus (Claude): per-turn new input = input_tokens + cache_creation_input_tokens;
total_cost_usd is cumulative per session, so the last value of each session is summed.
Argon (agy), per-turn builds: input_tokens is the uncached input, cache_read_tokens the cached input.
Argon (agy), @cumulative builds: context_tokens is the uncached input; cache_read_tokens is
cumulative per session, so its per-turn value is the difference from the previous turn."""
import json
import sys


def opus_stats(opus):
    cost = {r['session_id']: r['usage']['total_cost_usd'] for r in opus}
    return {'opus_new_input': [r['usage']['cache_creation_input_tokens'] + r['usage']['input_tokens'] for r in opus],
            'opus_cost_usd': round(sum(cost.values()), 4), 'opus_sessions': len(cost),
            'opus_ms': sum(r['duration_ms'] for r in opus)}


def argon_stats(argon, cumulative):
    if cumulative:
        uncached = [r['usage']['context_tokens'] for r in argon]
        cached, prev = [], (None, 0)
        for r in argon:
            c = r['usage']['cache_read_tokens']
            cached.append(c - prev[1] if prev[0] == r['session_id'] else c)
            prev = (r['session_id'], c)
    else:
        uncached = [r['usage']['input_tokens'] for r in argon]
        cached = [r['usage']['cache_read_tokens'] for r in argon]
    return {'argon_uncached': uncached, 'argon_cached': cached,
            'argon_sessions': len({r['session_id'] for r in argon}),
            'argon_ms': sum(r['duration_ms'] for r in argon)}


def load(arg):
    path, _, kind = arg.partition('@')
    rows = json.load(open(path, encoding='utf-8'))['rows']
    opus = [r for r in rows if r['seat'] == 'Opus'][:8]
    argon = [r for r in rows if r['seat'] == 'Argon'][:8]
    stats = argon_stats(argon, kind == 'cumulative')
    if opus:
        stats.update(opus_stats(opus))
    return stats


old, new = load(sys.argv[1]), load(sys.argv[2])
for k in old.keys() & new.keys():
    print(f'{k:16} old={old[k]}  new={new[k]}')
    if isinstance(old[k], list):
        print(f'{"":16} sum old={sum(old[k])} new={sum(new[k])}')
