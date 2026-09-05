package main

import (
	"fmt"
	"runtime"

	"golang.org/x/sys/unix"
)

// This tool is background maintenance: reclaiming disk space is never more
// urgent than whatever the user is actually doing. Left at normal priority it
// is a bad neighbour - walking millions of inodes across many snapshots
// saturates CPU and floods the block layer, and anything latency-sensitive
// suffers.
//
// That is not hypothetical. On a test machine, every scan produced a
// burst of firmware timeouts from a USB wifi adapter (20 kernel
// error events in one minute during a scan, against isolated events hours
// apart otherwise), ending in the adapter re-enumerating on the USB bus. There
// were no memory allocation failures, so the cause was scheduling and
// interrupt latency rather than memory pressure: that driver does synchronous
// register access over USB with short firmware timeouts, and it cannot keep up
// when the machine is busy.
//
// Running nice and with low I/O priority costs a little wall-clock time and
// makes the tool safe to run while the machine is in use.

// Linux I/O priority classes, from include/uapi/linux/ioprio.h.
const (
	ioprioWhoProcess = 1
	ioprioClassShift = 13

	IoprioClassBE   = 2 // best effort
	IoprioClassIdle = 3

	// Lowest level within the best-effort class.
	ioprioLowestBE = 7
)

// Priority describes how much the tool should get out of the way.
type Priority struct {
	Nice    int // 0..19; higher yields more CPU to everything else
	IOClass int
	IOLevel int

	// CPUs confines the scan to this many cores, 0 meaning all of them.
	//
	// Niceness alone does not fix an interrupt-latency problem: a niced thread
	// still occupies its core, and the driver above needs a core that is free
	// to service its interrupt promptly. Leaving cores out of the scan's
	// affinity mask gives it one, which nice cannot.
	CPUs int
}

// DefaultPriority yields CPU and disk to interactive work without making the
// scan crawl.
func DefaultPriority() Priority {
	return Priority{Nice: 10, IOClass: IoprioClassBE, IOLevel: ioprioLowestBE}
}

// GentlePriority is for machines with hardware that cannot tolerate load,
// such as the USB wifi adapters described above.
func GentlePriority() Priority {
	return Priority{Nice: 19, IOClass: IoprioClassIdle, IOLevel: 0, CPUs: max(runtime.NumCPU()/2, 1)}
}

// Apply lowers the priority of the calling OS thread.
//
// Both nice and I/O priority are per-thread on Linux, so this must run on
// every thread that does the work, not once in main. Callers that want it to
// stick for a goroutine must hold the thread with runtime.LockOSThread.
func (p Priority) Apply() error {
	if p.Nice != 0 {
		if err := unix.Setpriority(unix.PRIO_PROCESS, 0, p.Nice); err != nil {
			return fmt.Errorf("setpriority(nice=%d): %w", p.Nice, err)
		}
	}
	if p.IOClass != 0 {
		val := p.IOClass<<ioprioClassShift | p.IOLevel
		if _, _, errno := unix.Syscall(unix.SYS_IOPRIO_SET,
			ioprioWhoProcess, 0, uintptr(val)); errno != 0 {
			return fmt.Errorf("ioprio_set(class=%d level=%d): %w", p.IOClass, p.IOLevel, errno)
		}
	}
	if p.CPUs > 0 {
		if err := confineToCPUs(p.CPUs); err != nil {
			return err
		}
	}
	return nil
}

// confineToCPUs restricts the calling thread to n cores.
//
// The cores are taken from the top of whatever set the thread is already
// allowed, so this composes with taskset and with a cgroup cpuset instead of
// overriding them, and so that CPU 0 is the first core released. CPU 0 is where
// an unbalanced interrupt lands by default, which is exactly the core the wifi
// driver needs to be responsive on.
func confineToCPUs(n int) error {
	var allowed unix.CPUSet
	if err := unix.SchedGetaffinity(0, &allowed); err != nil {
		return fmt.Errorf("sched_getaffinity: %w", err)
	}
	if allowed.Count() <= n {
		return nil // already at or below the requested width
	}
	var want unix.CPUSet
	want.Zero()
	picked := 0
	// CPUSet has no exported bit count; len is in words. Probing above the real
	// width is harmless because IsSet bounds-checks and returns false.
	for cpu := len(allowed)*64 - 1; cpu >= 0 && picked < n; cpu-- {
		if allowed.IsSet(cpu) {
			want.Set(cpu)
			picked++
		}
	}
	if picked == 0 {
		return nil
	}
	if err := unix.SchedSetaffinity(0, &want); err != nil {
		return fmt.Errorf("sched_setaffinity(%d cpu(s)): %w", n, err)
	}
	return nil
}

// ApplyToWorker pins the goroutine to its thread and lowers that thread's
// priority. The returned function releases the thread again.
func (p Priority) ApplyToWorker() func() {
	runtime.LockOSThread()
	if err := p.Apply(); err != nil {
		Debugf("priority", "could not lower worker priority: %v", err)
	}
	return runtime.UnlockOSThread
}

func (p Priority) String() string {
	class := map[int]string{IoprioClassBE: "best-effort", IoprioClassIdle: "idle"}[p.IOClass]
	if class == "" {
		class = "default"
	}
	cpus := "all"
	if p.CPUs > 0 {
		cpus = fmt.Sprintf("%d", p.CPUs)
	}
	return fmt.Sprintf("nice=%d io=%s/%d cpus=%s", p.Nice, class, p.IOLevel, cpus)
}
