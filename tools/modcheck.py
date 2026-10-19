"""Find web modules that use a name defined top-level in another module without
importing it (the bug class that broke boot after the app.js split).

Usage: python tools/modcheck.py web/js   -> prints "problems N"; exit 1 if N > 0.
Limits: a rough token scan (comments and strings stripped). It only knows
top-level function/const/let/var names, so it cannot see names created other
ways, and a local variable that shadows another module's name hides a miss."""
import os
import re
import sys

root = sys.argv[1]
files = {f: open(os.path.join(root, f), encoding='utf-8').read() for f in os.listdir(root) if f.endswith('.js')}


def strip(src):
    src = re.sub(r'/\*.*?\*/', ' ', src, flags=re.S)
    src = re.sub(r'//[^\n]*', ' ', src)
    src = re.sub(r'`(?:\\.|[^`\\])*`', '""', src, flags=re.S)
    src = re.sub(r'"(?:\\.|[^"\\\n])*"', '""', src)
    src = re.sub(r"'(?:\\.|[^'\\\n])*'", "''", src)
    return src


top = {}
for f, s in files.items():
    names = set(re.findall(r'^(?:export\s+)?(?:async\s+)?function\s+([A-Za-z_$][\w$]*)', s, re.M))
    names |= set(re.findall(r'^(?:export\s+)?(?:const|let|var)\s+([A-Za-z_$][\w$]*)', s, re.M))
    top[f] = names

problems = 0
for f, s in files.items():
    imported = set()
    for m in re.finditer(r'import\s*\{([^}]*)\}\s*from', s):
        imported |= {x.strip().split(' as ')[-1] for x in m.group(1).split(',') if x.strip()}
    local = set(re.findall(r'\b(?:function|const|let|var|class)\s+([A-Za-z_$][\w$]*)', s))
    body = strip(s)
    tokens = set(re.findall(r'(?<![\w$.])([A-Za-z_$][\w$]*)\b(?!\s*:)', body))
    for other, names in top.items():
        if other == f:
            continue
        for n in names & tokens:
            if n not in imported and n not in local:
                print(f'{f}: uses {n} (defined in {other}) without import')
                problems += 1
print('problems', problems)
sys.exit(1 if problems else 0)
