"""Shared helpers for the Python orchestrate tools: settings, paths, git, Gitea (tea),
the roundtable HTTP API and the branch → room table.

Settings follow env.sh: ORCH_* environment variables win, then the ORCH_ENV file
(default local/orchestrate.env), then defaults. Python reads only `NAME=value` lines of
that file and expands $VAR/${VAR} (file values so far, ORCH_DIR, HOME, environment);
single-quoted values stay literal.
"""
import json
import os
import posixpath
import re
import subprocess
import urllib.error
import urllib.request
from pathlib import Path

ORCH_DIR = Path(__file__).resolve().parent


def load_env(environ=None):
    env = dict(os.environ if environ is None else environ)
    home = env.get('HOME') or os.path.expanduser('~')
    scope = {'HOME': home, 'ORCH_DIR': ORCH_DIR.as_posix()}
    path = env.get('ORCH_ENV') or str(ORCH_DIR / 'local' / 'orchestrate.env')
    found = {}
    if os.path.isfile(native_path(path)):
        with open(native_path(path), encoding='utf-8') as f:
            for line in f:
                m = re.match(r'\s*(?:export\s+)?([A-Za-z_]\w*)=(.*)$', line.rstrip('\n'))
                if not m:
                    continue
                name, raw = m.group(1), m.group(2).strip()
                if len(raw) >= 2 and raw[0] == raw[-1] == "'":
                    found[name] = raw[1:-1]
                    continue
                if len(raw) >= 2 and raw[0] == raw[-1] == '"':
                    raw = raw[1:-1]
                look = lambda k: found.get(k, scope.get(k, env.get(k, '')))
                found[name] = re.sub(r'\$\{(\w+)\}|\$(\w+)', lambda x: look(x.group(1) or x.group(2)), raw)
    cfg = {k: v for k, v in found.items() if k.startswith('ORCH_')}
    cfg.update({k: v for k, v in env.items() if k.startswith('ORCH_')})
    defaults = {
        'ORCH_RT_URL': 'http://127.0.0.1:18787', 'ORCH_RT_BIN': 'roundtable', 'ORCH_TEA': 'tea',
        'ORCH_BASE': 'main', 'ORCH_REMOTE': 'origin', 'ORCH_STATE': posixpath.join(home.replace('\\', '/'), '.rt-orchestrate'),
        'ORCH_BRANCH_ROOMS': (ORCH_DIR / 'local' / 'branch_rooms.json').as_posix(),
        'ORCH_SCHEMA_REGEX': r'const schemaVersion = (\d+)',
    }
    for k, v in defaults.items():
        if not cfg.get(k):
            cfg[k] = v
    return cfg


def need(cfg, *names):
    missing = [n for n in names if not cfg.get(n)]
    if missing:
        raise SystemExit('%s is not set (environment or ORCH_ENV file)' % ', '.join(missing))


def native_path(p):
    """Git Bash style /d/x → D:/x on Windows; unchanged elsewhere."""
    p = str(p)
    if os.name == 'nt':
        m = re.match(r'^/([A-Za-z])(/|$)(.*)', p)
        if m:
            return '%s:/%s' % (m.group(1).upper(), m.group(3))
    return p


def path_key(p):
    """Comparable form of a folder path: native, forward slashes, no trailing slash,
    case-folded on Windows."""
    s = posixpath.normpath(native_path(p).replace('\\', '/'))
    return s.lower() if os.name == 'nt' else s


def git(repo, *args, check=True, input=None):
    r = subprocess.run(['git', '-C', native_path(repo), *args], capture_output=True, input=input,
                       text=True, encoding='utf-8', errors='replace')
    if check and r.returncode != 0:
        raise SystemExit('git %s failed: %s' % (' '.join(args[:3]), r.stderr.strip()[:500]))
    return r


def read_blobs(repo, specs):
    """{"rev:path": text or None} for many blobs through one `git cat-file --batch`."""
    specs = list(dict.fromkeys(specs))
    if not specs:
        return {}
    r = subprocess.run(['git', '-C', native_path(repo), 'cat-file', '--batch'], capture_output=True,
                       input=('\n'.join(specs) + '\n').encode('utf-8'))
    out, pos, blobs = r.stdout, 0, {}
    for spec in specs:
        end = out.index(b'\n', pos)
        header = out[pos:end].split()
        pos = end + 1
        if len(header) < 3 or header[-1] == b'missing':
            blobs[spec] = None
            continue
        size = int(header[2])
        data = out[pos:pos + size]
        pos += size + 1
        blobs[spec] = None if b'\0' in data[:8000] else data.decode('utf-8', 'replace')
    return blobs


def tea_api(cfg, path):
    r = subprocess.run([cfg['ORCH_TEA'], 'api', path], capture_output=True, text=True, encoding='utf-8')
    if r.returncode != 0:
        raise SystemExit('tea api %s failed: %s' % (path, r.stderr.strip()[:300]))
    try:
        return json.loads(r.stdout)
    except ValueError:
        raise SystemExit('tea api %s: not JSON: %s' % (path, r.stdout[:300]))


def open_pulls(cfg):
    need(cfg, 'ORCH_REPO')
    pulls, page = [], 1
    while True:
        batch = tea_api(cfg, 'repos/%s/pulls?state=open&limit=50&page=%d' % (cfg['ORCH_REPO'], page)) or []
        pulls += batch
        if len(batch) < 50:
            return sorted(pulls, key=lambda p: p['number'])
        page += 1


def rt_http(cfg, method, path, body=None):
    url = cfg['ORCH_RT_URL'].rstrip('/') + path
    data = None if body is None else json.dumps(body).encode('utf-8')
    req = urllib.request.Request(url, data=data, method=method, headers={'Content-Type': 'application/json'})
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            return json.loads(resp.read().decode('utf-8'))
    except (urllib.error.URLError, ValueError) as e:
        raise SystemExit('roundtable %s %s failed: %s' % (method, url, e))


def rooms(cfg):
    """[{id, title, active, ...}] for every room."""
    return rt_http(cfg, 'GET', '/api/rooms')


def room_workspace(cfg, room):
    r = rt_http(cfg, 'GET', '/api/rooms/%s?limit=1' % room)
    return r.get('room', r).get('workspace') or ''


def post_room(cfg, room, text):
    sent = rt_http(cfg, 'POST', '/api/rooms/%s/messages' % room, {'text': text, 'target': ''})
    if not sent.get('seq'):
        raise SystemExit('room %s: post reply has no seq; read the room before resending' % room)
    return sent['seq']


def branch_rooms(cfg):
    """{branch: {"room": id, "worktree": path or ""}} from ORCH_BRANCH_ROOMS (missing file = {}).
    The file maps a branch to a room id, or to {"room": id, "worktree": path}."""
    path = native_path(cfg['ORCH_BRANCH_ROOMS'])
    if not os.path.isfile(path):
        return {}
    with open(path, encoding='utf-8') as f:
        raw = json.load(f)
    out = {}
    for branch, v in raw.items():
        v = {'room': v} if isinstance(v, str) else dict(v)
        if not v.get('room'):
            raise SystemExit('%s: branch %s has no room' % (path, branch))
        out[branch] = {'room': v['room'], 'worktree': v.get('worktree') or ''}
    return out


def branch_worktree(repo, branch):
    """Folder where `branch` is checked out in repo's worktree list, or ''."""
    cur = None
    for line in git(repo, 'worktree', 'list', '--porcelain').stdout.splitlines():
        if line.startswith('worktree '):
            cur = line[len('worktree '):]
        elif line == 'branch refs/heads/' + branch:
            return cur
    return ''


def md_table(headers, rows):
    esc = lambda c: str(c).replace('|', '\\|').replace('\n', ' ')
    lines = ['| ' + ' | '.join(headers) + ' |', '|' + '---|' * len(headers)]
    lines += ['| ' + ' | '.join(esc(c) for c in r) + ' |' for r in rows]
    return '\n'.join(lines)
