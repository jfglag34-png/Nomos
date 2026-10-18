package mcp

import (
	"strings"
	"encoding/hex"
	"crypto/rand"
	"strconv"
	"net"
	"math"
	"github.com/wykserdex/wedra/internal/common"
	"fmt"
	"os"
	"path/filepath"
	"time"
	"context"
	"encoding/json"
	"github.com/wykserdex/wedra/internal/journal"
	"github.com/wykserdex/wedra/internal/pipeline"
	"github.com/wykserdex/wedra/internal/plugin"
)

func (s *Server) runStatus(runID string) string {
	s.mu.Lock()
	st, ok := s.runs[runID]
	s.mu.Unlock()
	if ok {
		select {
		case <-st.done:
			s.mu.Lock()
			defer s.mu.Unlock()
			return st.status
		default:
		}
	}
	// Ран мог быть запущен другим процессом (или до перезапуска сервера), поэтому
	// журнал — источник истины, а не память процесса. Раньше здесь был ранний
	// return "unknown", из-за которого get_run отдавал "unknown" вместе с
	// полным списком событий.
	dir, err := journal.SafeRunDir(s.runsDir, runID)
	if err != nil {
		return "unknown"
	}
	// Журнала нет — такого рана не существует. Без этой проверки нечитаемый
	// каталог выглядел бы как "running", и get_run на выдуманный run_id сообщал
	// бы, что ран идёт.
	if _, err := os.Stat(filepath.Join(dir, "journal.jsonl")); err != nil {
		return "unknown"
	}
	// P2 F-03: статус определяется последними событиями журнала (run_end/
	// run_failed/последний gate_wait), поэтому хватает хвоста в пределах
	// потолка, а не разбора всего журнала в память.
	res, err := journal.NewReader(dir).EventsBounded(0, runJournalLimits(true))
	if err != nil {
		return "running"
	}
	pending := false
	for _, e := range res.Events {
		switch e["type"] {
		case "gate_wait":
			pending = true
		case "gate_decision":
			pending = false
		case "run_end":
			// abort в run_end означает провал: агент не должен увидеть "done"
			// у рана, где прерваны шаги. Для foreach это штатная фильтрация, но
			// из журнала её не видно — склоняемся к осторожности и не врём про успех.
			status := "done"
			if n, _ := e["aborted"].(float64); n > 0 {
				status = "failed"
			}
			s.recordRunStatus(runID, status, "")
			return status
		case "run_cancelled":
			s.recordRunStatus(runID, "cancelled", "")
			return "cancelled"
		case "run_failed":
			msg, _ := e["error"].(string)
			s.recordRunStatus(runID, "failed", msg)
			return "failed"
		}
	}
	if pending {
		return "waiting_human"
	}
	return "running"
}

// recordRunStatus — запомнить статус рана, полученный из журнала. Запись может
// отсутствовать (ран запущен другим процессом), поэтому создаём её, иначе
// обращение к st было бы разыменованием nil.
func (s *Server) recordRunStatus(runID, status, errMsg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.runs[runID]
	if !ok {
		st = &runState{id: runID, done: make(chan struct{})}
		s.runs[runID] = st
	}
	st.status = status
	if errMsg != "" {
		st.errMsg = errMsg
	}
}

// runJournalLimits — потолок окна журнала для инструментов. tail=true —
// последние события (статус рана), иначе окно от позиции since (get_run).
func runJournalLimits(tail bool) journal.ReadLimits {
	return journal.ReadLimits{
		MaxBytes:  maxRunJournalBytes,
		MaxEvents: maxRunJournalEvents,
		Tail:      tail,
	}
}

func truncateJSON(v interface{}, limit int) interface{} {
	b, _ := json.Marshal(v)
	if len(b) <= limit {
		return v
	}
	return common.Truncate(string(b), limit) + "(обрезано до 20KB)"
}

func (s *Server) toolGetRun(args map[string]interface{}) (string, bool, *RPCError) {
	runID, _ := args["run_id"].(string)
	if runID == "" {
		return "", false, rpcErr("", "нужен run_id")
	}
	dir, err := journal.SafeRunDir(s.runsDir, runID)
	if err != nil {
		return "", false, rpcErr("", "небезопасный run_id")
	}
	s.mu.Lock()
	_, ok := s.runs[runID]
	s.mu.Unlock()
	if !ok {
		// может, ран из прошлой сессии процесса? проверяем папку
		if _, err := os.Stat(dir); err != nil {
			return "", false, rpcErr("", "ран не найден: "+runID)
		}
	}
	since := 0
	if f, ok := args["since"].(float64); ok {
		since = int(f)
	}
	rd := journal.NewReader(dir)
	// P2 F-03: окно событий ограничено; total/first/next/truncated описывают
	// границы окна, поэтому догрузка по since работает как раньше.
	res, err := rd.EventsBounded(since, runJournalLimits(false))
	if err != nil {
		return "", false, rpcErr("", err.Error())
	}
	status := s.runStatus(runID)
	out := map[string]interface{}{
		"run_id": runID, "status": status, "total": res.Total, "events": res.Events,
		"first": res.First, "next": res.Next, "truncated": res.Truncated,
	}
	s.mu.Lock()
	if rs, ok := s.runs[runID]; ok {
		select {
		case <-rs.done:
			out["stats"] = map[string]int{"ok": rs.okItems, "aborted": rs.aborted}
			if rs.errMsg != "" {
				out["error"] = rs.errMsg
				out["code"] = rs.code
			}
		default:
		}
	}
	s.mu.Unlock()
	if status == "waiting_human" {
		// gate_wait ищется по хвосту журнала, а не по окну от since: иначе
		// догружающий агент (since>0) потерял бы подсказку о гейте
		var lastWait map[string]interface{}
		if tail, tailErr := journal.NewReader(dir).EventsBounded(0, runJournalLimits(true)); tailErr == nil {
			for _, e := range tail.Events {
				if e["type"] == "gate_wait" {
					lastWait = e
				}
			}
		}
		if lastWait != nil {
			out["pending_gate"] = map[string]interface{}{
				"step": lastWait["step"], "form": lastWait["form"], "actions": lastWait["actions"],
			}
			hint := "попросите пользователя одобрить шаг в окне wedra (у агента нет инструмента одобрения)"
			if s.human != nil {
				if u := s.human.PublicURL(runID); u != "" {
					hint += "; консоль: " + u + " (вкладка уже открыта у человека)"
				}
			}
			out["hint"] = hint
		}
	}
	if snap, err := rd.ContextSnapshot(); err == nil && snap != nil {
		out["outputs"] = truncateJSON(snap, 20*1024)
	}
	// secrets никогда не попадают: в снапшоте их нет (только input/steps)
	return toJSON(out), false, nil
}

func (s *Server) toolCancel(args map[string]interface{}) (string, bool, *RPCError) {
	runID, _ := args["run_id"].(string)
	if runID == "" {
		return "", false, rpcErr("", "нужен run_id")
	}
	if _, err := journal.SafeRunDir(s.runsDir, runID); err != nil {
		return "", false, rpcErr("", "небезопасный run_id")
	}
	// Отмена одна на два входа: cancel_run (инструмент) и
	// notifications/cancelled (нотификация клиента). Разные реализации
	// разошлись бы на первой же правке гонки с регистрацией cancel().
	if rpcErr := s.cancelRun(runID); rpcErr != nil {
		return "", false, rpcErr
	}
	return toJSON(map[string]interface{}{"run_id": runID, "status": "cancelling"}), false, nil
}

// MaxConcurrentAgentExec — сколько запусков плагина агентом может идти
// одновременно. exec_plugin выполняется СИНХРОННО и держит процесс до
// maxAgentExecTimeout (300 с), поэтому без потолка агент занимал сервер
// неограниченным числом вызовов. Потолок меньше MaxParallelWidth=32 и
// MaxConcurrentBranches=32: там ограничение на число ветвей пайплайна, здесь —
// на число ОДНОВРЕМЕННЫХ ВНЕШНИХ ПРОЦЕССОВ, живущих до пяти минут.
const MaxConcurrentAgentExec = 4

// maxAgentExecTimeout — потолок длительности одного вызова, секунды.
const maxAgentExecTimeout = 300

// acquireExecSlot — занять слот или отказать. Отказ, а не ожидание в очереди:
// иначе вызовы копятся и MCP-сервер перестаёт отвечать на всё подряд.
func (s *Server) acquireExecSlot() *RPCError {
	if s.execSem == nil {
		return rpcErr("E_AGENT_EXEC_BUSY", "внутренняя ошибка: слоты запусков не инициализированы")
	}
	select {
	case s.execSem <- struct{}{}:
		return nil
	default:
		return rpcErr("E_AGENT_EXEC_BUSY",
			fmt.Sprintf("достигнут предел параллельных запусков (%d): дождитесь окончания текущих", MaxConcurrentAgentExec))
	}
}

func (s *Server) releaseExecSlot() {
	if s.execSem == nil {
		return
	}
	select {
	case <-s.execSem:
	default:
	}
}

func (s *Server) toolExecPlugin(call *inflightCall, args map[string]interface{}) (string, bool, *RPCError) {
	// Проверка разрешения ПЕРВОЙ, до разбора аргументов и до загрузки
	// манифеста: без явного согласия ядра агент не запускает код вообще.
	// Раньше этой проверки не было, и exec_plugin был доступен всегда.
	if !s.trust.AgentCanExec {
		return "", false, rpcErr("E_AGENT_EXEC_DENIED",
			"запуск плагинов агентом запрещён политикой (нужен флаг --allow-agent-exec)")
	}
	// Слот берём до любой работы с файлами: иначе отказ по пределу приходил бы
	// уже после чтения манифеста, и мы гоняли бы диск ради отказа.
	if busy := s.acquireExecSlot(); busy != nil {
		return "", false, busy
	}
	defer s.releaseExecSlot()
	pluginRef, _ := args["plugin"].(string)
	if pluginRef == "" {
		return "", false, rpcErr("", "нужен plugin")
	}
	if err := s.checkPluginRef(pluginRef); err != nil {
		return "", false, rpcErr("E_PLUGIN_OUTSIDE_ROOT", err.Error())
	}
	m, err := s.multi.LoadManifest(pluginRef)
	if err != nil {
		return "", false, rpcErr("", err.Error())
	}
	if err := s.checkPluginNetwork(m); err != nil {
		return "", false, rpcErr("E_NETWORK_DENIED", err.Error())
	}
	// Плагин, написанный агентом, всегда untrusted по построению. Здесь это
	// превращается в требование песочницы, а не в пометку.
	if plugin.IsAgentWrittenPlugin(m) && !s.trust.AllowUntrusted {
		return "", false, rpcErr("E_AGENT_PLUGIN_UNTRUSTED",
			"плагин написан агентом и не помечен как доверенный: передайте --allow-untrusted-plugins")
	}
	// Строгое правило, мимо которого агент не пройдёт.
	//
	// exec_plugin идёт вне пайплайна, поэтому human_gate к нему приделать
	// технически нельзя: ждать некого, состояния ожидания у инструмента нет.
	// Значит запрещаем ровно то, что гейт защищал бы, — плагины с объявленными
	// правами. Обойти это агент не может по построению: он говорит с ядром
	// только через MCP, а решение принимает код.
	//
	// Сеть уже закрыта строкой выше (E_NETWORK_DENIED), здесь остаются запись
	// на диск, чтение секретов и непрочитанный манифест (fail-closed: незнание
	// прав — не доказательство их отсутствия).
	//
	// Проверка стоит ПОСЛЕ проверки доверия, а не до неё: для плагина агента
	// «недоверен» — причина фундаментальнее, и подсказка «запусти через
	// run_pipeline с гейтом» была бы неверной на хосте без изолятора, где
	// такой плагин не запустится вообще.
	//
	// Чего правило НЕ делает: оно не измеряет поведение кода. Плагин, заявивший
	// filesystem: none, всё равно может писать правами пользователя. Против
	// врущего манифеста работает другое — изолятор для недоверенных.
	if caps := pipeline.ManifestCapabilities(m); caps.Dangerous() {
		return "", false, rpcErr("E_AGENT_EXEC_CAPABILITIES",
			"exec_plugin не запускает плагины с объявленными правами ("+caps.Why()+
				"): инструмент идёт мимо пайплайна, а значит мимо human_gate. "+
				"Такой плагин запускается только через run_pipeline с core/human_gate перед ним")
	}
	inputJSON := []byte("{}")
	if input, ok := args["input"].(map[string]interface{}); ok {
		if raw, err := json.Marshal(input); err == nil {
			inputJSON = raw
		}
	}
	timeout := 60.0
	if raw, present := args["timeout_seconds"]; present {
		t, ok := raw.(float64)
		// Как wait_seconds выше: неверное значение — явная ошибка, а не тихое
		// значение по умолчанию. Раньше timeout_seconds=1000, 0, -5, строка или
		// NaN молча превращались в 60, и агент думал, что получил запрошенное.
		if !ok || math.IsNaN(t) || math.IsInf(t, 0) || t <= 0 || t > maxAgentExecTimeout {
			return "", false, rpcErr("", fmt.Sprintf("timeout_seconds должен быть больше 0 и не больше %d секунд", maxAgentExecTimeout))
		}
		timeout = t
	}
	extraEnv := []string{"WEDRA_NETWORK=deny"}
	// Именно ExecWithEnvCtx, а не ExecWithEnv: политика обязана дойти до
	// enforceTrust. ExecWithEnv подставляет context.Background(), то есть
	// нулевую политику, и флаги --allow/--deny-untrusted-plugins не влияли
	// на запуск из MCP.
	// exec_plugin синхронный и может держать процесс до 300 секунд, поэтому
	// вызов отменяем: notifications/cancelled обрывает запуск, как и ран.
	execCtx, cancelExec := context.WithCancel(plugin.WithTrustPolicy(context.Background(), s.trust))
	defer cancelExec()
	s.bindExec(call, cancelExec)
	ctx := execCtx
	// Намерение пишется ДО запуска. Решение агента о запуске кода — событие
	// доверия, и оно обязано быть в журнале даже тогда, когда процесс
	// умирает на середине: иначе выполнение внешнего кода остаётся вообще
	// без следа. Ошибка записи здесь означает настоящий отказ — плагин не
	// запускается.
	execID, auditErr := s.auditAgentExecIntent(pluginRef, m, timeout)
	if auditErr != nil {
		return "", false, rpcErr("E_AGENT_EXEC_AUDIT", auditErr.Error())
	}
	started := time.Now()
	// Прогресс до запуска: единственная точка, где мы ещё можем что-то
	// сказать — сам ExecWithEnvCtx блокирует вызов до конца процесса.
	s.notifyProgress(call, 0, timeout, "плагин "+pluginRef+" запущен (таймаут "+strconv.FormatFloat(timeout, 'f', 0, 64)+" с)")
	// Дробные секунды — точно, а не усечением: time.Duration(timeout) отрезал
	// бы 1.5с до 1с, а 0.5с — до нуля, то есть до мгновенного таймаута.
	res := plugin.ExecWithEnvCtx(ctx, m, inputJSON, time.Duration(timeout*float64(time.Second)), extraEnv)
	// Исход дописывается после. Его потеря не отменяет уже состоявшееся
	// исполнение, поэтому здесь только отказ вернуть результат: в журнале
	// всё равно остаётся намерение с тем же exec_id.
	if auditErr := s.auditAgentExecResult(execID, pluginRef, m, res, time.Since(started)); auditErr != nil {
		return "", false, rpcErr("E_AGENT_EXEC_AUDIT", auditErr.Error())
	}
	if res.Platform {
		return "", false, &RPCError{Code: -32000, Message: res.ErrMsg, Data: map[string]string{"code": res.ErrCode}}
	}
	if res.ExitCode != 0 {
		return "", false, &RPCError{Code: -32000, Message: res.ErrMsg, Data: map[string]string{"code": res.ErrCode}}
	}
	out, dropped, err := plugin.EnforceOutput(m, res.Output)
	if err != nil {
		return "", false, &RPCError{Code: -32000, Message: err.Error(), Data: map[string]string{"code": "contract_output"}}
	}
	if len(dropped) > 0 {
		return toJSON(map[string]interface{}{"output": out, "dropped": dropped}), false, nil
	}
	return toJSON(map[string]interface{}{"output": out}), false, nil
}

// agentExecAuditFile — append-only журнал запусков плагинов агентом. Отдельный
// от журналов ранов: exec_plugin не создаёт ран, но это решение о доверии, и
// оно должно читаться так же надёжно, как gate_decision.
const agentExecAuditFile = "agent-exec.jsonl"

// События журнала. Их два на один запуск, и порядок обязателен: сначала
// намерение, потом результат.
const (
	// agentExecEventIntent — запись ДО запуска плагина. Именно она даёт
	// настоящую гарантию: не записалось намерение — плагин не запускается.
	agentExecEventIntent = "agent_plugin_exec_intent"
	// agentExecEventResult — запись ПОСЛЕ запуска: чем закончилось.
	agentExecEventResult = "agent_plugin_exec"
)

// newExecID связывает пару записей одного запуска. Читатель журнала видит
// намерение без результата (значит процесс упал или был убит) и результат без
// намерения (быть не должно — это признак дырки в журнале).
func newExecID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("сгенерировать id запуска: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// writeAuditRecord дописывает одну строку в журнал запусков плагинов агентом.
// Общий низ для обеих записей: иначе «не записалось» и «не сериализовалось»
// разъезжаются.
func (s *Server) writeAuditRecord(record map[string]interface{}) error {
	return s.writeAuditFile(agentExecAuditFile, record)
}

// writeAuditFile — дописать запись в указанный append-only журнал каталога ранов.
// Общий низ для журналов безопасности: fsync обязателен в обоих, потому что
// запись должна пережить падение процесса сразу после действия.
func (s *Server) writeAuditFile(name string, record map[string]interface{}) error {
	if s.runsDir == "" {
		return fmt.Errorf("не задан каталог ранов, запись о запуске агентом невозможна")
	}
	record["ts"] = time.Now().UTC().Format(time.RFC3339Nano)
	line, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("не сериализовать запись аудита: %w", err)
	}
	path := filepath.Join(s.runsDir, name)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("открыть журнал запусков агента (%s): %w", path, err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("записать в журнал запусков агента (%s): %w", path, err)
	}
	// Синхронизируем файл: намерение обязано оказаться на диске ДО запуска
	// плагина. При вылете процесса иначе буфер ушёл бы в никуда, и мы
	// получили бы ровно то, ради чего переставили записи: запуск без следа.
	if err := f.Sync(); err != nil {
		return fmt.Errorf("сбросить журнал запусков агента на диск (%s): %w", path, err)
	}
	return nil
}

// logf — единственный канал сообщений сервера в лог. В MCP-процессе stdout
// принадлежит протоколу (cli/mcp.go перенаправляет os.Stdout в stderr), поэтому
// лог всегда в stderr и никогда не попадает в JSON-RPC.
func (s *Server) logf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "wedra mcp: "+format+"\n", args...)
}

// auditUntrusted — что писать в журнал аудита как "untrusted".
//
// Раньше здесь стоял IsAgentWrittenPlugin(m), и это было равносильно верно:
// внешним кодом был ровно плагин из agent-plugins/. После инверсии доверия
// (H1) внешним стал ЛЮБОЙ плагин, которого нет в allow-list, — и запись
// IsAgentWrittenPlugin дала бы в журнале безопасности "untrusted": false для
// кода, который на самом деле ушёл в песочницу. Журнал, в котором враньё,
// хуже отсутствия журнала: на него смотрят при разборе инцидента.
func (s *Server) auditUntrusted(m *pipeline.Manifest) bool {
	return !plugin.IsTrusted(m, s.trust)
}

// auditAgentExecIntent фиксирует НАМЕРЕНИЕ запустить плагин и возвращает id
// запуска. Вызывается до исполнения, и её ошибка означает отказ: код не
// запускается вовсе.
//
// Почему именно до запуска, а не после. При записи после падения процесса
// посреди исполнения не остаётся НИЧЕГО: плагин уже отработал, а след — нет.
// Запись до запуска закрывает и это, и даёт настоящий запрет, а не
// «результат не отдан».
func (s *Server) auditAgentExecIntent(pluginRef string, m *pipeline.Manifest, timeout float64) (string, error) {
	execID, err := newExecID()
	if err != nil {
		return "", err
	}
	err = s.writeAuditRecord(map[string]interface{}{
		"event":     agentExecEventIntent,
		"exec_id":   execID,
		"source":    "agent_auto",
		"plugin":    pluginRef,
		"plugin_id": m.ID,
		"untrusted": s.auditUntrusted(m),
		"timeout":   timeout,
	})
	if err != nil {
		return "", err
	}
	return execID, nil
}

// auditAgentExecResult дописывает исход запуска. Ошибка здесь означает
// отказ вернуть результат агенту (E_AGENT_EXEC_AUDIT), но плагин к этому
// моменту уже отработал — намерение о нём в журнале уже есть.
func (s *Server) auditAgentExecResult(execID, pluginRef string, m *pipeline.Manifest, res *plugin.ExecResult, took time.Duration) error {
	return s.writeAuditRecord(map[string]interface{}{
		"event":     agentExecEventResult,
		"exec_id":   execID,
		"source":    "agent_auto",
		"plugin":    pluginRef,
		"plugin_id": m.ID,
		"untrusted": s.auditUntrusted(m),
		"exit_code": res.ExitCode,
		"err_code":  res.ErrCode,
		"duration":  took.String(),
	})
}

func (s *Server) checkPluginNetwork(m *pipeline.Manifest) error {
	for _, permission := range m.Permissions.Network {
		if permission.AnyHost {
			return fmt.Errorf("E_NETWORK_DENIED: плагин %s заявил any_host в exec_plugin", m.ID)
		}
		host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(permission.Host), "."))
		if host == "" {
			return fmt.Errorf("E_NETWORK_DENIED: пустой host в permissions плагина %s", m.ID)
		}
		if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
			return fmt.Errorf("E_NETWORK_DENIED: host %q запрещён в exec_plugin", permission.Host)
		}
		if ip := net.ParseIP(host); ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified()) {
			return fmt.Errorf("E_NETWORK_DENIED: private/loopback host %q запрещён в exec_plugin", permission.Host)
		}
	}
	return nil
}
