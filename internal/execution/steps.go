package execution

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/wykserdex/wedra/internal/common"
	"github.com/wykserdex/wedra/internal/gate"
	"github.com/wykserdex/wedra/internal/journal"
	"github.com/wykserdex/wedra/internal/pipeline"
	"github.com/wykserdex/wedra/internal/plugin"
	"github.com/wykserdex/wedra/internal/runctx"
)

func runStepFlow(eng Engine, pf *pipeline.PipelineFile, st *pipeline.Step, ctx *runctx.Ctx, j *journal.Journal, opts RunOptions) (string, error) {
	if st.When.IsSet() {
		ok, err := pipeline.EvaluateWhen(st.When, ctx.Data)
		if err != nil {
			j.Event("step_skipped", map[string]interface{}{"step": st.ID, "reason": "when", "error": err.Error()})
			return "", &RunError{Code: "when_error", Err: fmt.Errorf("шаг %s: when: %w", st.ID, err)}
		}
		if !ok {
			opts.logf("  → %-12s (условие не выполнено: %s — skipped)", st.ID, st.When.String())
			j.Event("step_skipped", map[string]interface{}{"step": st.ID, "reason": "when", "condition": st.When.String()})
			return "ok", nil
		}
	}
	if st.Foreach != "" {
		return runStepForeach(eng, pf, st, ctx, j, opts)
	}
	if st.Loop != "" {
		return runStepLoop(eng, pf, st, ctx, j, opts)
	}
	return runStep(eng, pf, st, ctx, j, opts)
}

// runStepForeach — v0.20: шаг по каждому элементу массива из пути.
// input.<foreach_item> перезаписывается на итерацию; steps.<id> — выход
// последней итерации; steps.<id>_all — массив всех выходов.
// В отличие от pipeline-foreach, «item» здесь не существует: on_error=stop
// останавливает весь ран.
func runStepForeach(eng Engine, pf *pipeline.PipelineFile, st *pipeline.Step, ctx *runctx.Ctx, j *journal.Journal, opts RunOptions) (string, error) {
	v, ok := ctx.Get(st.Foreach)
	if !ok {
		return "", runErr("foreach_path", "foreach: путь %s не найден в контексте", st.Foreach)
	}
	arr, ok := v.([]interface{})
	if !ok {
		return "", runErr("foreach_path", "foreach: %s не массив (шаг %s)", st.Foreach, st.ID)
	}
	if len(arr) > pipeline.MaxForeachItems {
		return "", runErr("resource_limit", "шаг %s: foreach: %d элементов, максимум %d", st.ID, len(arr), pipeline.MaxForeachItems)
	}
	itemKey := st.ForeachItem
	if itemKey == "" {
		itemKey = "item"
	}
	var results []interface{}
	for i, it := range arr {
		j.Event("foreach_item_start", map[string]interface{}{"step": st.ID, "index": i, "item": it})
		ctx.SetInput(itemKey, it)
		action, err := runStep(eng, pf, st, ctx, j, opts)
		if err != nil {
			j.Event("foreach_item_failed", map[string]interface{}{"step": st.ID, "index": i, "error": err.Error()})
			return "", fmt.Errorf("foreach шаг %s: элемент %d/%d: %w", st.ID, i+1, len(arr), err)
		}
		if action == "abort_item" {
			j.Event("foreach_item_failed", map[string]interface{}{"step": st.ID, "index": i, "reason": "on_error=stop"})
			return "", fmt.Errorf("foreach шаг %s: элемент %d/%d упал (on_error=stop) — ран остановлен", st.ID, i+1, len(arr))
		}
		if out, ok := ctx.Steps()[st.ID]; ok {
			results = append(results, out)
		}
		j.Event("foreach_item_end", map[string]interface{}{"step": st.ID, "index": i})
	}
	if stepsMap, ok := ctx.Data["steps"].(map[string]interface{}); ok {
		stepsMap[st.ID+"_all"] = results
	}
	return "ok", nil
}

func runStepLoop(eng Engine, pf *pipeline.PipelineFile, st *pipeline.Step, ctx *runctx.Ctx, j *journal.Journal, opts RunOptions) (string, error) {
	maxIter := st.MaxIterations
	if maxIter <= 0 {
		maxIter = pipeline.MaxLoopIterations
	}
	if maxIter > pipeline.MaxLoopIterations {
		maxIter = pipeline.MaxLoopIterations
	}
	condition := st.LoopCondition
	if condition == "" {
		condition = "steps." + st.ID + ".continue"
	}
	// Бюджет общий на ран, а не на шаг: иначе N loop-шагов давали N*1000
	// запусков плагина, а имя константы обещало «глобальный».
	for i := 0; i < maxIter; i++ {
		if !opts.consumeLoopIteration() {
			spent := opts.loopIterations()
			limit := pipeline.MaxTotalLoopBudget
			if opts.loopBudget != nil {
				limit = opts.loopBudget.effectiveLimit()
			}
			j.Event("loop_budget_exhausted", map[string]interface{}{"step": st.ID, "total_iterations": spent, "limit": limit})
			return "", runErr("resource_limit", "loop: исчерпан бюджет итераций на ран (%d), шаг %s", limit, st.ID)
		}
		if j.WriteErrors() > pipeline.MaxLoopJournalEvents {
			j.Event("loop_journal_limit", map[string]interface{}{"step": st.ID, "write_errors": j.WriteErrors()})
			return "", runErr("resource_limit", "loop шаг %s: превышен лимит событий журнала", st.ID)
		}
		j.Event("loop_iteration_start", map[string]interface{}{"step": st.ID, "iteration": i})
		action, err := runStep(eng, pf, st, ctx, j, opts)
		if err != nil {
			j.Event("loop_iteration_failed", map[string]interface{}{"step": st.ID, "iteration": i, "error": err.Error()})
			return "", fmt.Errorf("loop шаг %s: итерация %d/%d: %w", st.ID, i+1, maxIter, err)
		}
		if action == "abort_item" {
			j.Event("loop_iteration_failed", map[string]interface{}{"step": st.ID, "iteration": i, "reason": "on_error=stop"})
			return "", fmt.Errorf("loop шаг %s: итерация %d/%d упал (on_error=stop) — ран остановлен", st.ID, i+1, maxIter)
		}
		v, ok := ctx.Get(condition)
		if !ok {
			j.Event("loop_iteration_end", map[string]interface{}{"step": st.ID, "iteration": i, "reason": "condition_not_found"})
			return "ok", nil
		}
		cont, ok := v.(bool)
		if !ok || !cont {
			j.Event("loop_iteration_end", map[string]interface{}{"step": st.ID, "iteration": i, "reason": "condition_false"})
			return "ok", nil
		}
		j.Event("loop_iteration_end", map[string]interface{}{"step": st.ID, "iteration": i, "reason": "continue"})
	}
	j.Event("loop_max_iterations", map[string]interface{}{"step": st.ID, "max_iterations": maxIter})
	return "ok", nil
}

// gateService — UI гейта для шага. Единственное место, где выбирается
// источник ввода, чтобы правило «один терминальный ввод на ран» нельзя было
// обойти, создав Service прямо в runStep.
//
// Порядок веток значим: MCP-рену терминальный ввод запрещён, и проверка
// должна идти до подстановки stdin, иначе запрет перестал бы действовать.
func (o RunOptions) gateService(st *pipeline.Step) (*gate.Service, error) {
	switch {
	case o.GateUI != nil:
		// GUI/API/MCP: свой UI на шаг, жизненным циклом владеет вызывающий.
		return gate.NewServiceWithUI(o.GateUI(st)), nil
	case o.MCPMode:
		// stdin занят JSON-RPC: терминальный гейт съел бы сообщения клиента.
		return nil, runErr("E_NO_GATE_UI", "шаг %s: human_gate без GateUI в MCP-режиме (терминальный ввод запрещён)", st.ID)
	case o.gateUI != nil:
		// Терминал: экземпляр заведён на весь ран (runWithStore).
		return gate.NewServiceWithUI(o.gateUI), nil
	default:
		// runStep мимо рана (прямой вызов в тесте): берём терминал, как раньше.
		return gate.NewService(), nil
	}
}

func runStep(eng Engine, pf *pipeline.PipelineFile, st *pipeline.Step, ctx *runctx.Ctx, j *journal.Journal, opts RunOptions) (string, error) {
	if plugin.IsBuiltin(st.Plugin) {
		// Встроенных модулей больше одного, и гейт — не единственный из них.
		// Раньше здесь стояло безусловное «builtin значит гейт», что молча
		// отправляло любой будущий core/* в gate.Service.
		canonical, _ := common.CanonicalBuiltinRef(st.Plugin)
		if canonical != common.HumanGatePluginRef {
			return runBuiltinDataStep(eng, st, ctx, j, opts)
		}
		svc, err := opts.gateService(st)
		if err != nil {
			return "", err
		}
		action := svc.Run(st, ctx, j, gate.GateOptions{
			Yes: opts.Yes, Quiet: opts.Quiet,
			Policy:       gate.Policy{AllowAutoApprove: !opts.NoAutoApprove},
			RequireHuman: pf.Pipeline.Gates == "human_only",
		})
		// гейт закрыт отменой рана (ChannelUI.Close) — это cancel, не reject
		if opts.cancelled() {
			return "", ErrCancelled
		}
		return action, nil
	}
	if plugin.IsBuiltinNamespace(st.Plugin) {
		return "", runErr("E_PLUGIN_LOAD", "шаг %s: неизвестный встроенный модуль: %s", st.ID, st.Plugin)
	}
	m, err := eng.LoadManifest(st.Plugin)
	if err != nil {
		return "", err
	}
	input, err := buildInput(m, st, ctx)
	if err != nil {
		// Contract input (ERRORS.md): input could not be assembled (no path,
		// тип/формат) — судьба элемента по on_error, ран живёт. Раньше
		// return err ронял весь ран: один "bad-email" убивал 3 хороших.
		switch st.OnError {
		case "skip":
			opts.logf("    ! пропущен по on_error=skip: %s", err)
			j.Event("step_skipped", map[string]interface{}{"step": st.ID, "reason": "on_error", "code": "contract_input", "message": err.Error()})
			if stepsMap, ok := ctx.Data["steps"].(map[string]interface{}); ok {
				delete(stepsMap, st.ID)
			}
			return "ok", nil
		default:
			opts.logf("    × contract_input: %s — элемент остановлен", err)
			j.Event("step_failed", map[string]interface{}{"step": st.ID, "code": "contract_input", "message": err.Error()})
			return "abort_item", nil
		}
	}
	for _, w := range plugin.FileRefWarnings(m, input) {
		opts.logf("    ⚠ %s", w)
		j.Event("file_ref_warning", map[string]interface{}{"step": st.ID, "message": w})
	}
	rawIn, err := json.Marshal(input)
	if err != nil {
		j.Event("step_failed", map[string]interface{}{"step": st.ID, "code": "context_serialization", "message": err.Error()})
		return "", runErr("context_serialization", "шаг %s: input не сериализуется: %w", st.ID, err)
	}
	timeout := st.Timeout.Duration
	if timeout < 0 || timeout > pipeline.MaxStepTimeout {
		return "", runErr(pipeline.E_TIMEOUT_LIMIT, "шаг %s: timeout=%s, допустимо 0..%s", st.ID, timeout, pipeline.MaxStepTimeout)
	}
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	attempts := 1
	if st.OnError == "retry" {
		attempts = 3
		if st.Retry != nil && st.Retry.Attempts > 0 {
			attempts = st.Retry.Attempts
		}
	}
	if attempts > pipeline.MaxRetryAttempts {
		attempts = pipeline.MaxRetryAttempts
	}
	// v0.17: declare-now — subprocess получает контракт сети. Значение по
	// умолчанию — deny, а не allow: раньше здесь стоял literal "allow", и
	// пайплайн с незаполненным полем network отдавал плагину WEDRA_NETWORK=allow,
	// то есть пустое поле молча разрешало сеть.
	netEnv := "allow"
	if pipeline.EffectiveNetwork(&pf.Pipeline) == pipeline.NetworkDeny {
		netEnv = "deny"
	}
	var res *plugin.ExecResult
	for attempt := 1; attempt <= attempts; attempt++ {
		opts.logf("  → %-12s (попытка %d/%d)", st.ID, attempt, attempts)
		j.Event("step_start", map[string]interface{}{
			"step": st.ID, "attempt": attempt,
			"network": netEnv, "network_declared": pipeline.NetworkHostList(m),
		})
		res = plugin.ExecWithEnvCtx(opts.Ctx, m, rawIn, timeout, []string{"WEDRA_NETWORK=" + netEnv})
		j.Event("step_end", map[string]interface{}{
			"step": st.ID, "attempt": attempt, "exit_code": res.ExitCode,
			"duration_ms": res.Duration.Milliseconds(), "status": statusOf(res),
			"error": errOrNil(res), "stderr": common.Truncate(res.Stderr, 500),
		})
		if res.Cancelled {
			return "", ErrCancelled
		}
		if res.OK() || !res.ShouldRetry() {
			break
		}
		delay := retryDelay(st, attempt)
		opts.logf("    … retry через %s (%s: %s)", delay, res.ErrCode, res.ErrMsg)
		if opts.Ctx != nil {
			select {
			case <-opts.Ctx.Done():
				return "", ErrCancelled
			case <-time.After(delay):
			}
		} else {
			time.Sleep(delay)
		}
	}
	if res.OK() {
		out, dropped, err := plugin.EnforceOutput(m, res.Output)
		if len(dropped) > 0 {
			j.Event("contract_warning", map[string]interface{}{"step": st.ID, "dropped_fields": dropped})
		}
		if err != nil {
			// PROTOCOL §5: выход плагина нарушил контракт — платформенная ошибка
			return "", &RunError{Code: "contract_output", Err: err}
		}
		ctx.SetStep(st.ID, out)
		return "ok", nil
	}
	if res.Platform {
		// ERRORS.md: в журнал идёт ровно один префикс `platform:` —
		// на exit>=2 код плагина уже приходит с ним (plugin/process.go).
		return "", runErr(plugin.PlatformErrCode(res.ErrCode), "платформенная ошибка (%s): %s", res.ErrCode, res.ErrMsg)
	}
	switch st.OnError {
	case "skip":
		opts.logf("    ! пропущен по on_error=skip: %s", res.ErrMsg)
		j.Event("step_skipped", map[string]interface{}{"step": st.ID, "reason": "on_error", "code": res.ErrCode, "message": res.ErrMsg})
		// v0.20: skip = шаг не дал выход — чистим неймспейс, иначе
		// значение предыдущей итерации утёкнет в foreach-агрегацию
		if stepsMap, ok := ctx.Data["steps"].(map[string]interface{}); ok {
			delete(stepsMap, st.ID)
		}
		return "ok", nil
	default:
		opts.logf("    × %s: %s — элемент остановлен", res.ErrCode, res.ErrMsg)
		j.Event("step_failed", map[string]interface{}{"step": st.ID, "code": res.ErrCode, "message": res.ErrMsg})
		return "abort_item", nil
	}
}

func statusOf(r *plugin.ExecResult) string {
	if r.OK() {
		return "ok"
	}
	if r.Platform {
		return "platform_error"
	}
	return "domain_error"
}

func errOrNil(r *plugin.ExecResult) interface{} {
	if r.OK() {
		return nil
	}
	return map[string]interface{}{"code": r.ErrCode, "message": r.ErrMsg, "retryable": r.Retryable}
}

func buildInput(m *pipeline.Manifest, st *pipeline.Step, ctx *runctx.Ctx) (map[string]interface{}, error) {
	in := map[string]interface{}{}
	for name, port := range m.Input {
		from := pipeline.PortSource(name, port, st)
		v, ok := ctx.Get(from)
		if !ok {
			if port.Optional {
				continue
			}
			return nil, fmt.Errorf("вход %q: путь %s не найден в контексте", name, from)
		}
		// v0.23: симметричный контракт — вход тоже проверяется (тип+формат),
		// иначе сломанный upstream утекает дальше
		if err := plugin.CheckValue(name, fmt.Sprintf("вход плагина %s", m.ID), port, v); err != nil {
			return nil, err
		}
		in[name] = v
	}
	return in, nil
}

func retryDelay(st *pipeline.Step, attempt int) time.Duration {
	d := time.Second
	if st.Retry != nil {
		if st.Retry.Delay.Duration > 0 {
			d = st.Retry.Delay.Duration
		}
		if d > pipeline.MaxRetryDelay {
			d = pipeline.MaxRetryDelay
		}
		if st.Retry.Backoff == "exponential" {
			for i := 1; i < attempt; i++ {
				if d >= pipeline.MaxRetryDelay/2 {
					d = pipeline.MaxRetryDelay
					break
				}
				d *= 2
			}
		}
	}
	return d
}
