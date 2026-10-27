"""Summarize fork_tasks.py outputs.
usage: python fork_summary.py <out.json> [<out.json> ...]

Codex: input_tokens includes cached_input_tokens; uncached = input - cached.
Claude: new input = input_tokens + cache_creation_input_tokens; cached = cache_read_input_tokens;
total_cost_usd is already this turn's cost (the room subtracts the session's earlier cost)."""
import json
import sys


def tokens(row):
    u = row['usage']
    if 'cached_input_tokens' in u:
        return {'uncached': u['input_tokens'] - u['cached_input_tokens'], 'cached': u['cached_input_tokens'],
                'output': u['output_tokens'], 'cost': 0.0}
    return {'uncached': u.get('input_tokens', 0) + u.get('cache_creation_input_tokens', 0),
            'cached': u.get('cache_read_input_tokens', 0), 'output': u.get('output_tokens', 0),
            'cost': u.get('total_cost_usd', 0.0)}


def main():
    for path in sys.argv[1:]:
        data = json.load(open(path, encoding='utf-8'))
        print(f"## {path} — {data['model']['model']} {data['model']['effort']} (시작 {data['started']})")
        print('| 방식 | 단계 | 상태 | 비캐시 입력 | 캐시 입력 | 출력 | 도구 호출 | 시간(초) | 비용(USD) |')
        print('|---|---|---|---:|---:|---:|---:|---:|---:|')
        sums = {}
        for row in data['rows']:
            t = tokens(row)
            secs = (row['duration_ms'] or 0) / 1000
            print(f"| {row['arm']} | {row['step']} | {row['status']} | {t['uncached']:,} | {t['cached']:,} | {t['output']:,} | "
                  f"{row['tool_calls']} | {secs:.0f} | {t['cost']:.3f} |")
            s = sums.setdefault(row['arm'], {'uncached': 0, 'cached': 0, 'output': 0, 'tools': 0, 'secs': 0.0, 'cost': 0.0})
            s['uncached'] += t['uncached']
            s['cached'] += t['cached']
            s['output'] += t['output']
            s['tools'] += row['tool_calls']
            s['secs'] += secs
            s['cost'] += t['cost']
        for arm, s in sums.items():
            print(f"| **{arm} 합계** | | | {s['uncached']:,} | {s['cached']:,} | {s['output']:,} | {s['tools']} | {s['secs']:.0f} | {s['cost']:.3f} |")
        print()


main()
