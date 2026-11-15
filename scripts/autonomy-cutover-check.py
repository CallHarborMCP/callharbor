#!/usr/bin/env python3
"""Small read-only cutover checks. No GitHub or agent process is invoked here."""
from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import sys

WRITER_SCRIPTS = {'autonomous-loop.sh', 'autonomy-controller.sh'}


def active_writers(checkout: str, proc_root: str = '/proc') -> list[str]:
    """Detect manually launched legacy/new controllers and known agent writers in checkout.

    This is a local preflight, not a substitute for checking remote GitHub claims.
    """
    checkout = os.path.realpath(checkout)
    offenders: list[str] = []
    for entry in Path(proc_root).iterdir():
        if not entry.name.isdigit() or int(entry.name) == os.getpid():
            continue
        try:
            args = [a.decode('utf-8', 'replace') for a in (entry / 'cmdline').read_bytes().split(b'\0') if a]
            cwd = os.path.realpath(entry / 'cwd')
        except (OSError, ValueError):
            continue
        if not args:
            continue
        # Absolute/relative paths to real autonomous controller scripts.
        controller = any(Path(arg).name in WRITER_SCRIPTS for arg in args[1:3])
        # A CLI agent may be launched without the controller, from the checkout.
        worker = Path(args[0]).name.lower() in {'opencode', 'paperclip'} or any(
            Path(arg).name.lower() in {'opencode', 'paperclip'} for arg in args[1:2]
        )
        within = cwd == checkout or cwd.startswith(checkout + os.sep)
        if controller or (worker and within):
            offenders.append(f'pid={entry.name} executable={Path(args[0]).name}')
    return sorted(offenders)


def required_main_ci(data: object, sha: str) -> bool:
    if not isinstance(data, dict) or not isinstance(data.get('workflow_runs'), list):
        return False
    matched = [run for run in data['workflow_runs'] if isinstance(run, dict)
               and run.get('head_sha') == sha and run.get('head_branch') == 'main'
               and run.get('event') == 'push']
    # One completed green required workflow run on the EXACT current commit.
    # A stale green build for an earlier commit does not count.
    return bool(matched) and all(r.get('status') == 'completed' and
                                 r.get('conclusion') == 'success' for r in matched)


def main() -> None:
    p = argparse.ArgumentParser()
    sub = p.add_subparsers(dest='action', required=True)
    process = sub.add_parser('processes')
    process.add_argument('--checkout', required=True)
    ci = sub.add_parser('ci')
    ci.add_argument('--sha', required=True)
    args = p.parse_args()
    if args.action == 'processes':
        offenders = active_writers(args.checkout)
        if offenders:
            print('Local autonomous writer still active: ' + '; '.join(offenders), file=sys.stderr)
            sys.exit(75)
        print('No local autonomous writer detected (remote claims still need review).')
    else:
        try:
            data = json.load(sys.stdin)
        except (ValueError, OSError):
            sys.exit('Invalid GitHub CI response')
        if not required_main_ci(data, args.sha):
            sys.exit('Required main CI is not confirmed green for the current commit')
        print('Required main CI verified for exact current commit.')


if __name__ == '__main__':
    main()
