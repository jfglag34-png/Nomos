// Файл genmanifest.go — решение «что именно записать в манифест» и рендер
// plugin.yaml / requirements.lock.
//
// Манифест плагина проверяется по schemas/manifest.schema.json с
// additionalProperties: false, поэтому здесь пишутся ТОЛЬКО поля, которые есть
// в схеме и в структуре pipeline.Manifest. В частности, точек входа донора в
// манифесте нет и быть не может: такого поля нет ни в схеме, ни в
// структуре, а KnownFields(true) отверг бы лишнее поле с ошибкой разбора.
// Информация о доноре едет в main.py (комментариями и константами) и в README.

package importer

import (
	"fmt"
	"path/filepath"
	"strings"
)

// maxDescription — потолок для поля description. Описание донора из
// метаданных бывает абзацем, а манифест читают в списке плагинов, где абзац
// превращается в простыню. Обрезка не молчит: Generate кладёт предупреждение,
// а в само описание добавляет многоточие.
const maxDescription = 300

// skeleton — решения генерации, принятые ДО записи файлов. Один раз
// посчитано, дальше только рендерится: чтобы README и манифест не могли
// разойтись в утверждениях об одном и том же.
type skeleton struct {
	id          string
	description string
	// descFromDonor — description взят из метаданных донора, а не написан
	// импортёром. README обязан сказать об этом: описание донора, стоящее в
	// поле description плагина, читается как обещание функциональности, которой
	// в плагине нет.
	descFromDonor bool
	// donorReadme — путь к README донора. В манифест он не идёт (такого поля
	// нет в схеме), но ссылка на него в README плагина — единственное место,
	// где человек узнает, что донор вообще делает.
	donorReadme string
	// specs — строки runtime.requires. Либо ВСЕ зависимости плана, либо
	// ни одной: частичный список в манифесте выглядит готовым и падает
	// позже, в рантайме, сообщением об ImportError, которое ничего не
	// говорит человеку о причине.
	specs []string
	// donorRoot — каталог, который main.py добавит в sys.path.
	donorRoot string
	// donorModule/donorAttr/donorEntry — выбранная точка входа донора в виде
	// «модуль» / «функция» / «модуль:функция». Пустые, если точку выбрать
	// было не на чем.
	donorModule string
	donorAttr   string
	donorEntry  string
}

// decide принимает решения генерации и дополняет res. Порядок: зависимости,
// каталог донора, точка входа, описание — от того, что ломает плагин на
// запуске, к тому, что видно только в описании.
//
// Блокеры не подавляются, даже если план их уже наговорил: план описывает
// проблему чужого проекта, здесь — что из-за неё НЕ сделано. Формулировки
// разные и обе нужны.
func decide(plan ImportPlan, id string, opts GenerateOptions, res *Result) skeleton {
	skel := skeleton{id: id, donorReadme: plan.DescriptionFile}
	decideRequirements(plan, &skel, res)
	decideDonor(plan, &skel, res)
	decideDescription(plan, &skel, opts, res)
	return skel
}

// decideRequirements — единственное место, где решается судьба
// requirements.lock.
//
// Lock создаётся ТОЛЬКО когда записываемы все зависимости. Причина в том, что
// строки lock обязаны совпадать с runtime.requires построчно
// (internal/pipeline/validator.go), а в requires попадает ровно то, что
// ManifestSpec признал представимым, то есть `имя==версия`. Половина списка
// дала бы lock, который валидатор примет, а плагин — нет.
func decideRequirements(plan ImportPlan, skel *skeleton, res *Result) {
	specs, notWritable := manifestSpecs(plan)
	if len(plan.Requirements) == 0 && len(plan.Unresolved) == 0 {
		// Зависимостей нет — писать нечего, и это не блокер: плагину без
		// зависимостей не нужно ни pip, ни окружение.
		return
	}
	if len(notWritable) == 0 {
		skel.specs = specs
		return
	}
	res.Blockers = append(res.Blockers, fmt.Sprintf(
		"зависимости не запинены — requirements.lock не создан, runtime.requires пуст: "+
			"не записано %d (%s). Схема принимает в runtime.requires ровно `пакет==версия`, "+
			"а lock обязан совпадать с requires построчно; записывать половину списка нельзя — "+
			"плагин упал бы позже, в рантайме. Сначала разрешите версии в окружении проекта (pip freeze)",
		len(notWritable), strings.Join(notWritable, ", ")))
}

// manifestSpecs делит зависимости плана на записываемые и не записываемые.
//
// Unresolved обходятся вместе с Requirements, но не складываются в него:
// у плана это подмножество, и без проверки на дубли каждый незапиненный пакет
// попал бы в сообщение дважды — а сообщение читает человек, и «24 из 12» в нём
// выглядит как ошибка счёта, то есть как ненадёжный во всём остальном.
//
// Порядок сохраняется: манифест и lock читает человек, и детерминированный
// порядок нужен, чтобы diff двух запусков не показывал изменений, которых
// не было.
func manifestSpecs(plan ImportPlan) (specs []string, notWritable []string) {
	seenSpec := map[string]bool{}
	seenLabel := map[string]bool{}
	add := func(list []Requirement) {
		for _, req := range list {
			if spec, ok := req.ManifestSpec(); ok {
				if seenSpec[spec] {
					continue
				}
				seenSpec[spec] = true
				specs = append(specs, spec)
				continue
			}
			label := requirementLabel(req)
			if seenLabel[label] {
				continue
			}
			seenLabel[label] = true
			notWritable = append(notWritable, label)
		}
	}
	add(plan.Requirements)
	add(plan.Unresolved)
	return specs, notWritable
}

// requirementLabel — как называть зависимость в сообщении. У неразобранной
// строки имени нет, и молча выкинуть её из списка нельзя: человек должен
// видеть исходный текст, который не получилось записать.
func requirementLabel(req Requirement) string {
	if req.Name != "" {
		return req.Name
	}
	if strings.TrimSpace(req.Raw) != "" {
		return req.Raw
	}
	return "(зависимость без имени)"
}

// decideDonor решает, что main.py будет делать с модулем чужого проекта.
func decideDonor(plan ImportPlan, skel *skeleton, res *Result) {
	skel.donorRoot = plan.ModuleImportPath
	switch {
	case strings.TrimSpace(skel.donorRoot) == "":
		res.Blockers = append(res.Blockers,
			"нечем добавить каталог донора в sys.path — main.py не сможет импортировать модуль проекта; "+
				"укажите DONOR_ROOT вручную (или задайте переменную WEDRA_DONOR_ROOT при запуске)")
	case !filepath.IsAbs(skel.donorRoot):
		res.Blockers = append(res.Blockers, fmt.Sprintf(
			"каталог донора %q относительный, а плагин запускается из своей папки: разрешить его нечем; "+
				"укажите абсолютный путь в DONOR_ROOT вручную или задайте WEDRA_DONOR_ROOT",
			skel.donorRoot))
	}

	switch {
	case len(plan.EntryPoints) == 0:
		res.Blockers = append(res.Blockers,
			"точка входа донора не выбрана (в проекте их нет): функцию надо выбрать вручную в редакторе плагинов")
	default:
		ep, ok := pickEntryPoint(plan.EntryPoints)
		if !ok {
			res.Blockers = append(res.Blockers,
				"точка входа донора не выбрана автоматически: функцию надо выбрать вручную в редакторе плагинов")
			return
		}
		module, attr, _ := strings.Cut(ep.Target, ":")
		skel.donorModule, skel.donorAttr, skel.donorEntry = module, attr, ep.Target
		res.Blockers = append(res.Blockers, fmt.Sprintf(
			"функция донора %s найдена и main.py её загружает, но НЕ вызывает: сигнатуру импортёр не проверял, "+
				"а выдуманный вызов через месяц выглядел бы как сломанный плагин. "+
				"Вызов, разбор ответа и распределение по портам манифеста напишите вручную в редакторе плагинов",
			ep.Target))
	}
}

// pickEntryPoint выбирает точку входа, которую честно назвать можно. То есть
// любую, кроме похожей на CLI: плагину нужен «прими JSON — верни JSON», а
// команда `cli` ждёт sys.argv и возвращает код возврата. Эвристика «похоже на
// CLI» та же, что у плана (looksLikeCLI) — своя копия разошлась бы с ним и
// через месяц сказала бы другое на том же проекте.
func pickEntryPoint(eps []EntryPoint) (EntryPoint, bool) {
	for _, ep := range eps {
		if looksLikeCLI(ep) {
			continue
		}
		if !strings.Contains(ep.Target, ":") {
			continue
		}
		return ep, true
	}
	return EntryPoint{}, false
}

// decideDescription готовит поле description.
//
// Описание донора описывает донора, а не плагин. Подставить его можно только
// с оговоркой, и оговорка эта едет и в предупреждения, и в README: иначе
// плагин «про Telegram outreach» появится в списке и будет обещать чужую
// функциональность.
func decideDescription(plan ImportPlan, skel *skeleton, opts GenerateOptions, res *Result) {
	desc := strings.TrimSpace(opts.Description)
	fromOptions := desc != ""
	if desc == "" {
		desc = strings.TrimSpace(plan.Description)
	}
	switch {
	case desc == "":
		desc = fmt.Sprintf("Плагин-импорт проекта %s: адаптер создан, вызов функции донора не дописан", skel.id)
		res.Blockers = append(res.Blockers,
			"описания у донора нет — description в манифесте написан импортёром; замените его своим текстом")
	case !fromOptions:
		skel.descFromDonor = true
		res.Warnings = append(res.Warnings,
			"description в манифесте взято из метаданных донора и описывает донора, а не плагин")
	}
	if runes := []rune(desc); len(runes) > maxDescription {
		desc = strings.TrimSpace(string(runes[:maxDescription])) + " …"
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"описание донора длиннее %d символов — в манифест попал обрезанный текст", maxDescription))
	}
	skel.description = desc
}

// renderManifest собирает plugin.yaml. Порядок полей — как в шаблоне
// internal/core/plugincreate.go, чтобы два генератора выглядели родными.
func renderManifest(skel skeleton) string {
	var b strings.Builder
	b.WriteString("# Манифест плагина — контракт с ядром (подробнее: protocol/v0.2/PROTOCOL.md)\n")
	b.WriteString("# Сгенерировано импортёром чужого Python-проекта. То, что не удалось\n")
	b.WriteString("# догадаться, перечислено в README этого каталога, а не спрятано здесь.\n")
	b.WriteString("id: " + skel.id + "\n")
	b.WriteString("version: 0.1.0              # семвер плагина; версия ДОНОРА здесь не записывается\n")
	b.WriteString("platform_api: \"^0.1\"        # диапазон совместимости с ядром\n")
	b.WriteString("description: " + yamlQuoted(skel.description) + "\n")
	b.WriteString("\n")
	b.WriteString("runtime:\n")
	b.WriteString("  type: python              # python | binary | ...\n")
	b.WriteString("  entry: main.py\n")
	if len(skel.specs) == 0 {
		b.WriteString("  requires: []              # пусто: зависимости не запинены, см. README\n")
	} else {
		b.WriteString("  requires:\n")
		for _, spec := range skel.specs {
			b.WriteString("    - " + spec + "\n")
		}
	}
	b.WriteString("\n")
	b.WriteString("# Порты-заглушки. Их имена и типы никто не проверял: как только вызов\n")
	b.WriteString("# функции донора будет написан, переименуйте их под реальные данные.\n")
	b.WriteString("# Пока выход пустой, плагин честно возвращает доменную ошибку, а не ok.\n")
	b.WriteString("input:\n")
	b.WriteString("  request:\n")
	b.WriteString("    from: input.request     # путь в контексте: input.* или steps.<шаг>.<поле>\n")
	b.WriteString("    type: object\n")
	b.WriteString("\n")
	b.WriteString("output:\n")
	b.WriteString("  payload:\n")
	b.WriteString("    type: object            # ядро требует непустой output (minProperties: 1)\n")
	b.WriteString("\n")
	b.WriteString("# Декларативные разрешения (уровень L0). Импортёр НЕ угадывает, что делает\n")
	b.WriteString("# донор: пустой список — это не «донону разрешено всё», а «человек ещё не\n")
	b.WriteString("# разобрался». Заполните перед установкой, иначе сеть будет отрезана.\n")
	b.WriteString("permissions:\n")
	b.WriteString("  network: []\n")
	b.WriteString("  filesystem: none\n")
	b.WriteString("  secrets: []\n")
	return b.String()
}

// renderRequirementsLock — содержимое lock. Только записи: валидатор
// (internal/pipeline/validator.go) сверяет каждую непустую строку с
// runtime.requires и любую лишнюю строку считает ошибкой.
func renderRequirementsLock(specs []string) string {
	var b strings.Builder
	b.WriteString("# Точные версии зависимостей плагина.\n")
	b.WriteString("# Строки обязаны совпадать с runtime.requires в plugin.yaml построчно —\n")
	b.WriteString("# это проверяет internal/pipeline/validator.go.\n")
	for _, spec := range specs {
		b.WriteString(spec + "\n")
	}
	return b.String()
}

// yamlQuoted оборачивает произвольный текст в двойные кавычки YAML. Описание
// приходит из чужого файла, где оно может содержать что угодно, включая
// двоеточие, решётку и перевод строки: без экранирования описание тихо
// развалило бы манифест или, что хуже, прочиталось как другой YAML.
func yamlQuoted(value string) string {
	return `"` + escapeDoubleQuoted(value) + `"`
}
