package runs

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/core"
)

const (
	// DefaultMax is how many records a device keeps (config history_max).
	DefaultMax = 2000
	// LogTailBytes is how much of each job's log a device keeps.
	LogTailBytes = 512 << 10

	historyFile = "history.json"
	logsDir     = "logs"
	fileVersion = 1
)

// ErrNotFound means no record or log has that id.
var ErrNotFound = core.ErrNotFound

// idRe is the shape of a record id: a runner name (core.NamePattern), so an
// id is always safe in a file name.
var idRe = core.NamePattern

// ValidID reports whether id can name a record.
func ValidID(id string) bool { return idRe.MatchString(id) }

// Store keeps one device's records in <dir>/history.json (newest Max) and
// each job's log tail in <dir>/logs/<id>.log, both written with
// config.WriteAtomic. Safe for concurrent use.
type Store struct {
	dir string
	max int

	mu     sync.Mutex
	recs   []Record // oldest first
	loaded bool
	err    error
}

type historyDoc struct {
	Version int      `json:"version"`
	Records []Record `json:"records"`
}

// Open returns the store in dir, keeping at most max records (0 or less
// means DefaultMax). It reads nothing until first use.
func Open(dir string, max int) *Store {
	if max <= 0 {
		max = DefaultMax
	}
	return &Store{dir: dir, max: max}
}

// Dir is where the store keeps its files.
func (s *Store) Dir() string { return s.dir }

func (s *Store) historyPath() string { return filepath.Join(s.dir, historyFile) }

// LogPath is where id's log tail lives.
func (s *Store) LogPath(id string) string { return filepath.Join(s.dir, logsDir, id+".log") }

// loadLocked reads history.json once. A missing file is an empty history.
// A file that can't be parsed is moved aside to history.json.bad, so a
// broken file never stops new records being kept.
func (s *Store) loadLocked() {
	if s.loaded {
		return
	}
	s.loaded = true
	b, err := os.ReadFile(s.historyPath())
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		s.err = err
		return
	}
	var doc historyDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		_ = os.Rename(s.historyPath(), s.historyPath()+".bad")
		return
	}
	s.recs = doc.Records
	if len(s.recs) > s.max {
		s.recs = s.recs[len(s.recs)-s.max:]
	}
}

// Err is the last error reading the history, if any.
func (s *Store) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	return s.err
}

// Add keeps rec and, when log is not nil, its last LogTailBytes, redacted.
// A record with the same id replaces the old one. Records past Max are
// dropped oldest first, with their logs.
func (s *Store) Add(rec Record, log []byte) error {
	if !ValidID(rec.ID) {
		return fmt.Errorf("record id %q is not a runner name", trunc(rec.ID, 80))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	if log != nil {
		tail := Redact(Tail(log, LogTailBytes))
		if err := config.WriteAtomic(s.LogPath(rec.ID), tail, 0o600); err != nil {
			return fmt.Errorf("keep the job's log: %w", err)
		}
		rec.HasLog = true
	}
	for i, r := range s.recs {
		if r.ID == rec.ID {
			s.recs = append(s.recs[:i], s.recs[i+1:]...)
			break
		}
	}
	s.recs = append(s.recs, rec)
	var dropped []Record
	if over := len(s.recs) - s.max; over > 0 {
		dropped = append(dropped, s.recs[:over]...)
		s.recs = append([]Record(nil), s.recs[over:]...)
	}
	if err := s.saveLocked(); err != nil {
		return err
	}
	for _, r := range dropped {
		_ = os.Remove(s.LogPath(r.ID))
		_ = os.Remove(s.TestsPath(r.ID))
	}
	return nil
}

func (s *Store) saveLocked() error {
	b, err := json.Marshal(historyDoc{Version: fileVersion, Records: s.recs})
	if err != nil {
		return err
	}
	if err := config.WriteAtomic(s.historyPath(), append(b, '\n'), 0o644); err != nil {
		return fmt.Errorf("save the run history: %w", err)
	}
	return nil
}

// List returns the records q picks, newest first.
func (s *Store) List(q Query) []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	return Apply(s.recs, q)
}

// Len is how many records the store holds.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	return len(s.recs)
}

// Get returns the record with id.
func (s *Store) Get(id string) (Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	for i := len(s.recs) - 1; i >= 0; i-- {
		if s.recs[i].ID == id {
			return s.recs[i], true
		}
	}
	return Record{}, false
}

// Log returns id's kept log tail, or ErrNotFound.
func (s *Store) Log(id string) ([]byte, error) {
	if !ValidID(id) {
		return nil, ErrNotFound
	}
	f, err := os.Open(s.LogPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, LogTailBytes+1))
}

// Tail returns the last n bytes of b, starting after a newline when it
// cuts, so the first line is whole.
func Tail(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	b = b[len(b)-n:]
	for i, c := range b {
		if c == '\n' && i < len(b)-1 {
			return b[i+1:]
		}
	}
	return b
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
