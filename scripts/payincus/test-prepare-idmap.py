#!/usr/bin/env python3
"""Exercise root subid preparation only in temporary fixture directories."""
import importlib.util
from pathlib import Path
import tempfile
import subprocess
import unittest
from unittest.mock import patch


SCRIPT = Path(__file__).with_name('prepare-idmap.py')
SPEC = importlib.util.spec_from_file_location('prepare_idmap', SCRIPT)
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)


class RootMappingTests(unittest.TestCase):
    def setUp(self):
        self.work = tempfile.TemporaryDirectory()
        self.addCleanup(self.work.cleanup)
        self.root = Path(self.work.name)
        (self.root / 'etc').mkdir()
        self.tools = patch.object(MODULE.shutil, 'which', return_value='/fixture/uidmap')
        self.tools.start()
        self.addCleanup(self.tools.stop)
        self.commands = []
        self.command = patch.object(MODULE.subprocess, 'run', side_effect=self.usermod)
        self.command.start()
        self.addCleanup(self.command.stop)

    def usermod(self, command, check):
        self.assertTrue(check)
        self.assertEqual(command[:3], ['usermod', '--prefix', str(self.root)])
        self.assertEqual(command[-1], 'root')
        self.commands.append(command)
        for option, name in (('--add-subuids', 'subuid'), ('--add-subgids', 'subgid')):
            if option in command:
                start, end = map(int, command[command.index(option) + 1].split('-'))
                path = self.root / 'etc' / name
                with path.open('a') as file:
                    if path.stat().st_size and not path.read_bytes().endswith(b'\n'):
                        file.write('\n')
                    file.write(f'root:{start}:{end - start + 1}\n')

    def prepare(self, ids=None):
        MODULE.prepare(self.root, account_ids=ids if ids is not None else ([0, 1000], [0, 1000]))

    def write(self, name, text):
        path = self.root / 'etc' / name
        path.write_text(text)
        return path

    def test_clean_install_and_repeated_start(self):
        self.prepare()
        expected = b'root:1000000:1000000000\n'
        for name in ('subuid', 'subgid'):
            path = self.root / 'etc' / name
            self.assertEqual(path.read_bytes(), expected)
            self.assertEqual(path.stat().st_mode & 0o777, 0o644)
        self.prepare()
        for name in ('subuid', 'subgid'):
            self.assertEqual((self.root / 'etc' / name).read_bytes(), expected)
        self.assertFalse(list((self.root / 'etc').glob('*.before-payincus-*')))
        self.assertEqual(len(self.commands), 1)

    def test_empty_files_other_users_backups_and_permissions(self):
        uid = self.write('subuid', '# delegated IDs\nalice:100000:65536')
        gid = self.write('subgid', '')
        uid.chmod(0o640)
        self.prepare()
        self.assertEqual(uid.read_text(), '# delegated IDs\nalice:100000:65536\nroot:1000000:1000000000\n')
        self.assertEqual(uid.stat().st_mode & 0o777, 0o640)
        self.assertEqual(gid.read_text(), 'root:1000000:1000000000\n')
        backups = list((self.root / 'etc').glob('*.before-payincus-*'))
        self.assertEqual(len(backups), 2)
        for backup in backups:
            self.assertEqual(backup.stat().st_mode & 0o777, 0o600)
        self.assertEqual(next(path for path in backups if path.name.startswith('subuid.')).read_text(), '# delegated IDs\nalice:100000:65536')

    def test_custom_and_numeric_root_allocations_preserved(self):
        for owner in ('root', '0'):
            for name in ('subuid', 'subgid'):
                self.write(name, f'{owner}:200000:65536\n')
            self.prepare()
            for name in ('subuid', 'subgid'):
                self.assertEqual((self.root / 'etc' / name).read_text(), f'{owner}:200000:65536\n')
        self.assertFalse(list((self.root / 'etc').glob('*.before-payincus-*')))

    def test_partial_configuration_preserves_existing_base(self):
        uid = self.write('subuid', 'root:2000000:10000000\n')
        gid = self.write('subgid', 'root:1000:1\n')
        self.prepare()
        self.assertEqual(uid.read_text(), 'root:2000000:10000000\n')
        self.assertEqual(gid.read_text(), 'root:1000:1\nroot:2000000:10000000\n')

    def test_conflicts_and_bad_files_refused_before_any_write(self):
        for bad in ('alice:1000000:65536\n', 'broken\n', 'root:2:4294967295\n', 'root:123:0\n'):
            with self.subTest(bad=bad):
                uid = self.write('subuid', '')
                gid = self.write('subgid', bad)
                with self.assertRaises(RuntimeError):
                    self.prepare()
                self.assertEqual(uid.read_text(), '')
                self.assertEqual(gid.read_text(), bad)
        self.assertFalse(list((self.root / 'etc').glob('*.before-payincus-*')))

    def test_host_account_collision_and_missing_helper(self):
        with self.assertRaisesRegex(RuntimeError, 'host account ID 2000000'):
            self.prepare(([0], [2000000]))
        with patch.object(MODULE.shutil, 'which', return_value=None):
            with self.assertRaisesRegex(RuntimeError, 'newuidmap: command not found'):
                self.prepare()
        self.assertFalse((self.root / 'etc/subuid').exists())

    def test_symlink_refused_without_changing_target(self):
        target = self.write('original', 'alice:100000:65536\n')
        (self.root / 'etc/subuid').symlink_to(target)
        with self.assertRaisesRegex(RuntimeError, 'regular file'):
            self.prepare()
        self.assertEqual(target.read_text(), 'alice:100000:65536\n')

    def test_write_failure_preserves_original_and_raw_error(self):
        uid = self.write('subuid', '# original\n')
        cause = subprocess.CalledProcessError(6, ['usermod'], stderr='fixture database lock denied')
        with patch.object(MODULE.subprocess, 'run', side_effect=cause):
            with self.assertRaises(subprocess.CalledProcessError) as result:
                self.prepare()
        self.assertIs(result.exception, cause)
        self.assertEqual(uid.read_text(), '# original\n')
        self.assertEqual(len(list((self.root / 'etc').glob('subuid.before-payincus-*'))), 1)

    def test_startup_check_is_read_only(self):
        for name in ('subuid', 'subgid'):
            self.write(name, 'root:2000000:10000000\n')
        before = {path.name: path.read_bytes() for path in (self.root / 'etc').iterdir()}
        with patch.object(MODULE.os, 'open', side_effect=AssertionError('startup must not open a writable lock')):
            MODULE.prepare(self.root, check=True)
        self.assertEqual(before, {path.name: path.read_bytes() for path in (self.root / 'etc').iterdir()})
        self.assertFalse(self.commands)
        (self.root / 'etc/subgid').write_text('')
        with self.assertRaisesRegex(RuntimeError, 'no usable root ID mapping'):
            MODULE.prepare(self.root, check=True)
        self.assertEqual((self.root / 'etc/subgid').read_text(), '')

    def test_package_and_service_prepare_mapping_before_daemon(self):
        package = SCRIPT.with_name('package.sh').read_text()
        service = SCRIPT.parents[2].joinpath('deploy/payincus/incus.service').read_text()
        self.assertIn('scripts/payincus/prepare-idmap.py "$stage/rootfs/usr/libexec/incus/prepare-idmap"', package)
        self.assertIn('uidmap, passwd, python3, systemd', package)
        postinst = package.split("<<'POSTINST'", 1)[1].split('\nPOSTINST', 1)[0]
        self.assertIn('/usr/libexec/incus/prepare-idmap', postinst)
        self.assertLess(service.index('ExecStartPre=/usr/libexec/incus/prepare-idmap --check'), service.index('ExecStart=/usr/libexec/incus/incusd'))


if __name__ == '__main__':
    unittest.main()
