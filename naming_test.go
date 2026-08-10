package main

import "testing"

func TestRenderDropsDanglingSeparators(t *testing.T) {
	full := NameContext{Agent: "claude", Dir: "nixos", Status: "working", Number: 36, Title: "заголовок"}
	noAgent := NameContext{Dir: "nixos", Status: "working", Number: 36}
	noStatus := NameContext{Agent: "claude", Dir: "nixos", Number: 36}
	empty := NameContext{Number: 7}

	cases := []struct {
		name string
		tmpl string
		ctx  NameContext
		want string
	}{
		{"оба токена на месте", "{agent}:{dir}", full, "claude:nixos"},
		{"пустой первый токен съедает разделитель", "{agent}:{dir}", noAgent, "nixos"},
		{"пустой второй токен съедает разделитель", "{dir}:{agent}", noAgent, "nixos"},
		{"замыкающий литерал сохраняется", "{number}:{dir} [{status}]", full, "36:nixos [working]"},
		{"замыкающий литерал уходит с пустым токеном", "{dir} [{status}]", noStatus, "nixos"},
		{"ведущий литерал сохраняется", "[{status}] {dir}", full, "[working] nixos"},
		{"ведущий литерал уходит с пустым токеном", "[{status}] {dir}", noStatus, "nixos"},
		{"все токены пусты", "{agent}:{dir}", empty, ""},
		{"неизвестный токен считается пустым", "{agent}/{branch}", full, "claude"},
		{"только литералы без токенов", "статика", full, ""},
		{"число всегда непусто", "{number}", empty, "7"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ParseTemplate(c.tmpl).Render(c.ctx)
			if got != c.want {
				t.Errorf("шаблон %q: получено %q, ожидалось %q", c.tmpl, got, c.want)
			}
		})
	}
}

func TestTruncateCountsRunes(t *testing.T) {
	cases := []struct {
		in   string
		max  int
		want string
	}{
		{"короткое", 24, "короткое"},
		{"очень длинное имя таба", 10, "очень дли…"},
		{"кириллица", 0, "кириллица"},
		{"abc", 1, "a"},
	}
	for _, c := range cases {
		if got := Truncate(c.in, c.max); got != c.want {
			t.Errorf("Truncate(%q, %d) = %q, ожидалось %q", c.in, c.max, got, c.want)
		}
	}
	// Обрезка не должна ломать руны посередине.
	got := Truncate("абвгдеёжзийклмн", 5)
	for _, r := range got {
		if r == '�' {
			t.Fatalf("Truncate испортил руну: %q", got)
		}
	}
}

func TestIsGeneratedLabel(t *testing.T) {
	generated := []string{"", "1", "42", " 7 "}
	manual := []string{"claude:nixos", "работа", "1-й", "tab 2"}
	for _, l := range generated {
		if !IsGeneratedLabel(l) {
			t.Errorf("%q должно считаться автогенерированным", l)
		}
	}
	for _, l := range manual {
		if IsGeneratedLabel(l) {
			t.Errorf("%q должно считаться ручным", l)
		}
	}
}

func TestLeadPanePrefersAgent(t *testing.T) {
	panes := []paneInfo{
		{PaneID: "w1:p1", Focused: true},
		{PaneID: "w1:p2", Agent: "claude"},
	}
	if got := leadPane(panes); got == nil || got.PaneID != "w1:p2" {
		t.Errorf("ожидалась панель с агентом, получено %+v", got)
	}

	// Без агентов выигрывает сфокусированная.
	panes = []paneInfo{{PaneID: "w1:p1"}, {PaneID: "w1:p2", Focused: true}}
	if got := leadPane(panes); got == nil || got.PaneID != "w1:p2" {
		t.Errorf("ожидалась сфокусированная панель, получено %+v", got)
	}

	// Совсем без признаков — минимальный pane_id, детерминированно.
	panes = []paneInfo{{PaneID: "w1:p9"}, {PaneID: "w1:p3"}}
	if got := leadPane(panes); got == nil || got.PaneID != "w1:p3" {
		t.Errorf("ожидалась панель w1:p3, получено %+v", got)
	}

	if leadPane(nil) != nil {
		t.Error("на пустом списке ожидался nil")
	}
}

func TestContextForFallbacks(t *testing.T) {
	tab := tabInfo{Number: 3, AgentStatus: "idle"}
	pane := paneInfo{
		Agent:         "claude",
		Cwd:           "/home/uzz/opt/nixos",
		TerminalTitle: "сырой заголовок",
	}
	ctx := contextFor(tab, &pane, "", DefaultIcons)
	if ctx.Dir != "nixos" {
		t.Errorf("Dir = %q, ожидалось nixos", ctx.Dir)
	}
	if ctx.Title != "сырой заголовок" {
		t.Errorf("Title должен падать обратно на terminal_title, получено %q", ctx.Title)
	}

	// foreground_cwd и display_agent приоритетнее.
	pane.ForegroundCwd = "/home/uzz/projects/rmk"
	pane.DisplayAgent = "Claude Code"
	pane.TerminalTitleStrip = "чистый"
	ctx = contextFor(tab, &pane, "", DefaultIcons)
	if ctx.Dir != "rmk" || ctx.Agent != "Claude Code" || ctx.Title != "чистый" {
		t.Errorf("приоритеты не соблюдены: %+v", ctx)
	}
}

func TestContextForIconFollowsPaneStatus(t *testing.T) {
	tab := tabInfo{Number: 1, AgentStatus: "idle"}
	// Статус панели приоритетнее статуса таба, иконка должна идти за ним.
	pane := paneInfo{Agent: "claude", Cwd: "/tmp/x", AgentStatus: "blocked"}
	ctx := contextFor(tab, &pane, "", DefaultIcons)
	if ctx.Status != "blocked" || ctx.Icon != DefaultIcons["blocked"] {
		t.Errorf("ожидался blocked и его иконка, получено status=%q icon=%q", ctx.Status, ctx.Icon)
	}

	// unknown по умолчанию без иконки — токен должен исчезнуть вместе с пробелом.
	pane.AgentStatus = "unknown"
	ctx = contextFor(tab, &pane, "", DefaultIcons)
	if got := ParseTemplate("{icon} {agent}:{dir}").Render(ctx); got != "claude:x" {
		t.Errorf("пустая иконка должна исчезать вместе с пробелом, получено %q", got)
	}

	// А с иконкой — префикс на месте.
	pane.AgentStatus = "working"
	ctx = contextFor(tab, &pane, "", DefaultIcons)
	if got := ParseTemplate("{icon} {agent}:{dir}").Render(ctx); got != "🟡 claude:x" {
		t.Errorf("получено %q", got)
	}
}

func TestTokenAlternatives(t *testing.T) {
	agentOnly := NameContext{Agent: "claude", Dir: "nixos"}
	procOnly := NameContext{Proc: "btop", Dir: "rmk"}
	both := NameContext{Agent: "claude", Proc: "btop", Dir: "nixos"}
	neither := NameContext{Dir: "rmk"}

	cases := []struct {
		name string
		tmpl string
		ctx  NameContext
		want string
	}{
		{"процесс важнее агента в этом порядке", "{proc|agent}:{dir}", both, "btop:nixos"},
		{"без процесса берётся агент", "{proc|agent}:{dir}", agentOnly, "claude:nixos"},
		{"без агента берётся процесс", "{proc|agent}:{dir}", procOnly, "btop:rmk"},
		{"оба пусты — остаётся каталог", "{proc|agent}:{dir}", neither, "rmk"},
		{"тройная альтернатива без каталога", "{proc|agent|dir}", procOnly, "btop"},
		{"тройная альтернатива падает до каталога", "{proc|agent|dir}", neither, "rmk"},
		{"обратный приоритет тоже работает", "{agent|proc}", both, "claude"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ParseTemplate(c.tmpl).Render(c.ctx); got != c.want {
				t.Errorf("шаблон %q: получено %q, ожидалось %q", c.tmpl, got, c.want)
			}
		})
	}
}

func TestProcName(t *testing.T) {
	// Обычный случай: btop запущен в панели.
	info := &paneProcessInfo{
		ShellPid:                 100,
		ForegroundProcessGroupID: 200,
		ForegroundProcesses:      []paneProcess{{PID: 200, Name: "btop", Cmdline: "btop"}},
	}
	if got := ProcName(info); got != "btop" {
		t.Errorf("получено %q, ожидалось btop", got)
	}

	// Шелл сам по себе — не процесс.
	info.ForegroundProcesses = []paneProcess{{PID: 200, Name: "fish", Cmdline: "/run/current-system/sw/bin/fish"}}
	if got := ProcName(info); got != "" {
		t.Errorf("шелл не должен попадать в имя, получено %q", got)
	}

	// Обёртка NixOS: имя ".claude-wrapped", а cmdline честный.
	info.ForegroundProcesses = []paneProcess{{PID: 200, Name: ".claude-wrapped", Cmdline: "claude"}}
	if got := ProcName(info); got != "claude" {
		t.Errorf("получено %q, ожидалось claude", got)
	}

	// Если cmdline пуст, обёртку надо срезать с самого имени.
	info.ForegroundProcesses = []paneProcess{{PID: 200, Name: ".btop-wrapped"}}
	if got := ProcName(info); got != "btop" {
		t.Errorf("получено %q, ожидалось btop", got)
	}

	// Полный путь и аргументы: нужен только базовый бинарник.
	info.ForegroundProcesses = []paneProcess{{PID: 200, Name: "hx", Cmdline: "/nix/store/abc-helix-25.1/bin/hx flake.nix"}}
	if got := ProcName(info); got != "hx" {
		t.Errorf("получено %q, ожидалось hx", got)
	}

	// В группе несколько процессов — берём лидера, а не первого в списке.
	info.ForegroundProcessGroupID = 300
	info.ForegroundProcesses = []paneProcess{
		{PID: 301, Name: "mcp-server", Cmdline: "python3 mcp"},
		{PID: 300, Name: ".claude-wrapped", Cmdline: "claude"},
	}
	if got := ProcName(info); got != "claude" {
		t.Errorf("должен выбираться лидер группы, получено %q", got)
	}

	// Процесс совпадает с шеллом панели — значит ничего не запущено.
	info.ForegroundProcessGroupID = 100
	info.ForegroundProcesses = []paneProcess{{PID: 100, Name: "fish", Cmdline: "fish"}}
	if got := ProcName(info); got != "" {
		t.Errorf("получено %q, ожидалась пустая строка", got)
	}

	// Пограничные входы не должны паниковать.
	if got := ProcName(nil); got != "" {
		t.Errorf("nil: получено %q", got)
	}
	if got := ProcName(&paneProcessInfo{}); got != "" {
		t.Errorf("пустой список: получено %q", got)
	}
}

func TestParseIcons(t *testing.T) {
	// Пустая строка — набор по умолчанию.
	icons, err := ParseIcons("")
	if err != nil || icons["working"] != "🟡" {
		t.Fatalf("ожидались значения по умолчанию, получено %v (err=%v)", icons, err)
	}

	// Переопределение одного статуса не задевает остальные.
	icons, err = ParseIcons("working=⚡")
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
	if icons["working"] != "⚡" || icons["done"] != DefaultIcons["done"] {
		t.Errorf("частичное переопределение сломалось: %v", icons)
	}

	// Пустое значение убирает иконку.
	icons, err = ParseIcons("idle=")
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
	if icons["idle"] != "" {
		t.Errorf("idle должен остаться без иконки, получено %q", icons["idle"])
	}

	// Мусор и неизвестные статусы — ошибка, а не тихое игнорирование.
	if _, err := ParseIcons("working"); err == nil {
		t.Error("ожидалась ошибка на паре без знака =")
	}
	if _, err := ParseIcons("busy=🔥"); err == nil {
		t.Error("ожидалась ошибка на неизвестном статусе")
	}
}

func TestLooksOurs(t *testing.T) {
	r := &renamer{
		tmpl:   ParseTemplate("{icon} {agent}:{dir}"),
		icons:  DefaultIcons,
		maxLen: 24,
	}
	ctx := NameContext{Agent: "claude", Dir: "nixos", Status: "working", Icon: DefaultIcons["working"]}

	// Наше же имя, но со статусом, который был раньше.
	if !r.looksOurs(ctx, "🟢 claude:nixos") {
		t.Error("имя с другой иконкой статуса должно распознаваться как наше")
	}
	if !r.looksOurs(ctx, "🟡 claude:nixos") {
		t.Error("текущее имя должно распознаваться как наше")
	}
	// Человеческое имя — не наше.
	if r.looksOurs(ctx, "деплой") {
		t.Error("постороннее имя не должно считаться нашим")
	}
	// Без иконки — это наш же рендер при статусе unknown, у которого иконки нет.
	if !r.looksOurs(ctx, "claude:nixos") {
		t.Error("имя без иконки соответствует статусу unknown и должно считаться нашим")
	}
}

// Регрессия: таб звался "rmk" (в панели был только шелл), пользователь запустил
// btop — прежнее имя обязано опознаваться как наше, иначе таб замирает.
func TestLooksOursAcrossProcessAppearance(t *testing.T) {
	r := &renamer{
		tmpl:   ParseTemplate("{icon} {proc|agent}:{dir}"),
		icons:  DefaultIcons,
		maxLen: 24,
	}
	ctx := NameContext{Proc: "btop", Dir: "rmk", Status: "unknown"}

	if !r.looksOurs(ctx, "rmk") {
		t.Error("имя без процесса должно опознаваться как наше после запуска btop")
	}
	if !r.looksOurs(ctx, "btop:rmk") {
		t.Error("текущее имя должно опознаваться как наше")
	}
	if !r.looksOurs(ctx, DefaultIcons["working"]+" btop:rmk") {
		t.Error("имя с иконкой другого статуса должно опознаваться как наше")
	}

	// Симметрично для агента: таб звался "claude:nixos", агент отвалился.
	ctx = NameContext{Agent: "claude", Dir: "nixos", Status: "idle"}
	if !r.looksOurs(ctx, "nixos") {
		t.Error("имя без агента должно опознаваться как наше")
	}

	// Постороннее имя по-прежнему чужое.
	if r.looksOurs(ctx, "мониторинг") {
		t.Error("человеческое имя не должно считаться нашим")
	}
}
