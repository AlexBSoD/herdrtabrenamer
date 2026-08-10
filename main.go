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

// События, на которые подписываемся. Все они несут workspace_id, поэтому
// достаточно знать, какое рабочее пространство «поехало», и перезапросить его
// состояние целиком — payload'ы событий частичные (pane_agent_detected, к
// примеру, отдаёт только pane_id и agent, без cwd и заголовка).
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

// Общий вид payload события: берём только маршрутизирующие поля.
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
	only     string // фильтр по workspace_id, пусто = все

	// refreshMu сериализует обходы: опрос по таймеру и обход по событию иначе
	// могут пересечься и дважды переименовать один таб.
	refreshMu sync.Mutex

	mu       sync.Mutex
	state    *state            // наши метки, переживает перезапуск
	dryShown map[string]string // tab_id -> что уже показали в dry-run
	manual   map[string]bool   // tab_id -> имя задано человеком, не трогаем
	timers   map[string]*time.Timer
}

func main() {
	sock := flag.String("socket", SocketPath(), "путь до сокета herdr-сервера")
	format := flag.String("format", "{proc|dir}",
		"шаблон имени таба; токены {proc} {dir} {agent} {cwd} {title} {icon} {status} {number}, "+
			"альтернативы через | берут первое непустое")
	icons := flag.String("icons", "",
		"переопределение эмодзи статуса, например \"working=⚡,idle=\" (статусы: blocked working done idle unknown)")
	apply := flag.Bool("apply", false, "реально переименовывать (без флага — только показывать)")
	force := flag.Bool("force", false, "перезаписывать и вручную заданные имена")
	once := flag.Bool("once", false, "один проход по текущим табам и выход")
	maxLen := flag.Int("max-len", 24, "максимальная длина имени в символах, 0 — без ограничения")
	debounce := flag.Duration("debounce", 400*time.Millisecond, "задержка склейки событий")
	poll := flag.Duration("poll", 3*time.Second,
		"период опроса состояния; 0 — только события. Нужен потому, что herdr не "+
			"присылает событий на часть изменений (например смену agent_status)")
	workspace := flag.String("workspace", "", "ограничиться одним workspace_id")
	statePath := flag.String("state", StatePath(),
		"файл с метками, которые поставил демон; пусто — не запоминать между запусками")
	flag.Parse()

	if *sock == "" {
		log.Fatal("не удалось определить путь до сокета, укажите -socket")
	}

	log.SetFlags(log.Ltime)
	mode := "DRY-RUN (ничего не меняется)"
	if *apply {
		mode = "APPLY (табы будут переименованы)"
	}
	log.Printf("herdrtabrenamer: %s", mode)
	log.Printf("сокет: %s", *sock)

	iconMap, err := ParseIcons(*icons)
	if err != nil {
		log.Fatalf("флаг -icons: %v", err)
	}
	log.Printf("шаблон: %q, max-len=%d, debounce=%s, poll=%s", *format, *maxLen, *debounce, *poll)
	if strings.Contains(*format, "{icon}") || strings.Contains(*format, "{status}") {
		log.Printf("иконки: blocked=%q working=%q done=%q idle=%q unknown=%q",
			iconMap["blocked"], iconMap["working"], iconMap["done"], iconMap["idle"], iconMap["unknown"])
	}

	cli, err := Dial(*sock)
	if err != nil {
		log.Fatalf("herdr недоступен: %v (сервер запущен? `herdr status`)", err)
	}
	defer cli.Close()

	st := LoadState(*statePath)
	if len(st.Labels) > 0 {
		log.Printf("состояние: %d меток из %s", len(st.Labels), *statePath)
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
			log.Fatalf("первый проход не удался: %v", err)
		}
		return
	}
	// В режиме наблюдения первый проход делает watch сразу после подписки —
	// иначе события, случившиеся между проходом и подпиской, потерялись бы.

	if *poll > 0 {
		go r.pollLoop(ctx, *poll)
	}
	log.Printf("подписка на события: %v", defaultSubscriptions)
	if err := r.watch(ctx, *sock); err != nil && ctx.Err() == nil {
		log.Fatalf("поток событий оборвался: %v", err)
	}
	log.Print("остановлено")
}

// sweep пересчитывает имена всех табов во всех (или в заданном) workspace.
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

	// Метки закрытых табов больше не нужны. Чистим только при обходе всех
	// workspace: при -workspace список табов заведомо неполный.
	if r.only == "" && r.apply {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.state.Prune(alive) {
			if err := r.state.Save(); err != nil {
				log.Printf("состояние не сохранено: %v", err)
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

// refresh перезапрашивает табы и панели одного workspace и приводит имена
// в соответствие шаблону.
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
		// Имя процесса спрашиваем только для ведущей панели и только когда
		// агент не определён: у агентской панели в foreground-группе висят
		// ещё и её MCP-серверы, а само имя агента уже известно.
		proc := ""
		if lead := leadPane(panes); lead != nil && lead.Agent == "" {
			info, err := r.cli.ProcessInfo(lead.PaneID)
			if err != nil {
				log.Printf("%s: process_info не получен: %v", lead.PaneID, err)
			} else {
				proc = ProcName(info)
			}
		}
		r.reconcile(t, panes, proc)
	}
	return nil
}

// reconcile решает судьбу одного таба.
func (r *renamer) reconcile(tab tabInfo, panes []paneInfo, proc string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.manual[tab.TabID] && !r.force {
		return
	}

	lead := leadPane(panes)
	ctx := contextFor(tab, lead, proc, r.icons)
	desired := Truncate(r.tmpl.Render(ctx), r.maxLen)

	// Имя, которое не похоже ни на автогенерированное, ни на нашу работу,
	// задал человек. Уважаем и больше к этому табу не возвращаемся.
	if !r.force && !IsGeneratedLabel(tab.Label) &&
		tab.Label != r.state.Labels[tab.TabID] && !r.looksOurs(ctx, tab.Label) {
		r.manual[tab.TabID] = true
		log.Printf("%s: имя %q задано вручную — пропускаю", tab.TabID, tab.Label)
		return
	}

	if desired == "" || desired == tab.Label {
		return
	}

	if !r.apply {
		// Событий много, а имя меняется редко — печатаем только новое.
		if r.dryShown[tab.TabID] != desired {
			r.dryShown[tab.TabID] = desired
			log.Printf("[dry-run] %s: %q -> %q%s", tab.TabID, tab.Label, desired, describeLead(lead))
		}
		return
	}
	if err := r.cli.RenameTab(tab.TabID, desired); err != nil {
		log.Printf("%s: переименование не удалось: %v", tab.TabID, err)
		return
	}
	r.state.Labels[tab.TabID] = desired
	if err := r.state.Save(); err != nil {
		log.Printf("состояние не сохранено: %v", err)
	}
	log.Printf("%s: %q -> %q%s", tab.TabID, tab.Label, desired, describeLead(lead))
}

// looksOurs проверяет, не мы ли поставили это имя раньше — при другом
// состоянии таба. Без этой проверки перезапуск демона выглядел бы так: видим
// своё же "🟡 claude:nixos", в памяти пусто, значит «ручное» — и таб замирает
// навсегда.
//
// Варьируем всё, что меняется само по себе, пока имя остаётся нашим:
// статус (а с ним иконку), наличие запущенного процесса и наличие агента.
// Пример из практики: таб звался "rmk" (в панели был только шелл), потом
// пользователь запустил btop — и без варианта с пустым {proc} прежнее имя
// выглядело бы чужим.
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
		return " (панелей нет)"
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

// pollLoop периодически сверяет состояние. Событий на всё не хватает: herdr не
// присылает pane.updated при смене agent_status, а подписка
// pane.agent_status_changed требует pane_id, то есть на «все панели» подписаться
// нельзя. Измерено: демон с одной подпиской прожил 75 минут и получил 3 события,
// хотя состояние менялось десятки раз.
func (r *renamer) pollLoop(ctx context.Context, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.sweep(); err != nil && ctx.Err() == nil {
				log.Printf("опрос не удался: %v", err)
			}
		}
	}
}

// watch держит подписку и переподключается, если сервер её оборвал
// (перезапуск herdr, live handoff при обновлении).
func (r *renamer) watch(ctx context.Context, sock string) error {
	backoff := time.Second
	for ctx.Err() == nil {
		stream, err := Subscribe(sock, defaultSubscriptions)
		if err != nil {
			log.Printf("подписка не удалась (%v), повтор через %s", err, backoff)
			if !sleepCtx(ctx, backoff) {
				return ctx.Err()
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second

		// После (пере)подключения состояние могло уехать — сверяем всё.
		if err := r.sweep(); err != nil {
			log.Printf("сверка после подписки не удалась: %v", err)
		}

		err = r.consume(ctx, stream)
		stream.Close()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		log.Printf("поток событий закрыт (%v), переподключаюсь", err)
	}
	return ctx.Err()
}

func (r *renamer) consume(ctx context.Context, stream *EventStream) error {
	// Чтение блокирующее, поэтому закрываем сокет по отмене контекста —
	// Next() вернёт ошибку и горутина выйдет.
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

// schedule склеивает поток событий: pane.updated прилетает пачками, а
// перезапрашивать состояние на каждое — лишняя работа.
func (r *renamer) schedule(workspace string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if t, ok := r.timers[workspace]; ok {
		t.Reset(r.debounce)
		return
	}
	r.timers[workspace] = time.AfterFunc(r.debounce, func() {
		if err := r.refresh(workspace); err != nil {
			log.Printf("%s: обновление не удалось: %v", workspace, err)
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
