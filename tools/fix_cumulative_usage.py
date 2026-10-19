"""One-time correction of usage stored by builds that saved session totals as turn usage.

Before the fixes, three values were stored per answer as if they were that turn's:
- agy (Antigravity): every token count was the conversation total (fixed in 0ff0034).
- Codex: every token count was the thread total (fixed in a8e35bf).
- Claude: total_cost_usd was the session's running cost; token counts were already per turn.

This rewrites those answers to per-turn values by taking the difference from the previous
answer in the same session, keeps the running Claude cost as session_cost_usd, and
recomputes the affected participant totals in room.json.

Stop the roundtable server first: it rewrites room.json and appends messages itself.
Pass, per provider, the time (Unix ms) the fixed build started serving; answers from that
time on are already per turn. Without --apply it only prints what would change.

usage: python fix_cumulative_usage.py --data ~/.roundtable --agy-before MS --codex-before MS
       --claude-before MS [--apply]
"""
import argparse
import json
import os
import shutil
import sys

TOKEN_KEYS = {
    'agy': ['input_tokens', 'output_tokens', 'thinking_tokens', 'cache_read_tokens', 'total_tokens'],
    'codex': ['input_tokens', 'cached_input_tokens', 'cache_write_input_tokens', 'output_tokens', 'reasoning_output_tokens', 'total_tokens'],
}
CACHE_KEYS = ['cache_read_input_tokens', 'cached_input_tokens', 'cache_read_tokens']
MARKER = 'usage-fix-cumulative.done'


def read_messages(path):
    by_id, order = {}, []
    with open(path, encoding='utf-8') as f:
        for line in f:
            if not line.strip():
                continue
            m = json.loads(line)
            if m['id'] not in by_id:
                order.append(m['id'])
            by_id[m['id']] = m
    return sorted((by_id[i] for i in order), key=lambda m: m['seq'])


def correct_room(room, messages, cutoff):
    """Returns the corrected messages; raises on a total that went down within a session."""
    providers = {p['id']: p.get('provider') or p['id'] for p in room['participants']}
    last_raw = {}  # (provider, session) -> raw usage of the previous answer
    changed = []
    for m in messages:
        provider = m.get('provider') or providers.get(m['author'])
        usage = m.get('usage')
        if m['kind'] != 'agent' or not usage or provider not in cutoff or m['ts'] >= cutoff[provider]:
            continue
        session = m.get('session_id', '')
        prev = last_raw.get((provider, session), {}) if session else {}
        fixed = dict(usage)
        if provider == 'claude':
            if 'total_cost_usd' not in usage:
                continue
            raw = {'total_cost_usd': usage['total_cost_usd']}
            fixed['session_cost_usd'] = usage['total_cost_usd']
            fixed['total_cost_usd'] = usage['total_cost_usd'] - prev.get('total_cost_usd', 0)
        else:
            raw = {k: usage[k] for k in TOKEN_KEYS[provider] if k in usage}
            for k, v in raw.items():
                fixed[k] = v - prev.get(k, 0)
        bad = [k for k in raw if fixed[k] < -1e-9]
        if bad:
            raise SystemExit(f"room {room['id']} message {m['id']} (seq {m['seq']}): {bad} decreased within session {session}; not cumulative, stopping")
        if session:
            last_raw[(provider, session)] = raw
        m['usage'] = fixed
        changed.append(m)
    return changed


def recompute_totals(room, messages, cutoff):
    by_author = {}
    for m in messages:
        if m['kind'] == 'agent' and m.get('usage'):
            by_author.setdefault(m['author'], []).append(m)
    for p in room['participants']:
        provider = p.get('provider') or p['id']
        if provider not in cutoff:
            continue
        own = by_author.get(p['id'], [])
        totals = p.setdefault('totals', {})
        num = lambda u, k: u.get(k, 0) if isinstance(u.get(k, 0), (int, float)) else 0
        if provider == 'claude':
            totals['cost_usd'] = sum(num(m['usage'], 'total_cost_usd') for m in own)
        else:
            totals['input'] = sum(num(m['usage'], 'input_tokens') for m in own)
            totals['output'] = sum(num(m['usage'], 'output_tokens') for m in own)
            totals['cached'] = sum(sum(num(m['usage'], k) for k in CACHE_KEYS) for m in own)
        if own and own[-1]['ts'] < cutoff[provider]:
            p['usage'] = own[-1]['usage']


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--data', required=True)
    ap.add_argument('--agy-before', type=int, required=True)
    ap.add_argument('--codex-before', type=int, required=True)
    ap.add_argument('--claude-before', type=int, required=True)
    ap.add_argument('--apply', action='store_true')
    a = ap.parse_args()
    data = os.path.expanduser(a.data)
    if os.path.exists(os.path.join(data, MARKER)):
        raise SystemExit(f'already applied ({MARKER} exists); running again would subtract twice')
    cutoff = {'agy': a.agy_before, 'codex': a.codex_before, 'claude': a.claude_before}
    rooms_dir = os.path.join(data, 'rooms')
    report = {}
    plans = []
    for rid in sorted(os.listdir(rooms_dir)):
        d = os.path.join(rooms_dir, rid)
        room_path, msg_path = os.path.join(d, 'room.json'), os.path.join(d, 'messages.jsonl')
        if not os.path.exists(room_path) or not os.path.exists(msg_path):
            continue
        room = json.load(open(room_path, encoding='utf-8'))
        messages = read_messages(msg_path)
        before = {m['id']: dict(m['usage']) for m in messages if m.get('usage')}
        changed = correct_room(room, messages, cutoff)
        for m in changed:
            key = (m.get('provider') or '?', m.get('model') or '?')
            row = report.setdefault(key, {'answers': 0, 'input_before': 0, 'input_after': 0, 'cost_before': 0, 'cost_after': 0})
            row['answers'] += 1
            row['input_before'] += before[m['id']].get('input_tokens', 0)
            row['input_after'] += m['usage'].get('input_tokens', 0)
            row['cost_before'] += before[m['id']].get('total_cost_usd', 0)
            row['cost_after'] += m['usage'].get('total_cost_usd', 0)
        if changed:
            recompute_totals(room, messages, cutoff)
            plans.append((room_path, msg_path, room, changed))
    for (provider, model), r in sorted(report.items()):
        print(f"{provider:7} {model:24} answers {r['answers']:4}  input {r['input_before']:>12,} -> {r['input_after']:>10,}"
              f"  cost ${r['cost_before']:.3f} -> ${r['cost_after']:.3f}")
    print(f'{len(plans)} rooms to change')
    if not a.apply:
        print('dry run: nothing written (add --apply)')
        return
    for room_path, msg_path, room, changed in plans:
        shutil.copy2(room_path, room_path + '.bak-usagefix')
        shutil.copy2(msg_path, msg_path + '.bak-usagefix')
        with open(msg_path, 'a', encoding='utf-8') as f:  # the store keeps the last record per id
            for m in changed:
                f.write(json.dumps(m, ensure_ascii=False) + '\n')
        tmp = room_path + '.tmp'
        with open(tmp, 'w', encoding='utf-8') as f:
            json.dump(room, f, ensure_ascii=False, indent=2)
        os.replace(tmp, room_path)
    open(os.path.join(data, MARKER), 'w').write(json.dumps(cutoff) + '\n')
    print('applied; backups: *.bak-usagefix')


if __name__ == '__main__':
    sys.exit(main())
