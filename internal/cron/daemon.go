package cron

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wykserdex/wedra/internal/pipeline"
	"github.com/wykserdex/wedra/internal/registry"
)

// ScheduleState — куда сохраняем next_run/last_run для GUI и для catchUp.
// Хранится как <runsDir>/.state/<pipeline>.json, локально, без внешних зависимостей.
type ScheduleState struct {
	Pipeline string     `json:"pipeline"`
	Cron     string     `json:"cron"`
	NextRun  *time.Time `json:"next_run"`
	LastRun  *time.Time `json:"last_run"`
	Enabled  bool       `json:"enabled"`
	Overlap  string     `json:"overlap"`
}

// statePath — файл состояния для пайплайна (по имени без пути, с хешем)
func statePath(runsDir, pipelineFile string) string {
	base := filepath.Base(pipelineFile)
	// простой хеш по имени пути чтобы не коллизить
	h := 0
	for _, c := range pipelineFile {
		h = h*31 + int(c)
	}
	if h < 0 {
		h = -h
	}
	name := fmt.Sprintf("%s-%x.json", base, h)
	return filepath.Join(runsDir, ".state", name)
}

func loadState(p string) *ScheduleState {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var s ScheduleState
	if err := json.Unmarshal(b, &s); err != nil {
		return nil
	}
	return &s
}
func saveState(p string, s *ScheduleState) {
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	b, _ := json.MarshalIndent(s, "", "  ")
	_ = os.WriteFile(p, b, 0o644)
}

// lockPath — опциональный лок для демона (opt-in). Если runsDir/.daemon.lock существует — второй демон не стартует.
func lockPath(runsDir string) string { return filepath.Join(runsDir, ".daemon.lock") }
func tryLock(runsDir string) (func(), bool) {
	lp := lockPath(runsDir)
	f, err := os.OpenFile(lp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, false
	}
	fmt.Fprintf(f, "%d %s\n", os.Getpid(), time.Now().Format(time.RFC3339))
	f.Close()
	return func() { os.Remove(lp) }, true
}

// NextRunFor — считает next_run с учетом timezone
func NextRunFor(sched *pipeline.Schedule, now time.Time) *time.Time {
	if sched == nil || sched.Cron == "" {
		return nil
	}
	loc := time.Local
	if sched.Timezone != "" && sched.Timezone != "Local" && sched.Timezone != "local" {
		if l, err := time.LoadLocation(sched.Timezone); err == nil {
			loc = l
		}
	}
	nowInLoc := now.In(loc)
	n := Next(sched.Cron, nowInLoc)
	if n == nil {
		return nil
	}
	// Next возвращает в loc, конвертим
	return n
}

// ScanPipelines — найти все pipeline.yaml с schedule в dir
func ScanPipelines(dir string) ([]string, error) {
	var out []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() && (info.Name() == "*.yaml" || filepath.Ext(path) == ".yaml") {
			// quick check: contains schedule:
			b, _ := os.ReadFile(path)
			if containsSchedule(b) {
				out = append(out, path)
			}
		}
		return nil
	})
	// simpler: glob
	if len(out) == 0 {
		matches, _ := filepath.Glob(filepath.Join(dir, "*.yaml"))
		for _, m := range matches {
			b, _ := os.ReadFile(m)
			if containsSchedule(b) {
				out = append(out, m)
			}
		}
		// also examples/
		matches2, _ := filepath.Glob(filepath.Join("examples", "*.yaml"))
		for _, m := range matches2 {
			b, _ := os.ReadFile(m)
			if containsSchedule(b) {
				out = append(out, m)
			}
		}
	}
	return out, err
}

func containsSchedule(b []byte) bool {
	return len(b) > 0 && contains(b, []byte("schedule:")) && contains(b, []byte("cron:"))
}

func contains(b, sub []byte) bool {
	return len(b) >= len(sub) && indexOf(b, sub) >= 0
}
func indexOf(b, sub []byte) int {
	n := len(b) - len(sub) + 1
	for i := 0; i < n; i++ {
		ok := true
		for j := range sub {
			if b[i+j] != sub[j] {
				ok = false
				break
			}
		}
		if ok {
			return i
		}
	}
	return -1
}

// Daemon — минимальный демон: тикает каждую минуту, запускает pipeline run
// В wedra это будет вызываться из internal/cli/daemon.go или internal/api/server.go
type Daemon struct {
	PipelinesDir string
	RunsDir      string
	// runner — функция запуска: файл -> run_id
	Runner func(pipelineFile string, autoApprove bool) (string, error)
}

func (d *Daemon) Tick(ctx context.Context) error {
	// сканируем pipelines
	files, _ := ScanPipelines(d.PipelinesDir)
	now := time.Now()
	for _, f := range files {
		pf, err := pipeline.LoadPipelineFile(f)
		if err != nil {
			continue
		}
		sched := pf.Pipeline.Schedule
		if sched == nil || !sched.IsEnabled() {
			continue
		}
		stPath := statePath(d.RunsDir, f)
		st := loadState(stPath)
		next := NextRunFor(sched, now.Add(-time.Minute))
		if next == nil {
			continue
		}
		overlap := sched.Overlap
		if overlap == "" {
			overlap = "skip"
		}
		runningLock := filepath.Join(d.RunsDir, ".running."+filepath.Base(f)+".lock")
		isRunning := false
		if _, err := os.Stat(runningLock); err == nil {
			isRunning = true
		}
		if isRunning {
			switch overlap {
			case "skip":
				continue
			case "queue":
				_ = os.WriteFile(runningLock+".queued", []byte(now.Format(time.RFC3339)), 0o644)
				continue
			case "allow":
			}
		}
		shouldRun := next.After(now.Add(-time.Minute)) && next.Before(now.Add(5*time.Second))
		if !shouldRun && sched.CatchUp && st != nil && st.LastRun != nil {
			exp := NextRunFor(sched, st.LastRun.Add(time.Minute))
			if exp != nil && exp.Before(now) && exp.After(st.LastRun.Add(-5*time.Minute)) {
				shouldRun = true
			}
		}
		if !shouldRun {
			rr := ScheduleState{Pipeline: f, Cron: sched.Cron, NextRun: NextRunFor(sched, now), Enabled: sched.IsEnabled(), Overlap: sched.Overlap}
			if st != nil {
				rr.LastRun = st.LastRun
			}
			saveState(stPath, &rr)
			continue
		}
		autoApprove := sched.HumanGate == "autoApprove" || sched.HumanGate == "auto_approve"
		_ = os.WriteFile(runningLock, []byte(now.Format(time.RFC3339)), 0o644)
		if d.Runner != nil {
			runID, err := d.Runner(f, autoApprove)
			_ = runID
			if err != nil {
				fmt.Fprintf(os.Stderr, "schedule %s: %v\n", f, err)
				// Ошибка оповещения печатается, а не отбрасывается. Раньше
				// стояло `_ = fireAlert(...)`, из-за чего молчание о
				// неработающем оповещении было неотличимо от «оповещения нет».
				if aerr := fireAlert(sched, "on_failure", f, err.Error()); aerr != nil {
					fmt.Fprintf(os.Stderr, "schedule %s: %v\n", f, aerr)
				}
			} else if sched.HumanGate == "wait" {
				if aerr := fireAlert(sched, "on_waiting", f, "human gate waiting"); aerr != nil {
					fmt.Fprintf(os.Stderr, "schedule %s: %v\n", f, aerr)
				}
			}
		}
		_ = os.Remove(runningLock)
		if _, err := os.Stat(runningLock + ".queued"); err == nil {
			_ = os.Remove(runningLock + ".queued")
		}
		t := now
		saveState(stPath, &ScheduleState{Pipeline: f, Cron: sched.Cron, NextRun: NextRunFor(sched, now.Add(time.Minute)), LastRun: &t, Enabled: true, Overlap: sched.Overlap})
	}
	return nil
}

// alertPayload — тело POST в webhook.
//
// Плоское и без внутренних структур по двум причинам: получатель на той стороне
// не обязан знать про WEDRA, и лишняя вложенность в теле оповещения ничем не
// окупается. Время в UTC и RFC3339 — иначе разбирать придётся в догадках о
// часовом поясе отправителя.
type alertPayload struct {
	Event    string `json:"event"`
	Pipeline string `json:"pipeline"`
	Detail   string `json:"detail"`
	At       string `json:"at"`
}

// alertTimeout — потолок на одну попытку доставки.
//
// Ограничение обязательное: демон тикает раз в минуту, и висящий POST занял бы
// соединение на всё это время вместе с блокировкой ран и `.running.lock`.
const alertTimeout = 10 * time.Second

// alertClient — HTTP-клиент для вебхуков. Заменяется в тестах.
var alertClient = &http.Client{Timeout: alertTimeout}

// alertKindFor — вид события, на который подписан получатель: alertKind это
// уже «on_waiting» или «on_failure», то есть имя поля схемы.
func alertKindFor(sched *pipeline.Schedule, alertKind string) string {
	if sched == nil {
		return ""
	}
	if alertKind == "on_waiting" {
		return sched.Alerts.OnWaiting
	}
	if alertKind == "on_failure" {
		return sched.Alerts.OnFailure
	}
	return ""
}

// fireAlert — доставляет оповещение о событии расписания.
//
// Раньше здесь было два `return nil` при живых схеме, CLI и чейнджлоге: человек
// прописывал `alerts: webhook:` и получал тишину, причём демон даже не пытался
// ничего отправить. Теперь отправляет.
//
// Порядок и смысл:
//
//   - пустая подписка — тишина, это не ошибка: on_waiting без on_waiting не
//     должен ничего слать;
//   - подписка именем плагина проверяется на существование. Плагина
//     telegram_notifier в репозитории нет, и раньше это молча игнорировалось.
//     Теперь отсутствие плагина — внятная ошибка, а не «всё хорошо»;
//   - webhook — единственный канал, который реально доставляется: POST с
//     JSON-телом.
//
// Ошибка возвращается, а не глотается: вызывающий печатает её в stderr, потому
// что молчаливая потеря оповещения — это ровно тот дефект, который здесь и был.
func fireAlert(sched *pipeline.Schedule, kind, pipelineFile, detail string) error {
	if sched == nil {
		return nil
	}
	// Вебхук — единственный реально работающий канал. Если он задан, оповещение
	// уходит, и подписка плагином сюда не при чём.
	if sched.Alerts.Webhook != "" {
		return postAlert(sched.Alerts.Webhook, kind, pipelineFile, detail)
	}
	// Канала нет. Если событие вообще подписано — это молчащая поломка, и её
	// надо назвать: человек прописал on_waiting и ждал алерта.
	wanted := alertKindFor(sched, kind)
	if wanted == "" {
		return nil
	}
	found, err := pluginExists(wanted)
	if err != nil {
		return fmt.Errorf("алерт %s: подписка на %q, проверить не удалось (%v); "+
			"канал доставки не задан — alerts.webhook пуст", kind, wanted, err)
	}
	if !found {
		return fmt.Errorf("алерт %s: плагин-нотификатор %q не найден в реестре; "+
			"канал доставки не задан — alerts.webhook пуст, оповещение уйти не может", kind, wanted)
	}
	return fmt.Errorf("алерт %s: плагин-нотификатор %q есть, но канал доставки не задан — "+
		"задайте alerts.webhook: вызов нотификатора из демона не подключён", kind, wanted)
}

// postAlert — доставка в webhook.
func postAlert(webhook, kind, pipelineFile, detail string) error {
	if !strings.HasPrefix(webhook, "http://") && !strings.HasPrefix(webhook, "https://") {
		return fmt.Errorf("алерт %s: webhook %q не http(s)-адрес", kind, webhook)
	}
	body, err := json.Marshal(alertPayload{
		Event:    kind,
		Pipeline: pipelineFile,
		Detail:   detail,
		At:       time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return fmt.Errorf("алерт %s: тело запроса: %w", kind, err)
	}
	req, err := http.NewRequest(http.MethodPost, webhook, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("алерт %s: %w", kind, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := alertClient.Do(req)
	if err != nil {
		return fmt.Errorf("алерт %s: доставка в %s: %w", kind, webhook, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("алерт %s: %s ответил %s", kind, webhook, resp.Status)
	}
	return nil
}

// pluginExists — есть ли плагин с таким id в реестре.
//
// Резолвится через сам пакет registry, а не поиском по тексту файла: путь к
// реестру выбирается в одном месте (DefaultSource), и вторая реализация этого
// выбора разъехалась бы с первой при первом же переезде.
//
// Три исхода различаются намеренно: есть / нет / не удалось проверить. Средний
// случай — это «нет ответа», и выдавать его за «плагина нет» нельзя: тогда
// сетевой сбой реестра выглядел бы как ошибка конфигурации пайплайна.
func pluginExists(id string) (found bool, err error) {
	h, err := registry.Load(registry.DefaultSource())
	if err != nil {
		return false, fmt.Errorf("реестр недоступен: %w", err)
	}
	defer h.Close()
	_, found = h.GetPlugin(id)
	return found, nil
}

func (d *Daemon) Run(ctx context.Context) error {
	if unlock, ok := tryLock(d.RunsDir); ok {
		defer unlock()
	} else {
		return fmt.Errorf("daemon уже запущен (lock %s)", lockPath(d.RunsDir))
	}
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	// первый тик сразу
	_ = d.Tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			_ = d.Tick(ctx)
		}
	}
}
