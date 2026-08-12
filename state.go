package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// State is the record of which labels the daemon set itself. Without it every
// meaningful name looks human-made after a restart: the looksOurs heuristic can
// vary the status and the presence of a process, but it cannot guess the name
// of a program that has already exited. Example: a tab was named "btop", the
// user quit btop — restoring it to "rmk" is only possible while knowing that
// "btop" was ours.

type state struct {
	path   string
	Labels map[string]string `json:"labels"`
}

func StatePath() string {
	if p := os.Getenv("HERDRTABRENAMER_STATE"); p != "" {
		return p
	}
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "herdrtabrenamer", "labels.json")
}

// LoadState reads the state. A missing or corrupt file is not an error: we
// start from scratch, losing only the knowledge of our previous labels.
func LoadState(path string) *state {
	s := &state{path: path, Labels: map[string]string{}}
	if path == "" {
		return s
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	var loaded state
	if err := json.Unmarshal(data, &loaded); err != nil {
		return s
	}
	if loaded.Labels != nil {
		s.Labels = loaded.Labels
	}
	return s
}

// Save writes the state atomically: first into a temporary file next to it,
// then rename — an interrupted write leaves no broken JSON behind.
func (s *state) Save() error {
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("replacing %s: %w", s.path, err)
	}
	return nil
}

// Prune drops entries for tabs that no longer exist, so the file does not grow
// forever. Returns true when something was removed.
func (s *state) Prune(alive map[string]bool) bool {
	removed := false
	for tabID := range s.Labels {
		if !alive[tabID] {
			delete(s.Labels, tabID)
			removed = true
		}
	}
	return removed
}
