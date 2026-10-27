"""List one reviewer's activity (issue/PR comments, PR reviews, new issues/PRs) since a time.

usage: reviewer_check.py <since ISO-8601> [--user LOGIN] [--repos owner/a,owner/b]
       reviewer_check.py --approved-open [--user LOGIN] [--repos ...]
Defaults: --user $ORCH_REVIEWER, --repos $ORCH_WATCH_REPOS or $ORCH_REPO. tea is $ORCH_TEA.
Prints "time | repo | kind | number | id | text" per item, oldest first; nothing if quiet.
--approved-open instead lists open PRs whose current head carries the reviewer's latest
verdict APPROVED: a state, not an event, so an approval missed by the watch still shows.
A failed tea call is an error (exit 1), never "no activity".
"""
import argparse
import json
import os
import subprocess
import sys


def parse(argv, env):
    p = argparse.ArgumentParser(prog='reviewer_check.py')
    p.add_argument('since', nargs='?')
    p.add_argument('--approved-open', action='store_true')
    p.add_argument('--user', default=env.get('ORCH_REVIEWER'))
    p.add_argument('--repos', default=env.get('ORCH_WATCH_REPOS') or env.get('ORCH_REPO'))
    a = p.parse_args(argv)
    if not a.since and not a.approved_open:
        p.error('since is required unless --approved-open')
    if not a.user:
        p.error('--user or ORCH_REVIEWER is required')
    if not a.repos:
        p.error('--repos, ORCH_WATCH_REPOS or ORCH_REPO is required')
    a.repos = [r for r in a.repos.split(',') if r]
    if any(r.count('/') != 1 for r in a.repos):
        p.error('repos must be owner/name: %s' % ','.join(a.repos))
    return a


def api(tea, path):
    r = subprocess.run([tea, 'api', path], capture_output=True, text=True, encoding='utf-8')
    if r.returncode != 0:
        raise SystemExit('tea api %s failed: %s' % (path, r.stderr.strip()[:300]))
    try:
        return json.loads(r.stdout) or []
    except ValueError:
        raise SystemExit('tea api %s: not JSON: %s' % (path, r.stdout[:300]))


def activity(a, tea):
    since, user, out = a.since, a.user, []
    flat = lambda s: (s or '')[:400].replace('\n', ' ')
    for repo in a.repos:
        for c in api(tea, f'repos/{repo}/issues/comments?since={since}&limit=50'):
            if c['user']['login'] == user:
                out.append((c['created_at'], repo, 'comment', c['issue_url'].split('/')[-1], c['id'], flat(c['body'])))
        for p in api(tea, f'repos/{repo}/pulls?state=all&sort=recentupdate&limit=15'):
            if p['updated_at'] < since:
                continue
            for rv in api(tea, f'repos/{repo}/pulls/{p["number"]}/reviews'):
                if rv['user']['login'] == user and rv['state'] != 'REQUEST_REVIEW' and (rv.get('submitted_at') or '') >= since:
                    out.append((rv['submitted_at'], repo, 'review:' + rv['state'], p['number'], rv['id'], flat(rv.get('body'))))
        for i in api(tea, f'repos/{repo}/issues?state=all&type=all&since={since}&limit=30'):
            if i['user']['login'] == user and i['created_at'] >= since:
                out.append((i['created_at'], repo, 'new-' + ('pr' if i.get('pull_request') else 'issue'), i['number'], 0, i['title']))
    return sorted(out, key=lambda o: o[0])


def approved_open(repos, user, get):
    """Open PRs whose latest APPROVED/REQUEST_CHANGES review by user is APPROVED on the head."""
    out = []
    for repo in repos:
        for p in get(f'repos/{repo}/pulls?state=open&limit=50'):
            verdicts = [rv for rv in get(f'repos/{repo}/pulls/{p["number"]}/reviews')
                        if rv['user']['login'] == user and rv['state'] in ('APPROVED', 'REQUEST_CHANGES')]
            last = verdicts[-1] if verdicts else None
            if last and last['state'] == 'APPROVED' and last['commit_id'] == p['head']['sha']:
                out.append((last['submitted_at'], repo, 'approved-open', p['number'], last['id'], p['title']))
    return sorted(out, key=lambda o: o[0])


def main(argv):
    a = parse(argv, os.environ)
    tea = os.environ.get('ORCH_TEA') or 'tea'
    items = approved_open(a.repos, a.user, lambda path: api(tea, path)) if a.approved_open else activity(a, tea)
    for o in items:
        print(*o, sep=' | ')


if __name__ == '__main__':
    main(sys.argv[1:])
