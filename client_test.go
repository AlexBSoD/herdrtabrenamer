package main

import (
	"bufio"
	"encoding/json"
	"net"
	"path/filepath"
	"sync"
	"testing"
)

// fakeServer imitates the herdr socket API closely enough to test the client:
// one request per connection, a JSON line in and a JSON line out.
type fakeServer struct {
	ln      net.Listener
	replies map[string]any // method -> result

	mu    sync.Mutex
	calls map[string]int
}

func newFakeServer(t *testing.T, replies map[string]any) *fakeServer {
	t.Helper()
	// The socket lives in a temp dir; unix paths are length-limited, so keep
	// the name short.
	path := filepath.Join(t.TempDir(), "s.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("cannot listen on %s: %v", path, err)
	}
	s := &fakeServer{ln: ln, replies: replies, calls: map[string]int{}}
	t.Cleanup(func() { ln.Close() })
	go s.serve()
	return s
}

func (s *fakeServer) addr() string { return s.ln.Addr().String() }

func (s *fakeServer) count(method string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[method]
}

func (s *fakeServer) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go func() {
			// Exactly one request per connection, like the real server.
			defer conn.Close()
			line, err := bufio.NewReader(conn).ReadBytes('\n')
			if err != nil {
				return
			}
			var req struct {
				ID     string `json:"id"`
				Method string `json:"method"`
			}
			if json.Unmarshal(line, &req) != nil {
				return
			}
			s.mu.Lock()
			s.calls[req.Method]++
			s.mu.Unlock()

			result, ok := s.replies[req.Method]
			var out []byte
			if !ok {
				out, _ = json.Marshal(map[string]any{
					"id":    req.ID,
					"error": map[string]string{"code": "unknown_method", "message": req.Method},
				})
			} else {
				out, _ = json.Marshal(map[string]any{"id": req.ID, "result": result})
			}
			conn.Write(append(out, '\n'))
		}()
	}
}

func pongReply(protocol int) map[string]any {
	return map[string]any{"type": "pong", "version": "0.8.0", "protocol": protocol}
}

func snapshotReply(tabs, panes []any) map[string]any {
	return map[string]any{
		"type": "session_snapshot",
		"snapshot": map[string]any{
			"version":  "0.8.0",
			"protocol": protocolSnapshot,
			"tabs":     tabs,
			"panes":    panes,
		},
	}
}

func TestSnapshotUsesSessionSnapshotOnNewProtocol(t *testing.T) {
	srv := newFakeServer(t, map[string]any{
		"ping": pongReply(protocolSnapshot),
		"session.snapshot": snapshotReply(
			[]any{map[string]any{"tab_id": "w1:t1", "workspace_id": "w1", "number": 1, "label": "1"}},
			[]any{map[string]any{"pane_id": "w1:p1", "tab_id": "w1:t1", "workspace_id": "w1", "revision": 7, "cwd": "/tmp/x"}},
		),
	})

	cli, err := Dial(srv.addr())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if _, protocol := cli.Version(); protocol != protocolSnapshot {
		t.Fatalf("protocol = %d, want %d", protocol, protocolSnapshot)
	}

	tabs, panes, err := cli.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(tabs) != 1 || tabs[0].TabID != "w1:t1" {
		t.Errorf("tabs = %+v", tabs)
	}
	if len(panes) != 1 || panes[0].Revision != 7 {
		t.Errorf("panes = %+v", panes)
	}
	if got := srv.count("tab.list") + srv.count("pane.list"); got != 0 {
		t.Errorf("the legacy methods must not be called, got %d calls", got)
	}
}

func TestSnapshotFallsBackOnOldProtocol(t *testing.T) {
	srv := newFakeServer(t, map[string]any{
		"ping": pongReply(17),
		"tab.list": map[string]any{
			"type": "tab_list",
			"tabs": []any{map[string]any{"tab_id": "w1:t1", "workspace_id": "w1", "number": 1}},
		},
		"pane.list": map[string]any{
			"type":  "pane_list",
			"panes": []any{map[string]any{"pane_id": "w1:p1", "tab_id": "w1:t1", "workspace_id": "w1"}},
		},
	})

	cli, err := Dial(srv.addr())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	tabs, panes, err := cli.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(tabs) != 1 || len(panes) != 1 {
		t.Fatalf("tabs=%+v panes=%+v", tabs, panes)
	}
	if srv.count("session.snapshot") != 0 {
		t.Error("session.snapshot must not be attempted on protocol 17")
	}
	if srv.count("tab.list") != 1 || srv.count("pane.list") != 1 {
		t.Errorf("expected one call each, got tab.list=%d pane.list=%d",
			srv.count("tab.list"), srv.count("pane.list"))
	}
}

// A ping that never arrives must not stop the daemon: the client just stays on
// the legacy path.
func TestDialSurvivesMissingPing(t *testing.T) {
	srv := newFakeServer(t, map[string]any{
		"tab.list":  map[string]any{"type": "tab_list", "tabs": []any{}},
		"pane.list": map[string]any{"type": "pane_list", "panes": []any{}},
	})
	cli, err := Dial(srv.addr())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if version, protocol := cli.Version(); version != "" || protocol != 0 {
		t.Errorf("expected an unknown version, got %q/%d", version, protocol)
	}
	if _, _, err := cli.Snapshot(); err != nil {
		t.Errorf("Snapshot on the legacy path: %v", err)
	}
}

func newProbeRenamer(t *testing.T, srv *fakeServer, format string) *renamer {
	t.Helper()
	cli, err := Dial(srv.addr())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	tmpl := ParseTemplate(format)
	return &renamer{
		cli:       cli,
		tmpl:      tmpl,
		icons:     DefaultIcons,
		needProc:  tmpl.UsesToken("proc"),
		maxLen:    24,
		state:     LoadState(""),
		dryShown:  map[string]string{},
		manual:    map[string]bool{},
		procCache: map[string]procEntry{},
	}
}

// The pane revision is what makes the process_info cache safe, so an unchanged
// revision must not produce a second call — and a bumped one must.
func TestProcInfoCachedByRevision(t *testing.T) {
	pane := map[string]any{"pane_id": "w1:p1", "tab_id": "w1:t1", "workspace_id": "w1", "revision": 3, "cwd": "/tmp/x"}
	tab := map[string]any{"tab_id": "w1:t1", "workspace_id": "w1", "number": 1, "label": "1"}
	replies := map[string]any{
		"ping":             pongReply(protocolSnapshot),
		"session.snapshot": snapshotReply([]any{tab}, []any{pane}),
		"pane.process_info": map[string]any{
			"type": "pane_process_info",
			"process_info": map[string]any{
				"pane_id":                     "w1:p1",
				"shell_pid":                   10,
				"foreground_process_group_id": 20,
				"foreground_processes":        []any{map[string]any{"pid": 20, "name": "btop", "cmdline": "btop"}},
			},
		},
	}
	srv := newFakeServer(t, replies)
	r := newProbeRenamer(t, srv, "{proc|dir}")

	for i := 0; i < 3; i++ {
		if err := r.pass(); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
	if got := srv.count("pane.process_info"); got != 1 {
		t.Errorf("an unchanged revision should be asked about once, got %d calls", got)
	}
	if got := srv.count("session.snapshot"); got != 3 {
		t.Errorf("every pass needs a fresh snapshot, got %d calls", got)
	}

	// A bumped revision invalidates the cache.
	pane["revision"] = 4
	replies["session.snapshot"] = snapshotReply([]any{tab}, []any{pane})
	if err := r.pass(); err != nil {
		t.Fatalf("pass after the bump: %v", err)
	}
	if got := srv.count("pane.process_info"); got != 2 {
		t.Errorf("a bumped revision should trigger a new call, got %d calls", got)
	}

	// A pane that disappeared must not linger in the cache.
	replies["session.snapshot"] = snapshotReply([]any{tab}, []any{})
	if err := r.pass(); err != nil {
		t.Fatalf("pass without panes: %v", err)
	}
	if len(r.procCache) != 0 {
		t.Errorf("the cache should be empty, got %v", r.procCache)
	}
}

// A template without {proc} must never pay for pane.process_info.
func TestProcInfoSkippedWhenTemplateHasNoProc(t *testing.T) {
	srv := newFakeServer(t, map[string]any{
		"ping": pongReply(protocolSnapshot),
		"session.snapshot": snapshotReply(
			[]any{map[string]any{"tab_id": "w1:t1", "workspace_id": "w1", "number": 1, "label": "1"}},
			[]any{map[string]any{"pane_id": "w1:p1", "tab_id": "w1:t1", "workspace_id": "w1", "revision": 3, "cwd": "/tmp/x"}},
		),
	})
	r := newProbeRenamer(t, srv, "{dir}")
	if err := r.pass(); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if got := srv.count("pane.process_info"); got != 0 {
		t.Errorf("expected no process_info calls, got %d", got)
	}
}

// An agent pane is named after the agent, so its process group (which also
// holds MCP servers) must not be inspected.
func TestProcInfoSkippedForAgentPane(t *testing.T) {
	srv := newFakeServer(t, map[string]any{
		"ping": pongReply(protocolSnapshot),
		"session.snapshot": snapshotReply(
			[]any{map[string]any{"tab_id": "w1:t1", "workspace_id": "w1", "number": 1, "label": "1"}},
			[]any{map[string]any{
				"pane_id": "w1:p1", "tab_id": "w1:t1", "workspace_id": "w1",
				"revision": 3, "cwd": "/tmp/x", "agent": "claude",
			}},
		),
	})
	r := newProbeRenamer(t, srv, "{proc|agent}:{dir}")
	if err := r.pass(); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if got := srv.count("pane.process_info"); got != 0 {
		t.Errorf("expected no process_info calls for an agent pane, got %d", got)
	}
}

// Without a revision (an older server) the cache has to stay off, otherwise a
// zero revision would look like "nothing changed" forever.
func TestProcInfoNotCachedWithoutRevision(t *testing.T) {
	srv := newFakeServer(t, map[string]any{
		"ping": pongReply(17),
		"tab.list": map[string]any{
			"type": "tab_list",
			"tabs": []any{map[string]any{"tab_id": "w1:t1", "workspace_id": "w1", "number": 1, "label": "1"}},
		},
		"pane.list": map[string]any{
			"type":  "pane_list",
			"panes": []any{map[string]any{"pane_id": "w1:p1", "tab_id": "w1:t1", "workspace_id": "w1", "cwd": "/tmp/x"}},
		},
		"pane.process_info": map[string]any{
			"type": "pane_process_info",
			"process_info": map[string]any{
				"pane_id":                     "w1:p1",
				"shell_pid":                   10,
				"foreground_process_group_id": 20,
				"foreground_processes":        []any{map[string]any{"pid": 20, "name": "btop", "cmdline": "btop"}},
			},
		},
	})
	r := newProbeRenamer(t, srv, "{proc|dir}")
	for i := 0; i < 2; i++ {
		if err := r.pass(); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
	if got := srv.count("pane.process_info"); got != 2 {
		t.Errorf("without a revision every pass must ask again, got %d calls", got)
	}
}

func TestTemplateUsesToken(t *testing.T) {
	cases := []struct {
		tmpl  string
		token string
		want  bool
	}{
		{"{proc|dir}", "proc", true},
		{"{proc|dir}", "dir", true},
		{"{proc|dir}", "agent", false},
		{"{dir}", "proc", false},
		{"{icon} {agent}:{dir}", "icon", true},
		{"proc", "proc", false},
	}
	for _, c := range cases {
		if got := ParseTemplate(c.tmpl).UsesToken(c.token); got != c.want {
			t.Errorf("ParseTemplate(%q).UsesToken(%q) = %v, want %v", c.tmpl, c.token, got, c.want)
		}
	}
}
