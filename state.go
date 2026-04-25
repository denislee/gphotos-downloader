package main

import (
	"bufio"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// State tracks the IDs of photos that have already been included in a
// successful download, persisted as one ID per line. The file is human
// editable — blank lines and lines starting with '#' are ignored, which
// is useful if the user wants to re-download a specific photo.
type State struct {
	path string
	mu   sync.Mutex
	done map[string]bool
}

func LoadState(path string) (*State, error) {
	s := &State{path: path, done: map[string]bool{}}
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		id := sc.Text()
		if id == "" || id[0] == '#' {
			continue
		}
		s.done[id] = true
	}
	return s, sc.Err()
}

// Done returns the set of completed photo IDs. The map is shared with the
// State; callers must not mutate it.
func (s *State) Done() map[string]bool { return s.done }

// Append marks ids as done and appends new ones to the state file. Already
// known IDs are not re-written.
func (s *State) Append(ids []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, id := range ids {
		if !s.done[id] {
			s.done[id] = true
			if _, err := f.WriteString(id + "\n"); err != nil {
				return err
			}
		}
	}
	return nil
}
