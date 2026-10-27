#!/usr/bin/python3
"""PayIncus: prepare root subids before incusd caches its host ID mapping."""
import argparse
import fcntl
import grp
import os
from pathlib import Path
import pwd
import shutil
import stat
import subprocess
import uuid


DEFAULT_RANGE = (1000000, 1000000000)


def read_map(path):
    try:
        info = path.lstat()
    except FileNotFoundError:
        return None, None, []
    if not stat.S_ISREG(info.st_mode):
        raise RuntimeError(f'{path}: expected a regular file; refusing to replace it')
    content = path.read_bytes()
    entries = []
    for number, line in enumerate(content.decode().splitlines(), 1):
        line = line.split('#', 1)[0]
        if not line.strip():
            continue
        fields = line.split(':')
        if len(fields) != 3 or not fields[0] or not fields[1].isascii() or not fields[1].isdecimal() or not fields[2].isascii() or not fields[2].isdecimal():
            raise RuntimeError(f'{path}:{number}: malformed subordinate ID allocation')
        start, count = int(fields[1]), int(fields[2])
        if start < 0 or count < 1 or start + count > 4294967295:
            raise RuntimeError(f'{path}:{number}: invalid subordinate ID range')
        entries.append((fields[0], start, count))
    return content, info, entries


def root_ranges(entries):
    # Match Incus NewSetFromSystem: username takes precedence over numeric UID.
    owner = 'root' if any(row[0].lower() == 'root' for row in entries) else '0'
    return [(start, count) for name, start, count in entries if name.lower() == owner and count >= 65536]


def prepare(root=Path('/'), account_ids=None, check=False):
    root = Path(root)
    for tool in ('newuidmap', 'newgidmap'):
        if shutil.which(tool) is None:
            raise RuntimeError(f'{tool}: command not found; install the uidmap package')
    paths = [root / 'etc/subuid', root / 'etc/subgid']
    if check:
        for path in paths:
            if not root_ranges(read_map(path)[2]):
                raise RuntimeError(f'{path}: no usable root ID mapping; run /usr/libexec/incus/prepare-idmap during maintenance before restarting Incus')
        print('Root UID/GID mapping check passed (read-only).', flush=True)
        return
    if account_ids is None:
        account_ids = ([row.pw_uid for row in pwd.getpwall()], [row.gr_gid for row in grp.getgrall()])
    # Coordinate package preparation; usermod also holds the system account database locks.
    lock_fd = os.open(root / 'etc/.payincus-idmap.lock', os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    with os.fdopen(lock_fd, 'w') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        maps = [read_map(path) for path in paths]
        ranges = [root_ranges(data[2]) for data in maps]
        changes = []
        for index, path in enumerate(paths):
            if ranges[index]:
                print(f'{path}: preserving root mapping {ranges[index]}', flush=True)
                continue
            # A partially configured host must retain the already allocated base.
            candidate = ranges[1 - index][0] if ranges[1 - index] else DEFAULT_RANGE
            start, count = candidate
            for owner, other_start, other_count in maps[index][2]:
                if start < other_start + other_count and other_start < start + count:
                    raise RuntimeError(f'{path}: proposed root:{start}:{count} overlaps {owner}:{other_start}:{other_count}; existing allocations were not changed')
            for account_id in account_ids[index]:
                if start <= account_id < start + count:
                    raise RuntimeError(f'{path}: proposed root:{start}:{count} overlaps host account ID {account_id}; existing allocations were not changed')
            changes.append((index, start, count))
        # Validate both files before writing either; never truncate an existing allocation.
        if not changes:
            return
        if shutil.which('usermod') is None:
            raise RuntimeError('usermod: command not found; install the passwd package')
        for index, _, _ in changes:
            path = paths[index]
            content, _, _ = maps[index]
            current, _, _ = read_map(path)
            if current != content:
                raise RuntimeError(f'{path}: changed during ID mapping preparation; retry after other account management completes')
            if content is not None:
                backup = path.with_name(path.name + '.before-payincus-' + uuid.uuid4().hex)
                with backup.open('xb') as file:
                    os.fchmod(file.fileno(), 0o600)
                    file.write(content)
                    file.flush()
                    os.fsync(file.fileno())
                print(f'{path}: original saved to {backup}', flush=True)
            else:
                # shadow only maintains subid files when the files backend exists.
                with path.open('xb') as file:
                    os.fchmod(file.fileno(), 0o644)
        command = ['usermod']
        if root != Path('/'):
            command.extend(['--prefix', str(root)])
        for index, start, count in changes:
            command.extend(['--add-subuids' if index == 0 else '--add-subgids', f'{start}-{start + count - 1}'])
        command.append('root')
        print('$ ' + ' '.join(command), flush=True)
        # Inherit all stdout/stderr and propagate the actual command failure.
        subprocess.run(command, check=True)
        for index, start, count in changes:
            actual = read_map(paths[index])[2]
            if ('root', start, count) not in actual or any(row not in actual for row in maps[index][2]):
                raise RuntimeError(f'{paths[index]}: usermod result did not preserve old allocations and add the requested root mapping; inspect the backup')
        print('Root ID mapping prepared; an already running incusd must be restarted in a maintenance window to read it.', flush=True)


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--check', action='store_true', help='check only; do not write files or acquire a writable lock')
    arguments = parser.parse_args()
    if os.geteuid() != 0:
        raise SystemExit('Run prepare-idmap as root.')
    prepare(check=arguments.check)
