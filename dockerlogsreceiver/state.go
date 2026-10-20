package dockerlogsreceiver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// position is how far a container's log has been delivered: the timestamp of the last line and
// how many lines carried exactly that timestamp. Docker's `since` is inclusive and timestamps are
// only nanosecond-unique in practice, so on resume lines older than ts are dropped and the first
// n lines at ts are too — nothing is sent twice, nothing after it is lost.
type position struct {
	TS time.Time `json:"ts"`
	N  int       `json:"n"`
}

type stateData struct {
	SavedAt    time.Time           `json:"saved_at"`
	Containers map[string]position `json:"containers"`
}

// stateStore keeps positions in memory and writes them to a small JSON file now and then.
type stateStore struct {
	path string

	mu      sync.Mutex
	data    stateData
	dirty   bool
	existed bool // a previous run left a state file
}

// forgetAfter drops positions of containers not heard of for a week, so the file can't grow
// without bound on hosts that churn through containers.
const forgetAfter = 7 * 24 * time.Hour

func loadState(path string) *stateStore {
	s := &stateStore{path: path, data: stateData{Containers: map[string]position{}}}
	if path == "" {
		return s
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	var d stateData
	if json.Unmarshal(raw, &d) != nil {
		return s
	}
	s.existed = true
	s.data.SavedAt = d.SavedAt
	for id, p := range d.Containers {
		if time.Since(p.TS) < forgetAfter {
			s.data.Containers[id] = p
		}
	}
	return s
}

func (s *stateStore) get(id string) (position, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.data.Containers[id]
	return p, ok
}

// previousRun is when the last run last saved its state; zero on the first run ever.
func (s *stateStore) previousRun() (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.SavedAt, s.existed && !s.data.SavedAt.IsZero()
}

// advance records that lines up to and including ts were delivered.
func (s *stateStore) advance(id string, ts []time.Time) {
	if len(ts) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.data.Containers[id]
	for _, t := range ts {
		switch {
		case t.IsZero():
		case t.After(p.TS):
			p = position{TS: t, N: 1}
		case t.Equal(p.TS):
			p.N++
		}
	}
	s.data.Containers[id] = p
	s.dirty = true
}

func (s *stateStore) forget(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data.Containers[id]; ok {
		delete(s.data.Containers, id)
		s.dirty = true
	}
}

// save writes the file atomically. With force it writes even when nothing changed, which
// refreshes saved_at — the mark of how long the collector has been running.
func (s *stateStore) save(force bool) error {
	if s.path == "" {
		return nil
	}
	s.mu.Lock()
	if !s.dirty && !force {
		s.mu.Unlock()
		return nil
	}
	s.data.SavedAt = time.Now().UTC()
	raw, err := json.MarshalIndent(s.data, "", "  ")
	s.dirty = false
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o750); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// resumeFilter drops the lines a previous run already delivered.
type resumeFilter struct {
	pos  position
	seen int
}

func (f *resumeFilter) skip(ts time.Time) bool {
	if f == nil || f.pos.TS.IsZero() || ts.IsZero() {
		return false
	}
	if ts.Before(f.pos.TS) {
		return true
	}
	if ts.Equal(f.pos.TS) && f.seen < f.pos.N {
		f.seen++
		return true
	}
	return false
}
