#!/usr/bin/env python3
"""Exercise the real offline updater with stateful package/systemd fixtures."""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

SCRIPT = Path(__file__).with_name('upgrade.sh').read_text()

class UpgradeTests(unittest.TestCase):
    def run_sequence(self, actions, active=True, process=False):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / 'bin').mkdir()
            (root / 'lock').mkdir()
            (root / 'node.deb').write_bytes(b'fixture')
            states = {name: {'load': 'loaded', 'active': 'active' if active else 'inactive'} for name in (
                'incus.service', 'incus-agent.service', 'incus-user.service', 'incus-network.service')}
            states['incus-xray.service'] = {'load': 'not-found', 'active': 'inactive'}
            if active == 'mixed':
                for name in states: states[name]['active'] = 'active' if name == 'incus-agent.service' else 'inactive'
            (root / 'states').write_text(json.dumps(states))
            script = SCRIPT.replace('exec 9< /run/lock', f'exec 9< "{root}/lock"').replace('journal=/run/incus-upgrade', f'journal="{root}/checkpoint"')
            script = '\n'.join(line for line in script.splitlines() if not line.startswith('[[ $EUID -eq 0 ]]'))
            (root / 'upgrade.sh').write_text(script)
            mocks = {
                'flock': 'pass',
                'sleep': 'pass',
                'dpkg-deb': '''import sys
from pathlib import Path
if sys.argv[1]=='--field': print('incus'); sys.exit()
root=Path(sys.argv[-1])
for name in ['usr/libexec/incus/incusd','usr/bin/incus-tui','usr/libexec/incus/agent/incus-agent']:
 p=root/name;p.parent.mkdir(parents=True,exist_ok=True);p.write_text('#!/bin/sh\\nexit 0\\n');p.chmod(0o755)
for number in [1,2]:
 p=root/f'usr/libexec/incus/migrations/00{number}.sh';p.parent.mkdir(parents=True,exist_ok=True)
 p.write_text(f'echo "migration{number} $1" >> "$FIXTURE/log"\\nif [ "$FAILURE" = "migration{number}-$1" ]; then exit 42; fi\\n')
''',
                'apt-get': '''import os,sys
from pathlib import Path
root=Path(os.environ['FIXTURE']);args=' '.join(sys.argv[1:]);failure=os.environ['FAILURE']
with (root/'log').open('a') as f:f.write('apt '+args+'\\n')
kind='simulate' if '--simulate' in sys.argv else 'download' if '--download-only' in sys.argv else 'install'
if failure==kind:sys.exit(42)
if failure==kind+'-once' and not (root/(kind+'-once')).exists():
 (root/(kind+'-once')).touch();sys.exit(42)
''',
                'systemctl': '''import json,os,sys
from pathlib import Path
root=Path(os.environ['FIXTURE']);args=sys.argv[1:];failure=os.environ['FAILURE']
with (root/'log').open('a') as f:f.write('systemctl '+' '.join(args)+'\\n')
states=json.loads((root/'states').read_text())
if args[0]=='list-units':
 if failure=='list':sys.exit(1)
 print('incus-network.service loaded active running fixture\\nincus-xray.service not-found inactive dead cached')
elif args[0]=='show':
 if failure=='show': print('Failed to connect to bus',file=sys.stderr);sys.exit(1)
 state=states.get(args[1],{'load':'not-found','active':'inactive'})
 if failure=='transition' and args[1]=='incus.service' and not (root/'transition').exists():
  (root/'transition').touch();state={**state,'active':'activating'}
 print('LoadState='+state['load']+'\\nActiveState='+state['active'])
 if state['load']=='not-found':sys.exit(1)
elif args[0] in ('stop','start'):
 unit=args[1]
 if failure==args[0] and unit=='incus.service':sys.exit(42)
 if failure=='worker-start' and args[0]=='start' and unit=='incus-network.service':sys.exit(42)
 if failure=='vanish' and unit=='incus-network.service':
  states[unit]={'load':'not-found','active':'inactive'};(root/'states').write_text(json.dumps(states));sys.exit(5)
 if states[unit]['load']=='not-found':sys.exit(5)
 states[unit]['active']='active' if args[0]=='start' else 'inactive'
 (root/'states').write_text(json.dumps(states))
 if failure=='stop-settled' and args[0]=='stop':sys.exit(42)
elif args[0]=='daemon-reload' and failure=='reload':sys.exit(42)
''',
                'pgrep': "import os,sys\nsys.exit(0 if os.environ['PROCESS']=='1' else 1)\n",
            }
            for name, content in mocks.items():
                path = root / 'bin' / name
                path.write_text(f'#!{sys.executable}\n' + content)
                path.chmod(0o755)
            results = []
            for mode, failure in actions:
                env = {**os.environ, 'PATH': str(root / 'bin') + ':' + os.environ['PATH'], 'FIXTURE': str(root), 'FAILURE': failure, 'PROCESS': '1' if process else '0'}
                if failure == 'other-package': (root/'node.deb').write_bytes(b'different fixture')
                result = subprocess.run(['bash', str(root/'upgrade.sh'), mode, str(root/'node.deb')], env=env, text=True, capture_output=True, timeout=20)
                log = (root/'log').read_text() if (root/'log').exists() else ''
                checkpoint = root/'checkpoint'
                results.append((result, log, json.loads((root/'states').read_text()), (checkpoint/'phase').read_text().strip() if checkpoint.exists() else None))
            return results

    def run_upgrade(self, mode='--dry-run', failure='', active=True, process=False):
        return self.run_sequence([(mode, failure)], active, process)[0]

    def assert_running(self, states):
        for unit in ('incus.service', 'incus-agent.service', 'incus-user.service', 'incus-network.service'):
            self.assertEqual(states[unit]['active'], 'active', (unit, states))

    def test_dry_run_never_changes_services_installs_migrates_or_checkpoints(self):
        result, log, _, checkpoint = self.run_upgrade()
        self.assertEqual(result.returncode, 0, result.stderr)
        for forbidden in ['systemctl stop', 'systemctl start', 'migration1 --apply', '--download-only']:
            self.assertNotIn(forbidden, log)
        self.assertIsNone(checkpoint)

    def test_apply_orders_download_shutdown_migration_install_restore(self):
        result, log, states, checkpoint = self.run_upgrade('--apply')
        self.assertEqual(result.returncode, 0, result.stderr)
        ordered = ['--simulate', 'migration1 --dry-run', '--download-only', 'systemctl stop incus-agent.service', 'systemctl stop incus-user.service', 'systemctl stop incus.service', 'systemctl stop incus-network.service', 'migration1 --apply', 'migration2 --apply', ' install -y', 'systemctl daemon-reload', 'systemctl start incus-network.service', 'systemctl start incus.service', 'systemctl start incus-user.service', 'systemctl start incus-agent.service']
        position = -1
        for item in ordered: position = log.index(item, position + 1)
        self.assert_running(states)
        self.assertIsNone(checkpoint)

    def test_cached_not_found_optional_service_is_skipped(self):
        result, log, states, _ = self.run_upgrade('--apply')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn('systemctl stop incus-xray.service', log)
        self.assertNotIn('systemctl start incus-xray.service', log)
        self.assertIn('Skipping absent service incus-xray.service', result.stdout)
        self.assert_running(states)

    def test_preflight_and_download_failures_preserve_running_services(self):
        for failure in ['simulate', 'download', 'migration1---dry-run', 'list', 'show']:
            with self.subTest(failure=failure):
                result, log, states, checkpoint = self.run_upgrade('--apply', failure=failure)
                self.assertNotEqual(result.returncode, 0)
                self.assertNotIn('systemctl stop', log)
                self.assert_running(states)
                self.assertIsNone(checkpoint)

    def test_shutdown_failure_restores_original_services(self):
        result, log, states, checkpoint = self.run_upgrade('--apply', failure='stop')
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn('migration1 --apply', log)
        self.assertIn('Original services restored', result.stderr)
        self.assert_running(states)
        self.assertIsNone(checkpoint)

    def test_reported_stop_error_with_stopped_service_is_tolerated(self):
        result, _, states, _ = self.run_upgrade('--apply', failure='stop-settled')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assert_running(states)

    def test_optional_service_disappearing_during_stop_is_tolerated(self):
        result, _, states, _ = self.run_upgrade('--apply', failure='vanish')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(states['incus.service']['active'], 'active')

    def test_install_has_bounded_retry(self):
        result, log, states, _ = self.run_upgrade('--apply', failure='install-once')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(log.count(' install -y'), 2)
        self.assert_running(states)

    def test_failed_migration_and_install_keep_services_stopped(self):
        for failure in ['migration2---apply', 'install']:
            result, log, states, checkpoint = self.run_upgrade('--apply', failure=failure)
            self.assertEqual(result.returncode, 42, result.stderr)
            self.assertIn('Upgrade paused', result.stderr)
            self.assertNotIn('systemctl start', log)
            self.assertEqual(states['incus.service']['active'], 'inactive')
            self.assertIn(checkpoint, ['migration', 'install'])

    def test_retry_resumes_completed_migrations_and_original_active_set(self):
        for failure in ['migration2---apply', 'install', 'start', 'reload']:
            with self.subTest(failure=failure):
                first, second = self.run_sequence([('--apply', failure), ('--apply', '')])
                self.assertNotEqual(first[0].returncode, 0)
                result, log, states, checkpoint = second
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn('Resuming interrupted node upgrade', result.stdout)
                self.assertEqual(log.count('migration1 --apply'), 1)
                self.assert_running(states)
                self.assertIsNone(checkpoint)
                if failure == 'start': self.assertEqual(log.count(' install -y'), 1)

    def test_retry_does_not_accept_a_different_package(self):
        first, second = self.run_sequence([('--apply', 'install'), ('--apply', 'other-package')])
        self.assertNotEqual(first[0].returncode, 0)
        self.assertNotEqual(second[0].returncode, 0)
        self.assertIn('another package', second[0].stderr)
        self.assertEqual(first[1], second[1])

    def test_worker_restore_failure_does_not_start_daemon(self):
        result, log, states, checkpoint = self.run_upgrade('--apply', failure='worker-start')
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn('systemctl start incus.service', log)
        self.assertEqual(states['incus.service']['active'], 'inactive')
        self.assertEqual(checkpoint, 'restore')

    def test_stopped_services_stay_stopped(self):
        result, log, _, _ = self.run_upgrade('--apply', active=False)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn('systemctl start', log)

    def test_transitioning_service_can_settle(self):
        result, _, states, _ = self.run_upgrade('--apply', failure='transition')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('Waiting for service', result.stderr)
        self.assert_running(states)

    def test_inconsistent_services_fail_before_shutdown(self):
        result, log, _, _ = self.run_upgrade('--apply', active='mixed')
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn('systemctl stop', log)

    def test_running_daemon_refuses_migration_and_restores_services(self):
        result, log, states, _ = self.run_upgrade('--apply', process=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn('migration1 --apply', log)
        self.assertNotIn(' install -y', log)
        self.assert_running(states)

if __name__ == '__main__': unittest.main()
