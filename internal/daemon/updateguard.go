package daemon

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/baney75/tentmaker/internal/config"
)

// After a self-update the daemon leaves a pending-update marker in the
// state directory. The new binary deletes it once the control API serves
// and the worker has ticked. If the marker is still there 2 minutes after
// the update (the new binary keeps failing and the service manager keeps
// restarting it, or it hangs), the daemon rolls back to the previous
// binary, records the version as bad so auto-update skips it, and exits so
// the service manager starts the previous binary.
const (
	pendingFile = "update-pending.json"
	badFile     = "update-bad.json"
	// UpdateGrace is how long a new binary has to get healthy.
	UpdateGrace = 2 * time.Minute
	maxBad      = 20
)

// stateFS is the file access the guard needs; tests swap in memory.
type stateFS interface {
	ReadFile(path string) ([]byte, error)
	WriteFile(path string, data []byte) error
	Remove(path string) error
}

type osStateFS struct{}

func (osStateFS) ReadFile(p string) ([]byte, error) { return os.ReadFile(p) }
func (osStateFS) WriteFile(p string, b []byte) error {
	return config.WriteAtomic(p, b, 0o600)
}
func (osStateFS) Remove(p string) error {
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

type pendingUpdate struct {
	From string    `json:"from"`
	To   string    `json:"to"`
	At   time.Time `json:"at"`
}

type badVersions struct {
	Versions []string `json:"versions"`
}

type updateGuard struct {
	fs  stateFS
	dir string
}

func (g updateGuard) path(name string) string { return filepath.Join(g.dir, name) }

// markPending records an update about to restart into a new binary.
func (g updateGuard) markPending(from, to string, at time.Time) error {
	b, err := json.Marshal(pendingUpdate{From: from, To: to, At: at.UTC()})
	if err != nil {
		return err
	}
	return g.fs.WriteFile(g.path(pendingFile), b)
}

// pending reads the marker. A malformed marker counts as none.
func (g updateGuard) pending() (pendingUpdate, bool) {
	b, err := g.fs.ReadFile(g.path(pendingFile))
	if err != nil {
		return pendingUpdate{}, false
	}
	var p pendingUpdate
	if json.Unmarshal(b, &p) != nil || p.To == "" || p.At.IsZero() {
		return pendingUpdate{}, false
	}
	return p, true
}

func (g updateGuard) clearPending() error { return g.fs.Remove(g.path(pendingFile)) }

func (g updateGuard) bad() []string {
	b, err := g.fs.ReadFile(g.path(badFile))
	if err != nil {
		return nil
	}
	var v badVersions
	if json.Unmarshal(b, &v) != nil {
		return nil
	}
	return v.Versions
}

func (g updateGuard) isBad(version string) bool { return slices.Contains(g.bad(), version) }

func (g updateGuard) markBad(version string) error {
	vs := g.bad()
	if slices.Contains(vs, version) {
		return nil
	}
	vs = append(vs, version)
	if len(vs) > maxBad {
		vs = vs[len(vs)-maxBad:]
	}
	b, err := json.Marshal(badVersions{Versions: vs})
	if err != nil {
		return err
	}
	return g.fs.WriteFile(g.path(badFile), b)
}

// startVerdict is what a starting daemon does about a pending update.
type startVerdict int

const (
	verdictNone     startVerdict = iota // no marker
	verdictWatch                        // prove health before the grace ends, then clear
	verdictRollback                     // the grace ended: roll back
	verdictStale                        // the marker is for another version: clear it
)

// decideStart is the decision at start, or while watching: version is the
// running binary's.
func decideStart(p pendingUpdate, ok bool, version string, now time.Time) startVerdict {
	switch {
	case !ok:
		return verdictNone
	case p.To != version:
		return verdictStale
	case now.Sub(p.At) > UpdateGrace:
		return verdictRollback
	default:
		return verdictWatch
	}
}
