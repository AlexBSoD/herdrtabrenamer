package main

import "encoding/json"

// Data shapes of the herdr socket API (protocol 17+, schema_version 1).
// The full schema can be dumped with `herdr api schema --json`.

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

// eventEnvelope: {"event":"pane_updated","data":{"type":"pane_updated","pane":{...}}}
type eventEnvelope struct {
	Event string          `json:"event"`
	Data  json.RawMessage `json:"data"`
}

type eventKind struct {
	Type string `json:"type"`
}

// paneInfo holds the pane fields we consume. The API returns more (scroll,
// agent_session, tokens, state_labels); they are left out on purpose.
type paneInfo struct {
	// Revision (protocol 19+) is bumped by the server on every pane state
	// change, including a foreground process starting or exiting. Verified on
	// 0.8.0: an idle shell sat at 1, `sleep 45` took it to 2, and the process
	// exiting took it to 5. That makes it a valid cache key for
	// pane.process_info. It is absent (0) on older servers, where the cache
	// must stay disabled.
	Revision           uint64 `json:"revision"`
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

// session.snapshot (protocol 19+) returns the whole session in one response:
// {"type":"session_snapshot","snapshot":{"tabs":[...],"panes":[...],...}}.
// We only take the two lists; workspaces, layouts and agents are redundant for
// naming.
type sessionSnapshotResult struct {
	Type     string   `json:"type"`
	Snapshot snapshot `json:"snapshot"`
}

type snapshot struct {
	Version  string     `json:"version"`
	Protocol int        `json:"protocol"`
	Tabs     []tabInfo  `json:"tabs"`
	Panes    []paneInfo `json:"panes"`
}

// pong is the ping reply; it is how we learn the protocol version and thus
// which methods the server supports.
type pong struct {
	Type     string `json:"type"`
	Version  string `json:"version"`
	Protocol int    `json:"protocol"`
}

type paneProcessInfoResult struct {
	Type        string          `json:"type"`
	ProcessInfo paneProcessInfo `json:"process_info"`
}

type paneProcessInfo struct {
	PaneID string `json:"pane_id"`
	// ShellPid is the pane's own shell; foreground_processes holds whatever it
	// spawned, together with the children of the same process group.
	ShellPid                 int           `json:"shell_pid"`
	ForegroundProcessGroupID int           `json:"foreground_process_group_id"`
	ForegroundProcesses      []paneProcess `json:"foreground_processes"`
}

type paneProcess struct {
	PID  int    `json:"pid"`
	Name string `json:"name"`
	// Cmdline is more honest than Name: on NixOS a running claude shows up as
	// name=".claude-wrapped" while cmdline="claude".
	Cmdline string   `json:"cmdline"`
	Argv    []string `json:"argv"`
	Argv0   string   `json:"argv0"`
	Cwd     string   `json:"cwd"`
}

// Payloads of the events we subscribe to.
type panePayload struct {
	Type string   `json:"type"`
	Pane paneInfo `json:"pane"`
}

type tabPayload struct {
	Type string  `json:"type"`
	Tab  tabInfo `json:"tab"`
}
