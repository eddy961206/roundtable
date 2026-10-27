"""Refuse to send work into a worktree another running roundtable room is using.

usage:
  worktree_claims.py check (--worktree DIR | --branch NAME) [--room ID]
  worktree_claims.py claim DIR --room ID [--note TEXT] [--force]
  worktree_claims.py release DIR --room ID [--force]
  worktree_claims.py list
exit: 0 free / done, 1 occupied / refused, 2 usage or setting error.

A room occupies DIR while it is active (running or queued turn) and any of these ties it
to DIR: its workspace is DIR; ORCH_BRANCH_ROOMS maps one of its branches to worktree DIR;
it holds a claim on DIR (ORCH_CLAIMS, default $ORCH_STATE/claims.json). Claims exist for
rooms whose workspace is a parent folder, where the path alone says nothing. --room is the
room about to receive the work; it never blocks itself.
--branch finds DIR in ORCH_REPO_DIR's worktree list, or, without ORCH_REPO_DIR, uses the
--room's workspace.
"""
import argparse
import datetime
import json
import os
import posixpath
import sys

import orchlib


def claims_path(cfg):
    return orchlib.native_path(cfg.get('ORCH_CLAIMS') or posixpath.join(cfg['ORCH_STATE'], 'claims.json'))


def load_claims(cfg):
    p = claims_path(cfg)
    if not os.path.isfile(p):
        return {}
    with open(p, encoding='utf-8') as f:
        return json.load(f)


def save_claims(cfg, claims):
    p = claims_path(cfg)
    os.makedirs(os.path.dirname(p) or '.', exist_ok=True)
    tmp = p + '.tmp'
    with open(tmp, 'w', encoding='utf-8') as f:
        json.dump(claims, f, ensure_ascii=False, indent=2)
    os.replace(tmp, p)


def occupants(cfg, worktree, exclude=''):
    """[(room, title, [reasons])] of active rooms other than `exclude` tied to worktree."""
    key = orchlib.path_key(worktree)
    active = {r['id']: r.get('title', '') for r in orchlib.rooms(cfg) if r.get('active') and r['id'] != exclude}
    found = {}
    add = lambda room, why: found.setdefault(room, []).append(why)
    for room in active:
        ws = orchlib.room_workspace(cfg, room)
        if ws and orchlib.path_key(ws) == key:
            add(room, 'workspace')
    for branch, e in orchlib.branch_rooms(cfg).items():
        if e['worktree'] and orchlib.path_key(e['worktree']) == key and e['room'] in active:
            add(e['room'], 'branch_rooms ' + branch)
    c = load_claims(cfg).get(key)
    if c and c['room'] in active:
        add(c['room'], 'claim')
    return [(room, active[room], why) for room, why in found.items()]


def resolve(cfg, a):
    if a.worktree:
        return a.worktree
    if cfg.get('ORCH_REPO_DIR'):
        wt = orchlib.branch_worktree(cfg['ORCH_REPO_DIR'], a.branch)
        if not wt:
            raise SystemExit('branch %s is not checked out in any worktree of %s' % (a.branch, cfg['ORCH_REPO_DIR']))
        return wt
    if not a.room:
        raise SystemExit('--branch needs ORCH_REPO_DIR or --room')
    return orchlib.room_workspace(cfg, a.room)


def report_busy(worktree, busy):
    for room, title, why in busy:
        print('BUSY %s: room %s (%s) is running there [%s]' % (worktree, room, title, ', '.join(why)))


def main(argv, cfg=None):
    cfg = cfg or orchlib.load_env()
    p = argparse.ArgumentParser(prog='worktree_claims.py')
    sub = p.add_subparsers(dest='cmd', required=True)
    c = sub.add_parser('check')
    g = c.add_mutually_exclusive_group(required=True)
    g.add_argument('--worktree')
    g.add_argument('--branch')
    c.add_argument('--room', default='')
    for name in ('claim', 'release'):
        s = sub.add_parser(name)
        s.add_argument('worktree')
        s.add_argument('--room', required=True)
        s.add_argument('--force', action='store_true')
        if name == 'claim':
            s.add_argument('--note', default='')
    sub.add_parser('list')
    a = p.parse_args(argv)

    if a.cmd == 'list':
        for key, v in sorted(load_claims(cfg).items()):
            print('%s | room %s | %s | %s' % (v['worktree'], v['room'], v.get('at', ''), v.get('note', '')))
        return 0
    if a.cmd == 'check':
        wt = resolve(cfg, a)
        busy = occupants(cfg, wt, a.room)
        report_busy(wt, busy)
        if not busy:
            print('FREE %s' % wt)
        return 1 if busy else 0

    key = orchlib.path_key(a.worktree)
    claims = load_claims(cfg)
    held = claims.get(key)
    if held and held['room'] != a.room and not a.force:
        print('REFUSED %s: claimed by room %s at %s (%s)' % (a.worktree, held['room'], held.get('at', ''), held.get('note', '')))
        return 1
    if a.cmd == 'release':
        if not held:
            print('no claim on %s' % a.worktree)
            return 0
        del claims[key]
        save_claims(cfg, claims)
        print('released %s (was room %s)' % (a.worktree, held['room']))
        return 0
    busy = occupants(cfg, a.worktree, a.room)
    if busy and not a.force:
        report_busy(a.worktree, busy)
        return 1
    claims[key] = {'worktree': a.worktree, 'room': a.room, 'note': a.note,
                   'at': datetime.datetime.now(datetime.timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ')}
    save_claims(cfg, claims)
    print('claimed %s for room %s' % (a.worktree, a.room))
    return 0


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
