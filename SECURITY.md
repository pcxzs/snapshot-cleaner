# Security policy

## Supported versions

The latest release, and `main`.

## Reporting a vulnerability

Report privately through GitHub's
[private vulnerability reporting](https://github.com/pcxzs/snapshot-cleaner/security/advisories/new)
rather than in a public issue.

Please include the `snapshot-cleaner` version, your snapshot layout, and — if
you can reproduce it — a log from `make debug` (`./snapshot-cleaner-debug`),
which records the decision path in full.

## Scope

This tool runs as root and deletes files from read-only btrfs snapshots, so the
interesting classes of bug are:

- Data loss: deleting, truncating or altering anything other than the exact
  file selected, or in any snapshot other than the ones reported.
- Leaving a snapshot writable after a run, or after a failure or interrupt.
- Path handling that escapes the snapshot it is confined to (symlink or
  `..` traversal, a TOCTOU race between validation and unlink).
- Privilege issues in the mounts it creates, or in the lock, journal and cache
  files it writes.

Reports of data loss are treated as security issues even where no attacker is
involved.
