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
		t.Fatalf("отсутствующий файл должен давать пустое состояние, получено %v", s.Labels)
	}

	s.Labels["w4:t1F"] = "btop"
	s.Labels["w4:t14"] = "nixos"
	if err := s.Save(); err != nil {
		t.Fatalf("сохранение не удалось: %v", err)
	}

	// Каталог должен создаться сам, временный файл — не остаться.
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("временный файл остался после Save")
	}

	loaded := LoadState(path)
	if loaded.Labels["w4:t1F"] != "btop" || loaded.Labels["w4:t14"] != "nixos" {
		t.Errorf("состояние не восстановилось: %v", loaded.Labels)
	}
}

func TestLoadStateToleratesGarbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "labels.json")
	if err := os.WriteFile(path, []byte("{это не json"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := LoadState(path)
	if s.Labels == nil || len(s.Labels) != 0 {
		t.Errorf("битый файл должен давать пустое состояние, получено %v", s.Labels)
	}
	// И запись поверх мусора обязана работать.
	s.Labels["w1:t1"] = "x"
	if err := s.Save(); err != nil {
		t.Fatalf("перезапись битого файла не удалась: %v", err)
	}
	if got := LoadState(path).Labels["w1:t1"]; got != "x" {
		t.Errorf("получено %q", got)
	}
}

func TestStatePrune(t *testing.T) {
	s := &state{Labels: map[string]string{
		"w4:t1": "a",
		"w4:t2": "b",
		"w4:t3": "c",
	}}

	if !s.Prune(map[string]bool{"w4:t1": true, "w4:t3": true}) {
		t.Error("Prune должен сообщить об удалении")
	}
	if len(s.Labels) != 2 || s.Labels["w4:t2"] != "" {
		t.Errorf("остались лишние записи: %v", s.Labels)
	}
	if s.Prune(map[string]bool{"w4:t1": true, "w4:t3": true}) {
		t.Error("повторный Prune не должен ничего удалять")
	}
}

func TestStateWithoutPathIsNoop(t *testing.T) {
	s := LoadState("")
	s.Labels["w1:t1"] = "x"
	if err := s.Save(); err != nil {
		t.Errorf("пустой путь должен молча ничего не делать, получено %v", err)
	}
}
