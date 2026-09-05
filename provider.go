package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Snapshot is one point-in-time copy of a live subvolume.
type Snapshot struct {
	ID           string    `json:"id"`   // provider-local identifier
	Root         string    `json:"root"` // absolute path to the snapshot's subvolume root
	Created      time.Time `json:"created"`
	Tag          string    `json:"tag,omitempty"` // hourly, daily, ondemand, ...
	ReadOnly     bool      `json:"read_only"`
	ReceivedUUID string    `json:"received_uuid,omitempty"`

	// UUID and CTransID identify one immutable state of this snapshot and are
	// what the scan cache keys on. Both zero means the kernel would not tell
	// us, and the snapshot is then simply never cached.
	UUID     string `json:"uuid,omitempty"`
	CTransID uint64 `json:"ctransid,omitempty"`
}

// Cacheable reports whether this snapshot carries an identity strong enough to
// key a cache entry on.
func (s Snapshot) Cacheable() bool { return s.UUID != "" && s.CTransID != 0 }

// Pair links a live subvolume to the snapshots covering it. Everything the tool
// does is expressed in terms of pairs, which is what keeps it independent of
// any particular snapshot manager.
type Pair struct {
	Provider  string     `json:"provider"`
	Name      string     `json:"name"` // display name, e.g. "@home"
	Live      string     `json:"live"`
	Snapshots []Snapshot `json:"snapshots"`
}

// Provider discovers pairs for one snapshot manager's layout.
type Provider interface {
	Name() string
	Detect(sys *System) ([]Pair, error)
}

// System is the shared view of the machine handed to every provider.
type System struct {
	Mounts  []MountEntry
	Mounter *Mounter
	Verbose bool

	notes []string
}

// Notef records a diagnostic that `doctor` shows, so a user on an unrecognised
// layout can see why detection came up empty instead of just getting silence.
func (s *System) Notef(format string, args ...any) {
	s.notes = append(s.notes, fmt.Sprintf(format, args...))
}

func (s *System) Notes() []string { return s.notes }

// TopLevelFor returns a readable path to the top level of the filesystem that
// serves the given btrfs mount.
func (s *System) TopLevelFor(m MountEntry) (string, error) {
	return s.Mounter.TopLevel(m.Source, s.Mounts)
}

// NewSystem gathers the machine state providers need.
func NewSystem(mounter *Mounter, verbose bool) (*System, error) {
	mounts, err := ReadMounts()
	if err != nil {
		return nil, err
	}
	return &System{Mounts: mounts, Mounter: mounter, Verbose: verbose}, nil
}

// AllProviders returns the built-in providers in detection order.
func AllProviders() []Provider {
	return []Provider{
		&TimeshiftProvider{},
		&SnapperProvider{},
		&BtrbkProvider{},
	}
}

// Detect runs the requested providers and returns every pair found. An unknown
// provider name is an error; a provider that finds nothing is not.
func Detect(sys *System, want string) ([]Pair, error) {
	var chosen []Provider
	for _, p := range AllProviders() {
		if want == "auto" || want == p.Name() {
			chosen = append(chosen, p)
		}
	}
	if len(chosen) == 0 {
		names := []string{"auto", "generic"}
		for _, p := range AllProviders() {
			names = append(names, p.Name())
		}
		return nil, fmt.Errorf("unknown provider %q (known: %s)", want, strings.Join(names, ", "))
	}

	var all []Pair
	for _, p := range chosen {
		started := time.Now()
		pairs, err := p.Detect(sys)
		elapsed := time.Since(started).Round(time.Millisecond)
		if err != nil {
			Warnf("detect", "provider %s failed after %s: %v", p.Name(), elapsed, err)
			sys.Notef("provider %s: %v", p.Name(), err)
			continue
		}
		if len(pairs) == 0 {
			Infof("detect", "provider %s found nothing (%s)", p.Name(), elapsed)
			sys.Notef("provider %s: no snapshots found", p.Name())
		} else {
			Infof("detect", "provider %s found %d pair(s) in %s", p.Name(), len(pairs), elapsed)
		}
		all = append(all, pairs...)
	}
	deduped := dedupePairs(all)
	if len(deduped) != len(all) {
		Debugf("detect", "deduplicated %d pair(s) down to %d", len(all), len(deduped))
	}
	return deduped, nil
}

// DetectGeneric builds a pair from an explicit live/snapshots directory, the
// escape hatch for layouts no provider recognises.
func DetectGeneric(live, snapshotsDir string) (Pair, error) {
	live = filepath.Clean(live)
	snapshotsDir = filepath.Clean(snapshotsDir)

	if ok, err := IsBtrfs(live); err != nil || !ok {
		return Pair{}, fmt.Errorf("%s is not on a btrfs filesystem", live)
	}
	entries, err := os.ReadDir(snapshotsDir)
	if err != nil {
		return Pair{}, fmt.Errorf("reading %s: %w", snapshotsDir, err)
	}

	Debugf("detect", "generic: live=%s snapshots=%s (%d entries)", live, snapshotsDir, len(entries))
	pair := Pair{Provider: "generic", Name: filepath.Base(live), Live: live}
	for _, e := range entries {
		if !e.IsDir() {
			Tracef("detect", "generic: %s is not a directory, skipped", e.Name())
			continue
		}
		root := filepath.Join(snapshotsDir, e.Name())
		if !IsSubvolRoot(root) {
			Debugf("detect", "generic: %s is not a btrfs subvolume root, skipped", root)
			continue
		}
		pair.Snapshots = append(pair.Snapshots, describeSnapshot(e.Name(), root, time.Time{}, ""))
	}
	if len(pair.Snapshots) == 0 {
		return Pair{}, fmt.Errorf("no btrfs subvolumes found directly under %s", snapshotsDir)
	}
	sortSnapshots(pair.Snapshots)
	return pair, nil
}

// describeSnapshot fills in the flags every provider needs, falling back to the
// directory mtime when the manager records no creation time of its own.
func describeSnapshot(id, root string, created time.Time, tag string) Snapshot {
	s := Snapshot{ID: id, Root: root, Created: created, Tag: tag}
	if info, err := GetSubvolInfo(root); err == nil {
		s.ReadOnly = info.ReadOnly
		s.ReceivedUUID = info.ReceivedUUID
		s.UUID = info.UUID
		s.CTransID = info.CTransID
	} else {
		// Never let "could not tell" quietly become "writable": that is the
		// wrong direction to guess in for a flag that governs whether purge
		// must restore it afterwards.
		Warnf("detect", "cannot read subvolume info for %s: %v", root, err)
		if ro, rerr := IsReadOnlySubvol(root); rerr == nil {
			s.ReadOnly = ro
		} else {
			Warnf("detect", "cannot read subvolume flags for %s either: %v", root, rerr)
		}
	}
	if s.Created.IsZero() {
		if st, err := os.Stat(root); err == nil {
			s.Created = st.ModTime()
		}
	}
	return s
}

func sortSnapshots(s []Snapshot) {
	sort.Slice(s, func(i, j int) bool {
		if !s[i].Created.Equal(s[j].Created) {
			return s[i].Created.After(s[j].Created)
		}
		return s[i].ID > s[j].ID
	})
}

// dedupePairs drops pairs that cover the same live path with the same snapshot
// roots, which happens when two providers recognise one layout.
func dedupePairs(pairs []Pair) []Pair {
	seen := map[string]bool{}
	var out []Pair
	for _, p := range pairs {
		var key strings.Builder
		key.WriteString(p.Live)
		for _, s := range p.Snapshots {
			key.WriteString("\x00")
			key.WriteString(s.Root)
		}
		if seen[key.String()] {
			continue
		}
		seen[key.String()] = true
		out = append(out, p)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Live < out[j].Live })
	return out
}

// Validate checks that a pair is usable: the live path and every snapshot must
// sit on the same btrfs filesystem, since extents cannot be shared across
// filesystems and the reclaim arithmetic would otherwise be meaningless.
func (p *Pair) Validate() error {
	liveID, err := FilesystemID(p.Live)
	if err != nil {
		Warnf("validate", "pair %s: cannot read filesystem id of live path %s: %v", p.Name, p.Live, err)
		return fmt.Errorf("%s: %w", p.Live, err)
	}
	var kept []Snapshot
	for _, s := range p.Snapshots {
		id, err := FilesystemID(s.Root)
		if err != nil {
			Warnf("validate", "pair %s: cannot read filesystem id of %s: %v", p.Name, s.Root, err)
			continue
		}
		if id != liveID {
			Warnf("validate", "pair %s: snapshot %s is on filesystem %s, live is on %s; dropped",
				p.Name, s.ID, id, liveID)
			continue
		}
		kept = append(kept, s)
	}
	Debugf("validate", "pair %s: %d of %d snapshot(s) share filesystem %s with %s",
		p.Name, len(kept), len(p.Snapshots), liveID, p.Live)
	if len(kept) == 0 {
		return fmt.Errorf("no snapshots for %s are on the same filesystem", p.Live)
	}
	p.Snapshots = kept
	return nil
}
