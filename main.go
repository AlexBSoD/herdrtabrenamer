package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The events we subscribe to. All of them carry a workspace_id, so it is enough
// to know which workspace moved and re-query its state as a whole — event
// payloads are partial (pane_agent_detected, for one, only returns pane_id and
// agent, without cwd or the title).
var defaultSubscriptions = []string{
	"tab.created",
	"tab.closed",
	"tab.renamed",
	"tab.moved",
	"pane.created",
	"pane.updated",
	"pane.closed",
	"pane.exited",
	"pane.agent_detected",
}

// The common shape of an event payload: we only take the routing fields.
type eventRoute struct {
	Type        string `json:"type"`
	WorkspaceID string `json:"workspace_id"`
	Tab         struct {
		WorkspaceID string `json:"workspace_id"`
	} `json:"tab"`
	Pane struct {
		WorkspaceID string `json:"workspace_id"`
	} `json:"pane"`
}

func (e eventRoute) workspace() string {
	switch {
	case e.WorkspaceID != "":
		return e.WorkspaceID
	case e.Tab.WorkspaceID != "":
		return e.Tab.WorkspaceID
	case e.Pane.WorkspaceID != "":
		return e.Pane.WorkspaceID
	}
	return ""
}

type renamer struct {
	cli      *Client
	tmpl     Template
	icons    map[string]string
	apply    bool
	force    bool
	maxLen   int
	debounce time.Duration
	only     string // workspace_id filter, empty means all

	// refreshMu serializes the passes: the timer poll and an event-driven pass
	// would otherwise overlap and rename the same tab twice.
	refreshMu sync.Mutex

	mu       sync.Mutex
	state    *state            // our labels, survives a restart
	dryShown map[string]string // tab_id -> what we already printed in dry-run
	manual   map[string]bool   // tab_id -> the name is human-made, do not touch
	timers   map[string]*time.Timer
}

func main() {
	sock := flag.String("socket", SocketPath(), "path to the herdr server socket")
	format := flag.String("format", "{proc|dir}",
		"tab name template; tokens {proc} {dir} {agent} {cwd} {title} {icon} {status} {number}, "+
			"alternatives separated by | take the first non-empty one")
	icons := flag.String("icons", "",
		"override the status emoji, e.g. \"working=⚡,idle=\" (statuses: blocked working done idle unknown)")
	apply := flag.Bool("apply", false, "actually rename (without this flag it only prints)")
	force := flag.Bool("force", false, "overwrite human-made names as well")
	once := flag.Bool("once", false, "make a single pass over the current tabs and exit")
	maxLen := flag.Int("max-len", 24, "maximum name length in characters, 0 means unlimited")
	debounce := flag.Duration("debounce", 400*time.Millisecond, "how long to coalesce a burst of events")
	poll := flag.Duration("poll", 3*time.Second,
		"state polling interval; 0 means events only. Needed because herdr does not "+
			"emit events for some changes (an agent_status change, for instance)")
	workspace := flag.String("workspace", "", "restrict to a single workspace_id")
	statePath := flag.String("state", StatePath(),
		"file holding the labels the daemon set; empty means do not remember across runs")
	flag.Parse()

	if *sock == "" {
		log.Fatal("cannot determine the socket path, pass -socket")
	}

	log.SetFlags(log.Ltime)
	mode := "DRY-RUN (nothing is changed)"
	if *apply {
		mode = "APPLY (tabs will be renamed)"
	}
	log.Printf("herdrtabrenamer: %s", mode)
	log.Printf("socket: %s", *sock)

	iconMap, err := ParseIcons(*icons)
	if err != nil {
		log.Fatalf("-icons flag: %v", err)
	}
	log.Printf("template: %q, max-len=%d, debounce=%s, poll=%s", *format, *maxLen, *debounce, *poll)
	if strings.Contains(*format, "{icon}") || strings.Contains(*format, "{status}") {
		log.Printf("icons: blocked=%q working=%q done=%q idle=%q unknown=%q",
			iconMap["blocked"], iconMap["working"], iconMap["done"], iconMap["idle"], iconMap["unknown"])
	}

	cli, err := Dial(*sock)
	if err != nil {
		log.Fatalf("herdr is unreachable: %v (is the server running? `herdr status`)", err)
	}
	defer cli.Close()

	st := LoadState(*statePath)
	if len(st.Labels) > 0 {
		log.Printf("state: %d labels from %s", len(st.Labels), *statePath)
	}

	r := &renamer{
		cli:      cli,
		tmpl:     ParseTemplate(*format),
		icons:    iconMap,
		apply:    *apply,
		force:    *force,
		maxLen:   *maxLen,
		debounce: *debounce,
		only:     *workspace,
		state:    st,
		dryShown: map[string]string{},
		manual:   map[string]bool{},
		timers:   map[string]*time.Timer{},
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if *once {
		if err := r.sweep(); err != nil {
			log.Fatalf("the first pass failed: %v", err)
		}
		return
	}
	// In watch mode the first pass is made by watch right after subscribing —
	// otherwise events happening between the pass and the subscription would be
	// lost.

	if *poll > 0 {
		go r.pollLoop(ctx, *poll)
	}
	log.Printf("subscribing to events: %v", defaultSubscriptions)
	if err := r.watch(ctx, *sock); err != nil && ctx.Err() == nil {
		log.Fatalf("the event stream broke: %v", err)
	}
	log.Print("stopped")
}

// sweep recomputes the names of every tab in all workspaces (or in the given one).
func (r *renamer) sweep() error {
	tabs, err := r.cli.ListTabs(r.only)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	alive := make(map[string]bool, len(tabs))
	for _, t := range tabs {
		if r.only != "" && t.WorkspaceID != r.only {
			continue
		}
		seen[t.WorkspaceID] = true
		alive[t.TabID] = true
	}
	spaces := make([]string, 0, len(seen))
	for ws := range seen {
		spaces = append(spaces, ws)
	}
	sort.Strings(spaces)
	for _, ws := range spaces {
		if err := r.refresh(ws); err != nil {
			return err
		}
	}

	// Labels of closed tabs are no longer needed. We only prune during a full
	// pass: with -workspace the tab list is knowingly incomplete.
	if r.only == "" && r.apply {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.state.Prune(alive) {
			if err := r.state.Save(); err != nil {
				log.Printf("state not saved: %v", err)
			}
		}
		for tabID := range r.manual {
			if !alive[tabID] {
				delete(r.manual, tabID)
			}
		}
	}
	return nil
}

// refresh re-queries the tabs and panes of a single workspace and brings the
// names in line with the template.
func (r *renamer) refresh(workspace string) error {
	r.refreshMu.Lock()
	defer r.refreshMu.Unlock()

	tabs, err := r.cli.ListTabs(workspace)
	if err != nil {
		return err
	}
	panes, err := r.cli.ListPanes(workspace)
	if err != nil {
		return err
	}

	byTab := map[string][]paneInfo{}
	for _, p := range panes {
		byTab[p.TabID] = append(byTab[p.TabID], p)
	}

	sort.Slice(tabs, func(i, j int) bool { return tabs[i].Number < tabs[j].Number })
	for _, t := range tabs {
		if workspace != "" && t.WorkspaceID != workspace {
			continue
		}
		panes := byTab[t.TabID]
		// We ask for the process name only for the lead pane and only when no
		// agent was detected: an agent's foreground group also holds its MCP
		// servers, and the agent name itself is already known.
		proc := ""
		if lead := leadPane(panes); lead != nil && lead.Agent == "" {
			info, err := r.cli.ProcessInfo(lead.PaneID)
			if err != nil {
				log.Printf("%s: process_info failed: %v", lead.PaneID, err)
			} else {
				proc = ProcName(info)
			}
		}
		r.reconcile(t, panes, proc)
	}
	return nil
}

// reconcile decides the fate of a single tab.
func (r *renamer) reconcile(tab tabInfo, panes []paneInfo, proc string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.manual[tab.TabID] && !r.force {
		return
	}

	lead := leadPane(panes)
	ctx := contextFor(tab, lead, proc, r.icons)
	desired := Truncate(r.tmpl.Render(ctx), r.maxLen)

	// A name that looks neither auto-generated nor like our own work was set by
	// a human. Respect it and never come back to this tab.
	if !r.force && !IsGeneratedLabel(tab.Label) &&
		tab.Label != r.state.Labels[tab.TabID] && !r.looksOurs(ctx, tab.Label) {
		r.manual[tab.TabID] = true
		log.Printf("%s: the name %q is human-made — skipping", tab.TabID, tab.Label)
		return
	}

	if desired == "" || desired == tab.Label {
		return
	}

	if !r.apply {
		// Events are many while the name changes rarely — print only new ones.
		if r.dryShown[tab.TabID] != desired {
			r.dryShown[tab.TabID] = desired
			log.Printf("[dry-run] %s: %q -> %q%s", tab.TabID, tab.Label, desired, describeLead(lead))
		}
		return
	}
	if err := r.cli.RenameTab(tab.TabID, desired); err != nil {
		log.Printf("%s: rename failed: %v", tab.TabID, err)
		return
	}
	r.state.Labels[tab.TabID] = desired
	if err := r.state.Save(); err != nil {
		log.Printf("state not saved: %v", err)
	}
	log.Printf("%s: %q -> %q%s", tab.TabID, tab.Label, desired, describeLead(lead))
}

// looksOurs checks whether we set this name ourselves earlier, in a different
// tab state. Without this check a daemon restart would look like this: we see
// our own "🟡 claude:nixos", the memory is empty, so it must be "human-made" —
// and the tab freezes forever.
//
// We vary everything that changes on its own while the name stays ours: the
// status (and with it the icon), the presence of a running process and the
// presence of an agent. A case from practice: a tab was named "rmk" (only a
// shell in the pane), then the user started btop — and without the variant
// where {proc} is empty, the previous name would have looked foreign.
func (r *renamer) looksOurs(ctx NameContext, label string) bool {
	procs := []string{ctx.Proc}
	if ctx.Proc != "" {
		procs = append(procs, "")
	}
	agents := []string{ctx.Agent}
	if ctx.Agent != "" {
		agents = append(agents, "")
	}

	for status := range DefaultIcons {
		for _, proc := range procs {
			for _, agent := range agents {
				probe := ctx
				probe.Status = status
				probe.Icon = r.icons[status]
				probe.Proc = proc
				probe.Agent = agent
				if Truncate(r.tmpl.Render(probe), r.maxLen) == label {
					return true
				}
			}
		}
	}
	return false
}

func describeLead(p *paneInfo) string {
	if p == nil {
		return " (no panes)"
	}
	agent := p.DisplayAgent
	if agent == "" {
		agent = p.Agent
	}
	if agent == "" {
		agent = "-"
	}
	cwd := p.ForegroundCwd
	if cwd == "" {
		cwd = p.Cwd
	}
	return fmt.Sprintf(" [pane=%s agent=%s status=%s cwd=%s]", p.PaneID, agent, p.AgentStatus, cwd)
}

// pollLoop periodically reconciles the state. Events do not cover everything:
// herdr sends no pane.updated on an agent_status change, and the
// pane.agent_status_changed subscription requires a pane_id, so subscribing to
// "all panes" is impossible. Measured: a daemon with only a subscription lived
// for 75 minutes and received 3 events, while the state changed dozens of times.
func (r *renamer) pollLoop(ctx context.Context, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.sweep(); err != nil && ctx.Err() == nil {
				log.Printf("poll failed: %v", err)
			}
		}
	}
}

// watch holds the subscription and reconnects when the server drops it
// (a herdr restart, a live handoff during an update).
func (r *renamer) watch(ctx context.Context, sock string) error {
	backoff := time.Second
	for ctx.Err() == nil {
		stream, err := Subscribe(sock, defaultSubscriptions)
		if err != nil {
			log.Printf("subscription failed (%v), retrying in %s", err, backoff)
			if !sleepCtx(ctx, backoff) {
				return ctx.Err()
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second

		// The state may have drifted while we were (re)connecting — reconcile
		// everything.
		if err := r.sweep(); err != nil {
			log.Printf("post-subscription reconcile failed: %v", err)
		}

		err = r.consume(ctx, stream)
		stream.Close()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		log.Printf("event stream closed (%v), reconnecting", err)
	}
	return ctx.Err()
}

func (r *renamer) consume(ctx context.Context, stream *EventStream) error {
	// Reading blocks, so we close the socket when the context is cancelled —
	// Next() then returns an error and the goroutine exits.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			stream.Close()
		case <-done:
		}
	}()

	for {
		ev, err := stream.Next()
		if err != nil {
			return err
		}
		var route eventRoute
		if err := json.Unmarshal(ev.Data, &route); err != nil {
			continue
		}
		ws := route.workspace()
		if ws == "" || (r.only != "" && ws != r.only) {
			continue
		}
		r.schedule(ws)
	}
}

// schedule coalesces the event stream: pane.updated arrives in bursts, and
// re-querying the state for each one is wasted work.
func (r *renamer) schedule(workspace string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if t, ok := r.timers[workspace]; ok {
		t.Reset(r.debounce)
		return
	}
	r.timers[workspace] = time.AfterFunc(r.debounce, func() {
		if err := r.refresh(workspace); err != nil {
			log.Printf("%s: refresh failed: %v", workspace, err)
		}
	})
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
