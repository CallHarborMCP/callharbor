#!/usr/bin/env python3
"""Seed complete v1.0.0 GitHub milestone + scoped issues; safe to rerun.
No local state database or coordinator. GitHub Issues are the sole work state.
"""
import json
import os
import re
import subprocess
import sys

REPO = os.environ.get('CALLHARBOR_FULL_REPO', 'CallHarborMCP/callharbor')
MILESTONE = 'v1.0.0'
MANAGED_LABEL = 'agent:callharbor'
MANAGED_MARKER = '<!-- callharbor-managed:v1 -->'

# ID, title, area, priority, dependencies, acceptance (each is a real verification goal)
TASKS = [
('CH-001','Verify locked Telnyx/realtime protocol contracts and test fixtures','testing','p0',[],
 'Architecture, runtime, auth, storage, Telnyx and OpenAI-compatible Realtime provider are ALREADY DECIDED in .autonomous/ARCHITECTURE.md and .autonomous/PRODUCT_SCOPE.md. Do NOT redesign or evaluate alternative stacks. FIRST satisfy locked .autonomous/PROTOCOL_FEASIBILITY.md with runnable offline compatibility/negative tests and a sourced capability matrix; use this as a hard blocker for dependent protocol features. Produce concrete protocol compatibility evidence: extract official Telnyx Call Control API schemas (request/response + signed webhook), bidirectional streaming events/codecs/handshake, Realtime provider session events/interruptions/transcription capabilities, and generate real contract fixtures and automated negative tests. Identify exact unresolved upstream ambiguities with source links and reproduction, not speculative architectural planning. Explicitly validate dual-party transcription and actually-played audio after interruptions; if any required capability is unavailable, report a real blocker, do NOT infer transcript from model intent and do NOT add a paid STT fallback. Record implementation decisions only when a genuine standards-level ambiguity emerges. No real Telnyx calls or paid AI requests.'),
('CH-002','Go modular monolith skeleton, configuration and observability','backend','p0',[],
 'Go service starts cleanly via Docker, exposes health/readiness and embedded asset route, structured secret-safe logs, safe config validation/shutdown, no external DB; meaningful unit tests. Use standard Go modules and match GitHub CI without changing global host tooling.'),
('CH-003','SQLite WAL schema, migrations, durability and data model','storage','p0',['CH-002'],
 'Define tenant-safe users, profiles, call briefs/events/outcomes/transcripts, retention, recordings, secrets and audit tables; transaction/restart/wal backup/migration+rollback tests and indexes.'),
('CH-004','Identity bootstrap, local users and admin/user RBAC','identity','p0',['CH-003'],
 'Secure first-admin wizard, login/logout/session expiry, safe password KDF/CSRF/brute force controls, user lifecycle and admin role grants; tests for two users + multiple admins and IDOR denies.'),
('CH-005','Optional external OpenID Connect login and account mapping','identity','p1',['CH-004'],
 'Multiple supported OIDC issuers/config, callback/nonce/state and issuer+sub identity, safe linking, no email-based hijack, logout and disabled accounts; PocketID-compatible but not dependent upon it.'),
('CH-006','Built-in OAuth2.1 authorization server for remote MCP','identity','p0',['CH-004'],
 'Trusted library with Authorization Code/PKCE-S256, discovery/AS+resource metadata, audience/scope, client identity and registration/metadata, token refresh/revoke, consent/login; protocol integration tests and OAuth attack negatives.'),
('CH-007','REST and MCP shared typed authorization/application layer','api','p0',['CH-003','CH-004','CH-006'],
 'Versioned REST OpenAPI + OAuth-protected Streamable HTTP MCP, stable validation/error schema, pagination, role-aware context from auth not caller input; no stubbed responses for production tools.'),
('CH-008','Telnyx account profile CRUD and credential validation','telephony','p0',['CH-003','CH-004'],
 'Per-user keys, Connection/Call Control application, numbers and webhook verification key; secure encryption, read-only Telnyx credentials check, shared grant rules; no sensitive data response/log.'),
('CH-009','Telnyx Call Control adapter + outbound state machine','telephony','p0',['CH-001','CH-003','CH-008'],
 'Start outbound via actual Telnyx REST contract, correct IDs/headers/call legs, signed webhooks verified on raw bytes and deduplicated, ring/answer/busy/noanswer/hangup state, no accidental double dial; official OpenAPI contract tests.'),
('CH-010','Telnyx bidirectional media WebSocket adapter and codecs','audio','p0',['CH-001','CH-009'],
 'Authenticated stream setup, connected/start/media/stop support, PCM/PCMU/L16 sample rates/endian/timing, jitter/loss/bounded queues, correct media playout frame semantics, failure handling; independent test media harness.'),
('CH-011','OpenAI-compatible realtime provider/voice session adapter','audio','p0',['CH-001','CH-003'],
 'Per-user configurable realtime WebSocket endpoint/model/headers/auth/voice; exact session event handling, speech/audio stream, errors and disconnects, no hard-coded vendor gateway or secondary paid AI API; protocol emulator tests.'),
('CH-012','Full-duplex conversational engine with barge-in','audio','p0',['CH-010','CH-011'],
 'Interruption stops speaker output quickly, clears Telnyx playback queue, truncates prior model response accurately, preserves ongoing context, supports natural multi-turn dialog, caller silence/voicemail and DTMF where available; automated audio trace assertions.'),
('CH-013','Call command, long-context brief and retry idempotency','calls','p0',['CH-007','CH-009','CH-011'],
 'Implement make_call with recipient/objective/long context/instructions/constraints/fallbacks/success criteria/max duration/recording/idempotency key, version immutable brief and async return; validate authorization, never silently truncate, enforce safe limits.'),
('CH-014','Call safety guards, timeouts, concurrency and recovery','calls','p0',['CH-009','CH-012','CH-013'],
 'Configurable rate/concurrency/duration/cost guards, emergency hangup, media/provider failures, process crash/restart reconciliation, deduplicated transitions, no lingering billable calls; deterministic fault tests.'),
('CH-015','Structured technical outcome vs task result + provenance','calls','p0',['CH-012','CH-013'],
 'Technical status separate from success/partial/failed/unknown; inferred task outcome evidence-backed and no fabricated booking confirmation, unknown on insufficient evidence, status and history in MCP/REST; end-to-end acceptance cases.'),
('CH-016','Two-party timed transcripts and event/audit pipeline','recording','p0',['CH-001','CH-012','CH-003'],
 'Persist speaker-tagged timed transcript, correction/finality/partial quality and raw provider events securely; audit access and changes; assert audible/playback-truncated outgoing transcription and independently sourced incoming approximate transcription; preserve original provider event/turn IDs, uncertainty and quality flags. Prove actual provider event coverage or block if faithful two-party transcript impossible without an extra paid API; never invent verbatim speech.'),
('CH-017','Streaming local dual-channel WAV recorder and media API','recording','p0',['CH-010','CH-003','CH-004'],
 'Correct two-party audio timing based on incoming samples and actually played outgoing frames (not generated-but-cleared audio), incremental crash-safe file, partial recording flags, secure encrypted storage policy, authenticated HTTP range/seek/download, no memory buffering whole calls; actual playback and concurrency tests.'),
('CH-018','Retention and cleanup, backup/restore with encryption keys','storage','p0',['CH-017','CH-016'],
 'Recording disabled for every new user and migration, configurable OFF/ON in WebGUI with mandatory warning acknowledgement before enabling; REST/MCP cannot override OFF with record=true. Retention independently managed for recording/transcripts/audit with admin guardrails; deletion from files/backups, usage monitoring and verified restore incl WAL and encryption.'),
('CH-019','Frontend design system + appearance settings','frontend','p0',['CH-002','CH-004'],
 'React/TS/Tailwind+accessible mature components, original Apple-like design, light/dark/system, accents, 2+ presets, density stored per user; English-only GUI copy everywhere, no language selector/i18n; user-call conversation languages remain configurable. FIRST implement and independently compare TWO genuinely different interactive design prototypes with phone+desktop screenshots, document direction review and real design system. Responsive 320/360/390/768/1024/1440px, WCAG 2.2 AA-oriented checks, browser screenshots and theme tests. Commit npm lockfile and align Node/Playwright dependencies with GitHub CI.'),
('CH-020','Responsive login/installation wizard/profile settings','frontend','p0',['CH-019','CH-005','CH-008','CH-011'],
 'First-run guided setup, login, local/OIDC, per-user Telnyx and AI profiles, secure credential status and validation, appearance settings, recording toggle default OFF and accessible legal-warning acknowledgement before enabling; ALL visible GUI and accessibility strings English-only; production-built browser E2E mobile/tablet/desktop.'),
('CH-021','Responsive dashboard and call history','frontend','p1',['CH-019','CH-007','CH-015'],
 'Accessible UI with filters/pagination, states, success statistics and provider cost availability distinctions, security isolation, mobile-friendly list and desktop table, empty/error/loading screenshot coverage.'),
('CH-022','Responsive call detail, timed transcript and recording player','frontend','p0',['CH-019','CH-015','CH-016','CH-017'],
 'Immutable full prompt view, correlated event timeline, transcript, technical/task outcomes, partials, inline audio playback with seeking and pause, download; cross-user and mobile E2E tests.'),
('CH-023','Admin UI for users, roles, policies, diagnostics and audit','frontend','p0',['CH-019','CH-004','CH-018'],
 'Admin multiuser management and role changes, global defaults/limits/retention, auditable cross-user read, status diagnostics, clean responsive UI; non-admin cannot reach data via API or navigation.'),
('CH-024','Telnyx-independent contract/media integration harness','testing','p0',['CH-009','CH-010','CH-011'],
 'Use CH-001 acceptance evidence and pinned official Telnyx OpenAPI+Prism contract plus independent signed-webhook and WebSocket media fixture server based on upstream OSS; deterministic real frame/audio assertions for codecs, barge-in, DTMF, errors. No paid Telnyx/API calls.'),
('CH-025','Docker Compose end-to-end contract suite and chaos tests','testing','p0',['CH-024','CH-014','CH-017','CH-006'],
 'Launch production app container with test-only Telnyx and realtime servers, OAuth-protected actual MCP make_call, full audio/recording/outcome assertions, concurrent/fault/restart, no human test. Run on CI.'),
('CH-026','UI browser E2E, accessibility and visual regression CI','testing','p0',['CH-020','CH-021','CH-022','CH-023'],
 'Playwright against actual production-built app and real local SQLite; real mobile/desktop screenshots and themes, a11y/keyboard, interactive WAV player, recording OFF/ON warning and forbidden override tests, authorization; verify all screens, toasts, forms, dialogs, errors and a11y labels are English-only and that no GUI locale selector appears; CI MUST verify committed README screenshots match live functional UI, required CI mandatory, no default skipped tests.'),
('CH-027','Security adversarial review, performance benchmark and hardening','testing','p0',['CH-014','CH-018','CH-025','CH-026'],
 'Threat model, bearer/PKCE/webhook replay negative tests, SSRF/IDOR/path traversal, recorder range and disk tests, idle/1/multiple call measurements RAM/CPU/latency, real evidence and fixes, document results; no arbitrary RAM cap.'),
('CH-028','Operator documentation, reproducible Docker deploy and backups','release','p0',['CH-020','CH-018','CH-027'],
 'One nonroot Docker image/one volume, docker-compose and reverse proxy/WSS guide; complete professional user/admin/operator/developer docs, Telnyx and AI gateway setup, ChatGPT OAuth/MCP, security, recording opt-in/retention, backup/restore/upgrade/rollback and troubleshooting; polished README with real embedded production-build screenshots under docs/assets/screenshots, no placeholders; test clean installation and doc commands.'),
('CH-029','Release pipeline, GHCR assets and independently verified v1.0.0','release','p0',['CH-025','CH-026','CH-027','CH-028'],
 'Verify all DoD items except CH-030 finalization; run genuine artifact and release-qualification gate BEFORE marker PR, and publish signed/checksummed multi-platform GHCR linux/amd64+linux/arm64 v1.0.0 image and latest same manifest digest, verify both platforms, compose/download bundle, **professional complete public README with verifiable real UI screenshots**, full documentation, human-quality release notes describing all implemented features/installation/upgrade/known limitations, verified clean-install/restore smoke proof.'),
('CH-030','Final V1 completion gate and protected marker PR','release','p0',['CH-029'],
 'Verify every managed issue except CH-030 is closed with evidence, green main and a genuine published release (tag, assets, signed/digested GHCR linux/amd64+linux/arm64, clean install/restore). Open the final protected PR adding .autonomous/V1_COMPLETE with a Closes reference ONLY for this managed CH-030 issue; its merge closes CH-030. Recheck all managed issues closed, exact release artifacts and clean main, then let runner exit 0. Do not require CH-030 to be closed before its own final PR.'),
]

COLORS = {'p0':'d73a4a','p1':'fbca04','architecture':'0052cc','backend':'1d76db','storage':'5319e7','identity':'c5def5','api':'0075ca','telephony':'f9d0c4','audio':'bfe5bf','calls':'dbb5d4','recording':'e99695','frontend':'a2eeef','testing':'0e8a16','release':'fef2c0'}

def gh(*args, json_output=False):
    p = subprocess.run(['gh', *args], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    if p.returncode:
        raise RuntimeError(f"gh {' '.join(args)} failed: {p.stderr.strip() or p.stdout.strip()}")
    return json.loads(p.stdout) if json_output else p.stdout.strip()

def get_milestone():
    milestones=gh('api',f'repos/{REPO}/milestones?state=all&per_page=100',json_output=True)
    for item in milestones:
        if item['title']==MILESTONE:
            return item['number']
    x=gh('api',f'repos/{REPO}/milestones','-X','POST','-f','title='+MILESTONE,'-f','description=CallHarbor full production v1.0.0 (not MVP)',json_output=True)
    return x['number']

def create_labels():
    for key,color in COLORS.items():
        name = 'priority:'+key if key.startswith('p') else 'area:'+key
        gh('label','create',name,'-R',REPO,'--color',color,'--force')
    for state,color in [('idle','d4c5f9'),('in-progress','fbca04'),('blocked','d73a4a')]:
        gh('label','create','status:'+state,'-R',REPO,'--color',color,'--force')
    gh('label','create','scope:v1','-R',REPO,'--color','006b75','--force')
    gh('label','create',MANAGED_LABEL,'-R',REPO,'--color','0969da',
       '--description','Reserved for GitHub-managed CallHarbor V1 development by OpenCode + Paperclip','--force')

def list_repo_issues():
    """Read complete paginated GitHub REST issue inventory (exclude pull requests)."""
    pages=gh('api','--paginate','--slurp',f'repos/{REPO}/issues?state=all&per_page=100',json_output=True)
    if not isinstance(pages, list) or any(not isinstance(page, list) for page in pages):
        raise RuntimeError('Malformed paginated GitHub issue inventory')
    issues = [issue for page in pages for issue in page
              if isinstance(issue, dict) and 'pull_request' not in issue]
    numbers = [issue['number'] for issue in issues]
    if len(numbers) != len(set(numbers)):
        raise RuntimeError('Duplicate issue in paginated inventory')
    return issues


def main():
    print(f'[callharbor] GitHub Issues bootstrap: {REPO}',flush=True)
    milestone=get_milestone()
    create_labels()
    existing=list_repo_issues()
    known={}
    for issue in existing:
        match=re.search(r'<!-- callharbor-issue-id:(CH-\d{3}) -->',issue.get('body') or '')
        if match:
            key = match.group(1)
            if key in known:
                raise RuntimeError(f'duplicate managed bootstrap issue ID: {key}')
            names={x['name'] for x in issue.get('labels',[])}
            if not issue['title'].startswith(key+':') or 'scope:v1' not in names:
                raise RuntimeError(f'Issue #{issue["number"]} impersonates bootstrap ID {key}; refusing to adopt')
            known[key] = issue
    for item in TASKS:
        issue_id,title,area,priority,deps,acceptance=item
        if issue_id in known:
            item_existing=known[issue_id]
            number=item_existing['number']
            existing_body=item_existing.get('body') or ''
            existing_labels={x['name'] for x in item_existing.get('labels',[])}
            # Never silently re-adopt or mutate a previously seeded issue.
            # Removing its ownership label/marker is an explicit stop condition,
            # exactly as in Tendo V3. A rerun does NOT rewrite agent work.
            if MANAGED_MARKER not in existing_body or MANAGED_LABEL not in existing_labels:
                raise RuntimeError(f'CH seed {issue_id} #{number} lost its ownership label/marker; manual review required')
            if (item_existing.get('milestone') or {}).get('title') != MILESTONE:
                raise RuntimeError(f'CH seed {issue_id} #{number} moved outside milestone')
            print(f'  verified managed {issue_id} #{number}',flush=True)
            continue
        text=(f'{MANAGED_MARKER}\n<!-- callharbor-issue-id:{issue_id} -->\n\n' 
              f'## Scope and criteria\n{acceptance}\n\n'
              f'## Dependencies\n'+(', '.join(deps) if deps else 'None')+'\n\n'
              '## Policy\n- Deliver full production functionality and regression coverage; no MVP shortcuts.\n'
              '- Claim via GitHub comment before work; branch/PR/required green CI; close only after merged evidence.\n'
              '- Follow locked `.autonomous/PRODUCT_SCOPE.md` and `.autonomous/DEFINITION_OF_DONE.md`.\n'
              '- Never call production Telnyx in automated tests.\n')
        bodyargs=['issue','create','-R',REPO,'--title',f'{issue_id}: {title}','--body',text,'--milestone',MILESTONE]
        for label in [MANAGED_LABEL,'scope:v1','priority:'+priority,'area:'+area,'status:idle']:
            bodyargs+=['--label',label]
        url=gh(*bodyargs)
        print(f'  created {issue_id} {url}',flush=True)
    total=len(TASKS)
    # Readback is essential: the agent must never operate on an unmarked task.
    seen = {m.group(1) for i in list_repo_issues()
            if (m := re.search(r'<!-- callharbor-issue-id:(CH-\d{3}) -->', i.get('body') or ''))}
    missing = {task[0] for task in TASKS} - seen
    if missing:
        raise RuntimeError(f'Created issues not visible in REST readback: {sorted(missing)}')
    print(f'[callharbor] milestone #{milestone}: {total} agent-managed scoped issue definitions; ready.',flush=True)

if __name__ == '__main__':
    try: main()
    except Exception as exc:
        print('[callharbor] ISSUE BOOTSTRAP FAILED:',str(exc),file=sys.stderr)
        sys.exit(1)
