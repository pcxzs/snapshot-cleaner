package main

import (
	"fmt"
	"runtime"
	"testing"

	"golang.org/x/sys/unix"
)

func TestDefaultPriorityYields(t *testing.T) {
	p := DefaultPriority()
	if p.Nice <= 0 {
		t.Errorf("default niceness = %d; the tool must yield CPU by default", p.Nice)
	}
	if p.IOClass != IoprioClassBE || p.IOLevel != ioprioLowestBE {
		t.Errorf("default I/O priority = %v, want lowest best-effort", p)
	}
}

func TestGentlePriorityIsWeakerThanDefault(t *testing.T) {
	d, g := DefaultPriority(), GentlePriority()
	if g.Nice <= d.Nice {
		t.Errorf("gentle nice %d should exceed default %d", g.Nice, d.Nice)
	}
	if g.IOClass != IoprioClassIdle {
		t.Errorf("gentle I/O class = %d, want idle (%d)", g.IOClass, IoprioClassIdle)
	}
}

// Both nice and I/O priority are per-thread on Linux, so Apply must actually
// change the calling thread rather than silently doing nothing.
func TestApplyLowersCallingThreadPriority(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	before, err := unix.Getpriority(unix.PRIO_PROCESS, 0)
	if err != nil {
		t.Skipf("cannot read priority: %v", err)
	}
	// Getpriority returns 20-nice to keep the value positive.
	beforeNice := 20 - before

	if err := (Priority{Nice: 12}).Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	after, err := unix.Getpriority(unix.PRIO_PROCESS, 0)
	if err != nil {
		t.Fatal(err)
	}
	afterNice := 20 - after

	if afterNice != 12 {
		t.Errorf("nice = %d after Apply, want 12 (was %d)", afterNice, beforeNice)
	}
}

func TestPriorityStringIsReadable(t *testing.T) {
	if got := DefaultPriority().String(); got != "nice=10 io=best-effort/7 cpus=all" {
		t.Errorf("String() = %q", got)
	}
	// --gentle reserves cores, so its width depends on the machine.
	want := fmt.Sprintf("nice=19 io=idle/0 cpus=%d", max(runtime.NumCPU()/2, 1))
	if got := GentlePriority().String(); got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

// Confining to fewer cores must actually narrow the thread's affinity mask, and
// must leave the low-numbered cores - where an unbalanced interrupt lands - out
// of the scan's reach.
func TestConfineToCPUsNarrowsTheMask(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	var before unix.CPUSet
	if err := unix.SchedGetaffinity(0, &before); err != nil {
		t.Skipf("cannot read affinity here: %v", err)
	}
	if before.Count() < 2 {
		t.Skip("needs at least two usable cores")
	}
	t.Cleanup(func() { _ = unix.SchedSetaffinity(0, &before) })

	if err := confineToCPUs(1); err != nil {
		t.Fatal(err)
	}
	var after unix.CPUSet
	if err := unix.SchedGetaffinity(0, &after); err != nil {
		t.Fatal(err)
	}
	if after.Count() != 1 {
		t.Fatalf("confined to %d core(s), want 1", after.Count())
	}
	if after.IsSet(0) {
		t.Error("CPU 0 must be the first core released, not the one kept")
	}
}

// Asking for more cores than are available must leave the mask alone rather
// than widen it past what a cgroup or taskset already allows.
func TestConfineToCPUsNeverWidens(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	var before unix.CPUSet
	if err := unix.SchedGetaffinity(0, &before); err != nil {
		t.Skipf("cannot read affinity here: %v", err)
	}
	t.Cleanup(func() { _ = unix.SchedSetaffinity(0, &before) })

	if err := confineToCPUs(before.Count() + 8); err != nil {
		t.Fatal(err)
	}
	var after unix.CPUSet
	if err := unix.SchedGetaffinity(0, &after); err != nil {
		t.Fatal(err)
	}
	if after.Count() != before.Count() {
		t.Errorf("mask changed from %d to %d core(s)", before.Count(), after.Count())
	}
}
