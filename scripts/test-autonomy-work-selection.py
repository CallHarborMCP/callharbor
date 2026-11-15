#!/usr/bin/env python3
"""Pure offline scheduler regression fixtures. Does not call GitHub or start agents."""
from datetime import datetime, timezone, timedelta
import importlib.util
import pathlib
import unittest

SCRIPT = pathlib.Path(__file__).with_name('autonomy-work-selection.py')
spec = importlib.util.spec_from_file_location('autonomy_selector', SCRIPT)
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)
NOW = datetime(2026, 11, 15, tzinfo=timezone.utc)


def base():
    return dict(issues=[], prs=[], runs=[dict(id=10, event='push', created_at='2026-11-15T12:00:00Z', status='completed', conclusion='success')], alerts=[], releases=[], actor='builder')


def issue(n=1, state='open', label='release', deps='', status='status:idle', title=None):
    return dict(number=n, state=state, title=title or f'CH-{n:03d}: work', body=f'## Dependencies\n{deps}', labels=[dict(name=label), dict(name=status)])


def pr(n=9, owner='renovate[bot]', body='', branch='renovate/go-1', created_at='2026-11-10T00:00:00Z'):
    return dict(number=n, state='open', title='Update dependency v2', user=dict(login=owner), body=body, head=dict(ref=branch), created_at=created_at, labels=[])


class SchedulerTests(unittest.TestCase):
    def test_idle(self):
        self.assertEqual(m.decision(base(), NOW)['kind'], 'idle')

    def test_unmigrated_legacy_never_idle(self):
        s = base(); i = issue(label='agent:callharbor'); i['body'] += '\n<!-- callharbor-managed:v1 -->'; s['issues'] = [i]
        with self.assertRaises(m.SelectionError): m.decision(s, NOW)

    def test_unowned_and_pr_are_never_owned(self):
        s=base(); s['issues']=[issue(label='community')]
        self.assertEqual(m.decision(s, NOW)['kind'], 'idle')

    def test_ready_issue_and_blocked_dependency(self):
        s=base(); s['issues']=[issue(1,deps='CH-2'),issue(2)]
        self.assertEqual(m.decision(s, NOW)['number'],2)
        s['issues'][1]['state']='closed'
        self.assertEqual(m.decision(s, NOW)['number'],1)

    def test_main_red_beats_security_and_work(self):
        s=base(); s['runs'][0]['conclusion']='failure'; s['issues']=[issue()]
        s['prs']=[pr()]; s['alerts']=[dict(number=88,state='open',security_vulnerability=dict(first_patched_version=dict(identifier='1.3')))]
        self.assertEqual(m.decision(s,NOW)['kind'],'repair-main-ci')

    def test_actionable_security_beats_release_work(self):
        s=base(); s['issues']=[issue()]; s['alerts']=[dict(number=88,state='open',security_vulnerability=dict(first_patched_version=dict(identifier='1.3')))]
        self.assertEqual(m.decision(s,NOW)['kind'],'security-alert')

    def test_aged_dependency_triage(self):
        s=base(); s['prs']=[pr()]
        self.assertEqual(m.decision(s,NOW)['kind'],'triage-dependency')
        s['prs'][0]['triage_comments']=[dict(body='TRIAGE agent=builder time=2026-11-15 decision=blocked target=Update dependency v2',created_at='2026-11-15T00:00:00Z')]
        self.assertEqual(m.decision(s,NOW)['kind'],'idle')
        self.assertEqual(m.decision(s,NOW+timedelta(days=8))['kind'],'triage-dependency')

    def test_own_active_claim_vs_other_worker(self):
        s=base(); i=issue(status='status:in-progress')
        i['claim_comments']=[dict(body='CLAIM: worker, GitHub @builder',created_at='2026-11-15T10:00:00Z')]
        s['issues']=[i]
        self.assertEqual(m.decision(s,NOW)['kind'],'resume-issue')
        i['claim_comments'][0]['body']='CLAIM: worker, GitHub @someoneelse'
        self.assertEqual(m.decision(s,NOW)['kind'],'idle')

    def test_fairness_merged_features(self):
        s=base(); s['issues']=[issue(1)]; b=pr(created_at='2026-11-15T11:00:00Z'); s['prs']=[b]
        for n in (10,11):
            x=pr(n,owner='builder',branch=f'work-{n}',body='Refs #1'); x['state']='closed'; x['merged_at']=f'2026-11-15T1{n-10}:00:00Z'; s['prs'].append(x)
        self.assertEqual(m.decision(s,NOW)['kind'],'dependency-pr')

    def test_release_cadence_requires_verified_runtime_changes(self):
        s=base(); s['releases']=[dict(tag_name='v1.0.0',published_at='2026-10-01T00:00:00Z')]
        self.assertEqual(m.decision(s,NOW)['kind'],'idle')
        s['runtime_changes_verified']=True
        self.assertEqual(m.decision(s,NOW)['kind'],'patch-release')

    def test_release_only_issue_is_ready(self):
        s = base(); i = issue(); i['labels'] = [dict(name='release')]; s['issues'] = [i]
        self.assertEqual(m.decision(s, NOW), dict(kind='claim-issue', number=1, reason='highest priority ready owned issue'))

    def test_release_only_claimed_by_other_is_not_stolen(self):
        s = base(); i = issue(); i['labels'] = [dict(name='release')]
        i['claim_comments'] = [dict(body='CLAIM: worker, GitHub @another', created_at='2026-11-15T10:00:00Z')]
        s['issues'] = [i]
        self.assertEqual(m.decision(s, NOW)['kind'], 'idle')

    def test_release_only_claimed_by_self_is_resumed(self):
        s = base(); i = issue(); i['labels'] = [dict(name='release')]
        i['claim_comments'] = [dict(body='CLAIM: worker, GitHub @builder', created_at='2026-11-15T10:00:00Z')]
        s['issues'] = [i]
        self.assertEqual(m.decision(s, NOW)['kind'], 'resume-issue')

    def test_renovate_when_no_issues(self):
        s = base(); s['prs'] = [pr(created_at='2026-11-15T11:00:00Z')]
        self.assertEqual(m.decision(s, NOW)['kind'], 'dependency-pr')

    def test_renovate_when_all_release_issues_have_unmet_dependencies(self):
        s = base(); i = issue(deps='CH-999'); i['labels'] = [dict(name='release')]
        s['issues'] = [i]
        s['prs'] = [pr(created_at='2026-11-15T11:00:00Z')]
        self.assertEqual(m.decision(s, NOW)['kind'], 'dependency-pr')

    def test_optional_blocked_label_skips_issue(self):
        s = base(); i = issue(); i['labels'] = [dict(name='release'), dict(name='status:blocked')]
        i['updated_at'] = '2026-11-15T11:00:00Z'
        s['issues'] = [i]
        self.assertEqual(m.decision(s, NOW)['kind'], 'idle')

    def test_missing_state_fails_closed(self):
        s=base(); del s['runs']
        with self.assertRaises(m.SelectionError): m.decision(s,NOW)
        s=base(); s['runs']=[]
        with self.assertRaises(m.SelectionError): m.decision(s,NOW)


if __name__=='__main__':
    unittest.main(verbosity=2)
