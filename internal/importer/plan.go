package importer

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// EntryPoint — точка входа чужого проекта в виде, в каком она объявлена.
type EntryPoint struct {
	// Name — имя команды из [project.scripts] («telegram-outreach»).
	Name string
	// Target — «модуль:функция» («telegram_outreach.main:cli»).
	Target string
}

// ImportPlan — результат анализа: что можно взять в плагин, что требует
// решения человека и что прямо мешает. Ничего из этого ещё не записано на
// диск — план только описывает следующий шаг.
//
// Warnings и Blockers — разные уровни: warning это «проверь, что так и
// задумано», blocker это «автоматический импорт в плагин не получится».
type ImportPlan struct {
	// PluginID — идентификатор плагина, пригодный для id в манифесте и имени
	// каталога: snake_case по правилу internal/core/plugincreate.go.
	PluginID string
	// Requirements — все разобранные зависимости (основные плюс выбранная
	// необязательная группа).
	Requirements []Requirement
	// Unresolved — те, у которых нет точного пина `==версия`. В
	// `runtime.requires` они попасть не могут: схема требует ровно `==`.
	Unresolved []Requirement
	// EntryPoints — точки входа из [project.scripts].
	EntryPoints []EntryPoint
	// ModuleImportPath — каталог, который надо добавить в sys.path (или в
	// PYTHONPATH), чтобы модули чужого проекта нашлись. Для src-layout это
	// `src`, для flat — корень проекта. Без него плагин не импортируется.
	ModuleImportPath string
	Warnings         []string
	Blockers         []string
}

// pluginIDRe — правило id плагина из internal/core/plugincreate.go
// (`^[a-z][a-z0-9_]*$`). Копия нужна, чтобы план не предлагал id, который
// потом отвергнет создание плагина.
var pluginIDRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// targetRe — «модуль:функция»: модуль может быть точечным
// («telegram_outreach.main»), функция может быть со схемой атрибутов
// («cli.command»). Всё остальное — не точка входа, а опечатка.
var targetRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*:[A-Za-z_][A-Za-z0-9_.]*$`)

// cliLikeTargets — имена, по которым функция с высокой вероятностью является
// CLI-службой. Это ЭВРИСТИКА, а не факт, и она специально узкая: в мире
// плагинов «прими JSON — верни JSON» функцию часто зовут run, поэтому run в
// список не внесён. Ошибка в обе стороны возможна (см. Plan), поэтому
// срабатывание формулируется как «похоже», и человек может это снять.
var cliLikeTargets = map[string]bool{
	"cli":             true,
	"main_cli":        true,
	"console_main":    true,
	"console":         true,
	"__main__":        true,
	"serve":           true,
	"serve_forever":   true,
	"run_server":      true,
	"start_server":    true,
	"start_service":   true,
	"launcher":        true,
	"launch":          true,
	"main_app":        true,
	"application_cli": true,
}

// Plan превращает метаданные в план импорта. Ошибок не возвращает: проблемы
// перечисляются в Blockers и Warnings, потому что решение «импортировать с
// блокерами, починив их по пути» — это тоже решение, и человек должен видеть
// список, а не только первый отказ.
func Plan(p *Project) ImportPlan {
	var plan ImportPlan
	if p == nil {
		plan.Blockers = append(plan.Blockers, "проект не задан (nil) — планировать нечего")
		return plan
	}

	plan.Warnings = append(plan.Warnings, p.notes...)
	plan.PluginID = pluginIDFor(p)

	for _, raw := range selectedRequirements(p, &plan) {
		req, err := ParseRequirement(raw)
		if err != nil {
			// Строку, которую не разобрали, всё равно показываем: иначе
			// зависимость исчезнет из плана и человек её не увидит.
			plan.Requirements = append(plan.Requirements, Requirement{Raw: raw})
			plan.Blockers = append(plan.Blockers, fmt.Sprintf("зависимость %q не разобралась: %v", raw, err))
			continue
		}
		plan.Requirements = append(plan.Requirements, req)
	}

	for _, req := range plan.Requirements {
		if !req.IsPinned {
			plan.Unresolved = append(plan.Unresolved, req)
			plan.Blockers = append(plan.Blockers, unresolvedReason(req))
			continue
		}
		if _, ok := req.ManifestSpec(); !ok {
			// Пин есть, а записать его нечем: экстра-функция, маркер или
			// версия, которую шаблон манифеста не принимает. Отдельным
			// сообщением, потому что «просто убери экстру» — это тихая
			// потеря зависимости.
			plan.Blockers = append(plan.Blockers, fmt.Sprintf(
				"%q зафиксирован на %s, но в runtime.requires не записывается: %s",
				req.Name, req.Version, manifestSpecReason(req)))
		}
	}

	if len(plan.Requirements) > 0 {
		plan.Warnings = append(plan.Warnings,
			"рядом с манифестом понадобится requirements.lock с ровно теми же строками — валидатор сверяет их построчно (internal/pipeline/validator.go)")
	}

	plan.EntryPoints = entryPointsOf(p, &plan)
	plan.ModuleImportPath = importPathOf(p, &plan)

	if p.RequiresPython != "" {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf(
			"проект требует Python %s; плагин запускается тем же интерпретатором, что в PATH, и версию мы не проверяем — exec в импортёре запрещён",
			p.RequiresPython))
	}
	if p.Version == "" {
		plan.Warnings = append(plan.Warnings,
			"версия проекта не объявлена — манифест WEDRA требует version, придётся поставить свою")
	}
	if p.DescriptionFile == "" {
		plan.Warnings = append(plan.Warnings,
			"README не найден — описание плагина придётся писать руками")
	}
	return plan
}

// selectedRequirements собирает строки зависимостей: основные плюс явно
// выбранная необязательная группа. Все группы молча не включаются — там
// pytest, ruff и прочие инструменты, которые плагину не нужны.
func selectedRequirements(p *Project, plan *ImportPlan) []string {
	raw := make([]string, 0, len(p.Dependencies)+8)
	seen := map[string]bool{}
	add := func(list []string, origin string) {
		for _, item := range list {
			// Ключ сравнения — имя пакета, а не исходная строка: `httpx==1.0`
			// и `HTTPX==2.0` это один и тот же пакет, и молча взять первый
			// вариант значит потерять второе ограничение без предупреждения.
			key := item
			if req, err := ParseRequirement(item); err == nil {
				key = req.Name
			}
			if key != "" && seen[key] {
				plan.Warnings = append(plan.Warnings,
					fmt.Sprintf("%s повторяется (%s) — беру один раз", item, origin))
				continue
			}
			seen[key] = true
			raw = append(raw, item)
		}
	}
	add(p.Dependencies, "dependencies")

	switch {
	case p.OptionalGroup == "":
		if groups := sortedKeys(p.OptionalDependencies); len(groups) > 0 {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf(
				"необязательные группы не включены (%s) — включаются только явно", strings.Join(groups, ", ")))
		}
	default:
		list, ok := p.OptionalDependencies[p.OptionalGroup]
		if !ok {
			plan.Blockers = append(plan.Blockers, fmt.Sprintf(
				"необязательная группа %q не заявлена в проекте — включать нечего", p.OptionalGroup))
			return raw
		}
		plan.Warnings = append(plan.Warnings,
			fmt.Sprintf("включена необязательная группа %q (%d шт.)", p.OptionalGroup, len(list)))
		add(list, "группа "+p.OptionalGroup)
	}
	return raw
}

// unresolvedReason объясняет, почему требование не годится в манифест. Формулировки
// разные не для красоты: ссылки на источник и маркеры требуют решения
// человека, а диапазон версий — разрешения окружением проекта.
func unresolvedReason(req Requirement) string {
	switch {
	case req.URL != "":
		return fmt.Sprintf(
			"%q ставится прямой ссылкой %s: в runtime.requires допустим только package==version, такую зависимость надо ставить за пределами манифеста",
			req.Name, req.URL)
	case req.Marker != "":
		return fmt.Sprintf(
			"%q зависит от маркера окружения (%q): условие решает человек, молча выбросить его или выполнить нельзя",
			req.Name, req.Marker)
	case req.Constraint == "":
		return fmt.Sprintf(
			"%q объявлен без версии вообще: в runtime.requires требуется package==version", req.Name)
	default:
		return fmt.Sprintf(
			"%q ограничен диапазоном %q: из диапазона нельзя получить одну версию, а runtime.requires требует ровно package==version — сначала разрешите версии в окружении проекта (pip freeze)",
			req.Name, req.Constraint)
	}
}

// manifestSpecReason — почему зафиксированное требование всё равно не
// записывается. Совпадает с тем, что проверяет ManifestSpec.
func manifestSpecReason(req Requirement) string {
	switch {
	case len(req.Extras) > 0:
		return fmt.Sprintf(
			"в нём есть экстра-функции %s, а шаблон runtime.requires скобок не допускает и requirements.lock обязан совпадать с requires построчно — экстра-функцию в манифесте выразить нечем",
			strings.Join(req.Extras, ","))
	case req.Marker != "":
		return fmt.Sprintf("есть маркер окружения %q, а версии он не задаёт", req.Marker)
	default:
		return fmt.Sprintf("строка %q не проходит шаблон runtime.requires из schemas/manifest.schema.json", req.Name+"=="+req.Version)
	}
}

// entryPointsOf строит список точек входа и проверяет их форму. Порядок
// детерминированный (по имени команды) — иначе два запуска на одном проекте
// давали бы разный вывод.
func entryPointsOf(p *Project, plan *ImportPlan) []EntryPoint {
	names := sortedKeys(p.Scripts)
	usable := make([]EntryPoint, 0, len(names))
	cliLike := make([]EntryPoint, 0, len(names))
	for _, name := range names {
		target := strings.TrimSpace(p.Scripts[name])
		ep := EntryPoint{Name: name, Target: target}
		if !targetRe.MatchString(target) {
			plan.Blockers = append(plan.Blockers, fmt.Sprintf(
				"точка входа %q: %q не похоже на \"модуль:функция\"", name, target))
			continue
		}
		usable = append(usable, ep)
		if looksLikeCLI(ep) {
			cliLike = append(cliLike, ep)
		}
	}

	switch {
	case len(names) == 0:
		plan.Blockers = append(plan.Blockers,
			"точек входа нет: [project.scripts] пуст, а плагину нужна функция «прими JSON — верни JSON»; точку входа придётся написать руками")
	case len(usable) == 0:
		// Все цели не разобраны, блокеры по ним уже добавлены.
	case len(cliLike) == len(usable):
		plan.Blockers = append(plan.Blockers, fmt.Sprintf(
			"все точки входа похожи на CLI (%s): контракт плагина WEDRA — JSON на входе и JSON на выходе, а не командная строка; нужна обёртка",
			strings.Join(targetList(cliLike), ", ")))
	case len(cliLike) > 0:
		plan.Warnings = append(plan.Warnings, fmt.Sprintf(
			"часть точек входа похожа на CLI (%s) — если это не так, план можно принять как есть",
			strings.Join(targetList(cliLike), ", ")))
	}
	return usable
}

func targetList(eps []EntryPoint) []string {
	out := make([]string, 0, len(eps))
	for _, ep := range eps {
		out = append(out, ep.Target)
	}
	return out
}

// looksLikeCLI — эвристика «похоже на CLI», см. cliLikeTargets. Смотрит и на
// имя функции, и на имя модуля: `pkg.__main__:cli` указывает на запуск как
// программы, а не на вызов из конвейера.
func looksLikeCLI(ep EntryPoint) bool {
	module, function, _ := strings.Cut(ep.Target, ":")
	if cliLikeTargets[strings.ToLower(function)] {
		return true
	}
	parts := strings.Split(module, ".")
	return len(parts) > 1 && cliLikeTargets[strings.ToLower(parts[len(parts)-1])]
}

// importPathOf считает каталог для sys.path. Неизвестная раскладка — всегда
// блокер: что бы ни было в проекте, модули его без пути не находятся. Если
// PackageRoot не задан (Plan вызван прямо на разобранном файле, без Inspect),
// путь остаётся относительным, и об этом говорится предупреждением, чтобы
// вызывающий не принял «src» за абсолютный путь.
func importPathOf(p *Project, plan *ImportPlan) string {
	switch p.Layout {
	case LayoutSrcLayout:
		if p.PackageRoot == "" {
			plan.Warnings = append(plan.Warnings,
				"PackageRoot не задан (Plan вызван без Inspect) — ModuleImportPath указан относительно корня проекта")
			return "src"
		}
		return filepath.Join(p.PackageRoot, "src")
	case LayoutFlat:
		return p.PackageRoot
	default:
		plan.Blockers = append(plan.Blockers,
			"раскладка исходников неизвестна: нечем добавить путь в sys.path (в проекте нет ни src/, ни пакета в корне)")
		return ""
	}
}

// pluginIDFor строит id плагина. Порядок проб: имя дистрибутива, потом имя
// каталога, потом фиксированная заглушка — результат обязан пройти
// ^[a-z][a-z0-9_]*$, иначе создание плагина его отвергнет.
func pluginIDFor(p *Project) string {
	candidates := []string{p.Name}
	if p.PackageRoot != "" {
		candidates = append(candidates, filepath.Base(filepath.Clean(p.PackageRoot)))
	}
	candidates = append(candidates, "imported_plugin")
	for _, candidate := range candidates {
		if id := sanitizePluginID(candidate); pluginIDRe.MatchString(id) {
			return id
		}
	}
	return "imported_plugin"
}

// sanitizePluginID приводит произвольную строку к snake_case. Не-ASCII
// выбрасывается, а не транслитерируется: транслитерация даёт два разных id
// для одной и той же кириллицы в зависимости от таблицы, и это хуже, чем
// явная потеря.
func sanitizePluginID(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-', r == '_', r == '.', r == ' ', r == '+':
			b.WriteRune('_')
		}
	}
	id := b.String()
	for strings.Contains(id, "__") {
		id = strings.ReplaceAll(id, "__", "_")
	}
	id = strings.Trim(id, "_")
	if id == "" {
		return ""
	}
	// Правило плагина требует букву первой. Префикс p_ честнее, чем отказ или
	// транслитерация: он виден в выводе и не выдаётся за имя проекта.
	if id[0] < 'a' || id[0] > 'z' {
		return "p_" + id
	}
	return id
}
