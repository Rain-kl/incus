#!/usr/bin/env python3
"""Execute the actual offline updater with isolated package/systemd fixtures."""
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

SCRIPT = Path(__file__).with_name('upgrade.sh').read_text()

class UpgradeTests(unittest.TestCase):
    def run_upgrade(self, mode='--dry-run', failure='', active=True, process=False):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / 'bin').mkdir()
            (root / 'lock').mkdir()
            (root / 'node.deb').write_bytes(b'fixture')
            script = SCRIPT.replace('exec 9< /run/lock', f'exec 9< "{root}/lock"')
            script = '\n'.join(line for line in script.splitlines() if not line.startswith('[[ $EUID -eq 0 ]]'))
            (root / 'upgrade.sh').write_text(script)
            mocks = {
                'flock': 'pass',
                'dpkg-deb': '''import os,sys
from pathlib import Path
if sys.argv[1]=='--field': print('incus'); sys.exit()
root=Path(sys.argv[-1])
for name in ['usr/libexec/incus/incusd','usr/bin/incus-tui','usr/libexec/incus/agent/incus-agent']:
 p=root/name;p.parent.mkdir(parents=True,exist_ok=True);p.write_text('#!/bin/sh\\nexit 0\\n');p.chmod(0o755)
p=root/'usr/libexec/incus/migrations/001.sh';p.parent.mkdir(parents=True)
p.write_text('echo "migration $1" >> "$FIXTURE/log"\\nif [ "$FAILURE" = "migration-$1" ]; then exit 42; fi\\n')
''',
                'apt-get': '''import os,sys
from pathlib import Path
args=' '.join(sys.argv[1:]);root=Path(os.environ['FIXTURE'])
with (root/'log').open('a') as f:f.write('apt '+args+'\\n')
if (os.environ['FAILURE']=='simulate' and '--simulate' in sys.argv) or (os.environ['FAILURE']=='install' and '-y' in sys.argv):sys.exit(42)
''',
                'systemctl': '''import os,sys
from pathlib import Path
root=Path(os.environ['FIXTURE']);args=sys.argv[1:]
with (root/'log').open('a') as f:f.write('systemctl '+' '.join(args)+'\\n')
if args[0]=='list-units':print('incus-network.service loaded active running fixture')
elif args[0]=='show':print('active' if os.environ['ACTIVE']=='1' or (os.environ['ACTIVE']=='mixed' and args[1]=='incus-agent') else 'inactive')
elif args[0]=='is-active':sys.exit(3)
elif args[0]=='stop' and os.environ['FAILURE']=='stop':sys.exit(42)
''',
                'pgrep': "import os,sys\nsys.exit(0 if os.environ['PROCESS']=='1' else 1)\n",
            }
            for name, content in mocks.items():
                path = root / 'bin' / name
                path.write_text(f'#!{sys.executable}\n' + content)
                path.chmod(0o755)
            env = {**os.environ, 'PATH': str(root / 'bin') + ':' + os.environ['PATH'], 'FIXTURE': str(root), 'FAILURE': failure, 'ACTIVE': 'mixed' if active == 'mixed' else '1' if active else '0', 'PROCESS': '1' if process else '0'}
            result = subprocess.run(['bash', str(root/'upgrade.sh'), mode, str(root/'node.deb')],env=env,text=True,capture_output=True)
            log = (root/'log').read_text() if (root/'log').exists() else ''
            return result, log

    def test_dry_run_never_stops_installs_or_migrates(self):
        result, log = self.run_upgrade()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('migration --dry-run', log)
        for forbidden in ['systemctl stop', 'systemctl start', 'migration --apply', 'apt install -y']:
            self.assertNotIn(forbidden, log)

    def test_apply_preflights_stops_migrates_installs_then_restores(self):
        result, log = self.run_upgrade('--apply')
        self.assertEqual(result.returncode, 0, result.stderr)
        ordered=['apt --simulate', 'migration --dry-run', 'systemctl stop incus-agent incus-user incus', 'systemctl stop incus-network.service', 'systemctl is-active', 'migration --apply', 'apt install -y', 'systemctl daemon-reload', 'systemctl start incus-network.service', 'systemctl start incus\n', 'systemctl start incus-user', 'systemctl start incus-agent']
        position=-1
        for item in ordered:
            position=log.index(item,position+1)

    def test_failed_preflight_does_not_stop_services(self):
        for failure in ['simulate', 'migration---dry-run']:
            result, log = self.run_upgrade('--apply',failure=failure)
            self.assertEqual(result.returncode,42,result.stderr)
            self.assertNotIn('systemctl stop',log)

    def test_failed_migration_or_install_does_not_start_services(self):
        for failure in ['migration---apply','install']:
            result,log=self.run_upgrade('--apply',failure=failure)
            self.assertEqual(result.returncode,42,result.stderr)
            self.assertIn('services remain stopped',result.stderr)
            self.assertNotIn('systemctl start',log)

    def test_stopped_services_stay_stopped(self):
        result,log=self.run_upgrade('--apply',active=False)
        self.assertEqual(result.returncode,0,result.stderr)
        self.assertNotIn('systemctl start',log)

    def test_inconsistent_services_fail_before_shutdown(self):
        result,log=self.run_upgrade('--apply',active='mixed')
        self.assertNotEqual(result.returncode,0)
        self.assertNotIn('systemctl stop',log)
        self.assertNotIn('migration --apply',log)

    def test_running_daemon_refuses_migration(self):
        result,log=self.run_upgrade('--apply',process=True)
        self.assertNotEqual(result.returncode,0)
        self.assertNotIn('migration --apply',log)
        self.assertNotIn('apt install -y',log)

if __name__=='__main__':unittest.main()
