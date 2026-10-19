package mcp

// Tool — описание инструмента для tools/list.
type Tool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"inputSchema"`
	// OutputSchema — схема структурированного результата (ревизия 2025-06-18).
	//
	// Спека: если схема объявлена, сервер ОБЯЗАН возвращать результат,
	// который ей соответствует. Поэтому схемы здесь только про то, что
	// действительно инвариант: имена и типы полей, которые есть всегда.
	// `required` не ставится там, где поле появляется не при каждом исходе
	// (run_pipeline при отказе валидации возвращает {ok, issues} без run_id) —
	// объявить его обязательным значило бы врать в контракте.
	OutputSchema map[string]interface{} `json:"outputSchema,omitempty"`
	// Annotations — подсказки хосту (ревизия 2025-06-18, добавлять их на
	// более старой ревизии нечестно, потому и версия поднята).
	//
	// Спека велит клиентам считать аннотации недоверенными, но именно по ним
	// хост решает, показывать ли человеку подтверждение перед вызовом. Без
	// них list_plugins (чтение каталога) и run_pipeline (запуск чужого кода)
	// выглядели для хоста одинаково.
	Annotations map[string]interface{} `json:"annotations,omitempty"`
}

func toolDefs() []Tool {
	obj := func(props map[string]interface{}, required ...string) map[string]interface{} {
		return map[string]interface{}{"type": "object", "properties": props, "required": required}
	}
	// annot — аннотации одним конструктором, чтобы «забыли поставить» было
	// видно в диффе, а не искалось по восьми литералам.
	//
	// readOnly — не меняет ничего на диске; destructive — может испортить
	// данные или запустить чужой код (спека считает destructive значением по
	// умолчанию, поэтому cancel_run объявляет false явно); idempotent —
	// повторный вызов с теми же аргументами даёт тот же результат;
	// openWorld — взаимодействует с внешними системами (у нас — сеть плагина).
	// objSchema — схема объекта с объявленными свойствами.
	objSchema := func(props map[string]interface{}, required ...string) map[string]interface{} {
		sch := map[string]interface{}{"type": "object", "properties": props}
		if len(required) > 0 {
			sch["required"] = required
		}
		return sch
	}
	strSch := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "string", "description": desc}
	}
	arrSch := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "array", "description": desc,
			"items": map[string]interface{}{"type": "object"}}
	}
	boolSch := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "boolean", "description": desc}
	}
	annot := func(readOnly, destructive, idempotent, openWorld bool) map[string]interface{} {
		return map[string]interface{}{
			"readOnlyHint":    readOnly,
			"destructiveHint": destructive,
			"idempotentHint":  idempotent,
			"openWorldHint":   openWorld,
		}
	}
	str := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "string", "description": desc}
	}
	num := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "number", "description": desc}
	}
	wait := num("сколько секунд ждать завершения (0 — вернуть сразу)")
	wait["minimum"] = 0
	wait["maximum"] = maxWaitSeconds
	return []Tool{
		{
			Name:        "compose_pipeline",
			Description: "Build and validate long pipelines (up to 256 steps) from installed plugins. First list_plugins and describe_plugin to discover actual ports. Stages run sequentially; steps within each stage run in parallel with a barrier. Explicit bind links data; do not invent plugin ports. Returns pipeline YAML and a fleet config repeating the WHOLE chain every N minutes with overlap protection. save writes new artifacts under workdir but NEVER activates schedules or auto-approves gates. Operator reviews and installs fleet config; alerts use fleet environment.",
			InputSchema: obj(map[string]interface{}{
				"name":          str("unique safe artifact name"),
				"every_minutes": map[string]interface{}{"type": "integer", "minimum": 1, "maximum": 525600},
				"timeout":       str("whole run timeout, e.g. 20m"),
				"input":         map[string]interface{}{"type": "object"},
				"stages":        map[string]interface{}{"type": "array", "minItems": 1, "maxItems": 256, "items": map[string]interface{}{"type": "object", "required": []string{"steps"}, "properties": map[string]interface{}{"steps": map[string]interface{}{"type": "array", "minItems": 1, "maxItems": 32, "items": map[string]interface{}{"type": "object", "required": []string{"id", "plugin"}, "properties": map[string]interface{}{"id": str("unique step id"), "plugin": str("installed plugin id"), "bind": map[string]interface{}{"type": "object", "additionalProperties": map[string]interface{}{"type": "string"}}}}}}}},
				"save":          boolSch("save new files without overwriting; default false"),
			}, "name", "every_minutes", "stages"),
			OutputSchema: objSchema(map[string]interface{}{"ok": boolSch("validation succeeded"), "activated": boolSch("always false"), "yaml": str("generated pipeline"), "fleet_yaml": str("generated schedule when valid"), "issues": arrSch("validation issues")}, "ok", "activated", "yaml", "issues"),
			Annotations:  annot(false, false, false, false),
		},
		{
			Name:        "list_plugins",
			Description: "Список плагинов: id, version, description, порты (type/format/optional), permissions",
			InputSchema: obj(map[string]interface{}{"filter": str("подстрока по id/description (опционально)")}),
			OutputSchema: objSchema(map[string]interface{}{
				"plugins": arrSch("плагины: id, version, description, порты, permissions"),
			}, "plugins"),
			Annotations: annot(true, false, true, false),
		},
		{
			Name:        "describe_plugin",
			Description: "Манифест плагина целиком",
			InputSchema: obj(map[string]interface{}{"id": str("id плагина или путь")}, "id"),
			OutputSchema: objSchema(map[string]interface{}{
				"id":      strSch("id плагина"),
				"version": strSch("версия плагина"),
				"input":   map[string]interface{}{"type": "object", "description": "входные порты"},
				"output":  map[string]interface{}{"type": "object", "description": "выходные порты"},
			}, "id", "version", "input", "output"),
			Annotations: annot(true, false, true, false),
		},
		{
			Name:        "validate_pipeline",
			Description: "Проверка пайплайна: {ok, issues[]}. ok:false — нормальный результат, не ошибка",
			InputSchema: obj(map[string]interface{}{
				"yaml": str("YAML пайплайна (или укажите path)"),
				"path": str("путь к YAML в workdir (или укажите yaml)"),
			}),
			OutputSchema: objSchema(map[string]interface{}{
				"ok":     boolSch("цепочка прошла проверку"),
				"issues": arrSch("замечания с кодами из ERRORS.md"),
			}, "ok", "issues"),
			Annotations: annot(true, false, true, false),
		},
		{
			Name:        "plan_pipeline",
			Description: "DAG пайплайна: порядок, параллельные группы, фазы",
			InputSchema: obj(map[string]interface{}{
				"yaml": str("YAML пайплайна"),
				"path": str("путь к YAML в workdir (альтернатива yaml)"),
			}),
			OutputSchema: objSchema(map[string]interface{}{
				"ok":       boolSch("цепочка прошла проверку"),
				"issues":   arrSch("замечания с кодами из ERRORS.md"),
				"pipeline": strSch("имя пайплайна"),
				"foreach":  strSch("выражение foreach, если оно есть"),
				"dag":      map[string]interface{}{"type": "object", "description": "граф шагов"},
			}, "ok", "issues"),
			Annotations: annot(true, false, true, false),
		},
		{
			Name: "run_pipeline",
			Description: "Запуск пайплайна: сначала validate, затем {run_id, status}. " +
				"Первый шаг с capabilities «сеть/запись на диск/чтение секретов» обязан идти после core/human_gate, иначе E_GATE_REQUIRED. " +
				"Решения гейтов через MCP невозможны — статус waiting_human означает «попросите пользователя одобрить в окне wedra»",
			InputSchema: obj(map[string]interface{}{
				"yaml":         str("YAML пайплайна (или path)"),
				"path":         str("путь к YAML в workdir"),
				"wait_seconds": wait,
			}),
			OutputSchema: objSchema(map[string]interface{}{
				"run_id": strSch("id рана"),
				"status": strSch("running | waiting_human | done | failed | cancelled"),
				"ok":     boolSch("при отказе валидации: false + issues"),
				"issues": arrSch("при отказе валидации: замечания"),
			}),
			Annotations: annot(false, true, false, true),
		},
		{
			Name:        "get_run",
			Description: "Статус рана, stats, новые события, выходы шагов (~20KB), pending_gate без токена",
			InputSchema: obj(map[string]interface{}{
				"run_id": str("id рана из run_pipeline"),
				"since":  num("события начиная с индекса (опционально)"),
			}, "run_id"),
			OutputSchema: objSchema(map[string]interface{}{
				"run_id":  strSch("id рана"),
				"status":  strSch("running | waiting_human | done | failed | cancelled"),
				"total":   map[string]interface{}{"type": "number", "description": "всего событий в журнале"},
				"events":  arrSch("события окна: total/first/next/truncated описывают границы"),
				"outputs": map[string]interface{}{"type": "object", "description": "снапшот выходов шагов"},
			}, "run_id", "status"),
			Annotations: annot(true, false, true, false),
		},
		{
			Name:        "cancel_run",
			Description: "Отмена рана (Фаза 5)",
			InputSchema: obj(map[string]interface{}{"run_id": str("id рана")}, "run_id"),
			OutputSchema: objSchema(map[string]interface{}{
				"run_id": strSch("id рана"),
				"status": strSch("cancelling; далее get_run → cancelled"),
			}, "run_id", "status"),
			// destructive=false явно: спека считает инструмент разрушающим по
			// умолчанию, а отмена ничего не разрушает.
			Annotations: annot(false, false, true, false),
		},
		{
			// Инструмент вызывался диспетчером, но не публиковался здесь, и
			// агент не мог открыть его через tools/list — только зная имя.
			// Публикуется сразу, потому что список инструментов обязан совпадать
			// с тем, что сервер принимает: иначе возможности не видно, а отказ
			// приходит ни с чем, что объяснило бы отсутствие.
			Name: "exec_plugin",
			Description: "Запуск плагина напрямую, без пайплайна. Выключен по умолчанию: " +
				"нужен флаг оператора --allow-agent-exec. Плагин с объявленными правами " +
				"(сеть/запись на диск/чтение секретов) не запускается: инструмент идёт мимо " +
				"human_gate, ждать некого — используйте run_pipeline с core/human_gate",
			InputSchema: obj(map[string]interface{}{
				"plugin":          str("id плагина или путь внутри workdir"),
				"input":           obj(map[string]interface{}{}),
				"timeout_seconds": num("таймаут, максимум 300"),
			}, "plugin"),
			OutputSchema: objSchema(map[string]interface{}{
				"output":  map[string]interface{}{"type": "object", "description": "выход плагина, проверенный контрактом"},
				"dropped": arrSch("поля, выброшенные enforce (не объявлены в манифесте)"),
			}, "output"),
			Annotations: annot(false, true, false, true),
		},
	}
}
