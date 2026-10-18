package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"github.com/wykserdex/wedra/internal/core"
	"github.com/wykserdex/wedra/internal/execution"
	"github.com/wykserdex/wedra/internal/gate"
	"github.com/wykserdex/wedra/internal/journal"
	"github.com/wykserdex/wedra/internal/pipeline"
)

func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request) {
	// v0.22: filesystem — источник правды (весь вар/runs), runs.db — только
	// дополняет (artifacts; старые индексы дособираются)
	var ids []string
	seen := map[string]bool{}
	entries, _ := os.ReadDir(s.RunsDir)
	for _, e := range entries {
		if e.IsDir() {
			ids = append(ids, e.Name())
			seen[e.Name()] = true
		}
	}
	var artsMap map[string][]string
	dbPath := filepath.Join(s.RunsDir, "runs.db")
	if _, err := os.Stat(dbPath); err == nil {
		store := journal.NewJsonStore(s.RunsDir, dbPath)
		artsMap = map[string][]string{}
		storeIds, _ := store.ListRuns()
		for _, id := range storeIds {
			if !seen[id] {
				ids = append(ids, id)
				seen[id] = true
			}
			artsMap[id], _ = store.ListArtifacts(id)
		}
	}
	sortStringsDesc(ids)
	// P2 F-03: стабильный потолок ответа — самые новые раны. Форма ответа
	// (массив) не меняется, факт обрезания отдаётся заголовком.
	truncated := len(ids) > maxRunsListed
	if truncated {
		ids = ids[:maxRunsListed]
	}
	var list []map[string]interface{}
	for _, id := range ids {
		m := s.cachedRunSummary(filepath.Join(s.RunsDir, id))
		if artsMap != nil {
			m["artifacts"] = artsMap[id]
		}
		list = append(list, m)
	}
	if list == nil {
		list = []map[string]interface{}{}
	}
	w.Header().Set("Content-Type", "application/json")
	if truncated {
		w.Header().Set("X-Wedra-Runs-Truncated", "true")
	}
	json.NewEncoder(w).Encode(list)
}

func sortStringsDesc(a []string) {
	sort.Slice(a, func(i, j int) bool { return a[i] > a[j] })
}

func (s *Server) handleRunDetail(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/runs/")
	if id == "" {
		http.Error(w, "missing id", 400)
		return
	}
	// v0.24: /api/runs/<id>/gate — статус/решение браузерного гейта
	if rest, ok := strings.CutSuffix(id, "/gate"); ok {
		s.handleRunGate(w, r, rest)
		return
	}
	// v0.9: /api/runs/<id>/cancel — отмена активного рана (нужна сессия)
	if rest, ok := strings.CutSuffix(id, "/cancel"); ok {
		s.handleRunCancel(w, r, rest)
		return
	}
	// v0.22: /api/runs/<id>/journal?since=N — live-хвост для GUI
	if rest, ok := strings.CutSuffix(id, "/journal"); ok {
		dir, err := journal.SafeRunDir(s.RunsDir, rest)
		if err != nil {
			http.Error(w, "invalid run id", 400)
			return
		}
		since := 0
		if v := r.URL.Query().Get("since"); v != "" {
			fmt.Sscanf(v, "%d", &since)
		}
		if since < 0 {
			since = 0
		}
		// P2 F-03: окно ответа ограничено; курсор next продолжает поллинг
		// с того места, где окно оборвалось (total — для клиентов без next).
		res, err := journal.NewReader(dir).EventsBounded(since, runJournalLimits(false))
		if err != nil {
			http.Error(w, err.Error(), 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"total": res.Total, "events": res.Events,
			"first": res.First, "next": res.Next, "truncated": res.Truncated,
		})
		return
	}
	dir, err := journal.SafeRunDir(s.RunsDir, id)
	if err != nil {
		http.Error(w, "invalid run id", 400)
		return
	}
	rd := journal.NewReader(dir)
	// P2 F-03: деталка отдаёт хвост журнала в пределах потолка ответа.
	// total/first/truncated описывают окно, поэтому курсор since для
	// поллинга остаётся точным.
	res, err := rd.EventsBounded(0, runJournalLimits(true))
	if err != nil {
		http.Error(w, err.Error(), 404)
		return
	}
	snap, _ := rd.ContextSnapshot()
	dbPath := filepath.Join(s.RunsDir, "runs.db")
	var arts []string
	if _, err := os.Stat(dbPath); err == nil {
		store := journal.NewJsonStore(s.RunsDir, dbPath)
		arts, _ = store.ListArtifacts(id)
	} else {
		store := journal.NewFilesystemStore(s.RunsDir)
		arts, _ = store.ListArtifacts(id)
	}
	summary := runSummary(dir)
	if !res.Truncated {
		// журнал поместился в окно целиком — summary считаем по уже
		// разобранным событиям, без второго прохода по файлу
		summary = summarizeRun(dir, res.Events)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id": id, "events": res.Events, "context": snap, "artifacts": arts,
		"status": summary["status"], "pipeline": summary["pipeline"],
		"total": res.Total, "first": res.First, "truncated": res.Truncated,
	})
}

// runJournalLimits — потолок ответа с журналом рана (P2 F-03). tail=true —
// окно последних событий (деталка рана и статус гейта), иначе окно от
// позиции since (live-хвост с курсором).
func runJournalLimits(tail bool) journal.ReadLimits {
	return journal.ReadLimits{
		MaxBytes:  maxRunResponseBytes,
		MaxEvents: maxRunResponseEvents,
		Tail:      tail,
	}
}

func (s *Server) resolvePipelineFile(name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("нужно имя или путь к pipeline")
	}
	path, err := secureContainedPath(s.PipelinesDir, name)
	if err != nil {
		return "", err
	}
	if fi, err := os.Stat(path); err == nil && !fi.IsDir() {
		return path, nil
	}
	return "", fmt.Errorf("pipeline %q не найден внутри %s", name, s.PipelinesDir)
}

// handleRunStart — v0.22: POST /api/run {file, yes} — in-process запуск.
// v0.24: yes=false — человеческий гейт в браузере: ран блокируется на
// gate-шаге, решение — POST /api/runs/<runID>/gate. ID рана известен заранее
// (в ответе 202) — карточка гейта не гадает имя.
//
// Сессию здесь проверять не нужно: Routes() требует её для всего под /api/*,
// кроме publicAPI. Проверка в обработчике была бы второй копией одного правила.
func (s *Server) handleRunStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "POST {file, yes}", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		File string `json:"file"`
		Yes  bool   `json:"yes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "json: "+err.Error(), 400)
		return
	}
	path, err := s.resolvePipelineFile(req.File)
	if err != nil {
		http.Error(w, "file: "+err.Error(), 400)
		return
	}
	pf, err := pipeline.LoadPipelineFile(path)
	if err != nil {
		http.Error(w, "pipeline: "+err.Error(), 400)
		return
	}
	// v0.9: валидация и secrets — синхронно, ДО 202. Раньше 202 выдавался
	// сразу, а отказ валидации печатался только в stdout сервера: клиент
	// (агент) получал run_id несуществующего рана и не видел причину.
	eng := s.runEngine()
	issues := pipeline.ValidateIssues(pf, eng)
	errs, _ := pipeline.SplitIssues(issues)
	if len(errs) > 0 {
		writeJSON(w, 400, map[string]interface{}{"ok": false, "issues": issues})
		return
	}
	var missing []string
	for _, k := range pf.Pipeline.Secrets {
		if os.Getenv(k) == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		writeJSON(w, 400, map[string]interface{}{
			"ok": false, "code": "secrets_missing", "missing": missing,
			"error": "secrets: не заданы переменные окружения: " + strings.Join(missing, ", "),
		})
		return
	}
	if !s.runMu.TryLock() {
		writeJSON(w, 409, map[string]string{"error": "уже идёт ран — дождись завершения", "code": "E_RUN_BUSY"})
		return
	}
	runID, err := execution.NewRunID(pf.Pipeline.Name)
	if err != nil {
		s.runMu.Unlock()
		writeJSON(w, 500, map[string]string{"error": "не удалось создать run_id: " + err.Error()})
		return
	}
	runCtx, cancel := context.WithCancel(context.Background())
	s.setCancel(runID, cancel)
	writeJSON(w, 202, map[string]interface{}{"status": "started", "file": req.File, "run": runID, "issues": issues})

	go func() {
		defer s.runMu.Unlock()
		defer s.clearGate(runID)
		defer s.clearCancel(runID)
		defer cancel()
		opts := core.RunOptions{Yes: req.Yes, Quiet: true, RunsDir: s.RunsDir, RunID: runID, Ctx: runCtx, Trusted: s.Trusted}
		if !req.Yes || pipelineNeedsHuman(pf) {
			opts.GateUI = func(st *pipeline.Step) gate.GateUI {
				ui := gate.NewChannelUI()
				s.setGate(runID, ui)
				// отмена рана закрывает ожидающий гейт (иначе висит вечно)
				go func() { <-runCtx.Done(); ui.Close() }()
				return ui
			}
		}
		stats, err := core.Run(pf, eng, opts)
		if err != nil {
			fmt.Printf("[gui] %s: %v\n", req.File, err)
		} else {
			fmt.Printf("[gui] %s: ok=%d aborted=%d\n", req.File, stats.OK, stats.Aborted)
		}
	}()
}

// pipelineNeedsHuman — есть гейт, который --yes не одобрит (approval: human /
// gates: human_only): такому рану нужен браузерный GateUI даже при yes=true.
func pipelineNeedsHuman(pf *pipeline.PipelineFile) bool {
	for i := range pf.Pipeline.Steps {
		st := &pf.Pipeline.Steps[i]
		if pipeline.IsBuiltin(st.Plugin) && pf.Pipeline.RequiresHuman(st) {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// handleRunCancel — v0.9: POST /api/runs/<id>/cancel. Сессия — в Routes().
func (s *Server) handleRunCancel(w http.ResponseWriter, r *http.Request, id string) {
	if _, err := journal.SafeRunDir(s.RunsDir, id); err != nil {
		http.Error(w, "invalid run id", 400)
		return
	}
	if r.Method != "POST" {
		http.Error(w, "POST", http.StatusMethodNotAllowed)
		return
	}
	cancel := s.cancelFor(id)
	if cancel == nil {
		writeJSON(w, 409, map[string]string{"error": "ран не активен (завершён или не найден)", "code": "E_RUN_DONE"})
		return
	}
	cancel()
	writeJSON(w, 202, map[string]string{"status": "cancelling", "run": id})
}

// handleRunGate — v0.24: GET/POST /api/runs/<id>/gate.
// GET — ожидающий ли гейт (из журнала: gate_wait без gate_decision после).
// POST {action, edits} — решение: POST в ChannelUI активного рана (409, если
// гейта нет, уже решён или ран завершён).
func (s *Server) handleRunGate(w http.ResponseWriter, r *http.Request, id string) {
	dir, err := journal.SafeRunDir(s.RunsDir, id)
	if err != nil {
		http.Error(w, "invalid run id", 400)
		return
	}
	switch r.Method {
	case "GET":
		// P2 F-03: состояние гейта определяется последними gate_wait/gate_
		// decision, поэтому хватает хвоста журнала в пределах потолка.
		res, err := journal.NewReader(dir).EventsBounded(0, runJournalLimits(true))
		if err != nil {
			http.Error(w, err.Error(), 404)
			return
		}
		pending := false
		var lastWait map[string]interface{}
		for _, e := range res.Events {
			switch e["type"] {
			case "gate_wait":
				pending, lastWait = true, e
			case "gate_decision":
				pending, lastWait = false, nil
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if pending {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"pending": true, "step": lastWait["step"],
				"form": lastWait["form"], "actions": lastWait["actions"],
			})
		} else {
			json.NewEncoder(w).Encode(map[string]interface{}{"pending": false})
		}
	case "POST":
		// сессия человека уже проверена в Routes() — и на GET, и на POST
		var req struct {
			Action string                 `json:"action"`
			Edits  map[string]interface{} `json:"edits"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "json: "+err.Error(), 400)
			return
		}
		ui := s.gateFor(id)
		if ui == nil {
			w.WriteHeader(409)
			json.NewEncoder(w).Encode(map[string]string{"error": "нет ожидающего гейта (ран не на гейте или завершён)"})
			return
		}
		// v0.27: ран завершён (run_end в журнале) — решения не принимает, даже
		// если clearGate ещё не успел сработать (окно между записью run_end и
		// dereg под нагрузкой давало 202 на мёртвый ран)
		if sum := s.cachedRunSummary(dir); sum["status"] != "running" {
			st, _ := sum["status"].(string)
			w.WriteHeader(409)
			json.NewEncoder(w).Encode(map[string]string{"error": "ран завершён (" + st + ") — решения не принимает"})
			return
		}
		// source/session проставляет сервер: клиент их не задаёт (json:"-")
		d := gate.Decision{Action: req.Action, Edits: req.Edits, Source: gate.SourceGUI, Session: s.sessionHash(r)}
		if !ui.SendDecision(d) {
			w.WriteHeader(409)
			json.NewEncoder(w).Encode(map[string]string{"error": "гейт уже решён"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(202)
		json.NewEncoder(w).Encode(map[string]string{"status": "queued"})
	default:
		http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleValidatePipeline(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "POST yaml", http.StatusMethodNotAllowed)
		return
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	pf, err := pipeline.LoadPipelineFileFromBytes(data)
	if err != nil {
		http.Error(w, "parse: "+err.Error(), 400)
		return
	}
	issues := pipeline.ValidateIssues(pf, s.Engine)
	errs, warns := pipeline.SplitIssues(issues)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"errors": errs, "warnings": warns, "ok": len(errs) == 0, "issues": issues})
}

func (s *Server) handlePlanPipeline(w http.ResponseWriter, r *http.Request) {
	// v0.12 fix: раньше был алиас на validate, никакого DAG
	// теперь строит DAG: узлы + рёбра по bind/form зависимостям
	if r.Method != "POST" {
		http.Error(w, "POST yaml", http.StatusMethodNotAllowed)
		return
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	pf, err := pipeline.LoadPipelineFileFromBytes(data)
	if err != nil {
		http.Error(w, "parse: "+err.Error(), 400)
		return
	}
	issues := pipeline.ValidateIssues(pf, s.Engine)
	errs, warns := pipeline.SplitIssues(issues)
	// строим DAG
	nodes := []map[string]interface{}{}
	edges := []map[string]string{}
	// pre-phase для steps.* foreach
	preSteps := map[string]bool{}
	if pf.Pipeline.Foreach != "" && strings.HasPrefix(pf.Pipeline.Foreach, "steps.") {
		parts := strings.Split(pf.Pipeline.Foreach, ".")
		if len(parts) >= 2 {
			srcID := parts[1]
			for _, st := range pf.Pipeline.Steps {
				preSteps[st.ID] = true
				if st.ID == srcID {
					break
				}
			}
		}
	}
	for _, st := range pf.Pipeline.Steps {
		phase := "foreach"
		if preSteps[st.ID] {
			phase = "pre"
		}
		if st.AfterForeach {
			phase = "post"
		}
		whenLabel := ""
		if st.When.IsSet() {
			whenLabel = st.When.Path
			if st.When.Op != "" && st.When.Op != "truthy" {
				whenLabel += " " + st.When.Op
				if st.When.Value != nil {
					whenLabel += " " + fmt.Sprintf("%v", st.When.Value)
				}
			}
		}
		nodes = append(nodes, map[string]interface{}{
			"id": st.ID, "plugin": st.Plugin, "phase": phase, "on_error": st.OnError,
			"bind": st.Bind, "after_foreach": st.AfterForeach,
			"when": whenLabel, "foreach": st.Foreach, "foreach_item": st.ForeachItem,
			"parallel_group": st.ParallelGroup,
		})
		// v0.22: рёбра от путей when/foreach (зависимость на данные)
		for _, p := range []string{st.Foreach, st.When.Path} {
			if strings.HasPrefix(p, "steps.") {
				parts := strings.Split(p, ".")
				if len(parts) >= 2 {
					edges = append(edges, map[string]string{"from": parts[1], "to": st.ID, "via": p})
				}
			}
		}
		// рёбра: из bind и form
		for _, from := range st.Bind {
			if strings.HasPrefix(from, "steps.") {
				parts := strings.Split(from, ".")
				if len(parts) >= 2 {
					edges = append(edges, map[string]string{"from": parts[1], "to": st.ID, "via": from})
				}
			}
		}
		for _, f := range st.Form {
			if strings.HasPrefix(f.Field, "steps.") {
				parts := strings.Split(f.Field, ".")
				if len(parts) >= 2 {
					edges = append(edges, map[string]string{"from": parts[1], "to": st.ID, "via": f.Field})
				}
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"pipeline": pf.Pipeline.Name,
		"foreach":  pf.Pipeline.Foreach,
		"errors":   errs,
		"warnings": warns,
		"issues":   issues,
		"ok":       len(errs) == 0,
		"dag": map[string]interface{}{
			"nodes": nodes,
			"edges": edges,
		},
	})
}
