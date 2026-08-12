package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "labels.json")

	s := LoadState(path)
	if len(s.Labels) != 0 {
		t.Fatalf("a missing file should yield an empty state, got %v", s.Labels)
	}

	s.Labels["w4:t1F"] = "btop"
	s.Labels["w4:t14"] = "nixos"
	if err := s.Save(); err != nil {
		t.Fatalf("save failed: %v", err)
	}

	// The directory must be created on its own and the temp file must not remain.
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("the temporary file survived Save")
	}

	loaded := LoadState(path)
	if loaded.Labels["w4:t1F"] != "btop" || loaded.Labels["w4:t14"] != "nixos" {
		t.Errorf("state was not restored: %v", loaded.Labels)
	}
}

func TestLoadStateToleratesGarbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "labels.json")
	if err := os.WriteFile(path, []byte("{this is not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := LoadState(path)
	if s.Labels == nil || len(s.Labels) != 0 {
		t.Errorf("a corrupt file should yield an empty state, got %v", s.Labels)
	}
	// And writing over the garbage has to work.
	s.Labels["w1:t1"] = "x"
	if err := s.Save(); err != nil {
		t.Fatalf("overwriting a corrupt file failed: %v", err)
	}
	if got := LoadState(path).Labels["w1:t1"]; got != "x" {
		t.Errorf("got %q", got)
	}
}

func TestStatePrune(t *testing.T) {
	s := &state{Labels: map[string]string{
		"w4:t1": "a",
		"w4:t2": "b",
		"w4:t3": "c",
	}}

	if !s.Prune(map[string]bool{"w4:t1": true, "w4:t3": true}) {
		t.Error("Prune should report the removal")
	}
	if len(s.Labels) != 2 || s.Labels["w4:t2"] != "" {
		t.Errorf("stale entries remain: %v", s.Labels)
	}
	if s.Prune(map[string]bool{"w4:t1": true, "w4:t3": true}) {
		t.Error("a repeated Prune should remove nothing")
	}
}

func TestStateWithoutPathIsNoop(t *testing.T) {
	s := LoadState("")
	s.Labels["w1:t1"] = "x"
	if err := s.Save(); err != nil {
		t.Errorf("an empty path should silently do nothing, got %v", err)
	}
}
