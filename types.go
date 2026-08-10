package main

import "encoding/json"

// Формы данных socket API herdr (protocol 17, schema_version 1).
// Полную схему можно выгрузить через `herdr api schema --json`.

type request struct {
	ID     string `json:"id"`
	Method string `json:"method"`
	Params any    `json:"params"`
}

type response struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *errorBody      `json:"error,omitempty"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *errorBody) Error() string { return e.Code + ": " + e.Message }

// EventEnvelope: {"event":"pane.updated","data":{"type":"pane_updated","pane":{...}}}
type eventEnvelope struct {
	Event string          `json:"event"`
	Data  json.RawMessage `json:"data"`
}

type eventKind struct {
	Type string `json:"type"`
}

// PaneInfo — полный набор полей из схемы; используем только часть,
// остальные оставлены как документация того, что вообще доступно.
type paneInfo struct {
	PaneID             string `json:"pane_id"`
	TabID              string `json:"tab_id"`
	WorkspaceID        string `json:"workspace_id"`
	Label              string `json:"label"`
	Title              string `json:"title"`
	TerminalTitle      string `json:"terminal_title"`
	TerminalTitleStrip string `json:"terminal_title_stripped"`
	Agent              string `json:"agent"`
	DisplayAgent       string `json:"display_agent"`
	AgentStatus        string `json:"agent_status"`
	Cwd                string `json:"cwd"`
	ForegroundCwd      string `json:"foreground_cwd"`
	Focused            bool   `json:"focused"`
}

type tabInfo struct {
	TabID       string `json:"tab_id"`
	WorkspaceID string `json:"workspace_id"`
	Number      int    `json:"number"`
	Label       string `json:"label"`
	Focused     bool   `json:"focused"`
	PaneCount   int    `json:"pane_count"`
	AgentStatus string `json:"agent_status"`
}

type tabListResult struct {
	Type string    `json:"type"`
	Tabs []tabInfo `json:"tabs"`
}

type paneListResult struct {
	Type  string     `json:"type"`
	Panes []paneInfo `json:"panes"`
}

type paneProcessInfoResult struct {
	Type        string          `json:"type"`
	ProcessInfo paneProcessInfo `json:"process_info"`
}

type paneProcessInfo struct {
	PaneID string `json:"pane_id"`
	// ShellPid — сам шелл панели; foreground_processes содержит то, что он
	// запустил, вместе с дочерними процессами той же группы.
	ShellPid                 int           `json:"shell_pid"`
	ForegroundProcessGroupID int           `json:"foreground_process_group_id"`
	ForegroundProcesses      []paneProcess `json:"foreground_processes"`
}

type paneProcess struct {
	PID  int    `json:"pid"`
	Name string `json:"name"`
	// Cmdline честнее Name: на NixOS запущенный claude виден как
	// name=".claude-wrapped" при cmdline="claude".
	Cmdline string   `json:"cmdline"`
	Argv    []string `json:"argv"`
	Argv0   string   `json:"argv0"`
	Cwd     string   `json:"cwd"`
}

// Полезная нагрузка событий, на которые мы подписываемся.
type panePayload struct {
	Type string   `json:"type"`
	Pane paneInfo `json:"pane"`
}

type tabPayload struct {
	Type string  `json:"type"`
	Tab  tabInfo `json:"tab"`
}
