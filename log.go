package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Logging exists so that a run on a machine the author cannot reach can still
// be diagnosed afterwards. The log records what was detected, which
// measurement path each figure came from, and every decision the purge path
// made, because those are the things that are impossible to reconstruct from a
// summary line after the fact.

type LogLevel int

const (
	LevelOff LogLevel = iota
	LevelError
	LevelWarn
	LevelInfo
	LevelDebug
	LevelTrace
)

func (l LogLevel) String() string {
	switch l {
	case LevelError:
		return "ERROR"
	case LevelWarn:
		return "WARN "
	case LevelInfo:
		return "INFO "
	case LevelDebug:
		return "DEBUG"
	case LevelTrace:
		return "TRACE"
	}
	return "OFF  "
}

func ParseLogLevel(s string) (LogLevel, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "off", "none":
		return LevelOff, nil
	case "error":
		return LevelError, nil
	case "warn", "warning":
		return LevelWarn, nil
	case "info":
		return LevelInfo, nil
	case "debug":
		return LevelDebug, nil
	case "trace":
		return LevelTrace, nil
	}
	return LevelOff, fmt.Errorf("unknown log level %q (off, error, warn, info, debug, trace)", s)
}

// Logger writes to a file and, optionally, to stderr.
type Logger struct {
	mu       sync.Mutex
	file     *os.File
	mirror   io.Writer
	level    LogLevel
	start    time.Time
	path     string
	counters map[string]*atomic.Int64
	// traceBudget bounds per-file trace output so a scan of millions of
	// inodes cannot fill the disk it is trying to help you free.
	traceBudget atomic.Int64
}

var logbook = &Logger{level: LevelOff, start: time.Now(), counters: map[string]*atomic.Int64{}}

// InitLog opens the log file. A failure here is reported but never fatal: the
// tool's job is to work, not to log.
func InitLog(dir string, level LogLevel, mirror io.Writer, explicitPath string) {
	if level == LevelOff {
		return
	}
	path := explicitPath
	if path == "" {
		name := fmt.Sprintf("%s-%s.log", appName, time.Now().Format("20060102-150405"))
		path = filepath.Join(dir, name)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "warning: cannot create log directory: %v\n", err)
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: cannot open log file %s: %v\n", path, err)
		return
	}

	logbook.mu.Lock()
	logbook.file = f
	logbook.path = path
	logbook.level = level
	logbook.mirror = mirror
	logbook.start = time.Now()
	logbook.mu.Unlock()
	logbook.traceBudget.Store(200000)
}

func LogPath() string {
	logbook.mu.Lock()
	defer logbook.mu.Unlock()
	return logbook.path
}

func LogEnabled(l LogLevel) bool {
	logbook.mu.Lock()
	defer logbook.mu.Unlock()
	return logbook.level >= l && logbook.file != nil
}

func CloseLog() {
	logbook.mu.Lock()
	defer logbook.mu.Unlock()
	if logbook.file != nil {
		logbook.file.Sync()
		logbook.file.Close()
		logbook.file = nil
	}
}

func (lg *Logger) emit(level LogLevel, component, format string, args ...any) {
	lg.mu.Lock()
	if lg.file == nil || lg.level < level {
		lg.mu.Unlock()
		return
	}
	f, mirror, start := lg.file, lg.mirror, lg.start
	lg.mu.Unlock()

	msg := fmt.Sprintf(format, args...)
	line := fmt.Sprintf("[%9.3fs] %s %-12s %s\n", time.Since(start).Seconds(), level, component, msg)

	lg.mu.Lock()
	f.WriteString(line)
	lg.mu.Unlock()

	if mirror != nil {
		fmt.Fprint(mirror, line)
	}
}

func Errorf(component, format string, args ...any) {
	logbook.emit(LevelError, component, format, args...)
}
func Warnf(component, format string, args ...any) {
	logbook.emit(LevelWarn, component, format, args...)
}
func Infof(component, format string, args ...any) {
	logbook.emit(LevelInfo, component, format, args...)
}
func Debugf(component, format string, args ...any) {
	logbook.emit(LevelDebug, component, format, args...)
}

// Tracef is budgeted: per-file logging during a scan can run to millions of
// lines, so it stops itself and says so rather than filling the disk.
func Tracef(component, format string, args ...any) {
	if !LogEnabled(LevelTrace) {
		return
	}
	switch n := logbook.traceBudget.Add(-1); {
	case n < 0:
		return
	case n == 0:
		logbook.emit(LevelTrace, component, "trace budget exhausted; further trace lines suppressed")
		return
	}
	logbook.emit(LevelTrace, component, format, args...)
}

// Count accumulates a named tally reported in the run summary, which is easier
// to read than counting log lines by hand.
func Count(name string, delta int64) {
	logbook.mu.Lock()
	c, ok := logbook.counters[name]
	if !ok {
		c = &atomic.Int64{}
		logbook.counters[name] = c
	}
	logbook.mu.Unlock()
	c.Add(delta)
}

// LogCounters writes the accumulated tallies.
func LogCounters() {
	if !LogEnabled(LevelInfo) {
		return
	}
	logbook.mu.Lock()
	names := make([]string, 0, len(logbook.counters))
	for k := range logbook.counters {
		names = append(names, k)
	}
	vals := map[string]int64{}
	for _, n := range names {
		vals[n] = logbook.counters[n].Load()
	}
	logbook.mu.Unlock()

	if len(names) == 0 {
		return
	}
	sortStrings(names)
	Infof("summary", "counters:")
	for _, n := range names {
		Infof("summary", "  %-32s %d", n, vals[n])
	}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// LogEnvironment records the context every later line should be read against.
func LogEnvironment(args []string) {
	if !LogEnabled(LevelInfo) {
		return
	}
	Infof("startup", "%s %s", appName, Version)
	Infof("startup", "argv: %s", strings.Join(args, " "))
	Infof("startup", "go %s %s/%s, %d cpu", runtime.Version(), runtime.GOOS, runtime.GOARCH, runtime.NumCPU())
	Infof("startup", "uid=%d euid=%d gid=%d pid=%d", os.Getuid(), os.Geteuid(), os.Getgid(), os.Getpid())
	if b, err := os.ReadFile("/proc/version"); err == nil {
		Infof("startup", "kernel: %s", strings.TrimSpace(string(b)))
	}
	if b, err := os.ReadFile("/etc/os-release"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "PRETTY_NAME=") {
				Infof("startup", "distro: %s", strings.Trim(strings.TrimPrefix(line, "PRETTY_NAME="), `"`))
			}
		}
	}
	for _, v := range []string{"XDG_STATE_HOME", "XDG_RUNTIME_DIR", "XDG_CONFIG_HOME", "TMPDIR"} {
		if val := os.Getenv(v); val != "" {
			Debugf("startup", "env %s=%s", v, val)
		}
	}
}

// LogMounts records every btrfs mount, which is the ground truth all provider
// detection is derived from.
func LogMounts(mounts []MountEntry) {
	if !LogEnabled(LevelDebug) {
		return
	}
	btrfs := BtrfsMounts(mounts)
	Debugf("mounts", "%d mount(s) total, %d btrfs", len(mounts), len(btrfs))
	for _, m := range btrfs {
		free, err := FreeBytes(m.MountPoint)
		freeStr := FormatBytes(free)
		if err != nil {
			freeStr = "unknown"
		}
		fsid, _ := FilesystemID(m.MountPoint)
		Debugf("mounts", "  mnt=%-22s src=%-16s root=%-10s subvol=%-10s subvolid=%d free=%s fsid=%s",
			m.MountPoint, m.Source, m.Root, m.Subvol, m.SubvolID, freeStr, fsid)
	}
}

// LogPairs records the detected live/snapshot pairs in full.
func LogPairs(stage string, pairs []Pair) {
	if !LogEnabled(LevelDebug) {
		return
	}
	Debugf("pairs", "%s: %d pair(s)", stage, len(pairs))
	for _, p := range pairs {
		Debugf("pairs", "  pair name=%s provider=%s live=%s snapshots=%d",
			p.Name, p.Provider, p.Live, len(p.Snapshots))
		for _, s := range p.Snapshots {
			created := "unknown"
			if !s.Created.IsZero() {
				created = s.Created.Format(time.RFC3339)
			}
			Debugf("pairs", "    snap id=%-24s ro=%-5v created=%-25s tag=%-10s received=%q root=%s",
				s.ID, s.ReadOnly, created, s.Tag, s.ReceivedUUID, s.Root)
		}
	}
}
