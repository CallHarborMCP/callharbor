#!/usr/bin/env python3
"""Offline regression checks for cutover guards; never contacts GitHub or systemd."""
import importlib.util
import pathlib
import subprocess
import tempfile
import time
import unittest

spec = importlib.util.spec_from_file_location('cutover', pathlib.Path(__file__).with_name('autonomy-cutover-check.py'))
cutover = importlib.util.module_from_spec(spec)
spec.loader.exec_module(cutover)
SHA = 'a' * 40


class CutoverTests(unittest.TestCase):
    def test_exact_main_sha_required(self):
        good = {'workflow_runs': [{'head_sha': SHA, 'head_branch': 'main', 'event': 'push',
                                   'status': 'completed', 'conclusion': 'success'}]}
        self.assertTrue(cutover.required_main_ci(good, SHA))
        self.assertFalse(cutover.required_main_ci(good, 'b' * 40))
        for key, value in [('head_branch', 'dev'), ('event', 'pull_request'),
                           ('status', 'in_progress'), ('conclusion', 'failure')]:
            modified = {'workflow_runs': [dict(good['workflow_runs'][0], **{key: value})]}
            self.assertFalse(cutover.required_main_ci(modified, SHA))
        self.assertFalse(cutover.required_main_ci({'workflow_runs': []}, SHA))
        self.assertFalse(cutover.required_main_ci({'error': 'forbidden'}, SHA))

    def test_manual_legacy_process_detected(self):
        with tempfile.TemporaryDirectory(prefix='callharbor-guard-') as directory:
            script = pathlib.Path(directory) / 'autonomous-loop.sh'
            script.write_text('while true; do sleep 1; done\n')
            proc = subprocess.Popen(['/bin/bash', str(script)], cwd=directory,
                                    stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            try:
                found = False
                for _ in range(30):
                    if any(f'pid={proc.pid} ' in x for x in cutover.active_writers(directory)):
                        found = True
                        break
                    time.sleep(0.05)
                self.assertTrue(found, 'manually started legacy controller should block cutover')
            finally:
                proc.terminate()
                proc.wait(timeout=5)
            self.assertFalse(any(f'pid={proc.pid} ' in x for x in cutover.active_writers(directory)))


if __name__ == '__main__':
    unittest.main(verbosity=2)
