package daemon

import (
	"io/fs"
	"sync"
	"testing"
	"time"
)

// memFS is an in-memory stateFS.
type memFS struct {
	mu    sync.Mutex
	files map[string][]byte
}

func (m *memFS) ReadFile(p string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.files[p]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return append([]byte(nil), b...), nil
}

func (m *memFS) WriteFile(p string, b []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.files[p] = append([]byte(nil), b...)
	return nil
}

func (m *memFS) Remove(p string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.files, p)
	return nil
}

func TestUpdateGuardDecisions(t *testing.T) {
	mfs := &memFS{files: map[string][]byte{}}
	g := updateGuard{fs: mfs, dir: "/state"}
	t0 := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

	if p, ok := g.pending(); decideStart(p, ok, "1.1.0", t0) != verdictNone {
		t.Fatal("no marker should mean nothing to do")
	}
	if err := g.markPending("1.0.0", "1.1.0", t0); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		version string
		at      time.Duration
		want    startVerdict
	}{
		{"1.1.0", 5 * time.Second, verdictWatch},
		{"1.1.0", UpdateGrace, verdictWatch},
		{"1.1.0", UpdateGrace + time.Second, verdictRollback},
		{"1.1.0", -time.Minute, verdictWatch}, // clock stepped back
		{"1.0.0", time.Hour, verdictStale},    // the old binary already runs
	} {
		p, ok := g.pending()
		if got := decideStart(p, ok, c.version, t0.Add(c.at)); got != c.want {
			t.Errorf("version %s at +%s: %v, want %v", c.version, c.at, got, c.want)
		}
	}

	// A malformed marker counts as none.
	mfs.files[g.path(pendingFile)] = []byte("{nope")
	if _, ok := g.pending(); ok {
		t.Error("a malformed marker was read")
	}
	if err := g.clearPending(); err != nil {
		t.Fatal(err)
	}
	if _, ok := mfs.files[g.path(pendingFile)]; ok {
		t.Error("marker not cleared")
	}

	// Bad versions: recorded once, capped.
	for i := 0; i < 3; i++ {
		if err := g.markBad("1.1.0"); err != nil {
			t.Fatal(err)
		}
	}
	if !g.isBad("1.1.0") || g.isBad("1.0.0") || len(g.bad()) != 1 {
		t.Fatalf("bad = %v", g.bad())
	}
	for i := 0; i < maxBad+5; i++ {
		g.markBad("2.0." + string(rune('a'+i)))
	}
	if n := len(g.bad()); n != maxBad {
		t.Fatalf("%d bad versions kept", n)
	}
}
