package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Состояние — то, какие метки демон поставил сам. Без него любое осмысленное
// имя после перезапуска выглядит как заданное человеком: эвристика looksOurs
// умеет варьировать статус и наличие процесса, но не может угадать имя
// программы, которая уже завершилась. Пример: таб звался "btop", пользователь
// вышел из btop — вернуть его к "rmk" можно только зная, что "btop" наше.

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

// LoadState читает состояние. Отсутствующий или битый файл — не ошибка:
// начинаем с чистого листа, потеряв только знание о своих прошлых метках.
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

// Save пишет состояние атомарно: сначала во временный файл рядом, затем
// rename — оборванная запись не оставит битый JSON.
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
		return fmt.Errorf("подмена %s: %w", s.path, err)
	}
	return nil
}

// Prune выбрасывает записи табов, которых больше нет, чтобы файл не пух.
// Возвращает true, если что-то удалено.
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
