// Copyright (C) 2026 pcxzs
//
// This program is free software: you can redistribute it and/or modify it under
// the terms of the GNU General Public License as published by the Free Software
// Foundation, either version 3 of the License, or (at your option) any later
// version.
//
// This program is distributed in the hope that it will be useful, but WITHOUT
// ANY WARRANTY; without even the implied warranty of MERCHANTABILITY or FITNESS
// FOR A PARTICULAR PURPOSE. See the GNU General Public License for more
// details. You should have received a copy of the GNU General Public License
// along with this program. If not, see <https://www.gnu.org/licenses/>.

// snapshot-cleaner finds and reclaims disk space held by files that exist only
// in btrfs snapshots.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// Version is stamped at build time with -ldflags "-X main.Version=...".
var Version = "dev"

// defaultLogLevel is stamped at build time too. Release builds leave it "off"
// so nothing is written unless asked; `make debug` sets it to "trace" to
// produce a build that records everything it does for later review.
var defaultLogLevel = "off"

const usage = `%[1]s - find and reclaim space held by files that only exist in btrfs snapshots

When you delete a large file on a btrfs system with snapshots, the space is not
freed: every snapshot still referencing the file pins its extents. This tool
finds those files, ranks them by the space they actually cost, and can remove a
chosen file from every snapshot holding it, leaving the snapshots themselves in
place and read-only.

USAGE
  %[1]s <command> [flags]

COMMANDS
  doctor      Show what was detected and what the tool can do here. Read-only.
  snapshots   List the detected live/snapshot pairs.
  scan        Find and rank pinned files. Read-only.
  purge       Remove selected files from the snapshots holding them.
  cache       Inspect, prune or clear the scan cache.
  journal     Show past deletions.
  version     Print the version.

Run "%[1]s <command> -h" for a command's flags.

EXAMPLES
  sudo %[1]s doctor
  sudo %[1]s scan --min-size 100M
  sudo %[1]s purge 1,3,7-9          # dry run
  sudo %[1]s purge 1,3,7-9 --apply
  sudo %[1]s purge --interactive --apply
`

func main() {
	InstallSignalHandler()
	RegisterCleanup(func() {
		LogCounters()
		if path := LogPath(); path != "" {
			Infof("shutdown", "log written to %s", path)
			fmt.Fprintf(os.Stderr, "\nlog written to %s\n", path)
		}
		CloseLog()
	})

	if err := run(os.Args[1:]); err != nil {
		Errorf("main", "%v", err)
		RunCleanup()
		fmt.Fprintf(os.Stderr, "%s: %v\n", appName, err)
		os.Exit(1)
	}
	RunCleanup()
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Printf(usage, appName)
		return nil
	}
	switch args[0] {
	case "doctor":
		return cmdDoctor(args[1:])
	case "snapshots":
		return cmdSnapshots(args[1:])
	case "scan":
		return cmdScan(args[1:])
	case "purge":
		return cmdPurge(args[1:])
	case "cache":
		return cmdCache(args[1:])
	case "journal":
		return cmdJournal(args[1:])
	case "version", "--version", "-version":
		fmt.Printf("%s %s\n", appName, Version)
		return nil
	case "help", "-h", "--help":
		fmt.Printf(usage, appName)
		return nil
	default:
		return fmt.Errorf("unknown command %q (try `%s help`)", args[0], appName)
	}
}

// commonFlags are shared by every command that touches the filesystem.
type commonFlags struct {
	provider  string
	live      string
	snapshots string
	config    string
	stateDir  string
	runDir    string
	force     bool
	verbose   bool
	nice      int
	gentle    bool
	logLevel  string
	logFile   string
	debug     bool
	trace     bool
	cpus      int
	noCache   bool
	refresh   bool
	cacheMin  string
	walk      string
}

func (c *commonFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&c.provider, "provider", "auto", "snapshot manager to detect: auto, timeshift, snapper, btrbk")
	fs.StringVar(&c.live, "live", "", "live subvolume path (with --snapshots, bypasses detection)")
	fs.StringVar(&c.snapshots, "snapshots", "", "directory holding snapshot subvolumes (requires --live)")
	fs.StringVar(&c.config, "config", "", "config file path (default: searches XDG paths then /etc)")
	fs.StringVar(&c.stateDir, "state-dir", "", "override the state directory")
	fs.StringVar(&c.runDir, "runtime-dir", "", "override the runtime directory")
	fs.BoolVar(&c.force, "force", false, "proceed even if a snapshot manager is running")
	fs.IntVar(&c.nice, "nice", 10, "CPU niceness, 0-19; higher yields more to other work (0 disables)")
	fs.BoolVar(&c.gentle, "gentle", false, "minimise system impact: idle I/O priority, few workers, half the cores")
	fs.IntVar(&c.cpus, "cpus", 0, "confine the scan to this many cores, leaving the rest free to service interrupts (0 for all)")
	fs.BoolVar(&c.noCache, "no-cache", false, "neither read nor write the scan cache")
	fs.BoolVar(&c.refresh, "refresh", false, "ignore cached results and rewalk, replacing them")
	fs.StringVar(&c.cacheMin, "cache-min-size", "", "size floor recorded in the cache (default 1M); lower means more scans hit it")
	fs.StringVar(&c.walk, "walk", "auto", "how to list snapshot contents: auto, treesearch, readdir")
	fs.BoolVar(&c.verbose, "v", false, "verbose output")
	fs.StringVar(&c.logLevel, "log-level", defaultLogLevel, "log detail: off, error, warn, info, debug, trace")
	fs.StringVar(&c.logFile, "log-file", "", "write the log here (default: a timestamped file in the state directory)")
	fs.BoolVar(&c.debug, "debug", false, "shorthand for --log-level debug, mirrored to stderr")
	fs.BoolVar(&c.trace, "trace", false, "shorthand for --log-level trace, mirrored to stderr")
}

// startLogging opens the log as early as the state directory is known, and
// tells the user where it is: a log nobody can find is not much use.
func (c *commonFlags) startLogging(dirs Dirs, args []string) {
	levelName := c.logLevel
	var mirror io.Writer
	switch {
	case c.trace:
		levelName, mirror = "trace", os.Stderr
	case c.debug:
		levelName, mirror = "debug", os.Stderr
	}
	level, err := ParseLogLevel(levelName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v; logging disabled\n", err)
		return
	}
	if level == LevelOff {
		return
	}

	InitLog(dirs.State, level, mirror, c.logFile)
	if path := LogPath(); path != "" {
		fmt.Fprintf(os.Stderr, "logging at %s level to %s\n", level, path)
	}
	LogEnvironment(args)
	Infof("startup", "state dir: %s", dirs.State)
	Infof("startup", "runtime dir: %s", dirs.Runtime)
}

// priority builds the scheduling policy from the flags. The tool defaults to
// yielding, because reclaiming disk space is never more urgent than what the
// user is doing.
func (c *commonFlags) priority() Priority {
	p := DefaultPriority()
	if c.gentle {
		p = GentlePriority()
	}
	if c.nice != 10 {
		p.Nice = min(max(c.nice, 0), 19)
	}
	if c.cpus > 0 {
		p.CPUs = min(c.cpus, runtime.NumCPU())
	}
	return p
}

// walkMode maps the --walk flag onto a WalkMode.
func (c *commonFlags) walkMode() (WalkMode, error) {
	switch c.walk {
	case "", "auto":
		return WalkAuto, nil
	case "treesearch":
		return WalkTree, nil
	case "readdir":
		return WalkReaddir, nil
	default:
		return WalkAuto, fmt.Errorf("unknown --walk %q (known: auto, treesearch, readdir)", c.walk)
	}
}

// openCache opens the scan cache for this run. A cache that will not open is
// logged and skipped, never fatal.
func (a *app) openCache(refreshOnly bool) *Cache {
	return OpenCache(a.dirs, CacheOptions{
		Disabled: a.flags.noCache,
		Refresh:  a.flags.refresh || refreshOnly,
	})
}

// app is the assembled runtime context.
type app struct {
	flags   *commonFlags
	dirs    Dirs
	cfg     *Config
	sys     *System
	mounter *Mounter
	lock    *Lock
}

// setup performs the checks and discovery every filesystem-touching command
// needs. requireRoot is false only for commands that can degrade usefully.
func setup(c *commonFlags, requireRoot bool) (*app, error) {
	if requireRoot && os.Geteuid() != 0 {
		return nil, fmt.Errorf("this needs root\n  try: sudo %s", strings.Join(os.Args, " "))
	}
	if os.Geteuid() != 0 {
		// Not fatal: an already-reachable snapshot layout scans fine without
		// privileges. Say what will be degraded rather than refusing outright.
		Warnf("preflight", "not running as root; top-level mounts and exact measurement are unavailable")
		fmt.Fprintf(os.Stderr,
			"note: not running as root - layouts needing a top-level mount will not be found,\n"+
				"      and sizes fall back to FIEMAP estimates (marked ~). Re-run with sudo for exact figures.\n\n")
	}

	dirs, err := ResolveDirs(c.stateDir, c.runDir)
	if err != nil {
		return nil, err
	}
	c.startLogging(dirs, os.Args)

	cfg, err := LoadConfig(c.config)
	if err != nil {
		Errorf("config", "%v", err)
		return nil, err
	}
	if cfg.Source != "" {
		Infof("config", "loaded %s: %d pair(s), min-size=%q provider=%q",
			cfg.Source, len(cfg.Pairs), cfg.MinSize, cfg.Provider)
	} else {
		Debugf("config", "no config file found; searched %s", strings.Join(ConfigCandidates(c.config), ", "))
	}

	a := &app{flags: c, dirs: dirs, cfg: cfg, mounter: NewMounter(dirs.MountRoot())}
	RegisterCleanup(func() {
		for _, err := range a.mounter.Cleanup() {
			fmt.Fprintf(os.Stderr, "warning: %v\n", err)
		}
		a.lock.Release()
	})

	if os.Geteuid() == 0 {
		if a.lock, err = AcquireLock(dirs.LockFile()); err != nil {
			return nil, err
		}
		if running := RunningSnapshotManagers(); len(running) > 0 && !c.force {
			Errorf("preflight", "snapshot manager(s) running: %s", strings.Join(running, ", "))
			return nil, fmt.Errorf("%s is running; it may create or delete snapshots underneath us\n  wait for it to finish, or pass --force",
				strings.Join(running, " and "))
		}
	}

	if a.sys, err = NewSystem(a.mounter, c.verbose); err != nil {
		return nil, err
	}
	LogMounts(a.sys.Mounts)
	return a, nil
}

// pairs resolves the live/snapshot pairs, preferring an explicit command-line
// pairing, then the config file, then autodetection.
func (a *app) pairs() ([]Pair, error) {
	var pairs []Pair

	switch {
	case a.flags.live != "" && a.flags.snapshots != "":
		p, err := DetectGeneric(a.flags.live, a.flags.snapshots)
		if err != nil {
			return nil, err
		}
		pairs = []Pair{p}
	case a.flags.live != "" || a.flags.snapshots != "":
		return nil, fmt.Errorf("--live and --snapshots must be given together")
	case len(a.cfg.Pairs) > 0:
		for _, cp := range a.cfg.Pairs {
			p, err := DetectGeneric(cp.Live, cp.Snapshots)
			if err != nil {
				a.sys.Notef("config pair %s: %v", cp.Live, err)
				continue
			}
			pairs = append(pairs, p)
		}
	default:
		provider := a.flags.provider
		if provider == "auto" && a.cfg.Provider != "" {
			provider = a.cfg.Provider
		}
		var err error
		if pairs, err = Detect(a.sys, provider); err != nil {
			return nil, err
		}
	}

	LogPairs("detected", pairs)

	var valid []Pair
	for i := range pairs {
		if err := pairs[i].Validate(); err != nil {
			Warnf("pairs", "pair %s rejected: %v", pairs[i].Name, err)
			a.sys.Notef("pair %s: %v", pairs[i].Name, err)
			continue
		}
		valid = append(valid, pairs[i])
	}
	LogPairs("validated", valid)
	if len(valid) == 0 {
		return nil, fmt.Errorf("no usable snapshots found%s", detectionHint(a.sys))
	}
	return valid, nil
}

func detectionHint(sys *System) string {
	var b strings.Builder
	for _, n := range sys.Notes() {
		b.WriteString("\n  - " + n)
	}
	b.WriteString("\n\nIf your layout is not recognised, point the tool at it directly:")
	b.WriteString("\n  " + appName + " scan --live /home --snapshots /path/to/snapshots")
	b.WriteString("\nor list `pair = /home : /path/to/snapshots` lines in the config file.")
	return b.String()
}

// selectScope narrows the pairs to those named in --scope.
func selectScope(pairs []Pair, scope string) ([]Pair, error) {
	if scope == "" || scope == "all" {
		return pairs, nil
	}
	want := map[string]bool{}
	for _, s := range strings.Split(scope, ",") {
		want[strings.TrimSpace(s)] = true
	}
	var out []Pair
	for _, p := range pairs {
		if want[p.Name] || want[p.Live] {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		var names []string
		for _, p := range pairs {
			names = append(names, p.Name)
		}
		return nil, fmt.Errorf("--scope %q matched nothing (available: %s, or all)", scope, strings.Join(names, ", "))
	}
	return out, nil
}

func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	c := &commonFlags{}
	c.register(fs)
	if err := fs.Parse(permuteArgs(fs, args)); err != nil {
		return err
	}

	fmt.Printf("%s %s\n\n", appName, Version)

	fmt.Println("Privileges")
	if os.Geteuid() == 0 {
		fmt.Println("  running as root: yes")
	} else {
		fmt.Printf("  running as root: NO - most checks below will be unavailable\n  try: sudo %s doctor\n", appName)
	}

	a, err := setup(c, false)
	if err != nil {
		return err
	}

	fmt.Println("\nDirectories")
	fmt.Printf("  state:   %s\n", a.dirs.State)
	fmt.Printf("  runtime: %s\n", a.dirs.Runtime)
	if a.cfg.Source != "" {
		fmt.Printf("  config:  %s (%d pair(s) declared)\n", a.cfg.Source, len(a.cfg.Pairs))
	} else {
		fmt.Printf("  config:  none found (searched %s)\n", strings.Join(ConfigCandidates(c.config), ", "))
	}

	fmt.Println("\nCapabilities")
	reportCapabilities(a.dirs)

	fmt.Println("\nFilesystems")
	for _, m := range BtrfsMounts(a.sys.Mounts) {
		free, _ := FreeBytes(m.MountPoint)
		fmt.Printf("  %-24s %-16s subvol=%s free=%s\n", m.MountPoint, m.Source, m.Subvol, FormatBytes(free))
	}

	fmt.Println("\nDetection")
	pairs, err := a.pairs()
	if err != nil {
		fmt.Printf("  %v\n", err)
		return nil
	}
	for _, p := range pairs {
		fmt.Printf("  %-10s provider=%-10s live=%-20s snapshots=%d\n", p.Name, p.Provider, p.Live, len(p.Snapshots))
	}

	// Which walk will be used matters as much as which measurement path does:
	// the readdir fallback is what makes a cold scan expensive enough to
	// disturb the rest of the machine.
	if probe := firstSnapshotRoot(pairs); probe != "" {
		if err := TreeWalkSupported(probe); err == nil {
			fmt.Println("  listing method: TREE_SEARCH_V2 sweep (one ioctl per megabyte of metadata)")
		} else {
			fmt.Printf("  listing method: readdir walk (%v)\n", err)
			fmt.Println("                  far more syscalls per scan; run as root for the tree sweep")
		}
	}
	if notes := a.sys.Notes(); len(notes) > 0 {
		fmt.Println("\nNotes")
		for _, n := range notes {
			fmt.Printf("  - %s\n", n)
		}
	}
	return nil
}

// reportCapabilities probes the optional facilities so a user can see which
// measurement path will be used before trusting the numbers.
func reportCapabilities(dirs Dirs) {
	if err := ProbeTreeSearch(os.TempDir()); err == nil {
		fmt.Println("  extent accounting: TREE_SEARCH_V2 (exact, compression-aware)")
	} else {
		fmt.Printf("  extent accounting: FIEMAP fallback (%v)\n", err)
		fmt.Println("                     compressed extents are reported as upper bounds")
	}

	cache := OpenCache(dirs, CacheOptions{})
	if cache == nil {
		fmt.Printf("  scan cache:        none at %s (it is created by the first scan)\n", dirs.CacheFile())
	} else {
		if st, err := cache.Status(); err == nil {
			fmt.Printf("  scan cache:        %d snapshot listing(s), %d measurement(s), %s\n",
				st.Manifests, st.Measures, FormatBytes(st.Bytes))
		}
		cache.Close()
	}
	for _, tool := range []string{"compsize", "btrfs"} {
		if path, err := exec.LookPath(tool); err == nil {
			fmt.Printf("  %-8s (optional):  %s\n", tool, path)
		} else {
			fmt.Printf("  %-8s (optional):  not installed - not required\n", tool)
		}
	}
}

func cmdSnapshots(args []string) error {
	fs := flag.NewFlagSet("snapshots", flag.ExitOnError)
	c := &commonFlags{}
	c.register(fs)
	asJSON := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(permuteArgs(fs, args)); err != nil {
		return err
	}

	a, err := setup(c, false)
	if err != nil {
		return err
	}
	pairs, err := a.pairs()
	if err != nil {
		return err
	}
	if *asJSON {
		return RenderJSON(os.Stdout, &ScanState{Pairs: pairs})
	}
	RenderSnapshots(os.Stdout, pairs)
	return nil
}

func cmdScan(args []string) error {
	fs := flag.NewFlagSet("scan", flag.ExitOnError)
	c := &commonFlags{}
	c.register(fs)
	scope := fs.String("scope", "all", "which pairs to scan: all, or a comma-separated list of names")
	minSize := fs.String("min-size", "50M", "ignore files smaller than this")
	top := fs.Int("top", 25, "show only the top N rows (0 for all)")
	includeReplaced := fs.Bool("include-replaced", false, "also report files still present live but whose older content is pinned")
	costLimit := fs.Int("cost-limit", 200, "measure at most this many candidates (0 for all)")
	workers := fs.Int("workers", 0, "snapshots to walk in parallel (0 chooses based on CPU count)")
	asJSON := fs.Bool("json", false, "emit JSON")
	var excludes multiFlag
	fs.Var(&excludes, "exclude", "glob to skip (repeatable)")
	if err := fs.Parse(permuteArgs(fs, args)); err != nil {
		return err
	}

	min, err := ParseSize(*minSize)
	if err != nil {
		return err
	}
	a, err := setup(c, false)
	if err != nil {
		return err
	}
	if a.cfg.MinSize != "" && !flagSet(fs, "min-size") {
		if min, err = ParseSize(a.cfg.MinSize); err != nil {
			return err
		}
	}

	pairs, err := a.pairs()
	if err != nil {
		return err
	}
	if pairs, err = selectScope(pairs, *scope); err != nil {
		return err
	}

	walk, err := c.walkMode()
	if err != nil {
		return err
	}
	floor := uint64(DefaultCacheFloor)
	if a.cfg.CacheMinSize != "" {
		if floor, err = ParseSize(a.cfg.CacheMinSize); err != nil {
			return err
		}
	}
	if c.cacheMin != "" {
		if floor, err = ParseSize(c.cacheMin); err != nil {
			return err
		}
	}

	cache := a.openCache(false)
	defer cache.Close()

	var progress *os.File
	if !*asJSON && isTTY(os.Stderr) {
		progress = os.Stderr
	}
	opts := ScanOptions{
		MinSize:         min,
		IncludeReplaced: *includeReplaced,
		Excludes:        excludes,
		CostLimit:       *costLimit,
		Workers:         *workers,
		Priority:        c.priority(),
		Progress:        progress,
		Cache:           cache,
		CacheFloor:      floor,
		Walk:            walk,
	}
	if c.gentle && *workers <= 0 {
		opts.Workers = 2
	}
	if progress != nil {
		w := opts.Workers
		if w <= 0 {
			w = DefaultWorkers()
		}
		fmt.Fprintf(os.Stderr, "Scanning %d pair(s) for files of %s or more (%d workers, %s%s)...\n",
			len(pairs), FormatBytes(min), w, c.priority(), cacheNote(cache))
	}

	cands, err := Scan(pairs, opts)
	if err != nil {
		return err
	}
	if progress != nil {
		clearProgress(progress)
	}

	// Recorded after a successful scan only, so `cache status` reports a run
	// that actually completed.
	cache.RecordScan()
	if n, err := cache.GC(pairs); err != nil {
		Warnf("cache", "GC failed: %v", err)
	} else if n > 0 {
		Debugf("cache", "GC removed %d stale entry/entries", n)
	}

	costed := *costLimit
	if costed <= 0 || costed > len(cands) {
		costed = len(cands)
	}
	free, _ := FreeBytes(pairs[0].Live)
	fsid, _ := FilesystemID(pairs[0].Live)
	st := &ScanState{
		ScannedAt:    time.Now(),
		FilesystemID: fsid,
		MinSize:      min,
		Costed:       costed,
		FreeBytes:    free,
		Pairs:        pairs,
		Candidates:   cands,
	}
	if err := SaveState(a.dirs.StateFile(), st); err != nil {
		Errorf("scan", "saving state failed: %v", err)
		return fmt.Errorf("saving scan state: %w", err)
	}
	Infof("scan", "state saved to %s (free space on %s: %s)", a.dirs.StateFile(), pairs[0].Live, FormatBytes(free))

	if *asJSON {
		return RenderJSON(os.Stdout, st)
	}
	RenderTable(os.Stdout, st, *top, false)
	if line := cacheSummary(cache); line != "" {
		fmt.Fprintf(os.Stderr, "\n%s\n", line)
	}
	if len(cands) > 0 {
		fmt.Printf("\nTo remove one, review the dry run first:\n  sudo %s purge 1\n", appName)
	}
	return nil
}

func cmdPurge(args []string) error {
	fs := flag.NewFlagSet("purge", flag.ExitOnError)
	c := &commonFlags{}
	c.register(fs)
	apply := fs.Bool("apply", false, "actually remove the files (without this it is a dry run)")
	yes := fs.Bool("yes", false, "skip the confirmation prompt")
	interactive := fs.Bool("interactive", false, "pick files from a checklist")
	partial := fs.Bool("partial", false, "allow removing from only some of the snapshots holding a file")
	if err := fs.Parse(permuteArgs(fs, args)); err != nil {
		return err
	}

	// A dry run only reads: requiring root to preview would train the user to
	// reach for sudo before they have seen what the tool intends to do.
	a, err := setup(c, *apply)
	if err != nil {
		return err
	}
	st, err := LoadState(a.dirs.StateFile())
	if err != nil {
		return err
	}

	// Snapshots are normally reached through a temporary mount whose path
	// differs on every run, so the paths saved by `scan` must be re-resolved
	// against where those snapshots live in this process.
	current, err := a.pairs()
	if err != nil {
		return err
	}
	if dropped := st.Relocate(current); dropped > 0 {
		fmt.Fprintf(os.Stderr, "note: %d snapshot copy/copies from the last scan no longer exist (rotated out)\n", dropped)
	}

	if age := time.Since(st.ScannedAt); age > 24*time.Hour {
		fmt.Fprintf(os.Stderr, "note: the last scan is %s old; every target is re-validated before removal\n\n",
			age.Round(time.Hour))
	}

	var ids []int
	switch {
	case *interactive:
		if ids, err = RunPicker(st.Candidates); err != nil {
			return err
		}
		if len(ids) == 0 {
			fmt.Println("Nothing selected.")
			return nil
		}
	case fs.NArg() > 0:
		if ids, err = ParseIDs(fs.Args()); err != nil {
			return err
		}
	default:
		return fmt.Errorf("give ids to purge (e.g. `purge 1,3,7-9`) or use --interactive")
	}

	plan, err := BuildPlan(st, ids, *partial)
	if err != nil {
		return err
	}
	RenderPlan(os.Stdout, plan, *apply)
	if len(plan.Targets) == 0 {
		return nil
	}

	if !*apply {
		fmt.Printf("\nThis was a dry run. Nothing was changed.\n")
		fmt.Printf("Re-run with --apply (as root) to remove these files.\n")
		return nil
	}

	fmt.Println("\nThese files will be permanently removed from the snapshots listed above.")
	fmt.Println("The snapshots stay in place and stay read-only, but they will no longer be")
	fmt.Println("able to restore these files, and your snapshot manager will not know.")
	if !*yes && !confirm("\nType 'yes' to proceed: ") {
		fmt.Println("Aborted.")
		return nil
	}

	if err := c.priority().Apply(); err != nil {
		Debugf("priority", "could not lower priority: %v", err)
	}

	journal, err := OpenJournal(a.dirs.JournalFile())
	if err != nil {
		return fmt.Errorf("opening journal: %w", err)
	}
	defer journal.Close()

	freeBefore, _ := FreeBytes(st.Pairs[0].Live)
	purger := &Purger{
		Journal:    journal,
		Out:        os.Stdout,
		Mounter:    a.mounter,
		MountPoint: a.mounter.OwnedMountFor(plan.Targets[0].Copy.Path),
	}

	fmt.Println()
	n, execErr := purger.Execute(plan)
	// btrfs releases extents asynchronously, so reading free space at the
	// instant of the unlink undercounts badly: a real 13.4 GiB reclaim read as
	// 7.11 GiB immediately and reached the full figure moments later. Wait for
	// the number to stop moving, otherwise a correct prediction looks wrong.
	if n > 0 {
		fmt.Fprint(os.Stderr, "waiting for btrfs to release the extents...")
	}
	freeAfter, settled := SettleFreeBytes(st.Pairs[0].Live, 15*time.Second)
	if n > 0 {
		fmt.Fprint(os.Stderr, "\r\033[K")
	}

	Infof("purge", "free space before=%d after=%d delta=%d settled=%v estimated=%d",
		freeBefore, freeAfter, int64(freeAfter)-int64(freeBefore), settled, plan.Estimated)
	fmt.Printf("\nRemoved %d file copy/copies.\n", n)
	if freeAfter > freeBefore {
		fmt.Printf("Free space: %s -> %s (+%s, estimated %s)\n",
			FormatBytes(freeBefore), FormatBytes(freeAfter),
			FormatBytes(freeAfter-freeBefore), FormatBytes(plan.Estimated))
	} else {
		fmt.Printf("Free space: %s (no change measured)\n", FormatBytes(freeAfter))
	}
	if !settled {
		fmt.Println("btrfs is still releasing space; re-check with `df -h` in a moment.")
	}
	fmt.Printf("Recorded in %s\n", a.dirs.JournalFile())

	// The scan state now describes files that no longer exist.
	os.Remove(a.dirs.StateFile())

	// So do the cached listings of every snapshot that was written to. The
	// cache key already covers this - unlinking inside a snapshot moves its
	// CTransID, so the old entry stops matching - but this is the one place
	// the tool knows for certain which snapshots changed, and dropping them
	// explicitly does not depend on that reasoning holding.
	dropPurgedFromCache(a, plan)
	return execErr
}

func cmdJournal(args []string) error {
	fs := flag.NewFlagSet("journal", flag.ExitOnError)
	c := &commonFlags{}
	c.register(fs)
	tail := fs.Int("tail", 50, "show the last N entries (0 for all)")
	if err := fs.Parse(permuteArgs(fs, args)); err != nil {
		return err
	}
	dirs, err := ResolveDirs(c.stateDir, c.runDir)
	if err != nil {
		return err
	}
	entries, err := ReadJournal(dirs.JournalFile(), *tail)
	if err != nil {
		return err
	}
	RenderJournal(os.Stdout, entries)
	return nil
}

// multiFlag collects a repeatable string flag.
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

// permuteArgs reorders arguments so that flags may follow positional operands.
//
// The standard flag package stops parsing at the first non-flag argument, so
// "purge 1 --apply" would parse "--apply" as a positional and silently perform
// a dry run instead of the removal the user asked for. Since that is the exact
// order people naturally type, the arguments are reordered rather than the
// users being expected to adapt.
func permuteArgs(fs *flag.FlagSet, args []string) []string {
	var flags, operands []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			operands = append(operands, args[i+1:]...)
			break
		}
		if len(arg) < 2 || arg[0] != '-' {
			operands = append(operands, arg)
			continue
		}

		flags = append(flags, arg)
		name := strings.TrimLeft(arg, "-")
		if strings.Contains(name, "=") {
			continue // value is attached, nothing further to consume
		}
		// A non-boolean flag takes the next argument as its value. An unknown
		// flag consumes nothing; Parse reports it properly a moment later.
		if f := fs.Lookup(name); f != nil && !isBoolFlag(f) && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return append(flags, operands...)
}

func isBoolFlag(f *flag.Flag) bool {
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}

func flagSet(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

func confirm(prompt string) bool {
	fmt.Print(prompt)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return false
	}
	return strings.TrimSpace(strings.ToLower(line)) == "yes"
}

func isTTY(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// cacheNote describes the cache for the scan banner.
func cacheNote(c *Cache) string {
	if c == nil {
		return ", no cache"
	}
	return ", cached"
}

// cacheSummary reports what the cache saved, so the speed-up is visible rather
// than merely felt.
func cacheSummary(c *Cache) string {
	if c == nil {
		return ""
	}
	hits := c.Stats.ManifestHits.Load()
	total := hits + c.Stats.ManifestMisses.Load()
	if total == 0 {
		return ""
	}
	mh := c.Stats.MeasureHits.Load()
	mt := mh + c.Stats.MeasureMisses.Load()
	out := fmt.Sprintf("Reused %d of %d snapshot listing(s) from the cache", hits, total)
	if mt > 0 {
		out += fmt.Sprintf(", and %d of %d measurement(s)", mh, mt)
	}
	if hits < total {
		out += fmt.Sprintf("; %d listing(s) had to be walked, and are now cached", total-hits)
	}
	return out + "."
}

const cacheUsage = `%[1]s cache - inspect and manage the scan cache

A scan records what it found inside each snapshot. Snapshots are read-only, so
those findings cannot go stale while the snapshot is unchanged: entries are keyed
by the subvolume's UUID and the transid of its last change, so any modification
makes the entry stop matching rather than be wrongly reused. Nothing about the
live filesystem is cached, so files you delete or edit outside this tool are
picked up on the next scan exactly as before.

USAGE
  %[1]s cache <status|gc|clear> [flags]

  status   What is cached, how big it is, and what the last scan reused.
  gc       Drop entries for snapshots that no longer exist.
  clear    Empty the cache. The next scan will be a cold one.
`

func cmdCache(args []string) error {
	sub := "status"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "help", "-h", "--help":
		fmt.Printf(cacheUsage, appName)
		return nil
	case "status", "gc", "clear":
	default:
		return fmt.Errorf("unknown cache subcommand %q (known: status, gc, clear)", sub)
	}

	fs := flag.NewFlagSet("cache "+sub, flag.ExitOnError)
	c := &commonFlags{}
	c.register(fs)
	if err := fs.Parse(permuteArgs(fs, args)); err != nil {
		return err
	}

	dirs, err := ResolveDirs(c.stateDir, c.runDir)
	if err != nil {
		return err
	}
	c.startLogging(dirs, os.Args)

	// "status" only reads, so it works unprivileged; the others must write.
	cache := OpenCache(dirs, CacheOptions{})
	if cache == nil {
		fmt.Printf("No cache at %s.\n", dirs.CacheFile())
		return nil
	}
	defer cache.Close()

	switch sub {
	case "clear":
		if err := cache.Clear(); err != nil {
			return err
		}
		fmt.Printf("Cache cleared: %s\n", dirs.CacheFile())
		return nil

	case "gc":
		a, err := setup(c, false)
		if err != nil {
			return err
		}
		pairs, err := a.pairs()
		if err != nil {
			return err
		}
		n, err := cache.GC(pairs)
		if err != nil {
			return err
		}
		fmt.Printf("Removed %d stale entry/entries from %s\n", n, dirs.CacheFile())
		return nil
	}

	st, err := cache.Status()
	if err != nil {
		return err
	}
	fmt.Printf("Cache: %s\n", st.Path)
	fmt.Printf("  size:          %s\n", FormatBytes(st.Bytes))
	fmt.Printf("  snapshots:     %d listing(s) across %d subvolume(s)\n", st.Manifests, len(st.Subvols))
	fmt.Printf("  files listed:  %d\n", st.ManifestRows)
	fmt.Printf("  measurements:  %d\n", st.Measures)
	if !st.Oldest.IsZero() {
		fmt.Printf("  recorded:      %s to %s\n",
			st.Oldest.Format("2006-01-02 15:04"), st.Newest.Format("2006-01-02 15:04"))
	}
	if r := st.LastScan; r != nil {
		total := r.ManifestHits + r.ManifestMisses
		fmt.Printf("\nLast scan (%s):\n", r.At.Format("2006-01-02 15:04"))
		fmt.Printf("  listings reused:     %d of %d\n", r.ManifestHits, total)
		fmt.Printf("  measurements reused: %d of %d\n", r.MeasureHits, r.MeasureHits+r.MeasureMisses)
	}
	if st.Manifests == 0 {
		fmt.Printf("\nNothing cached yet. The next scan will fill this in.\n")
	}
	return nil
}

// firstSnapshotRoot returns any snapshot root, for probing what the kernel will
// allow before a scan commits to a strategy.
func firstSnapshotRoot(pairs []Pair) string {
	for _, p := range pairs {
		for _, s := range p.Snapshots {
			return s.Root
		}
	}
	return ""
}

// dropPurgedFromCache removes cached listings and measurements for every
// snapshot a purge unlinked from.
func dropPurgedFromCache(a *app, plan *PurgePlan) {
	cache := OpenCache(a.dirs, CacheOptions{})
	if cache == nil {
		return
	}
	defer cache.Close()

	fsIDs := map[string]string{} // live path -> filesystem id
	seen := map[string]bool{}
	for _, t := range plan.Targets {
		if t.Copy.SnapshotUUID == "" || seen[t.Copy.SnapshotUUID] {
			continue
		}
		seen[t.Copy.SnapshotUUID] = true
		fsID, ok := fsIDs[t.Candidate.Live]
		if !ok {
			id, err := FilesystemID(t.Candidate.Live)
			if err != nil {
				Warnf("cache", "cannot identify filesystem for %s: %v", t.Candidate.Live, err)
				continue
			}
			fsID, fsIDs[t.Candidate.Live] = id, id
		}
		cache.DropSnapshot(fsID, t.Copy.SnapshotUUID)
	}
}
