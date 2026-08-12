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

// Template of a tab name. Tokens: {agent} {dir} {cwd} {title} {status}
// {number}. Literals between tokens (separators) are dropped when either
// neighbouring token is empty — otherwise "{agent}:{dir}" without an agent
// would render as ":nixos".
type Template struct {
	parts []templatePart
}

type templatePart struct {
	literal string
	token   string // non-empty when this part is a token
}

// A token may list alternatives separated by "|": {proc|agent} takes the
// process name, falling back to the agent name.
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

// NameContext holds the token values for a single tab.
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

// token returns the value of a token; for "a|b|c" the first non-empty one.
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

// DefaultIcons: there is no way to set a colour in the tab bar (the socket API
// has neither colours nor styles), so the status indicator is an emoji —
// emoji carry their own colour.
var DefaultIcons = map[string]string{
	"blocked": "🔴",
	"working": "🟡",
	"done":    "🟢",
	"idle":    "⚪",
	"unknown": "",
}

// ParseIcons parses "working=🟡,done=✅" on top of the default set.
// An empty value on the right-hand side removes the icon for that status.
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
			return nil, fmt.Errorf("expected status=icon, got %q", pair)
		}
		k = strings.TrimSpace(k)
		if _, known := DefaultIcons[k]; !known {
			return nil, fmt.Errorf("unknown status %q (known: blocked, working, done, idle, unknown)", k)
		}
		icons[k] = strings.TrimSpace(v)
	}
	return icons, nil
}

func (t Template) Render(ctx NameContext) string {
	var out strings.Builder
	pending := ""     // literals whose fate is not decided yet
	dropNext := false // the previous token was empty — eat the separator after it too
	haveValue := false

	for _, p := range t.parts {
		if p.token == "" {
			pending += p.literal
			continue
		}
		v := strings.TrimSpace(ctx.token(p.token))
		if v == "" {
			// An empty token takes the separators on both sides with it:
			// "{agent}:{dir}" without an agent renders "nixos", not ":nixos".
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
	// A trailing literal is only kept when no token was eaten after it:
	// "[{status}]" renders "[idle]" with a status and "" without one.
	if haveValue && !dropNext {
		out.WriteString(pending)
	}
	return strings.TrimSpace(out.String())
}

// Truncate cuts a name by visible characters rather than bytes — otherwise
// non-ASCII text and status symbols get sliced in the middle of a rune.
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

// PrettyDir is a short directory name: the basename, except $HOME becomes "~",
// so that a repository root stays recognizable.
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

// shells lists what runs in a pane "by default": a shell name in a tab title
// carries no information, so such a process counts as absent.
var shells = map[string]bool{
	"fish": true, "bash": true, "zsh": true, "sh": true, "dash": true,
	"ksh": true, "csh": true, "tcsh": true, "nu": true, "elvish": true,
	"xonsh": true, "login": true,
}

// wrapperSuffixes are the wrappers NixOS adds: .claude-wrapped, btop-wrap and
// friends. They have to be stripped from the name.
var wrapperSuffixes = []string{"-wrapped", "-wrap"}

// ProcName extracts a human-readable name of the pane's foreground process.
// An empty string means "nothing more interesting than a shell is running".
func ProcName(info *paneProcessInfo) string {
	if info == nil || len(info.ForegroundProcesses) == 0 {
		return ""
	}
	// A group may contain several processes (an agent plus its MCP servers);
	// we want the group leader, not a random child.
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

// autoLabelRe matches the labels herdr generates on its own when
// ui.prompt_new_tab_name = false: just the ordinal number.
var autoLabelRe = regexp.MustCompile(`^\d+$`)

// IsGeneratedLabel reports whether a label looks auto-generated. Such labels
// may be overwritten without asking; everything else counts as a human name.
func IsGeneratedLabel(label string) bool {
	return label == "" || autoLabelRe.MatchString(strings.TrimSpace(label))
}

// leadPane picks the pane a tab is named after: first a pane with a detected
// agent, then the focused one, then the lowest pane_id.
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

// contextFor collects token values from a pane and a tab.
// proc is the foreground process name of the lead pane (may be empty).
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
