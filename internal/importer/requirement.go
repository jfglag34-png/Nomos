package importer

import (
	"fmt"
	"regexp"
	"strings"
)

// Requirement — разобранная строка зависимости из чужого проекта. Это НЕ
// спецификация для манифеста WEDRA: здесь хранится то, что написал автор
// проекта, дословно. План импорта решает, что из этого можно записать.
type Requirement struct {
	// Raw — исходная строка целиком. Нужна для сообщений: пользователь должен
	// видеть ровно то, что написано у него в файле.
	Raw string
	// Name — нормализованное имя дистрибутива: нижний регистр, `_` вместо `-`
	// (PEP 503). Именно в таком виде pip его и ставит.
	Name string
	// Extras — экстра-функции: `name[asyncio]` означает «поставить со
	// вспомогательными зависимостями».
	Extras []string
	// Constraint — ограничение версии исходным текстом: `>=1.36,<2.0`.
	Constraint string
	// Version — версия из оператора `==`, если он есть. Пусто, если пина нет:
	// выдумывать версию из диапазона нельзя (см. Plan).
	Version string
	// URL — требование вида `name @ https://...` (PEP 508, «прямая ссылка»).
	URL string
	// Marker — маркер окружения (`; python_version >= "3.9"`), исходным текстом.
	Marker string
	// IsPinned — ограничение содержит РОВНО ОДИН оператор `==` с конкретной
	// версией (без `*`). Диапазон `>=1.36,<2.0` пином не является: из него
	// нельзя вывести одну версию, а выбрать «последнюю подходящую» значит
	// зафиксировать догадку как факт.
	IsPinned bool
}

// manifestRequiresRe — ровно тот шаблон, что в schemas/manifest.schema.json
// для `runtime.requires`. Держим копию рядом с местом, где принимается
// решение: строка, которую мы предлагаем записать в манифест, обязана быть
// записана без последующих сюрпризов на валидации.
var manifestRequiresRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*==[A-Za-z0-9][A-Za-z0-9_.+-]*$`)

// distNameRe — имя дистрибутива по PEP 508. Строже не надо: авторы пишут
// точки и подчёркивания, и это легально.
var distNameRe = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9._-]*[A-Za-z0-9])?$`)

// extraNameRe — имя экстра-функции. PEP 685 нормализует их к нижнему регистру
// с подчёркиванием, мы делаем то же самое.
var extraNameRe = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9._-]*[A-Za-z0-9])?$`)

// constraintPartRe — одна часть ограничения: оператор и версия. Версия
// оставляем свободной по форме (1.0, 2.0.30, 1.0rc1, 1.0.*, 1!2.0 —
// последнее схемой манифеста потом отвергнется, но это проверка ManifestSpec,
// а не запрет разбирать).
var constraintPartRe = regexp.MustCompile(`^(===|==|!=|~=|<=|>=|<|>)\s*([A-Za-z0-9][A-Za-z0-9.*+!_-]*)$`)

// ParseRequirement разбирает подмножество PEP 508, которое встречается в
// dependencies: `name`, `name==1.2`, `name>=1.36,<2.0`, `name[extra]>=1.0`,
// `name @ url`, плюс маркер окружения после `;`.
//
// Чего пакет не делает и почему:
//   - не выбирает версию по диапазону — «последняя подходящая» это угадывание,
//     которое через месяц даст другой результат;
//   - не разбирает рекурсивные ссылки (`-e .`), строки `-r other.txt`,
//     переменные окружения в пин-файлах: они не несут версий и в манифест не
//     идут;
//   - не разбирает маркер как условие — сохраняет текстом. Решать, что означает
//     `python_version >= "3.9"` в чужом проекте, должен человек.
func ParseRequirement(s string) (Requirement, error) {
	req := Requirement{Raw: s}
	raw := strings.TrimSpace(s)
	if raw == "" {
		return req, fmt.Errorf("пустое требование: нечего разбирать")
	}
	// Комментарий в requirements-файле — стандартная практика, режем.
	if idx := strings.Index(raw, " #"); idx > 0 {
		raw = strings.TrimSpace(raw[:idx])
	}

	body := raw
	if idx := strings.Index(body, ";"); idx >= 0 {
		marker := strings.TrimSpace(body[idx+1:])
		if marker == "" {
			return req, fmt.Errorf("требование %q: после ';' маркер пустой", s)
		}
		req.Marker = marker
		body = strings.TrimSpace(body[:idx])
	}
	if body == "" {
		return req, fmt.Errorf("требование %q: нет имени пакета", s)
	}

	// Ссылка на репозиторий VCS (`git+https://…`, `pkg @ git+https://…`) и
	// локальная правка (`-e .`) — это не пакеты из индекса: версии у них нет,
	// а значит в runtime.requires им нечего записать. Проверка идёт ДО формы
	// `имя @ ссылка`, иначе такой текст распался бы на «имя = git+https://…» и
	// человек получил бы сообщение не про то. Обычная форма
	// `имя @ https://…/файл.tar.gz` — не ловится: там ровно один `@`, VCS
	// префикса нет, и это легальная, хоть и неразрешимая, запись PEP 508.
	if isVCSLink(body) {
		return req, fmt.Errorf(
			"требование %q задано прямой ссылкой на репозиторий, а не пакетом с версией: "+
				"в runtime.requires допустим только package==version", s)
	}

	if idx := strings.Index(body, "@"); idx >= 0 {
		namePart := strings.TrimSpace(body[:idx])
		url := strings.TrimSpace(body[idx+1:])
		if url == "" {
			return req, fmt.Errorf("требование %q: после '@' пустая ссылка", s)
		}
		if strings.ContainsAny(url, " \t") {
			return req, fmt.Errorf("требование %q: ссылка %q содержит пробелы — так не бывает", s, url)
		}
		name, extras, err := parseNameExtras(namePart, s)
		if err != nil {
			return req, err
		}
		req.Name, req.Extras, req.URL = name, extras, url
		req.IsPinned = false
		return req, nil
	}

	// Пробела между именем и ограничением может не быть (`httpx==0.27.0`),
	// поэтому граница ищется по первому символу, который в имени невозможен.
	namePart, rest := splitNameAndRest(body)
	rest = strings.TrimLeft(rest, " \t")
	if strings.HasPrefix(rest, "[") {
		close := strings.Index(rest, "]")
		if close < 0 {
			return req, fmt.Errorf("требование %q: '[' без закрывающей ']'", s)
		}
		namePart += rest[:close+1]
		rest = rest[close+1:]
	}
	name, extras, err := parseNameExtras(namePart, s)
	if err != nil {
		return req, err
	}
	req.Name, req.Extras = name, extras
	req.Constraint = strings.TrimSpace(rest)
	if err := applyVersion(&req, s); err != nil {
		return req, err
	}
	return req, nil
}

// isVCSLink отличает ссылку на репозиторий от легальной PEP 508 формы
// `имя @ ссылка-на-файл`. Признак — префикс системы контроля версий.
func isVCSLink(body string) bool {
	for _, prefix := range []string{"git+", "hg+", "svn+", "bzr+"} {
		if strings.HasPrefix(body, prefix) ||
			strings.Contains(body, "@"+prefix) ||
			strings.Contains(body, " "+prefix) {
			return true
		}
	}
	return false
}

// splitNameAndRest делит требование на имя и остаток по первому символу,
// который в имени дистрибутива встретиться не может. Пробел перед
// ограничением в PEP 508 необязателен, поэтому искать его пробелом нельзя.
func splitNameAndRest(body string) (string, string) {
	end := 0
	for end < len(body) && isDistNameChar(body[end]) {
		end++
	}
	return body[:end], body[end:]
}

func isDistNameChar(c byte) bool {
	return c == '-' || c == '_' || c == '.' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

func parseNameExtras(namePart, raw string) (string, []string, error) {
	name := namePart
	var extras []string
	if idx := strings.Index(namePart, "["); idx >= 0 {
		if !strings.HasSuffix(namePart, "]") {
			return "", nil, fmt.Errorf("требование %q: '[' без закрывающей ']'", raw)
		}
		name = strings.TrimSpace(namePart[:idx])
		inner := strings.TrimSpace(namePart[idx+1 : len(namePart)-1])
		for _, item := range strings.Split(inner, ",") {
			item = strings.TrimSpace(item)
			if item == "" {
				return "", nil, fmt.Errorf("требование %q: пустое имя экстра-функции в скобках", raw)
			}
			if !extraNameRe.MatchString(item) {
				return "", nil, fmt.Errorf("требование %q: экстра-функция %q выглядит не как имя", raw, item)
			}
			extras = append(extras, normalizeExtra(item))
		}
	}
	if name == "" {
		return "", nil, fmt.Errorf("требование %q: %q не похоже на имя пакета (нужно: %q)", raw, namePart, distNameRe.String())
	}
	if !distNameRe.MatchString(name) {
		return "", nil, fmt.Errorf("требование %q: %q не похоже на имя пакета", raw, name)
	}
	return normalizeDistName(name), extras, nil
}

// applyVersion проверяет ограничение и решает, есть ли точный пин. Ровно один
// оператор `==` без `*` — пин; всё остальное (диапазон, `~=`, `!=`, `==1.2.*`)
// требует разрешения и в `runtime.requires` не пишется.
func applyVersion(req *Requirement, raw string) error {
	if req.Constraint == "" {
		return nil
	}
	parts := strings.Split(req.Constraint, ",")
	clean := make([]string, 0, len(parts))
	pins := 0
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return fmt.Errorf("требование %q: пустая часть ограничения %q", raw, req.Constraint)
		}
		m := constraintPartRe.FindStringSubmatch(part)
		if m == nil {
			return fmt.Errorf("требование %q: не понимаю ограничение %q (ожидались ==, !=, ~=, <=, >=, <, >)", raw, part)
		}
		// Ограничение сохраняем в виде «оператор+версия» без косметических
		// пробелов: `== 0.27.0` и `==0.27.0` — одна и та же строка, и в
		// сообщениях незачем показывать различия, которых нет по сути.
		clean = append(clean, m[1]+m[2])
		if m[1] == "==" {
			pins++
			if pins > 1 {
				return fmt.Errorf("требование %q: два оператора == в одном ограничении %q", raw, req.Constraint)
			}
			if strings.Contains(m[2], "*") {
				// `==1.2.*` совместим с любой версией 1.2.x, то есть это не пин
				continue
			}
			req.Version = m[2]
		}
	}
	req.Constraint = strings.Join(clean, ",")
	req.IsPinned = pins == 1 && req.Version != ""
	return nil
}

// ManifestSpec — строка для `runtime.requires`, если требование в принципе
// представимо в манифесте.
//
// Второе значение false означает «в манифест это записать нельзя», и причины
// тут принципиально разные:
//   - `IsPinned == false` — нет точной версии (сначала разрешить);
//   - экстра-функции — шаблон манифеста вообще не допускает скобок
//     (`^[A-Za-z0-9][A-Za-z0-9_.-]*==...$`), а requirements.lock обязан
//     совпадать с requires построчно (internal/pipeline/validator.go), то
//     есть «спрятать» экстра в lock нельзя. Это ограничение платформы, а не
//     наша недоработка;
//   - маркер окружения и ссылка на источник — версии там нет вовсе.
func (r Requirement) ManifestSpec() (string, bool) {
	if !r.IsPinned || r.Version == "" {
		return "", false
	}
	spec := r.Name + "==" + r.Version
	if len(r.Extras) > 0 || r.URL != "" || r.Marker != "" {
		return "", false
	}
	if !manifestRequiresRe.MatchString(spec) {
		return "", false
	}
	return spec, true
}

// normalizeDistName приводит имя дистрибутива к виду, который сравнивают между
// собой pip и PEP 503: нижний регистр, «-» и «_» — одно и то же.
func normalizeDistName(name string) string {
	trimmed := strings.TrimSpace(name)
	return strings.ToLower(strings.NewReplacer("_", "-", ".", "-", "--", "-").Replace(trimmed))
}

// normalizeExtra — PEP 685: экстра-функции сравниваются в нижнем регистре с
// подчёркиванием.
func normalizeExtra(extra string) string {
	return strings.ToLower(strings.ReplaceAll(extra, "-", "_"))
}
