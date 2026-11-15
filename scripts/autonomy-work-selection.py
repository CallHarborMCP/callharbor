#!/usr/bin/env python3
"""Version-neutral, read-only work selection. Live state is from GitHub; no local backlog."""
import argparse
from datetime import datetime, timezone, timedelta
import json
import os
import re
import subprocess
import sys

REPO = os.environ.get('CALLHARBOR_REPO', 'CallHarborMCP/callharbor')
MAX_OBJECTS = 10000
CALL_TIMEOUT = 240


class SelectionError(Exception):
    pass


def gh(*args):
    cmd = ['gh', *args]
    try:
        p = subprocess.run(cmd, capture_output=True, text=True, timeout=CALL_TIMEOUT, check=False)
    except (OSError, subprocess.TimeoutExpired) as e:
        raise SelectionError(f'GitHub query unavailable: {e}') from e
    if p.returncode:
        raise SelectionError(f'GitHub query failed (exit {p.returncode}): {p.stderr[:500].strip()}')
    try:
        return json.loads(p.stdout)
    except ValueError as e:
        raise SelectionError('GitHub returned invalid JSON') from e


def pages(route):
    data = gh('api', '--paginate', '--slurp', '-X', 'GET', route)
    if not isinstance(data, list):
        raise SelectionError(f'Invalid collection for {route}')
    items = []
    for page in data:
        if not isinstance(page, list):
            raise SelectionError(f'Invalid GitHub page for {route}')
        items.extend(page)
        if len(items) > MAX_OBJECTS:
            raise SelectionError(f'Excessive pagination for {route}; refusing truncated data')
    if not all(isinstance(x, dict) for x in items):
        raise SelectionError(f'Invalid item for {route}')
    return items


def collect(repo):
    issues = pages(f'repos/{repo}/issues?state=all&per_page=100')
    pulls = pages(f'repos/{repo}/pulls?state=all&per_page=100')
    # Issue endpoint includes pull requests. Exclude them from work ownership.
    issues = [i for i in issues if 'pull_request' not in i]
    actor = gh('api', 'user').get('login')
    if not actor:
        raise SelectionError('Cannot identify authenticated worker')
    for i in issues:
        if 'release' in labels(i) and i.get('state') == 'open':
            # GitHub's issue.comments is a count. Fetch claims for any issue with
            # comments (including release-only issues), not only status-labelled ones.
            # If count is unexpectedly missing, fetch to fail safely rather than
            # assume nobody claimed the work.
            count = i.get('comments')
            if count is None or not isinstance(count, int) or count > 0 or 'status:in-progress' in labels(i):
                i['claim_comments'] = pages(f'repos/{repo}/issues/{i["number"]}/comments?per_page=100')
    for p in pulls:
        if p.get('state') == 'open' and is_dependency(p):
            p['triage_comments'] = pages(f'repos/{repo}/issues/{p["number"]}/comments?per_page=100')
    runs = gh('api', '-X', 'GET', f'repos/{repo}/actions/workflows/required.yml/runs?branch=main&event=push&per_page=100')
    if not isinstance(runs, dict) or not isinstance(runs.get('workflow_runs'), list):
        raise SelectionError('Cannot establish required main CI state')
    alerts = gh('api', '-X', 'GET', f'repos/{repo}/dependabot/alerts?state=open&per_page=100')
    if not isinstance(alerts, list):
        raise SelectionError('Cannot inspect security alerts')
    if len(alerts) == 100:
        raise SelectionError('Security alerts may be truncated')
    releases = pages(f'repos/{repo}/releases?per_page=100')
    runtime_changes_verified = False
    latest_release = next((r for r in releases if not r.get('draft') and not r.get('prerelease') and r.get('tag_name')), None)
    if latest_release:
        tag = latest_release['tag_name']
        if not re.fullmatch(r'[A-Za-z0-9._-]+', tag):
            raise SelectionError('Unexpected release tag')
        comparison = gh('api', '-X', 'GET', f'repos/{repo}/compare/{tag}...main?per_page=100')
        changed = comparison.get('files')
        if not isinstance(changed, list) or len(changed) >= 300:
            raise SelectionError('Cannot prove complete runtime change classification')
        runtime_changes_verified = any(
            f.get('filename', '').startswith(('internal/', 'cmd/', 'pkg/', 'web/src/', 'backend/', 'frontend/src/'))
            or f.get('filename', '') in ('go.mod', 'go.sum', 'Dockerfile', 'compose.yaml', 'api/openapi.yaml')
            for f in changed if isinstance(f, dict)
        )
    # Complete merged PR history is retained to calculate deterministic dependency fairness.
    for p in pulls:
        if p.get('state') == 'closed' and p.get('merged_at'):
            p['mergedAt'] = p['merged_at']
    return dict(issues=issues, prs=pulls, runs=runs['workflow_runs'], alerts=alerts,
                releases=releases, actor=actor, runtime_changes_verified=runtime_changes_verified)


def labels(item):
    return {x.get('name', '') if isinstance(x, dict) else x for x in (item.get('labels') or [])}


def login(item):
    user = item.get('user') or item.get('author') or {}
    return (user.get('login') or '') if isinstance(user, dict) else str(user)


def is_dependency(pr):
    author = login(pr).lower()
    head = pr.get('head') or {}
    branch = head.get('ref') or pr.get('headRefName') or ''
    return author in ('renovate[bot]', 'renovate', 'app/renovate', 'dependabot[bot]') or branch.startswith(('renovate/', 'dependabot/')) or bool({'dependencies', 'renovate'} & labels(pr))


def parse_date(s):
    if not s:
        return None
    try:
        return datetime.fromisoformat(s.replace('Z', '+00:00')).astimezone(timezone.utc)
    except (ValueError, TypeError):
        return None


def owned(issue):
    return issue.get('state') == 'open' and 'release' in labels(issue)


def refs(pr):
    return {int(n) for n in re.findall(r'\bRefs\s+#(\d+)\b', pr.get('body') or '', re.I)}


def dependencies(issue):
    body = issue.get('body') or ''
    m = re.search(r'^## Dependencies\s*\n(.*?)(?:\n## |\Z)', body, re.M | re.S)
    segment = m.group(1) if m else ''
    dep = re.search(r'^Blocked by:\s*(.*)$', body, re.M)
    if dep:
        segment += ' ' + dep.group(1)
    return {int(n) for n in re.findall(r'(?:CH-|#)(\d+)', segment)}


def issue_rank(i):
    ps = [int(x.split('p')[-1]) for x in labels(i) if re.fullmatch(r'priority:p\d+', x)]
    priority = min(ps) if ps else 5
    num = int(i['number'])
    coordinator = bool(re.search(r'\b(?:release|completion gate|final v\d+)\b', i.get('title', ''), re.I) and 'area:release' in labels(i))
    return (int(coordinator), priority, num)


def first_claim(issue):
    """Replay claims using GitHub's server timestamp and comment ID.

    A claimant may explicitly release its own claim. Later competing claims
    never become owners implicitly: a fresh claim is required after release.
    """
    comments = issue.get('claim_comments', [])
    if not isinstance(comments, list):
        raise SelectionError('Invalid claim comments')
    events = []
    for c in comments:
        body = c.get('body') or ''
        claim = re.search(r'^CLAIM:\s*.+?GitHub\s+@([\w-]+)', body, re.M | re.I)
        release = re.search(r'^RELEASE:\s*GitHub\s+@([\w-]+)', body, re.M | re.I)
        match = claim or release
        if not match:
            continue
        user = match.group(1).lower()
        author = ((c.get('user') or {}).get('login') or '').lower()
        if author and author != user:
            continue
        events.append((c.get('created_at', ''), int(c.get('id') or 0),
                       'claim' if claim else 'release', user))
    events.sort()
    owner = None
    for _, _, action, user in events:
        if action == 'claim' and owner is None:
            owner = user
        elif action == 'release' and owner == user:
            owner = None
    return owner


def claimed_by(issue, actor):
    return first_claim(issue) == actor.lower()


def triage_state(pr, now):
    for c in reversed(sorted(pr.get('triage_comments', []), key=lambda c: c.get('created_at', ''))):
        first = (c.get('body') or '').splitlines()[:1]
        if not first:
            continue
        m = re.match(r'TRIAGE\s+.*\bdecision=(actionable|blocked)\s+target=(.*)$', first[0])
        if not m:
            continue
        if m[2] != (pr.get('title') or ''):
            return 'untriaged'
        dt = parse_date(c.get('created_at'))
        if m[1] == 'blocked' and (dt is None or now - dt >= timedelta(days=7)):
            return 'untriaged'
        return m[1]
    return 'untriaged'


def decision(snapshot, now):
    for key in ('issues', 'prs', 'runs', 'alerts', 'releases', 'actor'):
        if key not in snapshot:
            raise SelectionError(f'Missing snapshot key: {key}')
    issues, prs = snapshot['issues'], snapshot['prs']
    if not all(isinstance(snapshot[x], list) for x in ('issues', 'prs', 'runs', 'alerts', 'releases')):
        raise SelectionError('Invalid snapshot collections')
    actor = snapshot['actor']
    if not isinstance(actor, str) or not actor:
        raise SelectionError('Missing actor')
    by_num = {int(x['number']): x for x in issues}
    unmigrated = [i['number'] for i in issues if i.get('state') == 'open' and
                  'agent:callharbor' in labels(i) and
                  '<!-- callharbor-managed:v1 -->' in (i.get('body') or '') and
                  'release' not in labels(i)]
    if unmigrated:
        raise SelectionError(f'Owner label cutover not complete for legacy Issues: {unmigrated[:10]}')
    owned_issues = {n: i for n, i in by_num.items() if 'release' in labels(i)}
    open_owned = [i for i in owned_issues.values() if owned(i)]
    for i in open_owned:
        states = labels(i) & {'status:idle', 'status:in-progress', 'status:blocked'}
        if len(states) != 1:
            raise SelectionError(f'Issue #{i["number"]} must have exactly one status label: {sorted(states)}')
    open_prs = [p for p in prs if p.get('state') == 'open']

    runs = [r for r in snapshot['runs'] if r.get('event') in (None, 'push')]
    if not runs:
        raise SelectionError('No required main push CI run found; cannot assume green')
    runs.sort(key=lambda r: r.get('created_at', ''), reverse=True)
    latest = runs[0]
    if latest.get('status') == 'completed' and latest.get('conclusion') not in ('success', 'skipped'):
        return dict(kind='repair-main-ci', reason=f'latest required run #{latest.get("id")} is {latest.get("conclusion")}')
    if latest.get('status') != 'completed':
        return dict(kind='idle', reason='required main CI still running; wait')

    for p in open_prs:
        if 'security' in labels(p):
            return dict(kind='security-pr', number=p['number'], reason='security-labeled PR')
    for a in snapshot['alerts']:
        if a.get('state') == 'open' and (a.get('security_vulnerability') or {}).get('first_patched_version'):
            return dict(kind='security-alert', number=a.get('number'), reason='patched security advisory')

    for p in open_prs:
        if login(p).lower() == actor.lower() and (is_dependency(p) or refs(p) & owned_issues.keys() or (p.get('head') or {}).get('ref', '').startswith('release/v')):
            return dict(kind='resume-pr', number=p['number'], reason='locally owned unfinished PR')
    for i in sorted(open_owned, key=issue_rank):
        if 'status:in-progress' in labels(i) and claimed_by(i, actor):
            return dict(kind='resume-issue', number=i['number'], reason='previous claim')

    renovate = [p for p in open_prs if is_dependency(p)]
    older = [p for p in renovate if triage_state(p, now) == 'untriaged' and
             (parse_date(p.get('created_at')) or now) < now - timedelta(hours=48)]
    if older:
        oldest = min(older, key=lambda p: p.get('created_at', ''))
        return dict(kind='triage-dependency', number=oldest['number'], reason='aged untriaged dependency PR')

    merged = sorted((p for p in prs if p.get('mergedAt') or p.get('merged_at')),
                    key=lambda p: p.get('mergedAt') or p.get('merged_at'))
    features = 0
    for p in merged:
        if is_dependency(p):
            features = 0
        elif refs(p) & owned_issues.keys():
            features += 1
    actionable = [p for p in renovate if triage_state(p, now) != 'blocked']
    actionable.sort(key=lambda p: p.get('created_at', ''))
    if features >= 2 and actionable:
        return dict(kind='dependency-pr', number=actionable[0]['number'], reason='fairness slot')

    ready = []
    referenced_by_open_pr = set().union(*(refs(p) for p in open_prs)) if open_prs else set()
    for i in open_owned:
        st = labels(i)
        if ('status:idle' not in st or first_claim(i) is not None
                or i['number'] in referenced_by_open_pr):
            continue
        deps = dependencies(i)
        if all((dep in by_num and by_num[dep].get('state') == 'closed') for dep in deps):
            ready.append(i)
    if ready:
        top = min(ready, key=issue_rank)
        return dict(kind='claim-issue', number=top['number'], reason='highest priority ready owned issue')
    if actionable:
        return dict(kind='dependency-pr', number=actionable[0]['number'], reason='remaining dependency update')

    # No automatic release if the current product has unresolved owned release work.
    if not open_owned and not open_prs:
        released = [r for r in snapshot['releases'] if not r.get('draft') and not r.get('prerelease')]
        dates = [parse_date(r.get('published_at')) for r in released]
        dates = [x for x in dates if x]
        if dates and now - max(dates) >= timedelta(days=14) and snapshot.get('runtime_changes_verified') is True:
            return dict(kind='patch-release', reason='14-day window, runtime changes verified')
    if open_owned and not ready:
        candidates = [i for i in open_owned if i['number'] not in referenced_by_open_pr and
                      ('status:blocked' in labels(i) or
                       ('status:idle' in labels(i) and first_claim(i) is None))]
        old = [i for i in candidates if (parse_date(i.get('updated_at')) or now - timedelta(days=8)) < now - timedelta(days=7)]
        if old:
            oldest = min(old, key=lambda i: i.get('updated_at') or '')
            return dict(kind='investigate-blocker', number=oldest['number'], reason='bounded deadlock/external-blocker re-evaluation')
        return dict(kind='idle', reason='no ready work; bounded re-evaluation of blockers later')
    return dict(kind='idle', reason='no actionable work')


def main():
    p = argparse.ArgumentParser()
    p.add_argument('--repo', default=REPO)
    p.add_argument('--snapshot', help='JSON fixture file (no GitHub calls)')
    p.add_argument('--now', help='UTC ISO-8601; for deterministic tests')
    args = p.parse_args()
    now = parse_date(args.now) if args.now else datetime.now(timezone.utc)
    if now is None:
        raise SelectionError('Invalid --now')
    data = json.load(open(args.snapshot)) if args.snapshot else collect(args.repo)
    print(json.dumps(decision(data, now), ensure_ascii=False, sort_keys=True))


if __name__ == '__main__':
    try:
        main()
    except (SelectionError, ValueError, KeyError, TypeError) as e:
        print(f'SELECTION ERROR: {e}', file=sys.stderr)
        sys.exit(2)
