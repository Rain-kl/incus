#!/usr/bin/env python3
"""Exercise node uninstall modes, safety guards, and path cleanup with mocked system tools."""
import os
from pathlib import Path
import re
import subprocess
import tempfile
import unittest

SCRIPT_PATH = Path(__file__).with_name('uninstall.sh')
SCRIPT = SCRIPT_PATH.read_text()


class UninstallTests(unittest.TestCase):
    def run_uninstall(self, args, *, pkg_installed=True, egress_units=None, absent_units=None, keep_euid=False):
        egress_units = egress_units or []
        absent_units = set(absent_units or [])
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / 'bin').mkdir()
            for rel in [
                'etc/incus/agent/config.yaml',
                'etc/incus/agent/config.example.yaml',
                'etc/incus/other.conf',
                'var/cache/incus/node-install/incus-2.0.1.deb',
                'run/incus-upgrade/state',
                'var/lib/incus/database/global/db.bin',
                'var/lib/incus-tui/state.json',
                'var/log/incus-tui/session-1.log',
            ]:
                path = root / rel
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text(rel)
            (root / 'run/systemd/system').mkdir(parents=True, exist_ok=True)

            script = SCRIPT
            if not keep_euid:
                script = '\n'.join(
                    line for line in script.splitlines()
                    if not line.startswith('[[ $EUID -eq 0 ]]')
                ) + '\n'
            script = re.sub(
                r'/etc/incus/agent/config\.yaml|/etc/incus/agent|/var/cache/incus/node-install|/run/incus-upgrade|/var/lib/incus-tui|/var/lib/incus|/var/log/incus-tui|/etc/incus|/run/systemd/system',
                lambda m: f'{root}{m.group(0)}',
                script,
            )
            (root / 'uninstall.sh').write_text(script)

            systemctl_mock = root / 'bin/systemctl'
            systemctl_mock.write_text(
                f"""#!/usr/bin/env python3
import os, sys
from pathlib import Path
log = Path(os.environ['FIXTURE']) / 'actions.log'
absent = set(os.environ.get('ABSENT_UNITS', '').split(',')) - {{''}}
egress = [u for u in os.environ.get('EGRESS_UNITS', '').split(',') if u]
args = sys.argv[1:]
if args[0] == 'show':
    unit = args[1]
    print('not-found' if unit in absent else 'loaded')
elif args[0] in ('list-units', 'list-unit-files'):
    for u in egress:
        print(f'{{u}} loaded active running')
else:
    with log.open('a') as f:
        f.write('systemctl ' + ' '.join(args) + '\\n')
"""
            )
            systemctl_mock.chmod(0o755)

            dpkg_mock = root / 'bin/dpkg-query'
            dpkg_mock.write_text(
                """#!/usr/bin/env python3
import os
if os.environ.get('PKG_INSTALLED') == '1':
    print('install ok installed')
"""
            )
            dpkg_mock.chmod(0o755)

            apt_mock = root / 'bin/apt-get'
            apt_mock.write_text(
                """#!/usr/bin/env python3
import os, sys
from pathlib import Path
with (Path(os.environ['FIXTURE']) / 'actions.log').open('a') as f:
    f.write('apt-get ' + ' '.join(sys.argv[1:]) + '\\n')
"""
            )
            apt_mock.chmod(0o755)

            env = {
                **os.environ,
                'PATH': f"{root / 'bin'}:{os.environ['PATH']}",
                'FIXTURE': str(root),
                'PKG_INSTALLED': '1' if pkg_installed else '0',
                'EGRESS_UNITS': ','.join(egress_units),
                'ABSENT_UNITS': ','.join(absent_units),
            }
            result = subprocess.run(
                ['bash', str(root / 'uninstall.sh'), *args],
                env=env,
                stdin=subprocess.DEVNULL,
                text=True,
                capture_output=True,
                timeout=15,
            )
            log_file = root / 'actions.log'
            actions = log_file.read_text().splitlines() if log_file.exists() else []
            existing = {
                rel: (root / rel).exists()
                for rel in [
                    'etc/incus/agent/config.yaml',
                    'etc/incus/agent/config.example.yaml',
                    'etc/incus/other.conf',
                    'var/cache/incus/node-install',
                    'run/incus-upgrade',
                    'var/lib/incus',
                    'var/lib/incus-tui',
                    'var/log/incus-tui',
                ]
            }
            return result, actions, existing

    def test_neutral_naming(self):
        self.assertIsNone(re.search(r'payincus|incudal', SCRIPT, re.I))

    def test_help_and_argument_validation(self):
        res, _, _ = self.run_uninstall(['--help'], keep_euid=True)
        self.assertEqual(res.returncode, 0)
        self.assertIn('--agent-only', res.stdout)

        for bad_args in [[], ['--agent-only', '--full'], ['--agent-only', '--purge-data'], ['--bogus']]:
            res, actions, _ = self.run_uninstall(bad_args)
            self.assertEqual(res.returncode, 2, bad_args)
            self.assertEqual(actions, [])

    def test_requires_yes_when_non_interactive(self):
        res, actions, existing = self.run_uninstall(['--agent-only'])
        self.assertEqual(res.returncode, 1)
        self.assertIn('--yes', res.stderr)
        self.assertEqual(actions, [])
        self.assertTrue(existing['etc/incus/agent/config.yaml'])

    def test_agent_only_dry_run_and_apply(self):
        res, actions, existing = self.run_uninstall(['--agent-only', '--dry-run'])
        self.assertEqual(res.returncode, 0, res.stderr)
        self.assertIn('[dry-run] systemctl disable --now incus-agent.service', res.stdout)
        self.assertEqual(actions, [])
        self.assertTrue(existing['etc/incus/agent/config.yaml'])

        res, actions, existing = self.run_uninstall(['--agent-only', '--yes'])
        self.assertEqual(res.returncode, 0, res.stderr)
        self.assertEqual(actions, ['systemctl disable --now incus-agent.service'])
        self.assertFalse(existing['etc/incus/agent/config.yaml'])
        self.assertTrue(existing['etc/incus/agent/config.example.yaml'])
        self.assertTrue(existing['var/lib/incus'])

    def test_full_uninstall_preserves_data_by_default(self):
        res, actions, existing = self.run_uninstall(
            ['--full', '--yes'],
            egress_units=['incus-egress-eth0.service'],
            absent_units=['incus-xray.service'],
        )
        self.assertEqual(res.returncode, 0, res.stderr)
        self.assertIn('systemctl disable --now incus-agent.service', actions)
        self.assertIn('systemctl disable --now incus-egress-eth0.service', actions)
        self.assertIn('systemctl disable --now incus.service', actions)
        self.assertNotIn('systemctl disable --now incus-xray.service', actions)
        self.assertIn('apt-get purge -y incus-lts', actions)
        self.assertIn('apt-get autoremove -y', actions)
        self.assertIn('systemctl daemon-reload', actions)
        self.assertFalse(existing['etc/incus/agent/config.yaml'])
        self.assertFalse(existing['var/cache/incus/node-install'])
        self.assertFalse(existing['run/incus-upgrade'])
        self.assertTrue(existing['var/lib/incus'])
        self.assertTrue(existing['etc/incus/other.conf'])

    def test_full_uninstall_with_purge_data_and_absent_package(self):
        res, actions, existing = self.run_uninstall(
            ['--full', '--purge-data', '--yes'],
            pkg_installed=False,
        )
        self.assertEqual(res.returncode, 0, res.stderr)
        self.assertFalse(any(a.startswith('apt-get purge') for a in actions))
        self.assertFalse(existing['var/lib/incus'])
        self.assertFalse(existing['var/lib/incus-tui'])
        self.assertFalse(existing['var/log/incus-tui'])
        self.assertFalse(existing['etc/incus/other.conf'])


if __name__ == '__main__':
    unittest.main()
