# snapshot-cleaner

[![CI](https://github.com/pcxzs/snapshot-cleaner/actions/workflows/ci.yml/badge.svg)](https://github.com/pcxzs/snapshot-cleaner/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/pcxzs/snapshot-cleaner)](https://goreportcard.com/report/github.com/pcxzs/snapshot-cleaner)
[![License: GPL v3](https://img.shields.io/badge/license-GPLv3-blue.svg)](LICENSE)

Find and reclaim disk space held by files that only exist in btrfs snapshots.

## The problem

On a btrfs system with snapshots, deleting a large file frees nothing. Every
snapshot still referencing that file pins its extents, so the space stays gone
and `df` keeps reporting it as used. You delete a 4 GB ISO, watch free space not
move, and have no way to find out which deleted files are still costing you or
how much each one costs.

Existing tools do not answer this:

- `btdu` shows where space went, but not what is deletable.
- `compsize` totals a set of files you must already know about.
- Snapshot managers only delete whole snapshots, throwing away restore points to
  reclaim one stray download.

`snapshot-cleaner` diffs the live filesystem against every snapshot, ranks the
pinned files by the space they actually cost, and can remove one file from every
snapshot holding it. The snapshots stay in place, stay read-only, and stay
restorable for everything else.

**[GUIDE.md](GUIDE.md)** is the full manual: how the space figure is computed
and why file size is the wrong answer, every command and flag, purge safety,
supported layouts, troubleshooting and recovery. This README is the short version.

## Install

From source:

```sh
git clone https://github.com/pcxzs/snapshot-cleaner
cd snapshot-cleaner
make build && sudo make install     # -> /usr/local/sbin/snapshot-cleaner
```

With the Go toolchain, or from a release:

```sh
go install github.com/pcxzs/snapshot-cleaner@latest
```

Prebuilt `linux/amd64` and `linux/arm64` binaries are attached to each
[release](https://github.com/pcxzs/snapshot-cleaner/releases), with
`SHA256SUMS`.

Requires Go 1.25+ to build. The result is a single static binary with **no
runtime dependencies** beyond a Linux kernel with btrfs: no `btrfs` CLI, no
`compsize`, no Python, no database server. It reads extent maps, lists snapshot
contents and toggles subvolume flags through ioctls directly, and keeps its scan
cache in an embedded key/value file.

## Use

```sh
sudo snapshot-cleaner doctor                 # what was detected, and what works here
sudo snapshot-cleaner scan --min-size 100M   # find and rank pinned files (read-only)
snapshot-cleaner purge 3                     # dry run: shows exactly what it would do
sudo snapshot-cleaner purge 3 --apply        # actually remove it
sudo snapshot-cleaner purge --interactive --apply
sudo snapshot-cleaner journal                # what was removed, and when
snapshot-cleaner cache status                # what a rescan will not have to redo
```

Root is required only where it genuinely is:

| Command | Root? |
|---|---|
| `scan`, `snapshots`, `doctor`, `journal`, `cache status` | No, but recommended |
| `purge` without `--apply` (dry run) | No — it only reads |
| `purge --apply` | **Yes** |

Without root the tool still works when the snapshots are already readable (an
explicit `--snapshots` directory, or a readable snapper layout), but layouts
needing the btrfs top level mounted will not be found, and sizes fall back to
FIEMAP estimates marked `~`. It says so rather than pretending otherwise.

A scan looks like this:

```
  ID  RECLAIM   APPARENT  SNAPS  KIND     SUBVOL  PATH
   1  2.61 GiB   687 MiB    4/8  deleted  @home   user/old-vm-image.qcow2
   2  549 MiB    575 MiB    3/8  deleted  @home   user/downloads/react.zip
```

- **RECLAIM** — the space freed by removing the file from **all** the snapshots
  holding it. This is the number to sort on, and it is not the file's size.
- **APPARENT** — the file's nominal size.
- **SNAPS** — how many of the pair's snapshots hold it (`4/8`).

## Scans are cached, so only the first one is slow

A snapshot subvolume is read-only and immutable: once listed, its contents
cannot change for as long as it exists. So the expensive half of a scan is
recorded and reused, and a rescan only lists whatever snapshot your manager has
taken since.

```
Reused 14 of 15 snapshot listing(s) from the cache, and 173 of 200
measurement(s); 1 listing(s) had to be walked, and are now cached.
```

The live filesystem is never cached — every scan re-stats the live path of every
candidate — so files you delete or edit **outside** this tool are picked up
exactly as before. A file you delete by hand becomes a new candidate on the next
scan, cache or no cache.

Entries are keyed by the subvolume's UUID and `ctransid`, the transid of its
last change. Any modification to a snapshot moves that, so a changed snapshot
stops matching its entry rather than being wrongly reused: the key
self-invalidates. And a stale entry could not cause a wrong deletion in any
case, because `purge` re-validates every target against the filesystem
immediately before unlinking it.

```sh
snapshot-cleaner cache status            # what is cached and what the last scan reused
sudo snapshot-cleaner scan --refresh     # rewalk everything and replace it
sudo snapshot-cleaner cache clear        # empty it; the next scan is a cold one
```

## Why RECLAIM is not just the file size

A 687 MB file present in four snapshots is one set of extents referenced four
times. Removing it from three of the four frees nothing at all; the fourth
snapshot still pins every extent. And under `compress=zstd` the bytes on disk
are well below the apparent size.

`btrfs filesystem du` cannot answer this. Given N paths it prints N independent
rows, and for a file reflinked across snapshots each row reads `Exclusive 0`
with the full size as `set shared` — so the rows sum to zero. There is no
set-wide aggregate.

So the tool computes the union itself, counting each physical extent once:

1. **`BTRFS_IOC_TREE_SEARCH_V2`** reads the file's extent items straight out of
   the subvolume tree, giving true on-disk lengths for compressed extents. This
   is what `compsize` does, in-process and without the dependency.
2. **`FS_IOC_FIEMAP`** is the portable fallback. It reports the *logical* length
   of compressed extents, so those figures are upper bounds; such rows are
   marked `~` rather than quietly overstated.
3. Allocated blocks of the largest copy, as a last resort, also marked `~`.

`doctor` reports which path is in use. Rows shown as `?` were not measured
because they fell outside `--cost-limit`.

The figure assumes nothing *outside* these snapshots references the same
extents. The live path is verified absent and every snapshot in the pair is
enumerated, so the remaining cases are a manual `cp --reflink` or a dedupe tool.

## What purge actually does

For each snapshot holding the file: clear the subvolume's read-only flag, unlink
that one file, restore the flag. The snapshot subvolume itself is never deleted,
recreated, or moved.

Be clear about the trade: **that snapshot can no longer restore that file**, and
your snapshot manager will not know anything happened. Everything else in the
snapshot is untouched.

Safety measures, none of them optional:

- Dry run by default. `--apply` is required, plus typing `yes`.
- Every target is re-validated against the filesystem immediately before
  removal — inode, size and mtime must still match what the scan recorded, and
  the live path must still be absent. Mismatches are skipped and reported.
- Each path component is opened with `O_NOFOLLOW` from the snapshot root's file
  descriptor, so a symlink inside a snapshot cannot redirect the unlink onto the
  live filesystem.
- The read-only flag is restored from a deferred call *and* from the signal
  handler, then verified. If any snapshot is left writable the tool says so
  loudly and tells you how to fix it.
- Snapshots received via `btrfs send` are refused: clearing read-only would
  break their send/receive chain.
- The unit of purge is a file across **all** its holders. Removing it from only
  some frees nothing, so partial selections need `--partial` and are flagged.
- Every deletion is journalled to `journal.jsonl`. Unlinking inside a snapshot
  cannot be undone; this is the only record.

## Supported layouts

Autodetected:

| Provider | Layout |
|---|---|
| `timeshift` | `<top level>/timeshift-btrfs/snapshots/<date>/<subvol>` |
| `snapper` | `<subvol>/.snapshots/<N>/snapshot` |
| `btrbk` | `<snapshot_dir>/<name>.<timestamp>` |

Nothing hardcodes `@` or `@home`; snapshot subvolumes are matched to their live
mount points through `/proc/self/mountinfo`. Snapshots may live on a different
device than the live subvolume.

For anything else, point the tool at it:

```sh
sudo snapshot-cleaner scan --live /home --snapshots /mnt/btr/snapshots/home
```

or declare the pairs once in `/etc/snapshot-cleaner.conf`:

```ini
min-size = 100M
pair = /home : /mnt/btr/snapshots/home
pair = /     : /mnt/btr/snapshots/root
```

`doctor` prints what each provider found and why, so an unrecognised layout
gives you a diagnosis instead of an empty list.

## System impact

A cold scan lists every file in every snapshot, which saturates CPU and floods
the block layer.

This matters more than it sounds. On a test machine every scan produced
a burst of firmware timeouts from a USB wifi adapter - 20 kernel
error events in a single minute during a scan, against isolated events hours
apart otherwise - ending with the adapter re-enumerating on the USB bus. There
were no memory allocation failures, so the cause was scheduling and interrupt
latency, not memory pressure: that driver does synchronous register access over
USB with short firmware timeouts and cannot keep up with a busy machine.

Four things address that, in descending order of how much they help:

1. **The cache.** A snapshot is listed once, ever. After the first scan there is
   almost no work left to do, so the heavy pass stops happening every time.
2. **Reading metadata in leaves, not files.** As root the scan sweeps the
   subvolume tree with `BTRFS_IOC_TREE_SEARCH_V2`, taking packed metadata a
   megabyte per ioctl instead of `readdir` and `lstat` per file. `doctor` says
   which method is in use.
3. **`--cpus N`.** Niceness does not fix an interrupt-latency problem, because a
   niced thread still occupies its core. `--cpus` confines the scan with
   `sched_setaffinity` so a core stays free to service the adapter, and it
   releases CPU 0 first, where an unbalanced interrupt lands by default.
4. **Niceness and I/O priority**, on by default (`nice=10`, lowest best-effort
   I/O). Reclaiming disk space is never more urgent than what the user is doing.

```sh
sudo snapshot-cleaner scan --cpus 7          # leave a core for interrupts
sudo snapshot-cleaner scan --gentle          # idle I/O, nice 19, 2 workers, half the cores
sudo snapshot-cleaner scan --workers 2       # just reduce parallelism
sudo snapshot-cleaner scan --walk readdir    # force the portable walk
```

`--gentle` takes longer but is close to unnoticeable while the machine is in
use.

## Logs

Release builds write no log unless asked:

```sh
snapshot-cleaner scan --debug            # debug level, mirrored to stderr
snapshot-cleaner scan --trace            # every file walked and extent read
snapshot-cleaner scan --log-level info --log-file /tmp/sc.log
```

`make debug` produces `./snapshot-cleaner-debug`, which defaults to `trace`,
always writes a log file, and prints its path on exit. It records the
environment, every btrfs mount, what each provider probed and rejected and why,
per-snapshot walk timings, which measurement method produced each figure and why
any preferred method was rejected, every purge validation decision, and a
counter summary. That is the build to run when something needs diagnosing on a
machine you cannot reach.

## Tests

```sh
make test          # unit tests, no root needed
sudo SNAPSHOT_CLEANER_INTEGRATION=1 make integration
```

The integration tests build a throwaway btrfs filesystem in a loopback image and
exercise the destructive path there. They never touch the host's own snapshots.
They assert the things that matter: that a file reflinked into three snapshots
measures as one file's worth of space and not three, that a partial purge frees
nothing, that the measurement agrees with `compsize` on compressed data, and
that every snapshot is intact and read-only afterwards.

They also assert the two properties the cache rests on: that a cached scan and a
cold one produce identical results, and that the tree sweep and the readdir walk
produce identical listings of the same snapshot. Plus the cases that would make
caching unsafe if they were wrong - a file deleted from the live tree outside the
tool is still reported, a snapshot modified behind the tool's back is rewalked,
and a purge takes its snapshots' cached listings with it.

## Limitations

- btrfs only. Other filesystems are detected and refused.
- Whole-snapshot deletion is out of scope; that is what snapshot managers do.
- Files smaller than `--min-size` (default 50M) are ignored.
- `--exclude` filters the report; it no longer speeds the scan up, because the
  tree sweep reads whole metadata leaves and cannot skip a subtree. It still
  prunes on the `readdir` walk, where that was the only thing it saved.

## Contributing

Bug reports and patches are welcome — see [CONTRIBUTING.md](CONTRIBUTING.md).
Because this tool deletes files, changes to the purge path need an integration
test.

## License

[GPL-3.0-or-later](LICENSE).
