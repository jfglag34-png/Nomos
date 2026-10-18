package pipeline

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// collector — аккумулятор Issue с хелперами err/warn.
// Сообщения Message — те же русские тексты, что раньше лежали в errs/warns,
// чтобы старый Validate() и GUI-редактор продолжали работать без изменений.
type collector struct {
	issues []Issue
}

func (c *collector) err(code, step, port, path, hint string, fix *Fix, format string, args ...interface{}) {
	c.issues = append(c.issues, Issue{
		Code: code, Severity: SeverityError, Step: step, Port: port, Path: path,
		Message: fmt.Sprintf(format, args...), Hint: hint, Fix: fix,
	})
}

func (c *collector) warn(code, step, port, path, hint string, fix *Fix, format string, args ...interface{}) {
	c.issues = append(c.issues, Issue{
		Code: code, Severity: SeverityWarning, Step: step, Port: port, Path: path,
		Message: fmt.Sprintf(format, args...), Hint: hint, Fix: fix,
	})
}

// portNameList — отсортированные имена портов (для Hint/Fix).
func portNameList(m map[string]Port) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// inputFieldList — доступные поля input.* пайплайна.
func inputFieldList(pf *PipelineFile) []string {
	var out []string
	for k := range pf.Pipeline.Input {
		out = append(out, "input."+k)
	}
	sort.Strings(out)
	return out
}

// compatibleOutputs — выходы предыдущих шагов, совместимые с wanted
// типом/форматом: "steps.<id>.<out>". Пустой wanted — любой подходит.
func compatibleOutputs(prior map[string]priorStep, wantType, wantFormat string) []string {
	var ids []string
	for id := range prior {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var out []string
	for _, id := range ids {
		ps := prior[id]
		if ps.manifest == nil {
			continue
		}
		var ports []string
		for pn := range ps.manifest.Output {
			ports = append(ports, pn)
		}
		sort.Strings(ports)
		for _, pn := range ports {
			p := ps.manifest.Output[pn]
			if wantType != "" && p.Type != "" && p.Type != wantType {
				continue
			}
			if wantFormat != "" && !formatsCompatible(p.Format, wantFormat) {
				continue
			}
			out = append(out, "steps."+id+"."+pn)
		}
	}
	return out
}

// ValidateIssues — структурная валидация с кодами. Основной метод;
// Validate() — тонкая обёртка для совместимости.
func ValidateIssues(pf *PipelineFile, eng Engine) []Issue {
	var v collector
	if pf.FormatVersion != "0.1" && pf.FormatVersion != "0.2" {
		if pf.FormatVersion != "" {
			// forward compatibility: unknown future version is warn, not error — ран с 0.3 должен пройти валидацию с предупреждением
			v.warn(E_FORMAT_VERSION, "", "", "format_version", "допустимые значения: \"0.1\", \"0.2\" (будущие версии — предупреждение)", &Fix{Op: "set", Target: "format_version", Candidates: []string{"0.1", "0.2"}}, "format_version %q не из списка поддерживаемых: 0.1, 0.2 (будущая версия — предупреждение)", pf.FormatVersion)
		} else {
			v.warn(W_FORMAT_VERSION_MISSING, "", "", "format_version", "укажите format_version: \"0.2\"", &Fix{Op: "set", Target: "format_version", Candidates: []string{"0.2"}}, "format_version %q не из списка поддерживаемых: 0.1, 0.2", pf.FormatVersion)
		}
	}
	for _, k := range pf.Pipeline.Secrets {
		if os.Getenv(k) == "" {
			v.warn(W_SECRETS_MISSING_ENV, "", "", "pipeline.secrets", "задайте переменную окружения перед запуском: export "+k+"=...", nil, "secrets: переменная окружения %s не задана (нужна для запуска)", k)
		}
	}
	if cycle := DetectCycle(pf); cycle != "" {
		v.err(E_CYCLE, "", "", "pipeline.steps", "разорвите цикл: шаги должны образовывать DAG", nil, "%s", cycle)
	}
	p := &pf.Pipeline
	if p.Gates != "" && p.Gates != "human_only" && p.Gates != "any" {
		v.err(E_GATES_VALUE, "", "", "pipeline.gates", "допускаются: human_only, any", &Fix{Op: "set", Target: "pipeline.gates", Candidates: []string{"human_only", "any"}}, "pipeline gates=%q, ожидается human_only|any", p.Gates)
	}
	if p.Network != "" && p.Network != "allow" && p.Network != "deny" {
		v.err(E_NETWORK_VALUE, "", "", "pipeline.network", "допускаются: allow, deny", &Fix{Op: "set", Target: "pipeline.network", Candidates: []string{"allow", "deny"}}, "pipeline network=%q, ожидается allow|deny", p.Network)
	}
	// v0.35 schedule — планировщик (тумблер соло/по расписанию)
	if p.Schedule != nil {
		if strings.TrimSpace(p.Schedule.Cron) == "" {
			v.err(E_SCHEDULE_CRON, "", "", "pipeline.schedule.cron", "укажите cron: \"0 9 * * *\"", &Fix{Op: "set", Target: "pipeline.schedule.cron", Candidates: []string{"0 9 * * *", "0 * * * *", "*/5 * * * *"}}, "schedule: cron не задан")
		} else if !isValidCron(p.Schedule.Cron) {
			v.err(E_SCHEDULE_CRON, "", "", "pipeline.schedule.cron", "формат cron: 5 полей \"* * * * *\"", &Fix{Op: "set", Target: "pipeline.schedule.cron", Candidates: []string{"0 9 * * *", "0 * * * *"}}, "schedule: cron %q невалиден (ожидается 5 полей)", p.Schedule.Cron)
		}
		if p.Schedule.Timezone != "" {
			if err := validateTimezone(p.Schedule.Timezone); err != nil {
				v.err(E_SCHEDULE_TIMEZONE, "", "", "pipeline.schedule.timezone", "IANA таймзона, напр. Europe/Minsk или UTC", &Fix{Op: "set", Target: "pipeline.schedule.timezone", Candidates: []string{"Europe/Minsk", "UTC", "Local"}}, "schedule: timezone %q: %v", p.Schedule.Timezone, err)
			}
		}
		if p.Schedule.Overlap != "" && p.Schedule.Overlap != "skip" && p.Schedule.Overlap != "queue" && p.Schedule.Overlap != "allow" {
			v.err(E_SCHEDULE_OVERLAP, "", "", "pipeline.schedule.overlap", "допускаются: skip, queue, allow", &Fix{Op: "set", Target: "pipeline.schedule.overlap", Candidates: []string{"skip", "queue", "allow"}}, "schedule: overlap=%q, ожидается skip|queue|allow", p.Schedule.Overlap)
		}
		if p.Schedule.HumanGate != "" && p.Schedule.HumanGate != "autoApprove" && p.Schedule.HumanGate != "wait" && p.Schedule.HumanGate != "auto_approve" {
			v.err(E_SCHEDULE_HUMAN_GATE, "", "", "pipeline.schedule.human_gate", "допускаются: autoApprove, wait", &Fix{Op: "set", Target: "pipeline.schedule.human_gate", Candidates: []string{"autoApprove", "wait"}}, "schedule: human_gate=%q, ожидается autoApprove|wait", p.Schedule.HumanGate)
		}
	}
	if p.Foreach != "" {
		if strings.HasPrefix(p.Foreach, "input.") {
			key := strings.TrimPrefix(p.Foreach, "input.")
			val, ok := p.Input[key]
			if !ok {
				v.err(E_FOREACH_NOT_FOUND, "", "", "pipeline.foreach", fmt.Sprintf("доступные поля: %s", strings.Join(inputFieldList(pf), ", ")), &Fix{Op: "bind", Target: "pipeline.foreach", Candidates: inputFieldList(pf)}, "foreach: массив %s не найден в input", p.Foreach)
			} else if arr, isArr := val.([]interface{}); isArr && len(arr) > MaxForeachItems {
				v.err(E_FOREACH_LIMIT, "", "", "pipeline.input."+key, "слишком много элементов", nil, "pipeline foreach: input.%s содержит %d элементов, максимум %d", key, len(arr), MaxForeachItems)
			} else if p.ItemType != "" || p.ItemFormat != "" {
				if arr, isArr := val.([]interface{}); isArr {
					for idx, elem := range arr {
						path := fmt.Sprintf("input.%s[%d]", key, idx)
						if p.ItemType != "" && KindOf(elem) != p.ItemType {
							v.warn(W_FOREACH_ITEM_TYPE, "", "", path, fmt.Sprintf("приведите элемент к типу %q или смените item_type", p.ItemType), nil, "input.%s[%d] = %v не соответствует типу %q (item_type)", key, idx, elem, p.ItemType)
							continue
						}
						if p.ItemFormat != "" {
							s, isStr := elem.(string)
							if !isStr {
								v.warn(W_FOREACH_ITEM_FORMAT, "", "", path, "формат применим только к строкам", nil, "input.%s[%d] не строка, а формат %q требует строку", key, idx, p.ItemFormat)
								continue
							}
							if !scalarMatchesFormat(s, p.ItemFormat) {
								v.warn(W_FOREACH_ITEM_FORMAT, "", "", path, "исправьте значение или уберите item_format; плохой элемент уронит только себя (per-item abort)", nil, "input.%s[%d] = %q не соответствует формату %q", key, idx, s, p.ItemFormat)
							}
						}
					}
				}
			}
		} else if strings.HasPrefix(p.Foreach, "steps.") {
			parts := strings.Split(p.Foreach, ".")
			if len(parts) < 3 {
				v.err(E_FOREACH_SHAPE, "", "", "pipeline.foreach", "формат: steps.<id>.<field>", nil, "foreach: steps.* должен быть вида steps.<id>.<field>")
			} else {
				found := false
				for _, st := range p.Steps {
					if st.ID == parts[1] {
						found = true
						break
					}
				}
				if !found {
					v.err(E_FOREACH_NOT_FOUND, "", "", "pipeline.foreach", "шаг-источник должен быть в списке steps", nil, "foreach: шаг %s не найден в пайплайне", parts[1])
				}
			}
		} else {
			v.err(E_FOREACH_PATH, "", "", "pipeline.foreach", "путь должен начинаться с input. или steps.", &Fix{Op: "bind", Target: "pipeline.foreach", Candidates: inputFieldList(pf)}, "foreach: путь должен начинаться с input. или steps.")
		}
	}
	seen := map[string]bool{}
	prior := map[string]priorStep{}
	for i := range p.Steps {
		st := &p.Steps[i]
		if st.ID == "" {
			v.err(E_STEP_ID_EMPTY, "", "", fmt.Sprintf("pipeline.steps[%d]", i), "дайте шагу непустой id", nil, "шаг #%d: пустой id", i+1)
			continue
		}
		if seen[st.ID] {
			v.err(E_STEP_ID_DUP, st.ID, "", "pipeline.steps."+st.ID, "id шагов должны быть уникальны", nil, "шаг %s: дублирующийся id", st.ID)
		}
		seen[st.ID] = true
		if !IsBuiltin(st.Plugin) && IsBuiltinNamespace(st.Plugin) {
			v.err(E_PLUGIN_LOAD, st.ID, "", "pipeline.steps."+st.ID+".plugin", "проверьте путь к плагину и plugin.yaml", nil, "шаг %s: неизвестный встроенный модуль: %s", st.ID, st.Plugin)
			continue
		}
		if st.When.IsSet() {
			if !WhenOps[st.When.Op] {
				v.err(E_WHEN_OP, st.ID, "", "pipeline.steps."+st.ID+".when", "допускаются: truthy, exists, missing, eq, neq, gt, gte, lt, lte, contains", &Fix{Op: "set", Target: "steps." + st.ID + ".when.op", Candidates: []string{"truthy", "exists", "missing", "eq", "neq", "gt", "gte", "lt", "lte", "contains"}}, "шаг %s: when: неизвестный оператор %q (допускаются: truthy, exists, missing, eq, neq, gt, gte, lt, lte, contains)", st.ID, st.When.Op)
			}
			if !strings.HasPrefix(st.When.Path, "input.") && !strings.HasPrefix(st.When.Path, "steps.") {
				v.err(E_WHEN_PATH, st.ID, "", "pipeline.steps."+st.ID+".when", "путь должен начинаться с input. или steps.", nil, "шаг %s: when: путь должен начинаться с input. или steps. (got %s)", st.ID, st.When.Path)
			} else if parts := strings.Split(st.When.Path, "."); strings.HasPrefix(st.When.Path, "steps.") {
				if len(parts) < 3 {
					v.err(E_WHEN_PATH, st.ID, "", "pipeline.steps."+st.ID+".when", "формат: steps.<id>.<field>", nil, "шаг %s: when: steps.* должен быть вида steps.<id>.<field>", st.ID)
				} else if _, ok := prior[parts[1]]; !ok {
					v.err(E_WHEN_FORWARD_REF, st.ID, "", "pipeline.steps."+st.ID+".when", "when читает только из шагов выше по списку", nil, "шаг %s: when: читает из шага %s, который ещё не выполняется", st.ID, parts[1])
				}
			}
		}
		if st.Foreach != "" {
			if !strings.HasPrefix(st.Foreach, "input.") && !strings.HasPrefix(st.Foreach, "steps.") {
				v.err(E_STEP_FOREACH_PATH, st.ID, "", "pipeline.steps."+st.ID+".foreach", "путь должен начинаться с input. или steps.", nil, "шаг %s: foreach: путь должен начинаться с input. или steps. (got %s)", st.ID, st.Foreach)
			} else if strings.HasPrefix(st.Foreach, "steps.") {
				parts := strings.Split(st.Foreach, ".")
				if len(parts) < 3 {
					v.err(E_STEP_FOREACH_SHAPE, st.ID, "", "pipeline.steps."+st.ID+".foreach", "формат: steps.<id>.<field>", nil, "шаг %s: foreach: steps.* должен быть вида steps.<id>.<field>", st.ID)
				} else if _, ok := prior[parts[1]]; !ok {
					v.err(E_STEP_FOREACH_NOT_FOUND, st.ID, "", "pipeline.steps."+st.ID+".foreach", "шаг-источник должен быть выше по списку", nil, "шаг %s: foreach: шаг %s не найден или ещё не выполняется", st.ID, parts[1])
				}
			} else if key := strings.TrimPrefix(st.Foreach, "input."); !strings.Contains(key, ".") {
				if _, ok := p.Input[key]; !ok {
					v.err(E_STEP_FOREACH_NOT_FOUND, st.ID, "", "pipeline.steps."+st.ID+".foreach", fmt.Sprintf("доступные поля: %s", strings.Join(inputFieldList(pf), ", ")), &Fix{Op: "bind", Target: "steps." + st.ID + ".foreach", Candidates: inputFieldList(pf)}, "шаг %s: foreach: массив %s не найден в input", st.ID, st.Foreach)
				}
			}
			if st.AfterForeach {
				v.err(E_FOREACH_COMBO, st.ID, "", "pipeline.steps."+st.ID, "уберите after_foreach или foreach", nil, "шаг %s: foreach и after_foreach не сочетаются", st.ID)
			}
			if st.ParallelGroup != "" {
				v.err(E_FOREACH_COMBO, st.ID, "", "pipeline.steps."+st.ID, "уберите parallel_group или foreach", nil, "шаг %s: foreach не сочетается с parallel_group", st.ID)
			}
			if st.ForeachItem != "" && strings.ContainsAny(st.ForeachItem, ". \t\"'") {
				v.err(E_FOREACH_ITEM_NAME, st.ID, "", "pipeline.steps."+st.ID+".foreach_item", "имя должно быть простым: буквы, цифры, _", nil, "шаг %s: foreach_item должно быть простым именем (got %q)", st.ID, st.ForeachItem)
			}
			if IsHumanGate(st.Plugin) {
				v.err(E_GATE_FOREACH, st.ID, "", "pipeline.steps."+st.ID, "human_gate работает с одним набором полей, не с массивом", nil, "шаг %s: human_gate не принимает foreach", st.ID)
			}
		}
		if st.ParallelGroup != "" && IsHumanGate(st.Plugin) {
			v.err(E_GATE_PARALLEL, st.ID, "", "pipeline.steps."+st.ID, "вынесите гейт из параллельной группы", nil, "шаг %s: human_gate нельзя ставить в параллельную группу %q (гейты сериализуют терминал)", st.ID, st.ParallelGroup)
		}
		switch st.OnError {
		case "", "stop", "skip", "retry":
		default:
			v.err(E_ON_ERROR, st.ID, "", "pipeline.steps."+st.ID+".on_error", "допустимы: stop, skip, retry", &Fix{Op: "set", Target: "steps." + st.ID + ".on_error", Candidates: []string{"stop", "skip", "retry"}}, "шаг %s: on_error=%s, ожидается stop|skip|retry", st.ID, st.OnError)
		}
		if st.OnError == "retry" && st.Retry != nil && st.Retry.Attempts < 1 {
			v.err(E_RETRY_ATTEMPTS, st.ID, "", "pipeline.steps."+st.ID+".retry.attempts", "attempts должен быть >= 1", nil, "шаг %s: retry.attempts < 1", st.ID)
		}
		if st.OnError == "retry" && st.Retry != nil && st.Retry.Attempts > MaxRetryAttempts {
			v.err(E_RETRY_LIMIT, st.ID, "", "pipeline.steps."+st.ID+".retry.attempts", "слишком много попыток", &Fix{Op: "set", Target: "steps." + st.ID + ".retry.attempts", Candidates: []string{"1", "3", "5"}}, "шаг %s: retry.attempts=%d, максимум %d", st.ID, st.Retry.Attempts, MaxRetryAttempts)
		}
		if st.Timeout.Duration < 0 || st.Timeout.Duration > MaxStepTimeout {
			v.err(E_TIMEOUT_LIMIT, st.ID, "", "pipeline.steps."+st.ID+".timeout", "укажите timeout от 0 до 30m", nil, "шаг %s: timeout=%s, допустимо 0..%s", st.ID, st.Timeout.Duration, MaxStepTimeout)
		}
		if st.Approval != "" && st.Approval != "human" && st.Approval != "any" {
			v.err(E_APPROVAL_VALUE, st.ID, "", "pipeline.steps."+st.ID+".approval", "допускаются: human, any", &Fix{Op: "set", Target: "steps." + st.ID + ".approval", Candidates: []string{"human", "any"}}, "шаг %s: approval=%s, ожидается human|any", st.ID, st.Approval)
		}
		if IsHumanGate(st.Plugin) {
			if len(st.Bind) > 0 {
				v.err(E_GATE_BIND, st.ID, "", "pipeline.steps."+st.ID+".bind", "у human_gate данные берутся из form, не из bind", nil, "шаг %s: human_gate не принимает bind", st.ID)
			}
			switch st.OnReject {
			case "", "stop", "continue":
			default:
				v.err(E_ON_REJECT, st.ID, "", "pipeline.steps."+st.ID+".on_reject", "допускаются: stop, continue", &Fix{Op: "set", Target: "steps." + st.ID + ".on_reject", Candidates: []string{"stop", "continue"}}, "шаг %s: on_reject=%s, ожидается stop|continue", st.ID, st.OnReject)
			}
			for _, action := range st.Actions {
				if action != "accept" && action != "reject" {
					v.err(E_GATE_ACTIONS, st.ID, "", "pipeline.steps."+st.ID+".actions", "разрешены только accept и reject", &Fix{Op: "set", Target: "steps." + st.ID + ".actions", Candidates: []string{"accept", "reject"}}, "шаг %s: actions=%q, допустимы accept|reject", st.ID, action)
				}
			}
			bnSeen := map[string][]string{}
			for _, f := range st.Form {
				bn := Basename(f.Field)
				bnSeen[bn] = append(bnSeen[bn], f.Field)
			}
			for bn, fields := range bnSeen {
				if len(fields) > 1 {
					v.warn(W_GATE_FORM_COLLISION, st.ID, "", "pipeline.steps."+st.ID+".form", "переименуйте поля или смиритесь с ключами <step_id>_<basename>", nil, "шаг %s, form: базовое имя %q встречается в %v — будут ключи вида <step_id>_%s", st.ID, bn, fields, bn)
				}
			}
			for _, f := range st.Form {
				if _, code, e := resolveSourceCoded(f.Field, prior, pf, nil); e != "" {
					_ = code
					v.warn(W_GATE_FORM_MISSING, st.ID, "", f.Field, "поле может отсутствовать в рантайме — проверьте путь", nil, "шаг %s, form: %s — поле может отсутствовать", st.ID, e)
				} else if src, _ := resolveSource(f.Field, prior, pf, nil); src.Step != nil && src.Step.OnError == "skip" {
					v.warn(W_GATE_FORM_SKIP, st.ID, "", f.Field, "шаг-источник может быть пропущен (on_error: skip)", nil, "шаг %s, form: %s читает из skip-able шага %s", st.ID, f.Field, src.Step.ID)
				}
			}
			prior[st.ID] = priorStep{step: st, manifest: &Manifest{ID: "core/human_gate"}}
			continue
		}
		// core/text_stats — встроенный, но не гейт: валидируем как плагин с известными портами
		if st.Plugin == "core/text_stats" {
			// синтетический манифест как в plugin/registry.go
			m := &Manifest{
				ID: "core/text_stats",
				Input: map[string]Port{"text": {From: "input.text", Type: "string", Format: "text", Optional: true}},
				Output: map[string]Port{
					"lines":        {Type: "number"},
					"words":        {Type: "number"},
					"unique_words": {Type: "number"},
					"longest_word": {Type: "string", Format: "text"},
				},
			}
			// reuse normal validation with m
			// (copied below but with m)
			for b := range st.Bind {
				if _, ok := m.Input[b]; !ok {
					ports := portNameList(m.Input)
					v.err(E_BIND_UNKNOWN_PORT, st.ID, b, "pipeline.steps."+st.ID+".bind."+b, fmt.Sprintf("порты плагина: %s", strings.Join(ports, ", ")), &Fix{Op: "bind", Target: "steps." + st.ID + "." + b, Candidates: ports}, "шаг %s: bind указывает на несуществующий порт %q (порты: %s)", st.ID, b, portNames(m.Input))
				}
			}
			for b, src := range st.Bind {
				if isContextRef(src) { continue }
				v.err(E_BIND_SOURCE_INVALID, st.ID, b, "pipeline.steps."+st.ID+".bind."+b, "укажите input.<поле> или steps.<шаг>.<выход>", nil, "шаг %s, порт %s: bind = %q не является ссылкой на контекст", st.ID, b, src)
			}
			// check input binding exists
			for portName, port := range m.Input {
				srcPath := PortSource(portName, port, st)
				if srcPath == "" {
					if port.Optional { continue }
					cands := compatibleOutputs(prior, port.Type, port.Format)
					cands = append(cands, inputFieldList(pf)...)
					v.err(E_PORT_UNBOUND, st.ID, portName, "pipeline.steps."+st.ID+".bind."+portName, "привяжите порт через bind", &Fix{Op: "bind", Target: "steps." + st.ID + "." + portName, Candidates: cands}, "шаг %s, порт %s: нет привязки", st.ID, portName)
					continue
				}
				src, code, perr := resolveSourceCoded(srcPath, prior, pf, st)
				if perr != "" {
					if port.Optional {
						v.warn(W_PORT_OPTIONAL_SOURCE, st.ID, portName, srcPath, "optional-порт: источник может отсутствовать", nil, "шаг %s, порт %s: %s (optional)", st.ID, portName, perr)
					} else {
						fixCands := append(inputFieldList(pf), compatibleOutputs(prior, port.Type, port.Format)...)
						v.err(code, st.ID, portName, srcPath, fmt.Sprintf("доступные поля: %s", strings.Join(inputFieldList(pf), ", ")), &Fix{Op: "bind", Target: "steps." + st.ID + "." + portName, Candidates: fixCands}, "шаг %s, порт %s: %s", st.ID, portName, perr)
					}
					continue
				}
				src.Name = srcPath
				if src.Type != "" && port.Type != "" && src.Type != port.Type {
					cands := compatibleOutputs(prior, port.Type, port.Format)
					v.err(E_TYPE_MISMATCH, st.ID, portName, srcPath, fmt.Sprintf("нужен тип %q, пришёл %q", port.Type, src.Type), &Fix{Op: "bind", Target: "steps." + st.ID + "." + portName, Candidates: cands}, "шаг %s, порт %s: тип %s несовместим с выходом %q (%s)", st.ID, portName, port.Type, src.Name, src.Type)
				}
			}
			prior[st.ID] = priorStep{step: st, manifest: m}
			continue
		}
		m, err := eng.LoadManifest(st.Plugin)
		if err != nil {
			v.err(E_PLUGIN_LOAD, st.ID, "", "pipeline.steps."+st.ID+".plugin", "проверьте путь к плагину и plugin.yaml", nil, "шаг %s: %s", st.ID, err.Error())
			continue
		}
		for b := range st.Bind {
			if _, ok := m.Input[b]; !ok {
				ports := portNameList(m.Input)
				v.err(E_BIND_UNKNOWN_PORT, st.ID, b, "pipeline.steps."+st.ID+".bind."+b, fmt.Sprintf("порты плагина: %s", strings.Join(ports, ", ")), &Fix{Op: "bind", Target: "steps." + st.ID + "." + b, Candidates: ports}, "шаг %s: bind указывает на несуществующий порт %q (порты: %s)", st.ID, b, portNames(m.Input))
			}
		}
		// Источник bind обязан быть ссылкой на контекст ранда. Литерал (например
		// top_n: 1) не резолвится никогда: для optional-порта движок его молча
		// пропускал, плагин брал значение по умолчанию, и ран завершался "done",
		// хотя YAML задавал другое. Это ошибка пайплайна, а не предупреждение.
		for b, src := range st.Bind {
			if isContextRef(src) {
				continue
			}
			v.err(E_BIND_SOURCE_INVALID, st.ID, b, "pipeline.steps."+st.ID+".bind."+b,
				"укажите input.<поле> или steps.<шаг>.<выход>", nil,
				"шаг %s, порт %s: bind = %q не является ссылкой на контекст "+
					"(нужно input.* или steps.*; литерал молча игнорируется, плагин возьмёт значение по умолчанию)",
				st.ID, b, src)
		}
		// Путь-литерал в bind при плагине без filesystem: readwrite почти всегда
		// означает отказ на запуске (path_escape), хотя валидатор знает и
		// значение, и манифест. Предупреждение, не ошибка: плагину с readwrite
		// абсолютный путь принимать можно.
		warnHostPathInBind(&v, pf, st, m)
		if len(m.Permissions.Network) > 0 {
			hosts := NetworkHosts(m)
			// Тот же отказ, что и в рантайме (network_not_enforceable):
			// валидатор не должен зелёным светом пускать пайплайн, который
			// ран затем откажет. Обе ветки независимы: шаг может быть плохим
			// и по сети, и по bind, и обе проверки должны прозвучать.
			notEnforceable := EffectiveNetwork(p) == NetworkAllow && NetworkHasStructuredDeclarations(m)
			switch {
			case EffectiveNetwork(p) == NetworkDeny:
				v.err(E_NETWORK_DENIED, st.ID, "", "pipeline.steps."+st.ID, "уберите сеть из манифеста или укажите network: allow", nil, "шаг %s: плагин %s заявил сеть (%s), а %s", st.ID, st.Plugin, hosts, networkDenyBecause(p.Network))
			case notEnforceable:
				v.err(E_NETWORK_NOT_ENFORCEABLE, st.ID, "", "pipeline.steps."+st.ID, "объявите any_host: true (точечный фильтр не реализован) или уберите сеть", nil, "шаг %s: плагин %s заявил сеть по host:port (%s), но точечный egress-фильтр не реализован — полный доступ вместо списка хостов выдавать нельзя", st.ID, st.Plugin, hosts)
			default:
				v.warn(W_NETWORK_DECLARED, st.ID, "", "pipeline.steps."+st.ID, "аудит сети — в журнале запуска", nil, "шаг %s: плагин заявил сеть: %s (declare-now, аудит — журнал)", st.ID, hosts)
			}
		}
		for portName, port := range m.Input {
			srcPath := PortSource(portName, port, st)
			if srcPath == "" {
				if port.Optional {
					v.warn(W_PORT_OPTIONAL_UNBOUND, st.ID, portName, "pipeline.steps."+st.ID+".bind."+portName, "optional-порт можно оставить без привязки", nil, "шаг %s, порт %s: нет привязки (optional)", st.ID, portName)
					continue
				}
				cands := compatibleOutputs(prior, port.Type, port.Format)
				cands = append(cands, inputFieldList(pf)...)
				v.err(E_PORT_UNBOUND, st.ID, portName, "pipeline.steps."+st.ID+".bind."+portName, "привяжите порт через bind", &Fix{Op: "bind", Target: "steps." + st.ID + "." + portName, Candidates: cands}, "шаг %s, порт %s: нет привязки", st.ID, portName)
				continue
			}
			src, code, perr := resolveSourceCoded(srcPath, prior, pf, st)
			if perr != "" {
				if port.Optional {
					v.warn(W_PORT_OPTIONAL_SOURCE, st.ID, portName, srcPath, "optional-порт: источник может отсутствовать", nil, "шаг %s, порт %s: %s (optional)", st.ID, portName, perr)
				} else {
					fixCands := append(inputFieldList(pf), compatibleOutputs(prior, port.Type, port.Format)...)
					v.err(code, st.ID, portName, srcPath, fmt.Sprintf("доступные поля: %s", strings.Join(inputFieldList(pf), ", ")), &Fix{Op: "bind", Target: "steps." + st.ID + "." + portName, Candidates: fixCands}, "шаг %s, порт %s: %s", st.ID, portName, perr)
				}
				continue
			}
			src.Name = srcPath
			// Пустое значение в input.* проходило валидацию так же чисто, как
			// заполненное: бейдж показывал «валиден», а ран падал уже в плагине
			// (maigret: empty_username). Значение вводит человек, и пустой вход
			// почти всегда — забытый ввод, а не намерение. Предупреждение, не
			// ошибка: пустое значение законно там, где поле необязательное или
			// значение подставляется при запуске.
			if !port.Optional && isEmptySourceValue(src, pf) {
				v.warn(W_PORT_EMPTY_SOURCE, st.ID, portName, srcPath, "введите значение на слое «Входы» или отвяжите порт", nil, "шаг %s, порт %s: значение %s пустое — плагин получит пустую строку", st.ID, portName, srcPath)
			}
			if src.Type != "" && port.Type != "" && src.Type != port.Type {
				cands := compatibleOutputs(prior, port.Type, port.Format)
				v.err(E_TYPE_MISMATCH, st.ID, portName, srcPath, fmt.Sprintf("нужен тип %q, пришёл %q", port.Type, src.Type), &Fix{Op: "bind", Target: "steps." + st.ID + "." + portName, Candidates: cands}, "шаг %s, порт %s: тип %s несовместим с выходом %q (%s)", st.ID, portName, port.Type, src.Name, src.Type)
			}
			literalChecked := false
			if src.Literal != nil && port.Format != "" {
				if s, isStr := src.Literal.(string); isStr {
					literalChecked = true
					if !scalarMatchesFormat(s, port.Format) {
						v.err(E_FORMAT_INPUT, st.ID, portName, srcPath, fmt.Sprintf("значение не соответствует формату %q", port.Format), nil, "шаг %s, порт %s: значение input %q не соответствует формату %q", st.ID, portName, s, port.Format)
					}
				}
			}
			if !literalChecked && !formatsCompatible(src.Format, port.Format) {
				v.err(E_FORMAT_MISMATCH, st.ID, portName, srcPath, fmt.Sprintf("источник даёт %q, а нужно %q", src.Format, port.Format), &Fix{Op: "bind", Target: "steps." + st.ID + "." + portName, Candidates: compatibleOutputs(prior, port.Type, port.Format)}, "шаг %s, порт %s: формат источника %q не покрывает %q", st.ID, portName, src.Format, port.Format)
			}
			if src.Step != nil && src.Step.OnError == "skip" && !port.Optional {
				v.err(E_OPTIONAL_REQUIRED, st.ID, portName, srcPath, "объявите порт optional или уберите on_error: skip у источника", &Fix{Op: "set", Target: "steps." + st.ID + "." + portName + ".optional", Candidates: []string{"true"}}, "шаг %s, порт %s: читает из skip-able шага %s — объявите optional", st.ID, portName, src.Step.ID)
			}
		}
		prior[st.ID] = priorStep{step: st, manifest: m}
	}
	groupLast := map[string]int{}
	groupSize := map[string]int{}
	for i := range p.Steps {
		g := p.Steps[i].ParallelGroup
		if g == "" {
			continue
		}
		groupSize[g]++
		if last, ok := groupLast[g]; ok && i != last+1 {
			v.err(E_PARALLEL_SPLIT, p.Steps[i].ID, "", "pipeline.steps."+p.Steps[i].ID+".parallel_group", "поставьте шаги группы рядом в списке", nil, "parallel_group %q: шаги группы должны быть рядом в списке (шаг %s отделён от группы)", g, p.Steps[i].ID)
		}
		groupLast[g] = i
	}
	for g, n := range groupSize {
		if n == 1 {
			v.warn(W_PARALLEL_SINGLE, "", "", "pipeline.parallel_group."+g, "уберите parallel_group или добавьте шаги", nil, "parallel_group %q: один шаг — параллелизм бессмыслен", g)
		}
		if n > MaxParallelWidth {
			v.err(E_PARALLEL_LIMIT, "", "", "pipeline.parallel_group."+g, "слишком широк", nil, "parallel_group %q: %d шагов, максимум %d", g, n, MaxParallelWidth)
		}
	}
	pluginSecrets := map[string]bool{}
	for _, ps := range prior {
		for _, s := range ps.manifest.Permissions.Secrets {
			pluginSecrets[s] = true
		}
	}
	pipelineSecrets := map[string]bool{}
	for _, k := range p.Secrets {
		pipelineSecrets[k] = true
	}
	for _, k := range p.Secrets {
		if !pluginSecrets[k] {
			v.warn(W_SECRETS_UNUSED, "", "", "pipeline.secrets."+k, "уберите лишнее или проверьте permissions плагинов", nil, "secrets: пайплайн объявляет %s, но ни один плагин не заявляет её в permissions.secrets", k)
		}
	}
	var undeclared []string
	for s := range pluginSecrets {
		if !pipelineSecrets[s] {
			undeclared = append(undeclared, s)
		}
	}
	sort.Strings(undeclared)
	for _, s := range undeclared {
		v.warn(W_SECRETS_UNDECLARED, "", "", "pipeline.secrets", "объявите ключ в pipeline.secrets", &Fix{Op: "declare", Target: "pipeline.secrets", Candidates: undeclared}, "secrets: плагину нужен ключ %s — объявите в pipeline secrets (иначе может не быть в env при запуске)", s)
	}
	return v.issues
}

// isContextRef — источник bind является ссылкой на контекст ранда. В контексте
// лежат только input.* и steps.*, поэтому любой другой литерал нерезолвится.
func isContextRef(src string) bool {
	return strings.HasPrefix(src, "input.") || strings.HasPrefix(src, "steps.")
}

// staticInputKey — имя ключа pipeline.input по выражению вида input.foo[.bar].
// ok=false, если значение вычисляется в рантайме (шаги, when) и на момент
// валидации неизвестно.
func staticInputKey(srcPath string) (string, bool) {
	if !strings.HasPrefix(srcPath, "input.") {
		return "", false
	}
	key := strings.TrimPrefix(srcPath, "input.")
	if idx := strings.Index(key, "."); idx >= 0 {
		key = key[:idx]
	}
	if key == "" {
		return "", false
	}
	return key, true
}

// looksLikeHostPath — значение похоже на путь хоста, а не на относительный путь
// внутри рабочего каталога: абсолютный путь (в т.ч. UNC и Windows-диск) или
// явный выход из каталога через "..".
func looksLikeHostPath(s string) bool {
	if s == "" || strings.ContainsAny(s, "*?\n") {
		return false
	}
	normalized := strings.ReplaceAll(s, "\\", "/")
	if strings.HasPrefix(normalized, "/") {
		return true
	}
	if len(s) >= 2 && s[1] == ':' {
		return true // C:\... / c:/...
	}
	for _, seg := range strings.Split(normalized, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

// warnHostPathInBind — путь-литерал в bind при плагине без filesystem:
// readwrite почти всегда означает отказ на запуске: плагин резолвит пути
// относительно своего каталога/workspace, и абсолютный путь он не примет. Раньше
// это выяснялось только в рантайме (path_escape), хотя валидатор знает и
// значение, и манифест плагина. Это предупреждение, а не ошибка: плагин с
// readwrite вправе принять абсолютный путь.
func warnHostPathInBind(v *collector, pf *PipelineFile, st *Step, m *Manifest) {
	if m.Permissions.Filesystem == "readwrite" {
		return
	}
	for b := range st.Bind {
		port, known := m.Input[b]
		if !known {
			continue
		}
		key, ok := staticInputKey(PortSource(b, port, st))
		if !ok {
			continue
		}
		rawVal, ok := pf.Pipeline.Input[key]
		if !ok {
			continue
		}
		s, ok := rawVal.(string)
		if !ok || !looksLikeHostPath(s) {
			continue
		}
		v.warn(W_FILESYSTEM_HOST_PATH, st.ID, b, "pipeline.input."+key,
			"укажите путь относительно рабочего каталога", nil,
			"шаг %s, порт %s: значение %q — абсолютный путь или выход за пределы каталога, "+
				"а плагин %s объявил filesystem: %s и, скорее всего, отклонит его на запуске",
			st.ID, b, s, m.ID, m.Permissions.Filesystem)
	}
}

// LintIssues — ValidateIssues + проверка file_ref (файлы должны существовать).
// Коды: E_FILE_REF_NOT_FOUND, W_FILE_REF_ROOT.
func LintIssues(pf *PipelineFile, eng Engine, projectRoot string) []Issue {
	var v collector
	v.issues = append(v.issues, ValidateIssues(pf, eng)...)
	if projectRoot == "" {
		projectRoot, _ = os.Getwd()
	}
	for _, st := range pf.Pipeline.Steps {
		if IsBuiltin(st.Plugin) {
			continue
		}
		m, err := eng.LoadManifest(st.Plugin)
		if err != nil {
			continue
		}
		for portName, port := range m.Input {
			if port.Format != "file_ref" {
				continue
			}
			srcPath := PortSource(portName, port, &st)
			if srcPath == "" {
				continue
			}
			if !strings.HasPrefix(srcPath, "input.") {
				continue
			}
			key := strings.TrimPrefix(srcPath, "input.")
			if idx := strings.Index(key, "."); idx >= 0 {
				key = key[:idx]
			}
			rawVal, ok := pf.Pipeline.Input[key]
			if !ok {
				continue
			}
			s, ok := rawVal.(string)
			if !ok || s == "" {
				continue
			}
			path := "pipeline.steps." + st.ID + ".bind." + portName
			if filepath.IsAbs(s) {
				if _, err := os.Stat(s); err != nil {
					v.err(E_FILE_REF_NOT_FOUND, st.ID, portName, path, "укажите существующий абсолютный путь", nil, "шаг %s, порт %s (file_ref): файл %q не найден (abs): %v", st.ID, portName, s, err)
				}
				continue
			}
			pluginAbs, _ := filepath.Abs(st.Plugin)
			if _, err := os.Stat(filepath.Join(pluginAbs, s)); err == nil {
				continue
			}
			if _, err := os.Stat(filepath.Join(projectRoot, s)); err == nil {
				v.warn(W_FILE_REF_ROOT, st.ID, portName, path, "укажите путь относительно плагина или абсолютный", nil, "шаг %s, порт %s (file_ref): %q найден от корня проекта, но не от плагина (%s) — укажите путь относительно плагина или абсолютный", st.ID, portName, s, pluginAbs)
				continue
			}
			v.err(E_FILE_REF_NOT_FOUND, st.ID, portName, path, "положите файл рядом с плагином или укажите абсолютный путь", nil, "шаг %s, порт %s (file_ref): файл %q не найден ни от плагина (%s) ни от корня (%s)", st.ID, portName, s, pluginAbs, projectRoot)
		}
	}
	return v.issues
}

// resolveSourceCoded — resolveSource + код Issue. Все ошибки резолва
// источника (нет поля input, шаг не выше, плагин не объявляет выход,
// isEmptySourceValue — значение, дошедшее до порта, пустое: пустая строка,
// ноль или пустой список/объект. Отличать «не задано вовсе» (это ловит
// E_PORT_UNBOUND) от «задано пустое».
func isEmptySourceValue(src srcInfo, pf *PipelineFile) bool {
	switch val := src.Literal.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(val) == ""
	case []interface{}:
		return len(val) == 0
	case map[string]interface{}:
		return len(val) == 0
	default:
		return false
	}
}

// v0.35: валидация cron — 5 полей, каждое */,-,цифры
func isValidCron(cron string) bool {
	parts := strings.Fields(cron)
	if len(parts) != 5 {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
		for _, ch := range p {
			if !(ch >= '0' && ch <= '9' || ch == '*' || ch == '/' || ch == '-' || ch == ',' ) {
				return false
			}
		}
	}
	return true
}

func validateTimezone(tz string) error {
	if tz == "Local" || tz == "UTC" || tz == "local" {
		return nil
	}
	// IANA: содержит "/" или известное сокращение
	if strings.Contains(tz, "/") {
		return nil
	}
	// разрешаем Europe/Minsk, Asia/Tokyo и т.п.
	if _, err := time.LoadLocation(tz); err == nil {
		return nil
	}
	return fmt.Errorf("неизвестная таймзона")
}

// кривой путь) — E_PORT_SOURCE: для агента это один класс «перепривяжи».
func resolveSourceCoded(path string, prior map[string]priorStep, pf *PipelineFile, st *Step) (srcInfo, string, string) {
	src, perr := resolveSource(path, prior, pf, st)
	if perr != "" {
		return src, E_PORT_SOURCE, perr
	}
	return src, "", ""
}
