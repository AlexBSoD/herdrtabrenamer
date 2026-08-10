package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Шаблон имени таба. Токены: {agent} {dir} {cwd} {title} {status} {number}.
// Литералы между токенами (разделители) выбрасываются, если хотя бы один из
// соседних токенов пуст — иначе "{agent}:{dir}" без агента давал бы ":nixos".
type Template struct {
	parts []templatePart
}

type templatePart struct {
	literal string
	token   string // непусто, если это токен
}

// Токен может перечислять альтернативы через "|": {proc|agent} берёт имя
// процесса, а если его нет — имя агента.
var tokenRe = regexp.MustCompile(`\{([a-z_|]+)\}`)

func ParseTemplate(s string) Template {
	var parts []templatePart
	last := 0
	for _, m := range tokenRe.FindAllStringSubmatchIndex(s, -1) {
		if m[0] > last {
			parts = append(parts, templatePart{literal: s[last:m[0]]})
		}
		parts = append(parts, templatePart{token: s[m[2]:m[3]]})
		last = m[1]
	}
	if last < len(s) {
		parts = append(parts, templatePart{literal: s[last:]})
	}
	return Template{parts: parts}
}

// NameContext — значения токенов для одного таба.
type NameContext struct {
	Agent  string
	Proc   string
	Dir    string
	Cwd    string
	Title  string
	Status string
	Icon   string
	Number int
}

// token отдаёт значение токена; для "a|b|c" — первое непустое.
func (c NameContext) token(name string) string {
	for _, alt := range strings.Split(name, "|") {
		if v := c.single(alt); v != "" {
			return v
		}
	}
	return ""
}

func (c NameContext) single(name string) string {
	switch name {
	case "agent":
		return c.Agent
	case "proc":
		return c.Proc
	case "dir":
		return c.Dir
	case "cwd":
		return c.Cwd
	case "title":
		return c.Title
	case "status":
		return c.Status
	case "icon":
		return c.Icon
	case "number":
		return strconv.Itoa(c.Number)
	default:
		return ""
	}
}

// DefaultIcons — цвет в таб-баре задать нечем (в socket API нет ни цветов, ни
// стилей), поэтому индикатор статуса делается эмодзи: они несут свой цвет.
var DefaultIcons = map[string]string{
	"blocked": "🔴",
	"working": "🟡",
	"done":    "🟢",
	"idle":    "⚪",
	"unknown": "",
}

// ParseIcons разбирает "working=🟡,done=✅" поверх набора по умолчанию.
// Пустое значение справа убирает иконку для статуса.
func ParseIcons(s string) (map[string]string, error) {
	icons := make(map[string]string, len(DefaultIcons))
	for k, v := range DefaultIcons {
		icons[k] = v
	}
	if strings.TrimSpace(s) == "" {
		return icons, nil
	}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		k, v, ok := strings.Cut(pair, "=")
		if !ok {
			return nil, fmt.Errorf("ожидалось статус=иконка, получено %q", pair)
		}
		k = strings.TrimSpace(k)
		if _, known := DefaultIcons[k]; !known {
			return nil, fmt.Errorf("неизвестный статус %q (известны: blocked, working, done, idle, unknown)", k)
		}
		icons[k] = strings.TrimSpace(v)
	}
	return icons, nil
}

func (t Template) Render(ctx NameContext) string {
	var out strings.Builder
	pending := ""     // литералы, ещё не решившие свою судьбу
	dropNext := false // предыдущий токен был пуст — съедаем и разделитель за ним
	haveValue := false

	for _, p := range t.parts {
		if p.token == "" {
			pending += p.literal
			continue
		}
		v := strings.TrimSpace(ctx.token(p.token))
		if v == "" {
			// Пустой токен забирает с собой разделители с обеих сторон:
			// "{agent}:{dir}" без агента даёт "nixos", а не ":nixos".
			pending = ""
			dropNext = true
			continue
		}
		if dropNext {
			pending = ""
			dropNext = false
		}
		out.WriteString(pending)
		pending = ""
		out.WriteString(v)
		haveValue = true
	}
	// Замыкающий литерал нужен, только если после него не было съеденного
	// токена: "[{status}]" со статусом даёт "[idle]", без статуса — "".
	if haveValue && !dropNext {
		out.WriteString(pending)
	}
	return strings.TrimSpace(out.String())
}

// Truncate обрезает имя по видимым символам, а не байтам — иначе кириллица
// и символы статусов режутся посередине рун.
func Truncate(s string, max int) string {
	if max <= 0 || utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	if max == 1 {
		return string(runes[:1])
	}
	return string(runes[:max-1]) + "…"
}

// PrettyDir — короткое имя каталога: basename, но $HOME превращается в "~",
// а корень репозитория остаётся узнаваемым.
func PrettyDir(path string) string {
	if path == "" {
		return ""
	}
	clean := filepath.Clean(path)
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if clean == home {
			return "~"
		}
	}
	if clean == "/" {
		return "/"
	}
	return filepath.Base(clean)
}

// shells — то, что запущено в панели «просто так»: имя шелла в названии таба
// бесполезно, поэтому такой процесс считается отсутствующим.
var shells = map[string]bool{
	"fish": true, "bash": true, "zsh": true, "sh": true, "dash": true,
	"ksh": true, "csh": true, "tcsh": true, "nu": true, "elvish": true,
	"xonsh": true, "login": true,
}

// wrapperSuffixes — обвязки, которые нацепляет NixOS: .claude-wrapped,
// btop-wrap и прочее. Из имени их нужно убрать.
var wrapperSuffixes = []string{"-wrapped", "-wrap"}

// ProcName вытаскивает человекочитаемое имя foreground-процесса панели.
// Пустая строка означает «ничего интереснее шелла не запущено».
func ProcName(info *paneProcessInfo) string {
	if info == nil || len(info.ForegroundProcesses) == 0 {
		return ""
	}
	// В группе может быть несколько процессов (агент плюс его MCP-сервера);
	// нужен лидер группы, а не случайный дочерний.
	proc := info.ForegroundProcesses[0]
	for _, p := range info.ForegroundProcesses {
		if info.ForegroundProcessGroupID != 0 && p.PID == info.ForegroundProcessGroupID {
			proc = p
			break
		}
	}
	if proc.PID != 0 && proc.PID == info.ShellPid {
		return ""
	}

	name := cleanProcName(firstField(proc.Cmdline))
	if name == "" {
		name = cleanProcName(proc.Argv0)
	}
	if name == "" {
		name = cleanProcName(proc.Name)
	}
	if shells[name] {
		return ""
	}
	return name
}

func firstField(s string) string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

func cleanProcName(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	s = filepath.Base(s)
	s = strings.TrimPrefix(s, ".")
	for _, suffix := range wrapperSuffixes {
		s = strings.TrimSuffix(s, suffix)
	}
	return s
}

// autoLabelRe — метки, которые herdr генерирует сам, когда
// ui.prompt_new_tab_name = false: просто порядковый номер.
var autoLabelRe = regexp.MustCompile(`^\d+$`)

// IsGeneratedLabel сообщает, выглядит ли метка как автоматическая. Такие метки
// можно перезаписывать без спроса; всё остальное считаем ручным именем.
func IsGeneratedLabel(label string) bool {
	return label == "" || autoLabelRe.MatchString(strings.TrimSpace(label))
}

// leadPane выбирает панель, по которой называется таб: сначала панель с
// определённым агентом, затем сфокусированная, затем первая по pane_id.
func leadPane(panes []paneInfo) *paneInfo {
	var withAgent, focused, first *paneInfo
	for i := range panes {
		p := &panes[i]
		if first == nil || p.PaneID < first.PaneID {
			first = p
		}
		if p.Focused && focused == nil {
			focused = p
		}
		if p.Agent != "" && withAgent == nil {
			withAgent = p
		}
	}
	switch {
	case withAgent != nil:
		return withAgent
	case focused != nil:
		return focused
	default:
		return first
	}
}

// contextFor собирает значения токенов из панели и таба.
// proc — имя foreground-процесса ведущей панели (может быть пустым).
func contextFor(tab tabInfo, pane *paneInfo, proc string, icons map[string]string) NameContext {
	ctx := NameContext{Number: tab.Number, Status: tab.AgentStatus, Proc: proc}
	if pane != nil {
		ctx.Agent = pane.DisplayAgent
		if ctx.Agent == "" {
			ctx.Agent = pane.Agent
		}
		cwd := pane.ForegroundCwd
		if cwd == "" {
			cwd = pane.Cwd
		}
		ctx.Cwd = cwd
		ctx.Dir = PrettyDir(cwd)
		ctx.Title = pane.TerminalTitleStrip
		if ctx.Title == "" {
			ctx.Title = pane.TerminalTitle
		}
		if pane.AgentStatus != "" {
			ctx.Status = pane.AgentStatus
		}
	}
	ctx.Icon = icons[ctx.Status]
	return ctx
}
