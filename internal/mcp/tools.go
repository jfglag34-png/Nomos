package mcp

import (
	"github.com/wykserdex/wedra/internal/gate"
	"github.com/wykserdex/wedra/internal/execution"
	"math"
	"sort"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"github.com/wykserdex/wedra/internal/pipeline"
	"github.com/wykserdex/wedra/internal/plugin"
)

func (s *Server) toolListPlugins(args map[string]interface{}) (string, bool, *RPCError) {
	filter, _ := args["filter"].(string)
	seen := map[string]bool{}
	var list []map[string]interface{}
	for _, base := range append(append([]string{}, s.pluginsDirs...), filepath.Join(s.workDir, "plugins")) {
		for _, sub := range []string{base, filepath.Join(base, "official"), filepath.Join(base, "community"),
			filepath.Join(base, plugin.AgentPluginDir)} {
			if err := s.multi.checkPluginDir(sub); err != nil {
				continue
			}
			entries, _ := os.ReadDir(sub)
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				dir := filepath.Join(sub, e.Name())
				if err := s.multi.checkPluginDir(dir); err != nil {
					continue
				}
				raw, err := os.ReadFile(filepath.Join(dir, "plugin.yaml"))
				if err != nil {
					continue
				}
				m, err := parseManifestBytes(raw)
				if err != nil {
					continue
				}
				m.Dir = dir
				if m.ID == "" {
					m.ID = e.Name()
				}
				if seen[m.ID] {
					continue
				}
				if filter != "" && !strings.Contains(m.ID, filter) && !strings.Contains(m.Description, filter) {
					continue
				}
				seen[m.ID] = true
				list = append(list, map[string]interface{}{
					"id": m.ID, "version": m.Version, "description": m.Description,
					"input": m.Input, "output": m.Output, "permissions": permissionsJSON(m),
				})
			}
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i]["id"].(string) < list[j]["id"].(string) })
	return toJSON(map[string]interface{}{"plugins": list}), false, nil
}

func (s *Server) toolDescribePlugin(args map[string]interface{}) (string, bool, *RPCError) {
	id, _ := args["id"].(string)
	if id == "" {
		return "", false, rpcErr("", "нужен id")
	}
	if err := s.checkPluginRef(id); err != nil {
		return "", false, rpcErr("E_PLUGIN_OUTSIDE_ROOT", err.Error())
	}
	m, err := s.multi.LoadManifest(id)
	if err != nil {
		return "", false, rpcErr("", err.Error())
	}
	// secrets — только имена (значения никогда не отдаём)
	return toJSON(map[string]interface{}{
		"id": m.ID, "version": m.Version, "description": m.Description, "author": m.Author,
		"runtime": m.Runtime, "input": m.Input, "output": m.Output,
		"permissions": permissionsJSON(m),
	}), false, nil
}

// policyIssueCodes — отказы политики MCP, которые для validate_pipeline являются
// нормальным результатом проверки, а не поломкой вызова. Раньше они приходили
// JSON-RPC ошибкой, хотя контракт инструмента прямо обещает «ok:false —
// нормальный результат», и агент получал исключение вместо разбора issues[].
//
// Код в issues[] — тот же E_-код, что в protocol/v0.2/ERRORS.md и у
// ValidateIssues: одна причина — один код, независимо от пути. Раньше здесь
// лежали строчные network_denied/..., и агент, читавший issues[].code по
// контракту, получал код вне контракта.
var policyIssueCodes = map[string]string{
	"E_NETWORK_DENIED":        "E_NETWORK_DENIED",
	"E_PLUGIN_OUTSIDE_ROOT":   "E_PLUGIN_OUTSIDE_ROOT",
	"E_FILE_REF_OUTSIDE_ROOT": "E_FILE_REF_OUTSIDE_ROOT",
	// E_UNSUPPORTED_PROTOCOL был здесь ключом, но нигде в проекте не
	// эмитился: мёртвая запись обещала код, которого агент никогда не видел.
	// Убрана; вернётся вместе с реальной проверкой версии протокола.
}

// policyIssue — превратить отказ политики в Issue. Исходный код политики
// сохраняется в начале сообщения, чтобы вызывающий мог отличить отказ от обычной
// ошибки разбора, а короткий код попадает в issues[].code.
func policyIssue(err error) (pipeline.Issue, bool) {
	msg := err.Error()
	for prefix, code := range policyIssueCodes {
		if strings.HasPrefix(msg, prefix) {
			return pipeline.Issue{
				Severity: pipeline.SeverityError,
				Code:     code,
				Message:  prefix + ": " + strings.TrimSpace(strings.TrimPrefix(msg, prefix+":")),
			}, true
		}
	}
	return pipeline.Issue{}, false
}

func (s *Server) toolValidate(args map[string]interface{}) (string, bool, *RPCError) {
	_, issues, err := s.validateArgs(args)
	if err != nil {
		// Отказ политики — результат проверки: ok:false плюс issue с кодом.
		if issue, ok := policyIssue(err); ok {
			return toJSON(map[string]interface{}{
				"ok":     false,
				"issues": issuesForJSON(append(issues, issue)),
			}), false, nil
		}
		return "", false, rpcErr("", err.Error())
	}
	errs, _ := pipeline.SplitIssues(issues)
	return toJSON(map[string]interface{}{"ok": len(errs) == 0, "issues": issuesForJSON(issues)}), false, nil
}

func (s *Server) toolPlan(args map[string]interface{}) (string, bool, *RPCError) {
	pf, issues, err := s.validateArgs(args)
	if err != nil {
		if strings.HasPrefix(err.Error(), "E_PLUGIN_OUTSIDE_ROOT") {
			return "", false, rpcErr("E_PLUGIN_OUTSIDE_ROOT", err.Error())
		}
		return "", false, rpcErr("", err.Error())
	}
	plan, err := pipeline.PlanPipeline(pf, s.multi)
	if err != nil {
		return "", false, rpcErr("", err.Error())
	}
	errs, _ := pipeline.SplitIssues(issues)
	return toJSON(map[string]interface{}{
		"ok": len(errs) == 0, "issues": issuesForJSON(issues),
		"pipeline": pf.Pipeline.Name, "foreach": pf.Pipeline.Foreach,
		"dag": plan.DAG,
	}), false, nil
}

func (s *Server) toolRun(call *inflightCall, args map[string]interface{}) (string, bool, *RPCError) {
	pf, issues, err := s.validateArgs(args)
	if err != nil {
		if strings.HasPrefix(err.Error(), "E_PLUGIN_OUTSIDE_ROOT") {
			return "", false, rpcErr("E_PLUGIN_OUTSIDE_ROOT", err.Error())
		}
		return "", false, rpcErr("", err.Error())
	}
	errs, _ := pipeline.SplitIssues(issues)
	if len(errs) > 0 {
		return toJSON(map[string]interface{}{"ok": false, "issues": issuesForJSON(issues)}), false, nil
	}
	waitSec := 0.0
	if w, ok := args["wait_seconds"].(float64); ok {
		// Ноль означает «не ждать» и потому разрешён, а timeout_seconds
		// (см. ниже) нужен строго положительным: там ноль — вырожденный
		// мгновенный таймаут. Формулировки различаются намеренно.
		if math.IsNaN(w) || math.IsInf(w, 0) || w < 0 || w > maxWaitSeconds {
			return "", false, rpcErr("", fmt.Sprintf("wait_seconds должен быть от 0 до %d включительно", maxWaitSeconds))
		}
		waitSec = w
	}
	// secrets — только имена, значений нет; missing → понятная ошибка
	var missing []string
	for _, k := range pf.Pipeline.Secrets {
		if os.Getenv(k) == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return "", false, rpcErr("secrets_missing", "secrets: нет env "+strings.Join(missing, ", ")+" (значения в YAML не живут)")
	}
	// H2: опасный шаг без гейта перед ним. Проверяются capabilities шага
	// (permissions плагина), а не «есть ли гейт вообще»: гейт ПОСЛЕ
	// опасного шага не защищает — человек увидит результат, а не намерение.
	//
	// Обход требования (--allow-unapproved-runs) не тихий: след пишется в
	// отдельный append-only журнал, и если след не записался — отказ.
	if req := pipeline.EvaluateGateApproval(pf, s.multi); req.Required {
		refusal := gateRefusal(req)
		if !s.allowUngatedRuns {
			return "", false, refusal
		}
		if err := s.auditGateBypass(pf.Pipeline.Name, req); err != nil {
			return "", false, &RPCError{Code: -32000,
				Message: refusal.Message + "; операторский обход не записан в аудит (" + err.Error() +
					"): неоплаченный запуск недопустим, отказ до старта",
				Data: map[string]string{"code": pipeline.E_GATE_REQUIRED, "step": req.Step}}
		}
		s.logf("! обход требования гейта (--allow-unapproved-runs): шаг %s (%s) с правами «%s» пойдёт без одобрения человека; след в %s",
			req.Step, req.Plugin, req.Why, filepath.Join(s.runsDir, gateBypassAuditFile))
	}
	// IsHumanGate, а не IsBuiltin: core/text_stats — встроенный модуль, но не
	// гейт. Раньше он считался гейтом, и пайплайн только с ним отклонялся по
	// E_NO_HUMAN_CHANNEL, хотя одобрять в нём нечего.
	hasGate := false
	for i := range pf.Pipeline.Steps {
		if pipeline.IsHumanGate(pf.Pipeline.Steps[i].Plugin) {
			hasGate = true
			break
		}
	}
	if hasGate && s.human == nil && !s.elicitGateAvailable() {
		return "", false, &RPCError{Code: -32000,
			Message: "в пайплайне есть human_gate, а канала к человеку нет: нет консоли гейтов " +
				"(wedra mcp запущен с --no-gui) и нет elicitation (нужны --gate-elicitation и клиент, " +
				"который объявил поддержку elicitation) — одобрить гейт некому",
			Data: map[string]string{"code": "E_NO_HUMAN_CHANNEL"}}
	}
	runID, err := execution.NewRunID(pf.Pipeline.Name)
	if err != nil {
		return "", false, rpcErr("", "не удалось создать run_id: "+err.Error())
	}
	s.mu.Lock()
	if s.running {
		cur := s.currentRunID
		s.mu.Unlock()
		return "", false, &RPCError{Code: -32000, Message: "уже идёт ран " + cur, Data: map[string]string{"code": "E_RUN_BUSY", "run_id": cur}}
	}
	s.running = true
	s.currentRunID = runID
	s.pruneRunsLocked()
	st := &runState{id: runID, dir: filepath.Join(s.runsDir, runID), done: make(chan struct{}), status: "running"}
	s.runs[runID] = st
	s.mu.Unlock()

	// Ран привязан к запросу: notifications/cancelled с этим id отменит
	// именно его, а не «какой-нибудь ран».
	s.bindRun(call, runID)
	s.notifyProgress(call, 0, waitSec, "ран "+runID+" запущен")

	go func() {
		defer close(st.done)
		defer func() {
			s.mu.Lock()
			s.running = false
			s.currentRunID = ""
			delete(s.cancels, runID)
			// Связь «запрос → ран» живёт ровно столько, сколько жив ран:
			// после его конца отменять нечего, и мусор в карте копился бы до
			// перезапуска сервера.
			for key, id := range s.runRequests {
				if id == runID {
					delete(s.runRequests, key)
				}
			}
			s.mu.Unlock()
		}()
		// Фаза 4.2: Quiet + ChannelUI (StdinUI в MCP запрещён), NoAutoApprove всегда.
		// Фаза 5: ctx для cancel_run.
		runCtx, cancel := context.WithCancel(context.Background())
		s.mu.Lock()
		s.cancels[runID] = cancel
		// Отмена могла прийти до этой строки: cancel() рождается на горутине
		// рана, а нотификация клиента приходит в любой момент. Намерение
		// записано в st.cancelRequested — исполняем его здесь.
		if st.cancelRequested {
			cancel()
		}
		s.mu.Unlock()
		defer cancel()
		ui := gate.NewChannelUI()
		// отмена закрывает гейт (иначе waiting_human висит вечно)
		go func() {
			<-runCtx.Done()
			ui.Close()
		}()
		if s.human != nil {
			// человек может отменить ран из консоли; гейт регистрируется
			// лениво — при первом gate-шаге (GateUI-фабрика ниже)
			s.human.AttachRun(runID, nil, cancel)
			defer s.human.DetachRun(runID)
		}
		gateShown := false
		// elicit — общий на ран: если elicitation не сработал на одном шаге,
		// дальше гейты идут в консоль, а не мигают двумя интерфейсами.
		elicit := &elicitRunner{}
		opts := execution.RunOptions{
			Yes: false, Quiet: true, RunsDir: s.runsDir, RunID: runID,
			NoAutoApprove: true, MCPMode: true, Ctx: runCtx,
			// Политика доверия обязана дойти до рана. Без неё runWithStore
			// собирает нулевую, то есть «не доверен никто», и ЛЮБОЙ плагин
			// падает с sandbox_unavailable — включая те, что оператор явно
			// внёс в allow-list. То же решение о доверии, что и у exec_plugin
			// (строка с WithTrustPolicy ниже), обязано быть и у раннера.
			Trusted: s.trust.Trusted,
		}
		if s.human != nil || s.elicitGateAvailable() {
			opts.GateUI = func(st *pipeline.Step) gate.GateUI {
				if s.elicitGateAvailable() {
					if s.human != nil {
						// Консоль остаётся фолбэком, и ран в ней уже
						// зарегистрирован (AttachRun выше) — вместе с
						// возможностью отменить ран из браузера.
						elicit.fallback = ui
					}
					return &elicitGateUI{srv: s, call: call, runID: runID, step: st,
						runCtx: runCtx, runner: elicit}
				}
				s.human.AttachRun(runID, ui, nil)
				if !gateShown {
					gateShown = true
					s.human.GateWaiting(runID)
				}
				return ui
			}
		}
		stats, err := execution.Run(pf, s.multi, opts)
		s.mu.Lock()
		defer s.mu.Unlock()
		st.aborted = stats.Aborted
		st.okItems = stats.OK
		if err != nil {
			st.code = execution.ErrorCode(err)
			if st.code == "cancelled" {
				st.status = "cancelled"
			} else {
				st.status = "failed"
			}
			st.errMsg = err.Error()
			return
		}
		// Прерванные шаги — тоже провал. Раньше здесь ставился безусловный "done",
		// и агент получал "done" для рана, где все шаги упали: err у такой рана nil.
		// Семантика совпадает с CLI (runExitCode).
		if !runSucceeded(stats.Aborted, stats.OK, pf.Pipeline.Foreach) {
			st.status = "failed"
			st.errMsg = fmt.Sprintf("ран неуспешен: прервано шагов %d, выполнено %d", stats.Aborted, stats.OK)
			return
		}
		st.status = "done"
	}()

	// wait_seconds: подождать завершения/гейта
	status := "running"
	if waitSec > 0 {
		start := time.Now()
		deadline := start.Add(time.Duration(waitSec * float64(time.Second)))
		lastTick := time.Now()
		for time.Now().Before(deadline) {
			time.Sleep(200 * time.Millisecond)
			status = s.runStatus(runID)
			// Прогресс раз в секунду, а не каждые 200 мс: клиенты сбрасывают
			// таймер по факту нотификации, но спамить ими незачем.
			if time.Since(lastTick) >= time.Second {
				lastTick = time.Now()
				s.notifyProgress(call, time.Since(start).Seconds(), waitSec, "статус: "+status)
			}
			if status == "done" || status == "failed" || status == "cancelled" || status == "waiting_human" {
				break
			}
		}
	} else {
		time.Sleep(300 * time.Millisecond)
		status = s.runStatus(runID)
	}
	s.notifyProgress(call, waitSec, waitSec, "статус: "+status)
	return toJSON(map[string]interface{}{"run_id": runID, "status": status}), false, nil
}

// runSucceeded — единая семантика «ран успешен» для MCP. Совпадает с CLI
// (runExitCode): прерванные шаги делают ран неуспешным, кроме foreach-пайплайнов,
// где abort — штатный способ отфильтровать элементы. Ран, в котором не отработал
// ни один шаг, "done" не бывает: агент обязан это увидеть в статусе.
//
// Уточнение про foreach: пустой батч (ни одного элемента) остаётся done —
// «нечего обрабатывать» не ошибка, как и батч целиком из битых элементов
// (per-item итоги в журнале). Требование ok>0 действует только вне foreach.
func runSucceeded(aborted, ok int, foreach string) bool {
	if foreach == "" && ok == 0 {
		return false
	}
	return !(aborted > 0 && foreach == "")
}

// gateRefusal — отказ E_GATE_REQUIRED. Текст отвечает на два вопроса агента:
// ЧТО опасно (шаг, плагин, конкретные права) и КАК починить (поставить
// core/human_gate перед этим шагом). Гейт после опасного шага называется
// отдельно: он выглядит как решение, но ничего не одобряет.
func gateRefusal(req pipeline.GateRequirement) *RPCError {
	msg := "шаг " + req.Step + " (плагин " + req.Plugin + ") объявил права: " + req.Why +
		" — такое требует одобрения человека, а human_gate перед ним нет"
	if req.GateAfter {
		msg += "; гейт " + req.GateID + " стоит ПОСЛЕ этого шага и одобрения не даёт"
	}
	if req.GateIneffective != "" {
		msg += "; гейт " + req.GateID + " перед ним одобрения не гарантирует: " + req.GateIneffective
	}
	msg += ". Добавьте шаг core/human_gate (actions: [accept, reject]) перед опасным шагом" +
		" — для локального доверенного использования оператор может запустить сервер с --allow-unapproved-runs"
	return &RPCError{Code: -32000, Message: msg,
		Data: map[string]string{"code": pipeline.E_GATE_REQUIRED, "step": req.Step, "plugin": req.Plugin}}
}

// gateBypassAuditFile — append-only журнал обходов требования одобрения.
// Отдельный от agent-exec.jsonl: там решения о запуске КОДА плагином, здесь —
// решение о запуске РАНА без человека. Оба журнала append-only и оба fsync-ятся
// до того, как действие состоялось.
const gateBypassAuditFile = "gate-bypass.jsonl"

// Событие журнала обходов: один на каждый обойдённый ран.
const gateBypassEvent = "agent_run_gate_bypassed"

// auditGateBypass — зафиксировать операторский обход. Ошибка означает отказ:
// обход без следа — это ровно тот тихий режим, ради которого проверка и
// добавлена. Права опасного шага пишутся списком, а не флагом, чтобы читатель
// журнала видел, ЧТО именно пошло без одобрения.
func (s *Server) auditGateBypass(pipelineName string, req pipeline.GateRequirement) error {
	return s.writeAuditFile(gateBypassAuditFile, map[string]interface{}{
		"event":            gateBypassEvent,
		"source":           "operator_flag",
		"flag":             "--allow-unapproved-runs",
		"pipeline":         pipelineName,
		"step":             req.Step,
		"plugin":           req.Plugin,
		"capabilities":     req.Why,
		"gate_after":       req.GateAfter,
		"gate_ineffective": req.GateIneffective,
	})
}

func (s *Server) pruneRunsLocked() {
	if len(s.runs) < maxRetainedRuns {
		return
	}
	for id, st := range s.runs {
		select {
		case <-st.done:
			delete(s.runs, id)
		default:
		}
		if len(s.runs) < maxRetainedRuns {
			return
		}
	}
}

