"""Per-task model calls of Codex rollouts: where a heavy turn's input goes.

usage: codex_rollout.py ROLLOUT.jsonl ...
       codex_rollout.py --session SESSION_ID ...   (finds ~/.codex/sessions/**/rollout-*ID.jsonl)

For each task (one `codex exec` turn) prints the number of model calls, the
input of the first/last/largest call, the summed input and how it splits by
the tool call that preceded each model call: wait (empty write_stdin poll),
edit (apply_patch), test (go test/vet, pytest, node --test), other.
Each model call re-reads the whole context, so calls x context is the cost.
"""
import collections, glob, json, os, re, sys

KINDS = ('wait', 'edit', 'test', 'other', 'answer')


def kind(src):
    if 'write_stdin' in src and re.search(r'chars:\s*""', src):
        return 'wait'
    if 'apply_patch' in src:
        return 'edit'
    if re.search(r'go test|go vet|pytest|node --test|npm test', src):
        return 'test'
    return 'other'


def tasks(path):
    out, cur, pending = [], None, None
    for line in open(path, encoding='utf-8'):
        try:
            e = json.loads(line)
        except ValueError:
            continue
        p = e.get('payload') or {}
        if p.get('type') == 'task_started':
            cur = {'calls': [], 'compacted': 0}
            out.append(cur)
        elif cur is None:
            continue
        elif e.get('type') == 'compacted':
            cur['compacted'] += 1
        elif p.get('type') == 'custom_tool_call':
            pending = kind(p.get('input', ''))
        elif p.get('type') == 'token_count' and p.get('info'):
            inp = (p['info'].get('last_token_usage') or {}).get('input_tokens', 0)
            cur['calls'].append((pending or 'answer', inp))
            pending = None
    return out


def main(args):
    paths = []
    if args and args[0] == '--session':
        for sid in args[1:]:
            paths += glob.glob(os.path.join(os.path.expanduser('~'), '.codex', 'sessions', '*', '*', '*', f'rollout-*{sid}.jsonl'))
    else:
        paths = args
    for path in paths:
        print(os.path.basename(path))
        for i, t in enumerate(tasks(path)):
            ins = [c[1] for c in t['calls']]
            if not ins:
                continue
            by = collections.Counter()
            for k, n in t['calls']:
                by[k] += n
            split = ' '.join(f'{k} {by[k] / sum(ins):.0%}' for k in KINDS if by[k])
            print(f'  task{i}: calls={len(ins)} start={ins[0]} end={ins[-1]} max={max(ins)} '
                  f'sum={sum(ins) / 1e6:.2f}M compacted={t["compacted"]} | {split}')


if __name__ == '__main__':
    main(sys.argv[1:])
