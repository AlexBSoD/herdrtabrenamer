package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

// SocketPath returns the path of the herdr server socket.
// herdr keeps it next to config.toml, see `herdr --help` (Config section).
func SocketPath() string {
	if p := os.Getenv("HERDR_SOCKET"); p != "" {
		return p
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "herdr", "herdr.sock")
}

// Client performs socket API requests.
//
// IMPORTANT: the server handles EXACTLY ONE request per connection and closes
// it right after; a second write into the same socket yields a broken pipe.
// Verified on herdr 0.7.5 (protocol 17) and still true on 0.8.0 (protocol 19).
// So every Call opens its own connection; the only long-lived connection is the
// event subscription (EventStream).
type Client struct {
	sock string
	seq  atomic.Uint64

	// version and protocol come from the ping made at Dial time; protocol
	// decides which of the newer methods may be used.
	version  string
	protocol int
}

// protocolSnapshot is the first protocol version that has session.snapshot and
// a pane revision counter (herdr 0.8.0).
const protocolSnapshot = 19

func Dial(sock string) (*Client, error) {
	// A probe connection, so that "server is not running" is distinguishable
	// from errors in the requests themselves.
	conn, err := net.DialTimeout("unix", sock, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", sock, err)
	}
	conn.Close()

	c := &Client{sock: sock}
	// A failed ping is not fatal: an older server simply keeps us on the
	// tab.list + pane.list path.
	if raw, err := c.Call("ping", nil); err == nil {
		var p pong
		if json.Unmarshal(raw, &p) == nil {
			c.version, c.protocol = p.Version, p.Protocol
		}
	}
	return c, nil
}

// Version reports what the server said at Dial time; both values are empty or
// zero when the ping did not go through.
func (c *Client) Version() (string, int) { return c.version, c.protocol }

// hasSnapshot reports whether session.snapshot may be used.
func (c *Client) hasSnapshot() bool { return c.protocol >= protocolSnapshot }

func (c *Client) Close() error { return nil }

func (c *Client) Call(method string, params any) (json.RawMessage, error) {
	if params == nil {
		params = struct{}{}
	}
	id := fmt.Sprintf("htr:%d", c.seq.Add(1))
	payload, err := json.Marshal(request{ID: id, Method: method, Params: params})
	if err != nil {
		return nil, err
	}

	conn, err := net.DialTimeout("unix", c.sock, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", c.sock, err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return nil, err
	}

	if _, err := conn.Write(append(payload, '\n')); err != nil {
		return nil, fmt.Errorf("writing %s: %w", method, err)
	}

	r := bufio.NewReaderSize(conn, 1<<20)
	line, err := r.ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("reading %s response: %w", method, err)
	}
	var resp response
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("cannot parse %s response: %w", method, err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("%s: %w", method, resp.Error)
	}
	return resp.Result, nil
}

func (c *Client) ListTabs(workspace string) ([]tabInfo, error) {
	params := map[string]any{}
	if workspace != "" {
		params["workspace_id"] = workspace
	}
	raw, err := c.Call("tab.list", params)
	if err != nil {
		return nil, err
	}
	var res tabListResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}
	return res.Tabs, nil
}

func (c *Client) ListPanes(workspace string) ([]paneInfo, error) {
	params := map[string]any{}
	if workspace != "" {
		params["workspace_id"] = workspace
	}
	raw, err := c.Call("pane.list", params)
	if err != nil {
		return nil, err
	}
	var res paneListResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}
	return res.Panes, nil
}

// Snapshot returns every tab and pane of the session.
//
// On protocol 19+ this is a single session.snapshot call. Before that the same
// picture took tab.list plus one pane.list per workspace, and since the server
// closes the connection after each response, every one of those was a separate
// connect. Naming needs the whole picture on every pass, so this is the hot
// path.
func (c *Client) Snapshot() ([]tabInfo, []paneInfo, error) {
	if !c.hasSnapshot() {
		tabs, err := c.ListTabs("")
		if err != nil {
			return nil, nil, err
		}
		panes, err := c.ListPanes("")
		if err != nil {
			return nil, nil, err
		}
		return tabs, panes, nil
	}

	raw, err := c.Call("session.snapshot", nil)
	if err != nil {
		return nil, nil, err
	}
	var res sessionSnapshotResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, nil, err
	}
	return res.Snapshot.Tabs, res.Snapshot.Panes, nil
}

func (c *Client) ProcessInfo(paneID string) (*paneProcessInfo, error) {
	raw, err := c.Call("pane.process_info", map[string]string{"pane_id": paneID})
	if err != nil {
		return nil, err
	}
	var res paneProcessInfoResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}
	return &res.ProcessInfo, nil
}

func (c *Client) RenameTab(tabID, label string) error {
	_, err := c.Call("tab.rename", map[string]string{"tab_id": tabID, "label": label})
	return err
}

// EventStream is a dedicated connection for the subscription. Mixing it with
// requests is not possible: events arrive without an id and would desynchronize
// the request/response pairing.
type EventStream struct {
	conn net.Conn
	r    *bufio.Reader
}

func Subscribe(sock string, kinds []string) (*EventStream, error) {
	conn, err := net.DialTimeout("unix", sock, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", sock, err)
	}
	subs := make([]map[string]string, 0, len(kinds))
	for _, k := range kinds {
		subs = append(subs, map[string]string{"type": k})
	}
	payload, err := json.Marshal(request{
		ID:     "htr:sub",
		Method: "events.subscribe",
		Params: map[string]any{"subscriptions": subs},
	})
	if err != nil {
		conn.Close()
		return nil, err
	}
	if _, err := conn.Write(append(payload, '\n')); err != nil {
		conn.Close()
		return nil, fmt.Errorf("writing events.subscribe: %w", err)
	}

	r := bufio.NewReaderSize(conn, 1<<20)
	line, err := r.ReadBytes('\n')
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("reading subscription ack: %w", err)
	}
	var resp response
	if err := json.Unmarshal(line, &resp); err != nil {
		conn.Close()
		return nil, fmt.Errorf("cannot parse subscription ack: %w", err)
	}
	if resp.Error != nil {
		conn.Close()
		return nil, fmt.Errorf("events.subscribe: %w", resp.Error)
	}
	var started eventKind
	if err := json.Unmarshal(resp.Result, &started); err != nil || started.Type != "subscription_started" {
		conn.Close()
		return nil, fmt.Errorf("unexpected subscription reply: %s", resp.Result)
	}
	return &EventStream{conn: conn, r: r}, nil
}

func (s *EventStream) Close() error { return s.conn.Close() }

// Next returns the next event. Lines that do not parse as an event envelope
// are skipped.
func (s *EventStream) Next() (*eventEnvelope, error) {
	for {
		line, err := s.r.ReadBytes('\n')
		if err != nil {
			return nil, err
		}
		var ev eventEnvelope
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		if ev.Event == "" || len(ev.Data) == 0 {
			continue
		}
		return &ev, nil
	}
}
