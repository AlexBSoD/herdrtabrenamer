package main

import "testing"

func TestRenderDropsDanglingSeparators(t *testing.T) {
	full := NameContext{Agent: "claude", Dir: "nixos", Status: "working", Number: 36, Title: "some title"}
	noAgent := NameContext{Dir: "nixos", Status: "working", Number: 36}
	noStatus := NameContext{Agent: "claude", Dir: "nixos", Number: 36}
	empty := NameContext{Number: 7}

	cases := []struct {
		name string
		tmpl string
		ctx  NameContext
		want string
	}{
		{"both tokens present", "{agent}:{dir}", full, "claude:nixos"},
		{"empty first token eats the separator", "{agent}:{dir}", noAgent, "nixos"},
		{"empty second token eats the separator", "{dir}:{agent}", noAgent, "nixos"},
		{"trailing literal is kept", "{number}:{dir} [{status}]", full, "36:nixos [working]"},
		{"trailing literal leaves with an empty token", "{dir} [{status}]", noStatus, "nixos"},
		{"leading literal is kept", "[{status}] {dir}", full, "[working] nixos"},
		{"leading literal leaves with an empty token", "[{status}] {dir}", noStatus, "nixos"},
		{"all tokens empty", "{agent}:{dir}", empty, ""},
		{"unknown token counts as empty", "{agent}/{branch}", full, "claude"},
		{"literals only, no tokens", "static", full, ""},
		{"number is never empty", "{number}", empty, "7"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ParseTemplate(c.tmpl).Render(c.ctx)
			if got != c.want {
				t.Errorf("template %q: got %q, want %q", c.tmpl, got, c.want)
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
			t.Errorf("Truncate(%q, %d) = %q, want %q", c.in, c.max, got, c.want)
		}
	}
	// Truncation must not split a rune in half.
	got := Truncate("абвгдеёжзийклмн", 5)
	for _, r := range got {
		if r == '�' {
			t.Fatalf("Truncate mangled a rune: %q", got)
		}
	}
}

func TestIsGeneratedLabel(t *testing.T) {
	generated := []string{"", "1", "42", " 7 "}
	manual := []string{"claude:nixos", "deploy", "1st", "tab 2"}
	for _, l := range generated {
		if !IsGeneratedLabel(l) {
			t.Errorf("%q should count as auto-generated", l)
		}
	}
	for _, l := range manual {
		if IsGeneratedLabel(l) {
			t.Errorf("%q should count as human-made", l)
		}
	}
}

func TestLeadPanePrefersAgent(t *testing.T) {
	panes := []paneInfo{
		{PaneID: "w1:p1", Focused: true},
		{PaneID: "w1:p2", Agent: "claude"},
	}
	if got := leadPane(panes); got == nil || got.PaneID != "w1:p2" {
		t.Errorf("expected the pane with an agent, got %+v", got)
	}

	// Without agents the focused pane wins.
	panes = []paneInfo{{PaneID: "w1:p1"}, {PaneID: "w1:p2", Focused: true}}
	if got := leadPane(panes); got == nil || got.PaneID != "w1:p2" {
		t.Errorf("expected the focused pane, got %+v", got)
	}

	// With no distinguishing marks at all — the lowest pane_id, deterministically.
	panes = []paneInfo{{PaneID: "w1:p9"}, {PaneID: "w1:p3"}}
	if got := leadPane(panes); got == nil || got.PaneID != "w1:p3" {
		t.Errorf("expected pane w1:p3, got %+v", got)
	}

	if leadPane(nil) != nil {
		t.Error("expected nil for an empty list")
	}
}

func TestContextForFallbacks(t *testing.T) {
	tab := tabInfo{Number: 3, AgentStatus: "idle"}
	pane := paneInfo{
		Agent:         "claude",
		Cwd:           "/home/uzz/opt/nixos",
		TerminalTitle: "raw title",
	}
	ctx := contextFor(tab, &pane, "", DefaultIcons)
	if ctx.Dir != "nixos" {
		t.Errorf("Dir = %q, want nixos", ctx.Dir)
	}
	if ctx.Title != "raw title" {
		t.Errorf("Title should fall back to terminal_title, got %q", ctx.Title)
	}

	// foreground_cwd and display_agent take precedence.
	pane.ForegroundCwd = "/home/uzz/projects/rmk"
	pane.DisplayAgent = "Claude Code"
	pane.TerminalTitleStrip = "clean"
	ctx = contextFor(tab, &pane, "", DefaultIcons)
	if ctx.Dir != "rmk" || ctx.Agent != "Claude Code" || ctx.Title != "clean" {
		t.Errorf("precedence broken: %+v", ctx)
	}
}

func TestContextForIconFollowsPaneStatus(t *testing.T) {
	tab := tabInfo{Number: 1, AgentStatus: "idle"}
	// The pane status wins over the tab status, and the icon must follow it.
	pane := paneInfo{Agent: "claude", Cwd: "/tmp/x", AgentStatus: "blocked"}
	ctx := contextFor(tab, &pane, "", DefaultIcons)
	if ctx.Status != "blocked" || ctx.Icon != DefaultIcons["blocked"] {
		t.Errorf("expected blocked and its icon, got status=%q icon=%q", ctx.Status, ctx.Icon)
	}

	// unknown has no icon by default — the token must vanish together with the space.
	pane.AgentStatus = "unknown"
	ctx = contextFor(tab, &pane, "", DefaultIcons)
	if got := ParseTemplate("{icon} {agent}:{dir}").Render(ctx); got != "claude:x" {
		t.Errorf("an empty icon should vanish together with the space, got %q", got)
	}

	// And with an icon the prefix is in place.
	pane.AgentStatus = "working"
	ctx = contextFor(tab, &pane, "", DefaultIcons)
	if got := ParseTemplate("{icon} {agent}:{dir}").Render(ctx); got != "🟡 claude:x" {
		t.Errorf("got %q", got)
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
		{"process beats agent in this order", "{proc|agent}:{dir}", both, "btop:nixos"},
		{"without a process the agent is taken", "{proc|agent}:{dir}", agentOnly, "claude:nixos"},
		{"without an agent the process is taken", "{proc|agent}:{dir}", procOnly, "btop:rmk"},
		{"both empty — the directory remains", "{proc|agent}:{dir}", neither, "rmk"},
		{"triple alternative without the directory", "{proc|agent|dir}", procOnly, "btop"},
		{"triple alternative falls back to the directory", "{proc|agent|dir}", neither, "rmk"},
		{"the reverse priority works too", "{agent|proc}", both, "claude"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ParseTemplate(c.tmpl).Render(c.ctx); got != c.want {
				t.Errorf("template %q: got %q, want %q", c.tmpl, got, c.want)
			}
		})
	}
}

func TestProcName(t *testing.T) {
	// The ordinary case: btop is running in the pane.
	info := &paneProcessInfo{
		ShellPid:                 100,
		ForegroundProcessGroupID: 200,
		ForegroundProcesses:      []paneProcess{{PID: 200, Name: "btop", Cmdline: "btop"}},
	}
	if got := ProcName(info); got != "btop" {
		t.Errorf("got %q, want btop", got)
	}

	// A shell on its own is not a process.
	info.ForegroundProcesses = []paneProcess{{PID: 200, Name: "fish", Cmdline: "/run/current-system/sw/bin/fish"}}
	if got := ProcName(info); got != "" {
		t.Errorf("a shell must not end up in the name, got %q", got)
	}

	// A NixOS wrapper: the name is ".claude-wrapped" while cmdline is honest.
	info.ForegroundProcesses = []paneProcess{{PID: 200, Name: ".claude-wrapped", Cmdline: "claude"}}
	if got := ProcName(info); got != "claude" {
		t.Errorf("got %q, want claude", got)
	}

	// When cmdline is empty, the wrapper has to be stripped off the name itself.
	info.ForegroundProcesses = []paneProcess{{PID: 200, Name: ".btop-wrapped"}}
	if got := ProcName(info); got != "btop" {
		t.Errorf("got %q, want btop", got)
	}

	// A full path with arguments: only the base binary is wanted.
	info.ForegroundProcesses = []paneProcess{{PID: 200, Name: "hx", Cmdline: "/nix/store/abc-helix-25.1/bin/hx flake.nix"}}
	if got := ProcName(info); got != "hx" {
		t.Errorf("got %q, want hx", got)
	}

	// Several processes in the group — take the leader, not the first in the list.
	info.ForegroundProcessGroupID = 300
	info.ForegroundProcesses = []paneProcess{
		{PID: 301, Name: "mcp-server", Cmdline: "python3 mcp"},
		{PID: 300, Name: ".claude-wrapped", Cmdline: "claude"},
	}
	if got := ProcName(info); got != "claude" {
		t.Errorf("the group leader should be chosen, got %q", got)
	}

	// The process is the pane's own shell — so nothing is running.
	info.ForegroundProcessGroupID = 100
	info.ForegroundProcesses = []paneProcess{{PID: 100, Name: "fish", Cmdline: "fish"}}
	if got := ProcName(info); got != "" {
		t.Errorf("got %q, want an empty string", got)
	}

	// Edge inputs must not panic.
	if got := ProcName(nil); got != "" {
		t.Errorf("nil: got %q", got)
	}
	if got := ProcName(&paneProcessInfo{}); got != "" {
		t.Errorf("empty list: got %q", got)
	}
}

func TestParseIcons(t *testing.T) {
	// An empty string means the default set.
	icons, err := ParseIcons("")
	if err != nil || icons["working"] != "🟡" {
		t.Fatalf("expected the defaults, got %v (err=%v)", icons, err)
	}

	// Overriding one status must not disturb the others.
	icons, err = ParseIcons("working=⚡")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if icons["working"] != "⚡" || icons["done"] != DefaultIcons["done"] {
		t.Errorf("partial override broke: %v", icons)
	}

	// An empty value removes the icon.
	icons, err = ParseIcons("idle=")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if icons["idle"] != "" {
		t.Errorf("idle should be left without an icon, got %q", icons["idle"])
	}

	// Garbage and unknown statuses are errors, not silent no-ops.
	if _, err := ParseIcons("working"); err == nil {
		t.Error("expected an error for a pair without an = sign")
	}
	if _, err := ParseIcons("busy=🔥"); err == nil {
		t.Error("expected an error for an unknown status")
	}
}

func TestLooksOurs(t *testing.T) {
	r := &renamer{
		tmpl:   ParseTemplate("{icon} {agent}:{dir}"),
		icons:  DefaultIcons,
		maxLen: 24,
	}
	ctx := NameContext{Agent: "claude", Dir: "nixos", Status: "working", Icon: DefaultIcons["working"]}

	// Our own name, but with the status it had earlier.
	if !r.looksOurs(ctx, "🟢 claude:nixos") {
		t.Error("a name with another status icon should be recognized as ours")
	}
	if !r.looksOurs(ctx, "🟡 claude:nixos") {
		t.Error("the current name should be recognized as ours")
	}
	// A human name is not ours.
	if r.looksOurs(ctx, "deploy") {
		t.Error("a foreign name must not count as ours")
	}
	// Without an icon it is our own rendering for the unknown status, which has none.
	if !r.looksOurs(ctx, "claude:nixos") {
		t.Error("a name without an icon matches the unknown status and should count as ours")
	}
}

// Regression: a tab was named "rmk" (only a shell in the pane), the user
// started btop — the previous name must be recognized as ours, otherwise the
// tab freezes.
func TestLooksOursAcrossProcessAppearance(t *testing.T) {
	r := &renamer{
		tmpl:   ParseTemplate("{icon} {proc|agent}:{dir}"),
		icons:  DefaultIcons,
		maxLen: 24,
	}
	ctx := NameContext{Proc: "btop", Dir: "rmk", Status: "unknown"}

	if !r.looksOurs(ctx, "rmk") {
		t.Error("the name without a process should be recognized as ours after btop started")
	}
	if !r.looksOurs(ctx, "btop:rmk") {
		t.Error("the current name should be recognized as ours")
	}
	if !r.looksOurs(ctx, DefaultIcons["working"]+" btop:rmk") {
		t.Error("a name with another status icon should be recognized as ours")
	}

	// Symmetrically for the agent: a tab was named "claude:nixos", the agent went away.
	ctx = NameContext{Agent: "claude", Dir: "nixos", Status: "idle"}
	if !r.looksOurs(ctx, "nixos") {
		t.Error("the name without an agent should be recognized as ours")
	}

	// A foreign name is still foreign.
	if r.looksOurs(ctx, "monitoring") {
		t.Error("a human name must not count as ours")
	}
}

func TestSSHName(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"bare host", []string{"zmb"}, "@zmb"},
		{"user@host", []string{"srv@192.168.6.220"}, "@192.168.6.220"},
		{"flag with a separate value", []string{"-p", "2222", "zmb"}, "@zmb"},
		{"flag with a glued value", []string{"-p2222", "zmb"}, "@zmb"},
		{"boolean flags bundled with one taking a value", []string{"-tp", "2222", "zmb"}, "@zmb"},
		{"option with an equals sign", []string{"-o", "BatchMode=yes", "zmb"}, "@zmb"},
		{"identity file", []string{"-i", "~/.ssh/bsod", "-A", "zmb"}, "@zmb"},
		{"jump host is a flag value, not the destination", []string{"-J", "gate", "zmb"}, "@zmb"},
		{"remote command", []string{"zmb", "btop"}, "@zmb:btop"},
		{"remote command with a path and arguments", []string{"zmb", "/usr/bin/btop", "-p"}, "@zmb:btop"},
		{"a remote shell adds nothing", []string{"zmb", "fish"}, "@zmb"},
		{"double dash ends the flags", []string{"--", "-weird-host"}, "@-weird-host"},
		{"ssh uri", []string{"ssh://srv@zmb:2222"}, "@zmb"},
		{"ipv6 in brackets keeps its colons", []string{"[2001:db8::1]:2222"}, "@2001:db8::1"},
		{"no destination at all", []string{"-V"}, ""},
		{"no arguments", nil, ""},
		{"a flag that swallows the only word left", []string{"-p"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := SSHName(c.args); got != c.want {
				t.Errorf("SSHName(%q) = %q, want %q", c.args, got, c.want)
			}
		})
	}
}

func TestProcNameNamesSSHAfterItsHost(t *testing.T) {
	// argv is the honest source when the server fills it.
	info := &paneProcessInfo{
		ShellPid:                 100,
		ForegroundProcessGroupID: 200,
		ForegroundProcesses: []paneProcess{{
			PID:     200,
			Name:    "ssh",
			Cmdline: "ssh zmb",
			Argv:    []string{"ssh", "-p", "22", "srv@zmb"},
		}},
	}
	if got := ProcName(info); got != "@zmb" {
		t.Errorf("got %q, want @zmb", got)
	}

	// Without argv the arguments have to come out of cmdline.
	info.ForegroundProcesses = []paneProcess{{PID: 200, Name: "ssh", Cmdline: "ssh zmb btop"}}
	if got := ProcName(info); got != "@zmb:btop" {
		t.Errorf("got %q, want @zmb:btop", got)
	}

	// An ssh with no destination keeps the plain process name.
	info.ForegroundProcesses = []paneProcess{{PID: 200, Name: "ssh", Cmdline: "ssh -V"}}
	if got := ProcName(info); got != "ssh" {
		t.Errorf("got %q, want ssh", got)
	}
}
