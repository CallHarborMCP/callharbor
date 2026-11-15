#!/usr/bin/env python3
"""Isolated real-process controller checks: NO real agents, GitHub, or systemd."""
import os
from pathlib import Path
import shutil
import signal
import subprocess
import tempfile
import time
import unittest

SOURCE = Path(__file__).with_name('autonomy-controller.sh')


def executable(path, content):
    path.write_text(content)
    path.chmod(0o755)


class ControllerTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='callharbor-controller-test-')
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        for d in ('scripts', '.autonomous', 'bin', 'runtime', 'state', 'home'):
            (self.root / d).mkdir()
        shutil.copy2(SOURCE, self.root / 'scripts/autonomy-controller.sh')
        (self.root / '.autonomous/AUTONOMY_PROMPT.md').write_text('Fake test-only task.\n')
        executable(self.root / 'scripts/autonomy-work-selection.py',
                   '#!/usr/bin/env python3\nimport json\nprint(json.dumps({"kind":"claim-issue","number":1}))\n')
        executable(self.root / 'bin/git', '''#!/bin/sh
case "$1" in
  branch) echo main;;
  status|fetch|pull) :;;
  rev-parse) echo abcdef;;
  *) exit 66;;
esac
''')
        executable(self.root / 'bin/gh', '#!/bin/sh\necho "unexpected GitHub call" >&2\nexit 99\n')
        self.marker = self.root / 'fake-agent-started'
        executable(self.root / 'fake-runner.sh', '''#!/bin/bash
set -eu
[[ "$#" == 2 && -s "$1" ]] || exit 3
printf 'started\\n' >>"$RUNNER_TEST_MARKER"
sleep 120 &
wait
''')
        self.env = dict(os.environ, PATH=str(self.root / 'bin') + os.pathsep + os.environ['PATH'],
                        HOME=str(self.root / 'home'), XDG_RUNTIME_DIR=str(self.root / 'runtime'),
                        XDG_STATE_HOME=str(self.root / 'state'),
                        AUTONOMY_RUNNER=str(self.root / 'fake-runner.sh'),
                        RUNNER_TEST_MARKER=str(self.marker), IDLE_POLL_SECONDS='300',
                        CYCLE_TIMEOUT_SECONDS='65')

    def start(self, **env_updates):
        return subprocess.Popen(['/bin/bash', str(self.root / 'scripts/autonomy-controller.sh')],
                                cwd=self.root, env=dict(self.env, **env_updates),
                                stdout=subprocess.DEVNULL, stderr=subprocess.PIPE,
                                text=True)

    def test_refuses_unreliable_lock_location(self):
        env = dict(self.env)
        env.pop('XDG_RUNTIME_DIR')
        p = subprocess.run(['/bin/bash', str(self.root / 'scripts/autonomy-controller.sh')],
                           cwd=self.root, env=env, capture_output=True, text=True, timeout=5)
        self.assertEqual(p.returncode, 2)
        self.assertFalse(self.marker.exists())

    def test_no_second_controller_and_stop_kills_fake_runner(self):
        p = self.start()
        try:
            for _ in range(120):
                if self.marker.exists() and self.marker.stat().st_size > 0:
                    break
                self.assertIsNone(p.poll(), f'controller ended early: {p.stderr.read() if p.poll() is not None else ""}')
                time.sleep(0.05)
            self.assertTrue(self.marker.exists(), 'isolated test runner never started')
            second = subprocess.run(['/bin/bash', str(self.root / 'scripts/autonomy-controller.sh')],
                                    cwd=self.root, env=self.env,
                                    capture_output=True, text=True, timeout=5)
            self.assertEqual(second.returncode, 75, second.stderr)
            self.assertEqual(self.marker.read_text().count('started'), 1)
            p.send_signal(signal.SIGTERM)
            p.wait(timeout=18)
            self.assertEqual(p.returncode, 0, p.stderr.read())
            self.assertEqual(self.marker.read_text().count('started'), 1)
        finally:
            if p.poll() is None:
                p.kill()
                p.wait(timeout=5)
            if p.stderr is not None:
                p.stderr.close()


if __name__ == '__main__':
    unittest.main(verbosity=2)
