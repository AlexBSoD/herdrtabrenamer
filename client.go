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

// SocketPath возвращает путь до сокета herdr-сервера.
// herdr держит его рядом с config.toml, см. `herdr --help` (раздел Config).
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

// Client выполняет запросы к socket API.
//
// ВАЖНО: сервер обрабатывает РОВНО ОДИН запрос на соединение и сразу его
// закрывает — второй Write в тот же сокет даёт broken pipe. Проверено на
// herdr 0.7.5 / protocol 17. Поэтому каждый Call открывает своё соединение;
// персистентным остаётся только поток подписки (EventStream).
type Client struct {
	sock string
	seq  atomic.Uint64
}

func Dial(sock string) (*Client, error) {
	// Пробное соединение, чтобы сразу отличить «сервер не запущен» от ошибок
	// в самих запросах.
	conn, err := net.DialTimeout("unix", sock, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("подключение к %s: %w", sock, err)
	}
	conn.Close()
	return &Client{sock: sock}, nil
}

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
		return nil, fmt.Errorf("подключение к %s: %w", c.sock, err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return nil, err
	}

	if _, err := conn.Write(append(payload, '\n')); err != nil {
		return nil, fmt.Errorf("запись %s: %w", method, err)
	}

	r := bufio.NewReaderSize(conn, 1<<20)
	line, err := r.ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("чтение ответа %s: %w", method, err)
	}
	var resp response
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("ответ %s не разобран: %w", method, err)
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

// EventStream — отдельное соединение под подписку. Смешивать его с запросами
// нельзя: события приходят без id и сбивают синхронный request/response.
type EventStream struct {
	conn net.Conn
	r    *bufio.Reader
}

func Subscribe(sock string, kinds []string) (*EventStream, error) {
	conn, err := net.DialTimeout("unix", sock, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("подключение к %s: %w", sock, err)
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
		return nil, fmt.Errorf("запись events.subscribe: %w", err)
	}

	r := bufio.NewReaderSize(conn, 1<<20)
	line, err := r.ReadBytes('\n')
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("чтение подтверждения подписки: %w", err)
	}
	var resp response
	if err := json.Unmarshal(line, &resp); err != nil {
		conn.Close()
		return nil, fmt.Errorf("подтверждение подписки не разобрано: %w", err)
	}
	if resp.Error != nil {
		conn.Close()
		return nil, fmt.Errorf("events.subscribe: %w", resp.Error)
	}
	var started eventKind
	if err := json.Unmarshal(resp.Result, &started); err != nil || started.Type != "subscription_started" {
		conn.Close()
		return nil, fmt.Errorf("неожиданный ответ на подписку: %s", resp.Result)
	}
	return &EventStream{conn: conn, r: r}, nil
}

func (s *EventStream) Close() error { return s.conn.Close() }

// Next возвращает следующее событие. Строки, которые не разбираются как
// конверт события, пропускаются.
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
