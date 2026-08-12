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

// The events we subscribe to. Every one of them only tells us that something
// moved; the state is then re-queried as a whole, because event payloads are
// partial (pane_agent_detected, for one, only returns pane_id and agent,
// without cwd or the title).
var baseSubscriptions = []string{
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

// Subscriptions added on protocol 19+. Focus drives the idle/done split (an
// agent is "done" until its tab has been seen), so these keep the status icon
// in sync without waiting for the next poll. They are gated by protocol
// version: an older server rejecting an unknown subscription type would fail
// the whole events.subscribe call.
var focusSubscriptions = []string{
	"tab.focused",
	"pane.focused",
	"pane.moved",
	"workspace.focused",
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

// procEntry caches one pane.process_info answer. The pane revision is what
// makes it safe: the server bumps it whenever the foreground process changes.
type procEntry struct {
	revision uint64
	name     string
}

type renamer struct {
	cli      *Client
	tmpl     Template
	icons    map[string]string
	apply    bool
	force    bool
	needProc bool // the template references {proc}; otherwise never ask for it
	maxLen   int
	debounce time.Duration
	only     string // workspace_id filter, empty means all

	// passMu serializes the passes: the timer poll and an event-driven pass
	// would otherwise overlap and rename the same tab twice.
	passMu sync.Mutex

	mu          sync.Mutex
	state       *state               // our labels, survives a restart
	dryShown    map[string]string    // tab_id -> what we already printed in dry-run
	manual      map[string]bool      // tab_id -> the name is human-made, do not touch
	procCache   map[string]procEntry // pane_id -> foreground process name
	timer       *time.Timer          // event coalescing; a pass covers all workspaces
	pollFailing bool                 // a poll error was already reported
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

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cli, err := Dial(*sock)
	if err != nil {
		if *once {
			log.Fatalf("herdr is unreachable: %v (is the server running? `herdr status`)", err)
		}
		// Running as a user service we are quite likely to start before herdr
		// does, and herdr may be stopped and started again at any time. Waiting
		// beats exiting and having the supervisor restart us in a loop.
		log.Printf("herdr is unreachable: %v — waiting for the socket", err)
		if cli, err = dialWait(ctx, *sock); err != nil {
			log.Print("stopped")
			return
		}
	}
	defer cli.Close()

	if version, protocol := cli.Version(); version != "" {
		log.Printf("herdr %s (protocol %d), session.snapshot %s",
			version, protocol, enabledIf(protocol >= protocolSnapshot))
	} else {
		log.Print("herdr version unknown (ping failed), falling back to tab.list + pane.list")
	}

	st := LoadState(*statePath)
	if len(st.Labels) > 0 {
		log.Printf("state: %d labels from %s", len(st.Labels), *statePath)
	}

	tmpl := ParseTemplate(*format)
	r := &renamer{
		cli:       cli,
		tmpl:      tmpl,
		icons:     iconMap,
		apply:     *apply,
		force:     *force,
		needProc:  tmpl.UsesToken("proc"),
		maxLen:    *maxLen,
		debounce:  *debounce,
		only:      *workspace,
		state:     st,
		dryShown:  map[string]string{},
		manual:    map[string]bool{},
		procCache: map[string]procEntry{},
	}

	if *once {
		if err := r.pass(); err != nil {
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
	subs := r.subscriptions()
	log.Printf("subscribing to events: %v", subs)
	if err := r.watch(ctx, *sock, subs); err != nil && ctx.Err() == nil {
		log.Fatalf("the event stream broke: %v", err)
	}
	log.Print("stopped")
}

// dialWait retries the connection until herdr shows up or we are asked to stop.
func dialWait(ctx context.Context, sock string) (*Client, error) {
	backoff := time.Second
	for ctx.Err() == nil {
		cli, err := Dial(sock)
		if err == nil {
			log.Print("herdr answered, carrying on")
			return cli, nil
		}
		if !sleepCtx(ctx, backoff) {
			break
		}
		backoff = min(backoff*2, 30*time.Second)
	}
	return nil, ctx.Err()
}

func enabledIf(ok bool) string {
	if ok {
		return "enabled"
	}
	return "unavailable"
}

func (r *renamer) subscriptions() []string {
	if _, protocol := r.cli.Version(); protocol >= protocolSnapshot {
		return append(append([]string{}, baseSubscriptions...), focusSubscriptions...)
	}
	return baseSubscriptions
}

// pass recomputes the names of every tab (or of the ones in -workspace) and
// brings them in line with the template.
//
// The whole session arrives in a single Snapshot call, so one pass costs one
// connection plus at most one pane.process_info per tab that needs {proc} and
// whose lead pane actually changed.
func (r *renamer) pass() error {
	r.passMu.Lock()
	defer r.passMu.Unlock()

	tabs, panes, err := r.cli.Snapshot()
	if err != nil {
		return err
	}

	byTab := map[string][]paneInfo{}
	live := make(map[string]bool, len(panes))
	for _, p := range panes {
		byTab[p.TabID] = append(byTab[p.TabID], p)
		live[p.PaneID] = true
	}

	sort.Slice(tabs, func(i, j int) bool { return tabs[i].Number < tabs[j].Number })
	alive := make(map[string]bool, len(tabs))
	for _, t := range tabs {
		if r.only != "" && t.WorkspaceID != r.only {
			continue
		}
		alive[t.TabID] = true
		panes := byTab[t.TabID]
		r.reconcile(t, panes, r.procName(leadPane(panes)))
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for paneID := range r.procCache {
		if !live[paneID] {
			delete(r.procCache, paneID)
		}
	}
	// Labels of closed tabs are no longer needed. We only prune during a full
	// pass: with -workspace the tab list is knowingly incomplete.
	if r.only == "" && r.apply {
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

// procName returns the foreground process name of the lead pane, empty when
// there is nothing worth naming a tab after.
//
// We ask herdr only when the template needs {proc} and only for a pane without
// a detected agent: the agent name is more precise, and an agent's foreground
// group also holds its MCP servers. The answer is cached against the pane
// revision, which the server bumps on every foreground process change — so a
// pane where nothing happens costs nothing on subsequent passes.
func (r *renamer) procName(lead *paneInfo) string {
	if !r.needProc || lead == nil || lead.Agent != "" {
		return ""
	}

	if lead.Revision != 0 {
		r.mu.Lock()
		cached, ok := r.procCache[lead.PaneID]
		r.mu.Unlock()
		if ok && cached.revision == lead.Revision {
			return cached.name
		}
	}

	info, err := r.cli.ProcessInfo(lead.PaneID)
	if err != nil {
		log.Printf("%s: process_info failed: %v", lead.PaneID, err)
		return ""
	}
	name := ProcName(info)
	if lead.Revision != 0 {
		r.mu.Lock()
		r.procCache[lead.PaneID] = procEntry{revision: lead.Revision, name: name}
		r.mu.Unlock()
	}
	return name
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
// "all panes" is impossible. Measured on 0.7.5: a daemon with only a
// subscription lived for 75 minutes and received 3 events, while the state
// changed dozens of times. On 0.8.0 the live stream is still quiet — a minute
// of a busy session went by without a single event once the initial backlog had
// been replayed.
func (r *renamer) pollLoop(ctx context.Context, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			err := r.pass()
			if ctx.Err() != nil {
				return
			}
			r.reportPoll(err)
		}
	}
}

// reportPoll logs a poll failure once and then stays quiet until it recovers.
// With herdr stopped every single poll fails, and a line per -poll interval
// would bury the journal — this daemon is meant to run as a user service.
func (r *renamer) reportPoll(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	switch {
	case err != nil && !r.pollFailing:
		r.pollFailing = true
		log.Printf("poll failed: %v (staying quiet until it recovers)", err)
	case err == nil && r.pollFailing:
		r.pollFailing = false
		log.Print("poll recovered")
	}
}

// watch holds the subscription and reconnects when the server drops it
// (a herdr restart, a live handoff during an update).
func (r *renamer) watch(ctx context.Context, sock string, subs []string) error {
	backoff := time.Second
	failing := false // the same silencing as in reportPoll, for the same reason
	for ctx.Err() == nil {
		stream, err := Subscribe(sock, subs)
		if err != nil {
			if !failing {
				failing = true
				log.Printf("subscription failed (%v), retrying quietly", err)
			}
			if !sleepCtx(ctx, backoff) {
				return ctx.Err()
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		if failing {
			failing = false
			log.Print("subscription restored")
		}
		backoff = time.Second

		// The state may have drifted while we were (re)connecting — reconcile
		// everything.
		if err := r.pass(); err != nil {
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
		r.schedule()
	}
}

// schedule coalesces the event stream: pane.updated arrives in bursts, and
// re-querying the state for each one is wasted work. A single timer is enough
// because one pass now covers every workspace at once.
//
// The coalescing also absorbs the backlog herdr 0.8.0 replays on every
// events.subscribe: a fresh subscription immediately delivers dozens of past
// events, including ones describing panes that no longer exist. Since a pass
// re-reads the current state and ignores the payloads, a stale replay costs one
// extra pass and nothing else.
func (r *renamer) schedule() {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.timer != nil {
		r.timer.Reset(r.debounce)
		return
	}
	r.timer = time.AfterFunc(r.debounce, func() {
		if err := r.pass(); err != nil {
			log.Printf("refresh failed: %v", err)
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
