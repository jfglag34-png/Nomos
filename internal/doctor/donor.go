package doctor

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// pipInstallRe — «pip install X» из описания манифеста. Именно отсюда человек
// берёт команду установки, поэтому и цитировать её будем оттуда же.
//
// Первая группа после `pip install` — то, что pip считает целью установки.
// Спецслучаи отсекаются: `-r requirements.txt` и `git+https://…` не называют
// пакета, и подставить вместо них имя — значит выдумать команду, которая не
// сработает (для VCS-цели pip вообще требует префикс git+, а не голое имя).
var pipInstallRe = regexp.MustCompile(`(?i)pip3?\s+install\s+([^\s]+)`)

// vcsPrefixes — цели, которые pip ставит не из индекса пакетов.
var vcsPrefixes = []string{"git+", "hg+", "svn+", "bzr+", "http://", "https://", "file://", "-e", "."}

// urlInstallTargetRe — «цель установки» с адресом: `https://…tar.gz?a=b`,
// `https://…#egg=x`, `git://…`, `git+https://…`, `file://…`.
//
// Проверяется ДО обрезания описания по запятой/двоеточию, и это единственное
// место, где правило может сработать: обрезание уничтожает `://`, после чего
// `pip install https://github.com/x/y-1.0.tar.gz?a=b` превращается в `https`
// — валидное имя пакета, то есть в донора, которого не существует.
//
// Перестановка проверки префиксов VCS в начало задачи не решает: список
// префиксов по построению неполон (`git://` в нём нет, `ssh://` тоже, а
// список пришлось бы дополнять всеми возможными схемами), и мусор, который
// обязан чиститься, от ссылки почти не отличим — `vt-py):` и `https://…`
// это одно и то же двоеточие в том же месте. Отличает их ровно `//` после
// двоеточия, поэтому здесь не список, а структурный признак: `схема://`.
var urlInstallTargetRe = regexp.MustCompile(`(?i)^[a-z][a-z0-9+.\-]*://`)

// whichRe — литералы shutil.which("…") из кода плагина. Это самый честный
// источник: плагин сам говорит, какое имя бинаря ищет, и это может отличаться
// от имени пакета (пакет scoutsuite даёт бинарь scout, oletools — olevba).
var whichRe = regexp.MustCompile(`shutil\.which\(\s*"([^"]+)"`)

// pinVersionRe — версия в пине `package==version`.
//
// Знак цифры на первом месте отсекает мусор: версия начинается с цифры, а
// «1.0;python_version<"3.9"» (маркер окружения) или «1.0=y» под неё не
// подходят — такой пин не разбирается и донор остаётся непроверенным.
var pinVersionRe = regexp.MustCompile(`^[0-9][0-9A-Za-z.*+!-]*$`)

// donorFromDescription достаёт имя пакета из «pip install X» в описании.
//
// Второй аргумент — «упомянут ли вообще». Совпадение есть всегда положительное;
// «нет» означает «в описании нечем учиться», и тогда doctor молчит, а не
// выдумывает донора.
func donorFromDescription(desc string) (string, bool) {
	m := pipInstallRe.FindStringSubmatch(desc)
	if len(m) < 2 {
		return "", false
	}
	// Регулярка берёт первый «кусок» без пробелов, а в описаниях это конец
	// фразы: «…(import vt, REST API v3, pip install vt-py): плагин идёт» даёт
	// `vt-py):`, а «`pip install webanalyze` на PyPI нет» — `webanalyze\``.
	// Такие хвосты и есть дефект 3: команда с ними не сработает.
	name := strings.TrimSpace(m[1])
	// Обратный слэш на конце — это закрывающая markdown-кавычка в
	// «`pip install webanalyze`», а не часть имени.
	name = strings.TrimRight(name, "\\")
	name = strings.Trim(name, `"'()[]{}<>`+"`")
	// Ссылка или VCS-цель пакетом не является: имени, по которому можно
	// проверить наличие в PATH, у неё нет, а подставленное вместо неё имя
	// дало бы человеку команду, которая не сработает. Именно здесь, до
	// обрезания по «,:;» — иначе `https://…` станет `https` (см. комментарий
	// к urlInstallTargetRe).
	if urlInstallTargetRe.MatchString(name) {
		return "", false
	}
	// Запятая и двоеточие могут быть и внутри: «pip install x, y».
	if i := strings.IndexAny(name, ",:;"); i >= 0 {
		name = name[:i]
	}
	name = strings.TrimSpace(name)
	name = strings.TrimSuffix(name, ".")
	name = strings.Trim(name, `"'()[]{}<>`+"` ")
	if name == "" {
		return "", false
	}
	// Пин `package==version` — единственный «хвост», который остаётся в
	// описании уже ПОСЛЕ чистки мусора выше: «Пин: pip install pefile==2024.8.26».
	// Имя пакета тут — то, что до `==`, и разбирать это надо ДО проверки
	// validPackageName: иначе плагин с честным пином (community/pefile,
	// community/detect_secrets) объявляется «донор не проверен» и попадает в
	// unverified_donors, хотя проверять тут нечего — пакет объявлен в
	// runtime.requires и виден среди requires.
	if strings.Contains(name, "==") {
		unpinned, ok := packageNameFromPin(name)
		if !ok {
			return "", false
		}
		name = unpinned
	}
	// `-r requirements.txt` и `--upgrade X` пакетом не являются: у них нет
	// имени, по которому можно проверить наличие в PATH. Ссылка и VCS-цель
	// отсечены выше (urlInstallTargetRe) — до обрезания, где от них остаются
	// обрубки вида `https`/`git`; vcsPrefixes здесь второй рубеж, на случай
	// если схема попадёт в цель установки в нестандартном написании.
	if strings.HasPrefix(name, "-") {
		return "", false
	}
	lower := strings.ToLower(name)
	for _, p := range vcsPrefixes {
		if strings.HasPrefix(lower, p) {
			return "", false
		}
	}
	// Остатки пути — это локальный каталог, а не пакет.
	if strings.ContainsAny(name, "/\\") {
		return "", false
	}
	// Последний рубеж: имя пакета не может начинаться с цифры и не может
	// содержать символов, которых в нём не бывает. Список — из фактического
	// мусора в 99 манифестах, а не из догадок.
	if !validPackageName(name) {
		return "", false
	}
	return name, true
}

// validPackageName — похоже ли это на имя пакета PyPI.
//
// Верхняя граница длины и запрет пробелов взяты из правил PyPI: длинное или с
// пробелом имя заведомо не пакет, и подставлять его в команду установки — значит
// выдумать команду, которая не выполнится.
func validPackageName(name string) bool {
	if name == "" || len(name) > 100 {
		return false
	}
	if strings.ContainsAny(name, " \t\n\r") {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
			if i == 0 {
				return false
			}
		case r == '-' || r == '_' || r == '.':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// packageNameFromPin — имя пакета из строки вида `package==version`.
//
// Строгая сторона: отбрасывается только `==` + версия, и только если с левой
// стороны САМОСТОЯТЕЛЬНО валидное имя пакета. Обрезание «всё до равных знаков»
// было бы ровно тем багом, который здесь и чинится. Мусор вроде `vt-py):` или
// обратной кавычки в `webanalyze` равных знаков не содержит и ловится другими
// правилами, но `git+https://…==1.0` обрезался бы до `git+https://…` и больше
// не отсекался бы префиксом VCS — то есть враньё просто переехало бы на другой
// адрес. Поэтому несовпавшая строка отвергается целиком: лучше «донор НЕ
// проверен», чем выдуманное имя.
func packageNameFromPin(s string) (string, bool) {
	i := strings.Index(s, "==")
	if i < 0 {
		return s, false
	}
	name, version := s[:i], s[i+2:]
	if !validPackageName(name) || !pinVersionRe.MatchString(version) {
		return "", false
	}
	return name, true
}

// donorCandidates — какие имена имеет смысл искать в PATH для донора.
//
// Имя пакета — не обязательно имя бинаря: scoutsuite даёт scout, oletools —
// olevba, wapiti3 — wapiti. Поэтому к имени пакета добавляются литералы
// shutil.which из кода самого плагина: если плагин ищет конкретное имя, мы
// проверяем именно его, а не угадываем по шаблону.
func donorCandidates(donor, pluginDir string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	add(donor)
	for _, w := range whichLiterals(pluginDir) {
		add(w)
	}
	return out
}

// whichLiterals — имена из shutil.which("…") по всем .py плагина.
func whichLiterals(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			if name == "__pycache__" || name == ".git" {
				continue
			}
			out = append(out, whichLiterals(filepath.Join(dir, name))...)
			continue
		}
		if !strings.HasSuffix(name, ".py") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		for _, m := range whichRe.FindAllStringSubmatch(string(raw), -1) {
			if len(m) > 1 {
				out = append(out, m[1])
			}
		}
	}
	return out
}

// donorFromFiles — пробует достать донора из README/* и .py файлов плагина,
// когда в описании манифеста его нет. Ищет тот же `pip install` паттерн,
// но в более широком наборе файлов. Это закрывает 13 плагинов где донор
// известен лишь из README (например community/webanalyze, community/holehe и т.д.)
func donorFromFiles(dir string) (string, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			if name == "__pycache__" || name == ".git" {
				continue
			}
			if donor, ok := donorFromFiles(filepath.Join(dir, name)); ok {
				return donor, true
			}
			continue
		}
		upper := strings.ToUpper(name)
		isReadme := strings.HasPrefix(upper, "README")
		isPy := strings.HasSuffix(name, ".py")
		if !isReadme && !isPy {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		if donor, ok := donorFromDescription(string(raw)); ok {
			return donor, true
		}
		// Fallback для README где donorFromDescription может быть слишком строгим
		// (например обрезанный хвост) — пробуем напрямую, но с тем же url-фильтром
		if m := pipInstallRe.FindStringSubmatch(string(raw)); len(m) >= 2 {
			candidate := strings.TrimSpace(m[1])
			candidate = strings.Trim(candidate, "\"'`()[]{}<>` ")
			if urlInstallTargetRe.MatchString(candidate) {
				continue
			}
			if idx := strings.IndexAny(candidate, ",:;"); idx >= 0 {
				candidate = candidate[:idx]
			}
			candidate = strings.TrimSpace(strings.TrimSuffix(candidate, "."))
			if strings.Contains(candidate, "==") {
				if name, ok := packageNameFromPin(candidate); ok {
					return name, true
				}
				continue
			}
			if validPackageName(candidate) && !strings.HasPrefix(strings.ToLower(candidate), "-") {
				low := strings.ToLower(candidate)
				isVCS := false
				for _, p := range vcsPrefixes {
					if strings.HasPrefix(low, p) {
						isVCS = true
						break
					}
				}
				if !isVCS && !strings.ContainsAny(candidate, "/\\") {
					return candidate, true
				}
			}
		}
	}
	return "", false
}
//
// donorInPath — есть ли такой донор в PATH.
//
// exec.LookPath, а не обход PATH вручную: он учитывает PATHEXT на Windows, то
// есть находит donor.exe и donor.cmd так же, как это делает плагин. Имя
// возвращается, чтобы показать человеку, КАКОЕ ИМЯ нашлось: у пакета
// scoutsuite бинарь называется scout, и «нашлось scout» полезнее, чем молчание.
func donorInPath(candidates []string) (found bool, foundName string) {
	for _, c := range candidates {
		if _, err := exec.LookPath(c); err == nil {
			return true, c
		}
	}
	return false, ""
}
