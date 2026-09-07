#!/usr/bin/env bash
#
# selftest.sh - build a throwaway btrfs fixture, drive every read-only code
# path across it, and leave behind one log that explains what happened.
#
# The fixture is built with unprivileged `btrfs subvolume create`, so this
# needs no sudo. It never touches the real filesystem, the real snapshots or
# the user's own scan cache: every path lives under $SELFTEST_DIR and the
# state and runtime directories are overridden to point there too.
#
#   ./scripts/selftest.sh              # build fixture, run everything
#   SELFTEST_REUSE=1 ./scripts/selftest.sh   # keep the fixture, re-run only
#   SELFTEST_APPLY=1 ./scripts/selftest.sh   # also do a real --apply purge
#   SELFTEST_CLEAN=1 ./scripts/selftest.sh   # tear the fixture down at the end
#
# Two files come out:
#   selftest-summary.log  small; the transcript, exit codes and timings
#   selftest.log          large; the above plus every trace line the tool wrote
#
# Failing steps do not stop the run - the point is to collect evidence, and a
# step that fails halfway is usually the most informative part of the log.

set -uo pipefail

REPO=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
WORK=${SELFTEST_DIR:-${TMPDIR:-/tmp}/snapshot-cleaner-selftest}
LOG=$WORK/selftest.log
SUMMARY=$WORK/selftest-summary.log

# Fixture shape. Overridable so a slow machine can shrink it; the defaults are
# sized so the folder view beats the file view by a wide, obvious margin.
SMALL_N=${SELFTEST_SMALL_N:-3000}     # files in the deleted project tree
SMALL_SZ=${SELFTEST_SMALL_SZ:-28672}  # 28 KiB each
PHOTO_N=${SELFTEST_PHOTO_N:-200}
PHOTO_SZ=${SELFTEST_PHOTO_SZ:-294912} # 288 KiB each
ISO_MB=${SELFTEST_ISO_MB:-160}
CLIP_MB=${SELFTEST_CLIP_MB:-48}
SNAPS_N=3

LIVE=$WORK/btr/live
SNAPS=$WORK/btr/snaps
STATE=$WORK/state
RUN=$WORK/run
BIN=$WORK/snapshot-cleaner-debug

step=0
unexpected=0
expected_fail=0

say()  { printf '%s\n' "$*" | tee -a "$SUMMARY" >>"$LOG"; }
term() { printf '%s\n' "$*"; }
both() { term "$*"; say "$*"; }

die() { term "selftest: $*"; exit 1; }

# ---------------------------------------------------------------- preflight

command -v btrfs >/dev/null || die "btrfs-progs is not installed"
command -v go    >/dev/null || die "go is not installed"

mkdir -p "$WORK" || die "cannot create $WORK"
fstype=$(stat -f -c %T "$WORK")
[ "$fstype" = "btrfs" ] || die "$WORK is on $fstype, not btrfs.
Point SELFTEST_DIR at a directory on a btrfs filesystem, e.g.
  SELFTEST_DIR=/home/\$USER/.cache/snapshot-cleaner-selftest $0"

# ------------------------------------------------------------------ fixture

subvol_rm() {
    # Only ever inside $WORK, and only things that really are subvolumes.
    case $1 in "$WORK"/*) ;; *) die "refusing to delete $1: outside $WORK" ;; esac
    [ -d "$1" ] || return 0
    btrfs property set -ts "$1" ro false >/dev/null 2>&1
    btrfs subvolume delete "$1" >/dev/null 2>&1 || rm -rf "$1"
}

teardown() {
    for s in "$SNAPS"/*; do subvol_rm "$s"; done
    subvol_rm "$LIVE"
    rm -rf "$WORK/btr" "$STATE" "$RUN"
}

build_fixture() {
    term "building fixture in $WORK (this writes ~$((ISO_MB + CLIP_MB + 120)) MiB of random data)"
    teardown
    mkdir -p "$WORK/btr" "$SNAPS"
    btrfs subvolume create "$LIVE" >/dev/null \
        || die "unprivileged 'btrfs subvolume create' failed.
This kernel or filesystem does not allow it, so the fixture needs root:
  sudo $0"

    mkdir -p "$LIVE"/user/{projects,Pictures/import-2025,Downloads,Videos,restored,archive}

    # A tree of many small files: no single file is big, the tree is. This is
    # the case the file view structurally cannot see.
    ( cd "$LIVE/user/projects" \
      && head -c $((SMALL_N * SMALL_SZ)) /dev/urandom \
         | split -b "$SMALL_SZ" -a 5 -d - obj- )

    # A photo import whose parent directory survives on the live tree: the
    # rollup must stop here and not climb to user/Pictures.
    ( cd "$LIVE/user/Pictures/import-2025" \
      && head -c $((PHOTO_N * PHOTO_SZ)) /dev/urandom \
         | split -b "$PHOTO_SZ" -a 4 -d - img- )
    head -c 1024 /dev/urandom > "$LIVE/user/Pictures/keep-me.jpg"

    # Two large files, which the file view does find - the baseline to beat.
    head -c $((ISO_MB * 1024 * 1024))  /dev/urandom > "$LIVE/user/Downloads/big.iso"
    head -c $((CLIP_MB * 1024 * 1024)) /dev/urandom > "$LIVE/user/Videos/clip.mkv"

    # A reflinked copy: the two share every extent, so a correct union counts
    # the space once and a naive sum reports double.
    cp --reflink=always "$LIVE/user/Downloads/big.iso" "$LIVE/user/Downloads/big-copy.iso"

    # Files that come back on the live tree after the snapshots are taken.
    # They must be refused at purge time, not deleted from the snapshots.
    head -c $((4 * 1024 * 1024)) /dev/urandom > "$LIVE/user/restored/data.bin"
    head -c $((6 * 1024 * 1024)) /dev/urandom > "$LIVE/user/archive/old.tar"

    # A file that is never deleted: it must never appear in any output.
    head -c $((12 * 1024 * 1024)) /dev/urandom > "$LIVE/user/Videos/kept.mkv"
    sync

    for i in $(seq 1 $SNAPS_N); do
        btrfs subvolume snapshot -r "$LIVE" "$SNAPS/2026-09-0$i" >/dev/null \
            || die "snapshot $i failed"
    done

    # Now delete from live. Everything below is pinned by the snapshots only.
    rm -rf "$LIVE/user/projects"                # whole tree gone
    rm -rf "$LIVE/user/Pictures/import-2025"    # gone, parent survives
    rm -f  "$LIVE/user/Downloads/big.iso" "$LIVE/user/Downloads/big-copy.iso"
    rm -f  "$LIVE/user/Videos/clip.mkv"
    rm -rf "$LIVE/user/restored" "$LIVE/user/archive"

    # ...and put two of them back, differently: one as a file, one as a whole
    # directory. Each exercises a different refusal path in ExpandFolder.
    mkdir -p "$LIVE/user/restored"
    head -c $((4 * 1024 * 1024)) /dev/urandom > "$LIVE/user/restored/data.bin"
    mkdir -p "$LIVE/user/archive"
    head -c 4096 /dev/urandom > "$LIVE/user/archive/old.tar"
    sync
}

# ---------------------------------------------------------------- run steps

# run_fails marks a step whose non-zero exit IS the result being checked, so
# a real regression elsewhere still stands out in the tally at the end.
run_fails() { want_fail=1 run "$@"; }

run() {
    local desc=$1; shift
    local expect_fail=${want_fail:-0}
    want_fail=0
    step=$((step + 1))
    local tag
    tag=$(printf '%02d' "$step")
    local out=$WORK/.step.out

    {
        echo
        echo "##### [$tag] $desc #####"
        echo "\$ $*"
    } | tee -a "$SUMMARY" >>"$LOG"
    term "[$tag] $desc"

    local start end rc
    start=$(date +%s.%N)
    "$@" >"$out" 2>&1
    rc=$?
    end=$(date +%s.%N)

    cat "$out" >>"$LOG"
    # The summary keeps short output whole and truncates anything long; the
    # full text is always in $LOG a few lines above the same banner.
    if [ "$(wc -l <"$out")" -gt 60 ]; then
        head -40 "$out" >>"$SUMMARY"
        echo "  [... $(($(wc -l <"$out") - 40)) more lines, see selftest.log]" >>"$SUMMARY"
    else
        cat "$out" >>"$SUMMARY"
    fi

    local dur
    dur=$(awk -v a="$start" -v b="$end" 'BEGIN{printf "%.3f", b-a}')
    local note=""
    if [ "$expect_fail" = 1 ]; then
        if [ $rc -ne 0 ]; then
            note="  (expected)"
            expected_fail=$((expected_fail + 1))
        else
            note="  (EXPECTED A NON-ZERO EXIT AND DID NOT GET ONE)"
            unexpected=$((unexpected + 1))
            term "     exit=0 but this step is supposed to fail  (see [$tag])"
        fi
    elif [ $rc -ne 0 ]; then
        unexpected=$((unexpected + 1))
        term "     exit=$rc  (see [$tag] in the log)"
    fi
    printf -- '--- [%s] exit=%d in %ss%s\n' "$tag" "$rc" "$dur" "$note" | tee -a "$SUMMARY" >>"$LOG"
    return 0
}

# ---------------------------------------------------------------------- go

term "snapshot-cleaner selftest"
term "work dir: $WORK"
term

if [ "${SELFTEST_REUSE:-0}" = 1 ] && [ -d "$LIVE" ]; then
    term "reusing the existing fixture (SELFTEST_REUSE=1)"
else
    build_fixture
fi
mkdir -p "$STATE" "$RUN"

: >"$LOG"
: >"$SUMMARY"

{
    echo "snapshot-cleaner selftest"
    echo "date:     $(date -Is)"
    echo "host:     $(uname -srm)"
    echo "go:       $(go version)"
    echo "btrfs:    $(btrfs --version)"
    echo "git:      $(cd "$REPO" && git describe --tags --always --dirty 2>/dev/null) on $(cd "$REPO" && git rev-parse --abbrev-ref HEAD 2>/dev/null)"
    echo "work dir: $WORK"
    echo "fixture:  $SMALL_N x $((SMALL_SZ / 1024))K in user/projects, $PHOTO_N x $((PHOTO_SZ / 1024))K in user/Pictures/import-2025,"
    echo "          ${ISO_MB}M big.iso (+ a reflinked copy), ${CLIP_MB}M clip.mkv, $SNAPS_N read-only snapshots"
    echo "mount:    $(findmnt -no FSTYPE,OPTIONS --target "$WORK" 2>/dev/null)"
} | tee -a "$SUMMARY" >>"$LOG"

run "go vet"          go -C "$REPO" vet ./...
run "gofmt -l ."      bash -c "cd '$REPO' && gofmt -l . | tee /dev/stderr | wc -l"
run "go test ./..."   go -C "$REPO" test ./...
run "build debug binary" bash -c "cd '$REPO' && make debug && cp snapshot-cleaner-debug '$BIN'"

[ -x "$BIN" ] || die "the debug binary was not built; see [04] in $SUMMARY"

C=(--live "$LIVE" --snapshots "$SNAPS" --state-dir "$STATE" --runtime-dir "$RUN"
   --log-file "$LOG" --log-level trace)

run "version"                       "$BIN" version
run "doctor"                        "$BIN" doctor "${C[@]}"
run "snapshots"                     "$BIN" snapshots "${C[@]}"

run "file view, 50M floor"          "$BIN" scan "${C[@]}" --min-size 50M --refresh
run "file view, 1M floor"           "$BIN" scan "${C[@]}" --min-size 1M --top 0
run "file view as JSON"             "$BIN" scan "${C[@]}" --min-size 50M --json

run "folder view, cold cache"       "$BIN" scan "${C[@]}" --folders --min-size 10M --refresh
run "folder view, warm cache"       "$BIN" scan "${C[@]}" --folders --min-size 10M
run "folder view, 1M floor"         "$BIN" scan "${C[@]}" --folders --min-size 1M --top 0
run "folder view, sampled"          "$BIN" scan "${C[@]}" --folders --min-size 10M --folder-sample 50 --refresh
run "folder view, file floor 100K"  "$BIN" scan "${C[@]}" --folders --min-size 1M --file-min-size 100K --refresh
run "folder view as JSON"           "$BIN" scan "${C[@]}" --folders --min-size 10M --json
run "folder view, excluded tree"    "$BIN" scan "${C[@]}" --folders --min-size 1M --exclude 'user/projects' --refresh
run "cache status"                  "$BIN" cache status "${C[@]}"

# Leave a folder scan as the saved state, then drive purge against it.
run "folder view, restore state"    "$BIN" scan "${C[@]}" --folders --min-size 1M --top 0
run "purge F1 (dry run)"            "$BIN" purge F1 "${C[@]}"
run "purge F1,F2 (dry run)"         "$BIN" purge F1,F2 "${C[@]}"
# F1-F9 over a scan that found four: the tail exercises unknown ids, and
# user/Videos is 'thinned', so the file still living there must be refused.
run "purge F1-F9 (dry run)"         "$BIN" purge F1-F9 "${C[@]}"
run "purge F1-F9 --partial"         "$BIN" purge F1-F9 "${C[@]}" --partial

# The invariant the whole folder purge rests on: a directory that came back on
# the live tree between the scan and the purge must be refused outright, not
# rebuilt from a snapshot and deleted.
mkdir -p "$LIVE/user/projects"
head -c 4096 /dev/urandom > "$LIVE/user/projects/new-work.txt"
run "purge F2, dir returned live"   "$BIN" purge F2 "${C[@]}"
rm -rf "$LIVE/user/projects"

run_fails "purge with no id"        "$BIN" purge "${C[@]}"
run_fails "purge 3-F5, bad range"   "$BIN" purge 3-F5 "${C[@]}"
run "purge F99, unknown id"         "$BIN" purge F99 "${C[@]}"

run "file view, restore state"      "$BIN" scan "${C[@]}" --min-size 1M --top 0
run "purge 1 (dry run)"             "$BIN" purge 1 "${C[@]}"
run "purge 1-3 (dry run)"           "$BIN" purge 1-3 "${C[@]}"
run_fails "purge F1 on a file scan" "$BIN" purge F1 "${C[@]}"

run "journal"                       "$BIN" journal "${C[@]}"

# --apply is the one thing here that needs root: flipping a snapshot read-write
# and deleting out of it is a privileged operation whoever owns the subvolume.
# Everything above this point runs unprivileged, so an ordinary run still
# covers every read-only path.
if [ "${SELFTEST_APPLY:-0}" = 1 ]; then
    SUDO=()
    if [ "$(id -u)" != 0 ]; then
        SUDO=(sudo)
        term
        term "--apply needs root; sudo will ask for a password."
    fi
    both "--- apply phase (SELFTEST_APPLY=1): this really deletes from the fixture snapshots"
    run "folder view before apply"  "${SUDO[@]}" "$BIN" scan "${C[@]}" --folders --min-size 1M --top 0 --refresh
    run "purge F1 --apply"          "${SUDO[@]}" "$BIN" purge F1 "${C[@]}" --apply --yes
    run "folder view after apply"   "${SUDO[@]}" "$BIN" scan "${C[@]}" --folders --min-size 1M --top 0 --refresh
    run "journal after apply"       "${SUDO[@]}" "$BIN" journal "${C[@]}"
    # Root just wrote into the cache, the journal and the log; hand them back
    # so a later unprivileged run is not locked out of its own work directory.
    [ ${#SUDO[@]} -gt 0 ] && sudo chown -R "$(id -u):$(id -g)" "$WORK"
fi

# ------------------------------------------------------------------ report
#
# The log interleaves our step banners with the tool's own trace lines, so the
# report attributes every interesting line to the step that produced it and
# keeps them in the order they happened. Repeated lines within one step are
# collapsed with a count: "treesearch unavailable" alone fires once per file.

# $2 caps the output; a section that would print one line per file is a
# section nobody reads.
extract() {
    local cap=${2:-40}
    awk -v pat="$1" '
        /^##### \[[0-9]+\]/ { match($0, /\[[0-9]+\]/); step = substr($0, RSTART+1, RLENGTH-2); next }
        $0 ~ pat {
            line = $0
            sub(/^\[ *[0-9.]+s\] */, "", line)          # drop the elapsed stamp
            gsub(/[^ ]*\/btr\/(live|snaps\/[^ \/]*)\//, "", line)  # drop fixture paths
            key = step "\x1f" line
            if (!(key in seen)) { order[++n] = key }
            seen[key]++
        }
        END {
            for (i = 1; i <= n; i++) {
                split(order[i], f, "\x1f")
                c = seen[order[i]] > 1 ? "  (x" seen[order[i]] ")" : ""
                printf "   [%s] %s%s\n", f[1], f[2], c
            }
        }' "$LOGSNAP" >"$WORK/.extract"
    local n
    n=$(wc -l <"$WORK/.extract")
    head -"$cap" "$WORK/.extract"
    [ "$n" -gt "$cap" ] && echo "   [... $((n - cap)) more like this, grep the full log]"
    return 0
}

LOGSNAP=$WORK/.logsnap
cp "$LOG" "$LOGSNAP"

{
    echo
    echo "##### report #####"
    echo "steps run:            $step"
    echo "expected failures:    $expected_fail"
    echo "unexpected failures:  $unexpected"
    echo
    echo "-- exit code of every step"
    grep -h "^--- \[" "$SUMMARY" | sed 's/^--- /   /'
    echo
    echo "-- what each scan found"
    awk '/^##### \[[0-9]+\]/ { match($0, /\[[0-9]+\]/); s = substr($0, RSTART, RLENGTH); d = substr($0, RSTART+RLENGTH+1); sub(/ #####$/, "", d) }
         /^Total shown:/ { printf "   %s %-28s %s\n", s, d, $0 }' "$SUMMARY"
    echo
    echo "-- rollups, cache reuse, sampling"
    extract "rolled .* candidate\\(s\\) up into|cache: |folder sampled:" 60
    echo
    echo "-- what the purge paths planned, refused and skipped"
    extract "plan +(folder|plan complete|building plan)|refusing|back on the live tree" 80
    echo
    echo "-- measurement method (expected without root: no TREE_SEARCH_V2)"
    extract "treesearch unavailable|INFO +summary +measure\\." 12
    echo
    echo "-- warnings and errors"
    extract " (WARN|ERROR) " 40
    echo
    echo "-- lines worth a second look"
    echo "   (RECLAIM above APPARENT is normal - whole blocks and metadata."
    echo "    RECLAIM below APPARENT means extents are shared, e.g. reflinks.)"
    awk '/^F?[0-9]+ +~?[0-9]/ { gsub(/  +/, " "); print "   " $0 }' "$SUMMARY" | sort -u | head -30
} >"$WORK/.report"
cat "$WORK/.report" | tee -a "$SUMMARY" >>"$LOG"
rm -f "$LOGSNAP" "$WORK/.report" "$WORK/.extract" "$WORK/.step.out"

if [ "${SELFTEST_CLEAN:-0}" = 1 ]; then
    teardown
    term
    term "fixture removed (SELFTEST_CLEAN=1)"
fi

term
term "steps: $step run, $expected_fail expected failure(s), $unexpected unexpected"
term "summary: $SUMMARY  ($(wc -l <"$SUMMARY") lines, $(du -h "$SUMMARY" | cut -f1))"
term "full log: $LOG  ($(wc -l <"$LOG") lines, $(du -h "$LOG" | cut -f1))"
term
term "Send the summary first; it is small and says which step to look at."
term "The fixture is still at $WORK - re-run with SELFTEST_REUSE=1 to skip"
term "rebuilding it, or SELFTEST_CLEAN=1 to remove it."
