# snapshot-cleaner — Complete Guide

Reclaim disk space held by files that were deleted from a btrfs filesystem but
are still pinned by snapshots.

---

## Table of contents

1. [The problem](#1-the-problem)
2. [Quick start](#2-quick-start)
3. [Installation](#3-installation)
4. [Concepts you need](#4-concepts-you-need)
5. [How the space figure is calculated](#5-how-the-space-figure-is-calculated)
6. [Command reference](#6-command-reference)
6a. [The scan cache](#6a-the-scan-cache)
7. [Reading the scan output](#7-reading-the-scan-output)
8. [Purging safely](#8-purging-safely)
9. [Snapshot layouts](#9-snapshot-layouts)
10. [Configuration](#10-configuration)
11. [Logs and diagnostics](#11-logs-and-diagnostics)
12. [System impact](#12-system-impact)
13. [Troubleshooting](#13-troubleshooting)
14. [If something goes wrong](#14-if-something-goes-wrong)
15. [FAQ](#15-faq)
16. [Development](#16-development)
17. [Limitations](#17-limitations)

---

## 1. The problem

You are low on disk space. You delete a 4 GB VM image. Free space does not move.

This is not a bug, and it is not a stale cache. On btrfs, a snapshot is a
second reference to the same physical extents on disk. Deleting the live copy
removes one reference; every snapshot that still contains the file keeps the
extents alive. The space is released only when the **last** reference goes.

So the file is invisible — it is not in your home directory any more — but it is
still costing you gigabytes, and nothing in a normal toolchain will tell you
which files those are or what each one is worth.

What existing tools do and do not answer:

| Tool | What it tells you |
|---|---|
| `df`, `du` | Space is used. Not by what. |
| `btdu` | Where space went, including snapshots. Not what is safely deletable. |
| `compsize` | Exact size of a set of files you already know about. |
| Timeshift, snapper, btrbk | Delete a *whole snapshot*. All-or-nothing. |

`snapshot-cleaner` answers the missing question: *which deleted files are still
being paid for, how much does each actually cost, and can I get rid of one
without throwing away a restore point?*

It does that by diffing the live filesystem against every snapshot, measuring
the true on-disk cost of each orphaned file, and — if you choose — removing that
one file from every snapshot holding it, leaving the snapshots themselves in
place and read-only.

---

## 2. Quick start

```sh
sudo snapshot-cleaner doctor              # what did it find? read-only
sudo snapshot-cleaner scan --min-size 1G  # rank the offenders. read-only
snapshot-cleaner purge 1                  # dry run. changes nothing
sudo snapshot-cleaner purge 1 --apply     # remove it
sudo snapshot-cleaner journal             # what was removed, and when
```

A real first session on a 932 GB filesystem at 86% full:

```
ID  RECLAIM   APPARENT  SNAPS  KIND     SUBVOL  PATH
1   5.37 GiB  4.27 GiB  1/7    deleted  @home   ...vm-images/dev-vm.img/userdata-qemu.img.qcow2
2   3.62 GiB  3.62 GiB  1/7    deleted  @home   ...vm-images/dev-vm.img/snapshots/default_boot/ram.bin
3   3.04 GiB  3.04 GiB  1/7    deleted  @home   ...vm-images/test-vm.img/snapshots/default_boot/ram.bin
4   1.41 GiB  1.41 GiB  1/7    deleted  @home   ...share/Trash/files/toolchain-2025.1.3.7-linux.deb

Total shown: 13.4 GiB reclaimable across 4 file(s)
```

Purging those four recovered 13.4 GiB. A second pass on stale toolchain jars
and an SDK archive recovered another 3.6 GiB. Total: **~17 GiB**, without deleting a
single snapshot.

---

## 3. Installation

### Build

```sh
git clone <repo> && cd snapshot-cleaner
make build
sudo make install          # installs to /usr/local/sbin/snapshot-cleaner
```

Requires Go 1.25 or newer to build. Nothing at runtime.

### Runtime requirements

**None beyond the kernel.** No `btrfs` CLI, no `compsize`, no Python. The binary
is static (`CGO_ENABLED=0`) and talks to btrfs through ioctls directly. This is
deliberate: `compsize` ships in a separate package (`btrfs-compsize`) that is
absent on most systems, and depending on it would make the tool unportable.

If `btrfs` or `compsize` happen to be installed, `doctor` reports them, but
nothing requires them.

### Other build targets

```sh
make debug         # ./snapshot-cleaner-debug — logs everything by default
make test          # unit tests, no root needed
make integration   # end-to-end tests on a loopback btrfs image (needs root)
make release       # static binaries for linux/amd64 and linux/arm64 in dist/
make clean
```

### Do you need root?

| Command | Root? |
|---|---|
| `doctor`, `snapshots`, `scan`, `journal` | No, but strongly recommended |
| `purge` (dry run) | No — it only reads |
| `purge --apply` | **Yes** |

Without root the tool still runs, but:

- layouts that need the btrfs top level mounted are **not found** (this includes
  every standard Timeshift setup);
- measurement falls back to FIEMAP, so figures over compressed data are upper
  bounds and are marked `~`.

It tells you both rather than pretending. A dry run deliberately does not
require root — you should be able to review what the tool intends to do before
granting it privileges.

---

## 4. Concepts you need

**Subvolume** — an independently snapshottable tree inside a btrfs filesystem.
Typical layouts name them `@` (for `/`) and `@home` (for `/home`).

**Snapshot** — a subvolume that starts as a second reference to another
subvolume's data. Almost always read-only. Cheap to make; it costs nothing until
the original diverges.

**Extent** — a contiguous run of bytes on disk. A file is a list of references
to extents. This is the unit btrfs actually allocates and frees.

**Reflink / shared extent** — when a snapshot is taken, the snapshot's files
point at the *same* extents. Nothing is copied. This is why snapshots are fast
and why deleting the live file frees nothing.

**Holder** — in this tool's language, a snapshot that still contains a given
file. A file with 5 holders is referenced by 5 snapshots, and **removing it from
4 of them frees nothing at all.** This idea drives the entire interface.

**Pair** — a live subvolume together with the snapshots covering it, e.g.
`/home` plus the seven `@home` snapshots under Timeshift's directory. All work
is expressed in terms of pairs, which is what keeps the tool independent of any
particular snapshot manager.

---

## 5. How the space figure is calculated

This is the heart of the tool, and it is worth understanding before you trust a
number it prints.

### Why file size is the wrong answer, twice

**Sharing.** A 687 MB file present in four snapshots is *one* set of extents
referenced four times. Its cost is 687 MB, not 2.7 GB — and you free that 687 MB
only by removing it from all four.

**Compression.** Under `compress=zstd`, a file's on-disk size is often well
below its apparent size. In testing, a 306 MiB Chromium binary occupied
262 MiB. A size-based tool overstates every such row.

Both directions matter. One scan row read `16.0 MiB` reclaim against `512 MiB`
apparent — a sparse VM `sdcard.img`. A size-based tool would have promised half
a gigabyte and delivered sixteen megabytes.

### Why `btrfs filesystem du` cannot answer it

A reasonable first instinct, but it does not work. Given N paths it prints N
independent rows, and for a file reflinked across snapshots every row reads:

```
     Total   Exclusive  Set shared  Filename
 575598592           0   575598592  react.zip
```

`Exclusive 0` for each — so the rows sum to zero. There is no set-wide
aggregate. The union has to be computed directly from the extents.

### What the tool actually does

The question is: *given this set of file copies, how many bytes are freed if all
of them are unlinked?* That is the total size of the physical extents referenced
by the set and by nothing else. Three methods, best first:

| Method | How | Accuracy |
|---|---|---|
| `treesearch` | `BTRFS_IOC_TREE_SEARCH_V2` reads the file's extent items straight out of the subvolume tree, giving true `disk_num_bytes` including compression. Extents are de-duplicated by disk address, so a shared extent counts once. | Exact. This is the `compsize` algorithm, in-process. |
| `fiemap` | `FS_IOC_FIEMAP` enumerates physical extents portably. On compressed extents the kernel reports *logical* length. | Exact for uncompressed data; an upper bound otherwise, marked `~`. |
| `stat` | Allocated blocks of the largest copy. | Rough. Always marked `~`. |

`treesearch` needs root; without it the tool falls back and says so. Run
`doctor` to see which is active.

Validation: on a loopback filesystem the integration suite compares the tool's
figure against `compsize` on compressed data — `ours=540 KiB, compsize=540 KiB`
— and asserts that a file reflinked into three snapshots measures as one file's
worth of space, not three.

### The assumption behind the number

The figure is the bytes this set of copies holds, **freed if nothing outside the
set references the same extents.** The tool verifies the live path is absent and
enumerates every snapshot in the pair, so the remaining cases are a manual
`cp --reflink` or a deduplication tool having created another reference. This
assumption is printed in the report footer, not buried here.

### When RECLAIM exceeds APPARENT

You will sometimes see a reclaim figure *larger* than the file:

```
1   5.37 GiB  4.27 GiB  1/7  deleted  @home  ...userdata-qemu.img.qcow2
```

That is correct. A file rewritten in place — a VM image, a database — ends up
referencing only *parts* of larger extents. The unused remainder of each extent
is still allocated on disk and is released along with the last reference. The
footer explains this whenever a visibly-larger row appears.

---

## 6. Command reference

### Global flags

Available on every command.

| Flag | Default | Meaning |
|---|---|---|
| `--provider NAME` | `auto` | `auto`, `timeshift`, `snapper`, `btrbk` |
| `--live PATH` | | Live subvolume. Requires `--snapshots`. Bypasses detection. |
| `--snapshots PATH` | | Directory whose children are snapshot subvolumes. Requires `--live`. |
| `--config PATH` | | Config file. Default: searches XDG paths then `/etc`. |
| `--state-dir PATH` | | Override where scan state and the journal live. |
| `--runtime-dir PATH` | | Override where the lock and temporary mounts live. |
| `--force` | off | Run even while a snapshot manager is running. |
| `--nice N` | `10` | CPU niceness 0–19. `0` disables. |
| `--cpus N` | all | Confine the scan to `N` cores, leaving the rest free to service interrupts. |
| `--gentle` | off | Idle I/O priority, nice 19, 2 workers, half the cores. |
| `--no-cache` | off | Neither read nor write the scan cache. |
| `--refresh` | off | Ignore cached results, rewalk, and replace them. |
| `--cache-min-size SIZE` | `1M` | Size floor recorded in the cache. Lower means more scans can reuse it. |
| `--walk MODE` | `auto` | `auto`, `treesearch`, `readdir`. How snapshot contents are listed. |
| `--log-level LEVEL` | `off` | `off`, `error`, `warn`, `info`, `debug`, `trace` |
| `--log-file PATH` | | Where to write the log. |
| `--debug` / `--trace` | off | Shorthands, also mirrored to stderr. |
| `-v` | off | Verbose. |

Flags may appear **before or after** positional arguments: `purge 1 --apply` and
`purge --apply 1` are equivalent.

### `doctor`

Read-only diagnosis. Run this first, and whenever something is not found.

```sh
sudo snapshot-cleaner doctor
```

Reports privileges, the resolved state and runtime directories, the config file
in use, which measurement method is available, every btrfs mount with its
filesystem UUID and free space, every detected pair, and — importantly — what
each provider probed and why it came up empty.

### `snapshots`

Lists detected pairs and their snapshots: id, creation time, tag, whether each
is read-only or received, and its path.

```sh
sudo snapshot-cleaner snapshots
sudo snapshot-cleaner snapshots --json
```

### `scan`

Finds and ranks pinned files. Read-only. Writes its results to a state file that
`purge` reads.

| Flag | Default | Meaning |
|---|---|---|
| `--scope NAMES` | `all` | Comma-separated pair names, or `all`. |
| `--min-size SIZE` | `50M` | Ignore smaller files. Accepts `50M`, `1.5G`, `10MiB`, `2GB`, `1024`. |
| `--top N` | `25` | Rows to display. `0` shows all. |
| `--include-replaced` | off | Also report files still present live whose *older* content is pinned. |
| `--exclude GLOB` | | Skip matching paths. Repeatable. |
| `--cost-limit N` | `200` | Measure at most this many candidates. `0` measures all. |
| `--workers N` | auto | Snapshots walked in parallel. Auto is `min(NumCPU, 8)`. |
| `--json` | off | Machine-readable output. |

```sh
sudo snapshot-cleaner scan
sudo snapshot-cleaner scan --scope @home --min-size 500M
sudo snapshot-cleaner scan --exclude '*.log' --exclude 'cache' --top 0
sudo snapshot-cleaner scan --json > pinned.json
```

Only the largest `--cost-limit` candidates are measured; the rest show `?`.
Measuring everything would mean reading extent maps for thousands of files to
rank a handful.

A scan reports what it reused when it finishes:

```
Reused 14 of 15 snapshot listing(s) from the cache, and 173 of 200
measurement(s); 1 listing(s) had to be walked, and are now cached.
```

That one walked listing is the snapshot your manager took since the last scan.
See [§ 6a](#6a-the-scan-cache) for why reusing the other fourteen is safe.

Note that `--exclude` is an output filter, not a shortcut. It no longer prunes
the walk, because the tree sweep reads whole metadata leaves and cannot skip a
subtree. The exception is the `readdir` walk, where pruning is the only thing
that ever made `--exclude` faster: there it still prunes, and the resulting
listing is marked incomplete so it is never cached.

---

## 6a. The scan cache

### Why a scan can be cached at all

A snapshot subvolume is **read-only and immutable**. Once it has been walked,
its contents cannot change for as long as it exists. The expensive half of a
scan — listing every file in every snapshot, then reading extent maps for the
largest of them — is therefore a pure function of an input that cannot move, and
the answer keeps.

So the first scan is a cold one, and every scan after it reuses all of that
except for whatever snapshot your manager has taken since.

### What is not cached

The live filesystem. Every scan re-`lstat`s the live path of every candidate,
because that is the half that genuinely changes. Deleting and editing files
outside this tool is the normal case and works exactly as it did before:

| You do this | What the next scan does |
|---|---|
| Delete a live file by hand | Re-stats the live path, finds it gone, reports it as a new `deleted` candidate. |
| Edit a live file | Compares a fresh live stat against the snapshot copies. The snapshot side cannot have moved, so the comparison is still right. |
| Delete a file *inside* a snapshot by hand | Modifying the subvolume tree moves its `ctransid`, so the cache key stops matching and the snapshot is rewalked. |
| Let your manager rotate snapshots | Entries are keyed by subvolume UUID, not path, so a recycled snapshot directory name cannot collide. GC drops what is gone. |

### How entries are keyed

`<filesystem UUID>/<subvolume UUID>/<ctransid>`, where `ctransid` is the transid
of the last change to that subvolume's tree, read with
`BTRFS_IOC_GET_SUBVOL_INFO`.

The point of including `ctransid` is that the key **self-invalidates**. Any
modification of a snapshot moves it, so a changed snapshot cannot match its old
entry. There is no invalidation logic to forget to call.

Three further guards, none of them load-bearing on their own:

- A snapshot found **not read-only** is rewalked whatever its transid says. That
  catches someone clearing the flag by hand to edit inside a snapshot.
- A snapshot the kernel will not identify (no UUID, on an old kernel or without
  privileges) is simply never cached, rather than keyed on something weaker.
- `purge` explicitly drops the entries for every snapshot it wrote to.

And a stale entry could not cause a wrong deletion in any case: `purge`
re-validates every target's inode, size and mtime against the filesystem
immediately before unlinking it, and re-checks that the live path is still
absent. A stale cache can only produce a wrong *report*.

### The floor, and why it is below `--min-size`

Manifests record every file at or above `--cache-min-size` (default 1 MiB),
regardless of the `--min-size` the scan was run with, and `--min-size` is
applied when the manifest is read back.

This costs nothing. The walk visits every file whatever the threshold is;
`--min-size` only decides which ones get reported. Recording at a lower floor
buys manifest bytes, not walk time — and it means a later
`scan --min-size 10M` still hits the cache instead of rewalking everything.

An entry is reused only when its floor is at or below the requested
`--min-size`. Ask for smaller files than the manifest holds and it misses, which
is correct: the manifest does not contain them.

### `cache`

```sh
snapshot-cleaner cache status      # what is cached, and what the last scan reused
sudo snapshot-cleaner cache gc     # drop entries for snapshots that no longer exist
sudo snapshot-cleaner cache clear  # empty it; the next scan will be cold
```

`gc` also runs automatically after every successful scan. Clearing the cache is
never necessary for correctness — it costs one slow scan and gains nothing — but
it is there.

The cache lives beside the scan state, so a scan run as root
(`/var/lib/snapshot-cleaner/cache.db`) and one run as your user
(`~/.local/state/snapshot-cleaner/cache.db`) keep separate caches, exactly as
they already keep separate `last-scan.json` files.

---

### `purge`

Removes selected files from the snapshots holding them.

| Flag | Default | Meaning |
|---|---|---|
| `--apply` | off | Actually remove. **Without this it is a dry run.** |
| `--yes` | off | Skip the typed confirmation. |
| `--interactive` | off | Choose from a checklist. |
| `--partial` | off | Allow removing from only some holders. |

```sh
snapshot-cleaner purge 3                  # dry run
snapshot-cleaner purge 1,3,7-9            # dry run, ids and ranges
sudo snapshot-cleaner purge 1-4 --apply
sudo snapshot-cleaner purge --interactive --apply
```

Ids come from the last `scan` and change every time you rescan. Always read the
dry run.

**Interactive keys:** `↑`/`↓` or `k`/`j` move · `Space` toggles · `a` selects all ·
`n` clears · `/` filters · `g`/`G` jump to top/bottom · `Enter` confirms ·
`q` cancels. The footer shows a running total of what you have selected.

### `journal`

Every deletion, ever. Unlinking inside a snapshot cannot be undone, so this is
the only record that the file was ever there.

```sh
sudo snapshot-cleaner journal
sudo snapshot-cleaner journal --tail 0     # everything
```

Stored as JSON Lines at `<state-dir>/journal.jsonl` — one object per deletion
with timestamp, snapshot, path, size, estimate and result.

### `version`, `help`

```sh
snapshot-cleaner version
snapshot-cleaner help
snapshot-cleaner <command> -h
```

Exit status is `0` on success and `1` on any error, including a cancelled
interactive selection.

---

## 7. Reading the scan output

```
ID  RECLAIM   APPARENT  SNAPS  KIND      SUBVOL  PATH
1   681 MiB   681 MiB   5/7    deleted   @home   user/toolkit-desktop-v2026.7.1.jar
7   16.0 MiB  512 MiB   1/7    deleted   @home   user/.local/vm/.../sdcard.img
9   ~204 MiB  238 MiB   3/8    deleted   @       .../share/spotify/libcef.so
12  ?         306 MiB   2/8    replaced  @       .../chromium/chrome
```

| Column | Meaning |
|---|---|
| `ID` | Selector for `purge`. **Valid only until the next scan.** |
| `RECLAIM` | Bytes freed by removing the file from **all** its holders. The sort key. |
| `APPARENT` | The file's nominal size. |
| `SNAPS` | Holders / total snapshots in the pair. `5/7` means five snapshots contain it. |
| `KIND` | `deleted` — gone from the live tree. `replaced` — still live, but the snapshot holds older content. |
| `SUBVOL` | Which pair. |
| `PATH` | Path relative to the subvolume root. |

Markers:

- `~` — an estimate. Either FIEMAP over compressed extents (an upper bound) or
  the `stat` fallback.
- `?` — not measured, because it fell outside `--cost-limit`.

**What is deliberately *not* listed:** files that exist in a snapshot and are
byte-identical to the live file. They share every extent with the live copy, so
removing them from snapshots frees nothing. On one real scan, 171 such
path-groups were correctly discarded.

---

## 8. Purging safely

### What it does

For each snapshot holding the file: clear the subvolume's read-only flag, unlink
that one file, restore the flag. The snapshot is never deleted, recreated, or
moved. It stays where it was, keeps its name and creation time, and your
snapshot manager still lists it.

### What you are giving up

**That snapshot can no longer restore that file, and your snapshot manager will
not know.** Everything else in the snapshot is untouched. Rolling back to it
gives you the system as it was, minus the file you deleted.

For `/home`, the risk is generally low — nothing there is needed to make a
restored system boot. For a root subvolume it deserves more thought: if you
purge, say, a Flatpak or Chromium payload from a system snapshot and later roll
that snapshot back, the restored system will have the application installed but
missing its binaries.

### The safety machinery

- **Dry run by default.** `--apply` is required, *and* you must type `yes`.
- **Everything is re-validated immediately before removal.** A scan may be hours
  old and snapshot managers rotate on timers. Each target must still exist with
  the same inode, size and mtime, and the live path must still be absent.
  Mismatches are skipped and reported, never guessed at.
- **Symlinks cannot escape.** Each path component is opened with `O_NOFOLLOW`
  from the snapshot root's file descriptor, so a symlink planted inside a
  snapshot cannot redirect the unlink onto live data.
- **The read-only flag is always restored** — from a deferred call *and* from the
  signal handler, so Ctrl-C is safe — then verified. If any snapshot is left
  writable the tool says so loudly and prints the command to fix it.
- **Received snapshots are refused.** Clearing read-only on a snapshot from
  `btrfs send`/`receive` would break its incremental chain.
- **All holders or none.** Removing a file from some of its holders frees
  nothing, so a candidate whose copies cannot all be validated is skipped
  entirely unless you pass `--partial`.
- **Everything is journalled**, flushed to disk per entry, so an interrupted run
  still leaves a record of what was already removed.

### Free space is released asynchronously

btrfs frees extents in the background. Reading free space the instant the unlink
returns undercounts badly — a real 13.4 GiB reclaim read as 7.11 GiB at that
moment and reached the full figure seconds later. The tool now waits for the
number to stop moving before reporting, and tells you if it gave up waiting.

```
Free space: 152 GiB -> 156 GiB (+3.62 GiB, estimated 3.62 GiB)
```

---

## 9. Snapshot layouts

Autodetected, in this order:

| Provider | Layout | Metadata |
|---|---|---|
| `timeshift` | `<btrfs top level>/timeshift-btrfs/snapshots/<date>/<subvol>` | `info.json` |
| `snapper` | `<subvol>/.snapshots/<N>/snapshot` | `info.xml` |
| `btrbk` | `<snapshot_dir>/<name>.<timestamp>` | `/etc/btrbk/btrbk.conf` |

Nothing hardcodes `@` or `@home`. Snapshot subvolumes are matched back to their
live mount points through `/proc/self/mountinfo`, so any naming works. Snapshots
may live on a different device from the live subvolume; pairs are validated by
filesystem UUID, because extents cannot be shared across filesystems.

Timeshift's rsync mode is detected and reported as not applicable — those
snapshots are file copies, not reflinks, and nothing here applies.

### Unrecognised layouts

Point the tool at it directly:

```sh
sudo snapshot-cleaner scan --live /home --snapshots /mnt/btr/snapshots/home
```

`--snapshots` is the directory whose *immediate children* are the snapshot
subvolumes. Or declare pairs once in the config file (below).

---

## 10. Configuration

Optional. Searched in order:

1. `$XDG_CONFIG_HOME/snapshot-cleaner/config`
2. `~/.config/snapshot-cleaner/config`
3. `/etc/snapshot-cleaner.conf`

Command-line flags override the file.

```ini
# Ignore files smaller than this.
min-size = 100M

# Size floor recorded in the scan cache. Lower means more future scans can
# reuse it; it costs cache bytes, not scan time. Default 1M.
cache-min-size = 1M

# Force one provider instead of trying all.
provider = snapper

# Declare pairs by hand. Any pair here disables autodetection entirely.
pair = /home : /mnt/btr/snapshots/home
pair = /     : /mnt/btr/snapshots/root
```

An unknown key or a malformed line is an error, not a silent skip.

### Where files live

| What | Default | Fallbacks |
|---|---|---|
| Scan state, cache, journal, logs | `/var/lib/snapshot-cleaner` | `$XDG_STATE_HOME`, `~/.local/state`, `$TMPDIR` |
| Lock, temporary mounts | `/run/snapshot-cleaner` | `$XDG_RUNTIME_DIR`, `$TMPDIR` |

First writable location wins. Override with `--state-dir` and `--runtime-dir`.

---

## 11. Logs and diagnostics

Release builds write nothing unless asked:

```sh
snapshot-cleaner scan --debug                       # mirrored to stderr
snapshot-cleaner scan --trace                       # every file and extent
snapshot-cleaner scan --log-level info --log-file /tmp/sc.log
```

`make debug` produces `./snapshot-cleaner-debug`, which defaults to `trace`,
always writes a log file, and prints its path on exit. Use it when something
needs diagnosing on a machine you cannot reach. It records:

- environment, kernel, distro, uid, CPU count;
- every btrfs mount with filesystem UUID and free space;
- what each provider probed, and why it rejected what it rejected;
- per-snapshot walk timings and file counts;
- **which measurement method produced each figure, and why any preferred method
  was rejected**;
- every purge validation decision, including each skip and its reason;
- a counter summary.

Trace output is budgeted so a scan of millions of inodes cannot fill the disk it
is trying to free; it says when it stops.

A typical counter summary:

```
diff.deleted                     37
diff.identical_to_live          171
diff.replaced_skipped             9
measure.treesearch               37
walk.files_over_threshold      1054
walk.snapshots                   15
```

---

## 12. System impact

### The problem

A cold scan lists every file in every snapshot. With several snapshots of a
large home directory that is a great many metadata reads, and it saturates CPU
and floods the block layer.

This matters more than it sounds. On a test machine, every scan
produced a burst of firmware timeouts from a USB wifi adapter —
20 kernel error events in one minute during a scan, against isolated events
hours apart otherwise — ending with the adapter re-enumerating on the USB bus.
There were no memory allocation failures, so the cause was scheduling and
interrupt latency, not memory pressure: that driver does synchronous register
access over USB with short firmware timeouts and cannot keep up with a busy
machine.

Four things address that, in descending order of how much they help.

### 1. Not doing the work at all

The [scan cache](#6a-the-scan-cache) means a snapshot is listed once, ever.
After the first scan, a rescan reads the listings back from a file and touches
the snapshots not at all; the only new work is whatever snapshot your manager
has taken since. The heavy scan stops being something you do every time and
becomes something that happens once a day, for one snapshot.

### 2. Reading metadata in leaves rather than in files

Where it is permitted (it needs `CAP_SYS_ADMIN`, so in practice root), the scan
lists a snapshot with a `BTRFS_IOC_TREE_SEARCH_V2` sweep of the subvolume tree
instead of `readdir` and `lstat`. The kernel hands over packed metadata leaves a
megabyte at a time, so one ioctl replaces thousands of syscalls, and the reads
are sequential rather than scattered. `doctor` reports which method is in use;
`--walk readdir` forces the old one.

### 3. Leaving a core free for interrupts

```sh
sudo snapshot-cleaner scan --cpus 7      # on an 8-core machine
```

Niceness does not fix an interrupt-latency problem: a niced thread still
occupies its core. `--cpus` confines the scan's threads with
`sched_setaffinity`, so there is a core that is genuinely free to service the
adapter's interrupt. The cores are taken from the top of whatever set the
process is already allowed — composing with `taskset` and cgroup cpusets rather
than overriding them — so CPU 0, where an unbalanced interrupt lands by default,
is the first one released.

### 4. Yielding CPU and disk

The tool runs **niced and at low I/O priority by default** (`nice=10`, lowest
best-effort I/O). Reclaiming disk space is never more urgent than what you are
doing.

```sh
sudo snapshot-cleaner scan --gentle       # idle I/O, nice 19, 2 workers, half the cores
sudo snapshot-cleaner scan --workers 2
sudo snapshot-cleaner scan --nice 19
```

### Measuring it on your own machine

```sh
journalctl -kf | grep --line-buffered -i 'usb\|firmware' &   # your flaky driver here
time sudo snapshot-cleaner scan --refresh          # a forced cold scan
time sudo snapshot-cleaner scan                    # the same scan, warm
```

For reference, on a 932 GB filesystem with 15 snapshots at 8 workers, a full
cold scan of both `@` and `@home` with the `readdir` walk took about 133
seconds. `--gentle` takes longer but is close to unnoticeable while the machine
is in use.

---

## 13. Troubleshooting

**"no usable snapshots found"**
Run `doctor`. It lists what each provider probed and why it found nothing. The
most common causes are not running as root (Timeshift layouts need the btrfs top
level mounted) and a layout no provider recognises — use `--live`/`--snapshots`.

**"this needs root"**
Only `purge --apply` requires it. Everything else, dry runs included, works
unprivileged with reduced capability.

**"another snapshot-cleaner is already running"**
A lock file prevents two runs from working on the same snapshots. If you are
certain no other run exists, remove `<runtime-dir>/lock`.

**"timeshift is running; it may create or delete snapshots underneath us"**
Wait for it, or pass `--force` if you know it is idle.

**"no scan found — run `snapshot-cleaner scan` first"**
`purge` reads the last scan. A successful purge deletes the state file, since it
then describes files that no longer exist. Rescan.

**Everything shows `~` and `doctor` says "FIEMAP fallback"**
`TREE_SEARCH_V2` needs root. Re-run with `sudo` for exact figures.

**Free space did not move**
Two possibilities. Either btrfs has not finished releasing the extents — wait a
few seconds and check `df -h` — or you removed the file from only *some* of its
holders. Check the `SNAPS` column: the count must reach zero.

**A row shows `RECLAIM` larger than `APPARENT`**
Expected for files rewritten in place. See
[section 5](#when-reclaim-exceeds-apparent).

**The scan is slow**
It is bound by btrfs metadata reads. Raise `--min-size`, narrow `--scope`, or
raise `--workers`. Note that raising workers increases system impact.

---

## 14. If something goes wrong

### A snapshot was left writable

The tool verifies this and shouts if it happens. Restore it with:

```sh
sudo btrfs property set -ts /path/to/snapshot ro true
```

The snapshot is otherwise intact. This is the only state that needs manual
repair, and the tool prints the exact path.

### A stale mount was left behind

Mounts live under `<runtime-dir>/mnt/` and are removed on exit, including on
Ctrl-C. If one survives a hard kill:

```sh
mount | grep snapshot-cleaner
sudo umount /run/snapshot-cleaner/mnt/top-XXXXXX
```

It is a read-only mount, so nothing can have been damaged through it.

### You purged something you needed

There is no undo. A file unlinked from a snapshot is gone from that snapshot.
The journal records what was removed:

```sh
sudo snapshot-cleaner journal --tail 0
```

If the file still exists in a *different* snapshot that you did not purge, copy
it back from there. If it existed in your live tree until recently, check your
Trash. Otherwise it is gone — which is why the dry run is the default and the
confirmation is a typed `yes`.

---

## 15. FAQ

**Does this delete my snapshots?**
No. It never deletes, creates or moves a snapshot. Whole-snapshot deletion is
deliberately not implemented — your snapshot manager already does that.

**Will Timeshift/snapper notice?**
No. They will list the snapshot exactly as before. They have no idea a file was
removed from inside it.

**Is it safe to run while Timeshift is scheduled?**
The tool refuses to run while a snapshot manager is active, and takes a lock.
A scan is read-only regardless.

**Can I run it on a mounted, live system?**
Yes. That is the intended use. It never modifies the live filesystem.

**What about files smaller than `--min-size`?**
Ignored. Thousands of small files can add up, but each one is individually not
worth the risk of removing from a restore point. Lower the threshold if you
disagree.

**Why does it need to mount anything?**
Snapshot managers usually keep snapshots outside any mounted subvolume, on the
btrfs top level (`subvolid=5`). The tool mounts that read-only in a private
directory and unmounts it on exit. If the top level is already mounted, it reuses
that instead.

**Can I automate it?**
Scanning, yes — `--json` gives machine-readable output. Purging, please do not.
It is irreversible and the whole design assumes a human reads the dry run.

**Does it work on ZFS / ext4 / XFS?**
No. btrfs only; other filesystems are detected and refused with a clear message.

**Why Go and not a shell script?**
The measurement requires ioctls (`TREE_SEARCH_V2`, `FIEMAP`) and safe path
handling (`openat` with `O_NOFOLLOW`) that shell cannot do. And a tool that
unlinks files inside snapshots as root should be testable.

---

## 16. Development

```
main.go            subcommand dispatch, flags, preflight
btrfsioctl.go      ioctl ABI: FIEMAP, TREE_SEARCH_V2, SUBVOL_[GS]ETFLAGS, INO_LOOKUP, FS_INFO
extents.go         extent-set accounting — the reclaim figure
mountinfo.go       /proc/self/mountinfo parsing
mount.go           temporary subvolid=5 mount, cleanup on defer and signal
provider*.go       layout detection: timeshift, snapper, btrbk, generic
scan.go            parallel snapshot walk, live diff, candidate grouping
purge.go           revalidation, containment, ro toggle, unlink, verify
report.go          state file, table and JSON rendering
picker.go          interactive multi-select
journal.go         append-only deletion record
priority.go        nice and I/O priority
log.go             leveled logging
cleanup.go         reseal registry and signal handling
```

### Tests

```sh
make test                                    # unit, no root
sudo SNAPSHOT_CLEANER_INTEGRATION=1 make integration
go test -race ./...
```

Integration tests build a throwaway btrfs filesystem in a loopback image and
exercise the destructive path there. They never touch the host's snapshots. They
assert what actually matters: that a file reflinked into three snapshots
measures as one file's worth of space, that a partial purge frees nothing, that
the figure agrees with `compsize` on compressed data, and that every snapshot is
intact and read-only afterwards.

For the cache they assert the two properties it rests on — a cached scan and a
cold one produce identical results, and the tree sweep and the readdir walk
produce identical listings of the same snapshot — plus the cases that would make
caching unsafe if they were wrong: a file deleted from the live tree outside the
tool is still reported, a snapshot modified behind the tool's back is rewalked
(and its `ctransid` is asserted to have actually moved), and a purge takes its
snapshots' cached listings with it.

The packed inode-item offsets the tree sweep depends on are checked in
`treewalk_test.go` against an item built field by field from the layout in
`btrfs_tree.h`. Getting one wrong would make every reported size and mtime
silently wrong, which is the same reason the ioctl numbers are pinned:

The ioctl request numbers and struct layouts are pinned in `ioctl_test.go`
against values produced by the kernel headers. Getting one wrong yields silently
wrong answers rather than a build error, so they are asserted rather than
trusted. Regenerate with a C program including `<linux/btrfs.h>`.

> Note the two read-only flags, which are easy to conflate and were the source
> of a real bug: `BTRFS_SUBVOL_RDONLY` (bit 1) is used by
> `SUBVOL_GETFLAGS`/`SETFLAGS`, while `BTRFS_ROOT_SUBVOL_RDONLY` (bit 0) is what
> `GET_SUBVOL_INFO` reports.

---

## 17. Limitations

- **btrfs only.**
- **No undo.** Unlinking from a snapshot is permanent. The journal is a record,
  not a backup.
- **No whole-snapshot deletion**, by design.
- **`--min-size` hides small files.** Default 50 MB.
- **`--exclude` filters the report, it does not speed up the scan.** The tree
  sweep reads whole metadata leaves and cannot skip a subtree. It still prunes
  on the `readdir` walk, and a pruned listing is never cached.
- **`--cost-limit` leaves a tail unmeasured**, shown as `?`. Default 200.
- **The reclaim figure assumes no external references** to the same extents —
  no manual `cp --reflink`, no deduplication tool. Stated in every report.
- **The first scan is I/O heavy**, and so is the first scan of each new
  snapshot. Every scan after that is served from the cache. See
  [section 12](#12-system-impact).
- **The tree sweep needs root.** Without `CAP_SYS_ADMIN` the scan falls back to
  the `readdir` walk, which is correct but far more expensive. `doctor` says
  which is in use.
- **Snapshots on a different filesystem from the live subvolume are dropped**,
  since extents cannot be shared across filesystems and the arithmetic would be
  meaningless.
