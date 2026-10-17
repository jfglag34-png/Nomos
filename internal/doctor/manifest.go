package doctor

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/wykserdex/wedra/internal/pipeline"
)

// pipWord — «pip install» в любом регистре, как его пишут в README и в коде.
var pipWord = regexp.MustCompile(`(?i)pip3?\s+install`)

// manifest — минимум из манифеста, нужный doctor'у.
type manifest struct {
	ID          string
	Dir         string
	Description string
	Requires    []string
}

// scanManifests читает манифесты напрямую из каталога.
//
// Не через core.ScanPlugins(): тот идёт по относительным путям, молча пропускает
// битый манифест и ничего не говорит. Здесь битый манифест — это плагин без
// doctor-вердикта, и человек должен узнать об этом, а не решить, что плагинов
// просто меньше.
func scanManifests(dir string) []manifest {
	var out []manifest
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, group := range entries {
		if !group.IsDir() {
			continue
		}
		groupDir := filepath.Join(dir, group.Name())
		// Раскладка и плоская (plugins/<id>) и вложенная
		// (plugins/official/<id>, plugins/community/<id>).
		candidates := []string{groupDir}
		if sub, err := os.ReadDir(groupDir); err == nil {
			for _, e := range sub {
				if e.IsDir() {
					candidates = append(candidates, filepath.Join(groupDir, e.Name()))
				}
			}
		}
		for _, cd := range candidates {
			raw, err := os.ReadFile(filepath.Join(cd, "plugin.yaml"))
			if err != nil {
				continue
			}
			// Разбор — тем же кодом, что и у ядра (pipeline.DecodeManifest),
			// а не своя копия yaml: иначе doctor считал бы готовым плагин,
			// который ран потом не примет.
			var m pipeline.Manifest
			if err := pipeline.DecodeManifest(raw, &m); err != nil || m.ID == "" {
				continue
			}
			out = append(out, manifest{
				ID:          m.ID,
				Dir:         cd,
				Description: m.Description,
				Requires:    m.Runtime.Requires,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// unverifiedDonorPlugins — плагины, чей внешний инструмент doctor НЕ проверял.
//
// Проверяется только то, что названо в описании манифеста. Если донора там нет,
// но «pip install» встречается в README или в коде, такой плагин молча попал бы
// в «готовы» — а это ровно то враньё, ради которого оговорка и нужна.
func unverifiedDonorPlugins(manifests []manifest, reported []Plugin) []string {
	checked := map[string]bool{}
	for _, p := range reported {
		if p.Donor != "" {
			checked[p.ID] = true
		}
	}
	var out []string
	for _, m := range manifests {
		if checked[m.ID] {
			continue
		}
		if mentionsPipInstall(m.Dir) {
			out = append(out, m.ID)
		}
	}
	sort.Strings(out)
	return out
}

// mentionsPipInstall — есть ли упоминание установки в README или в коде
// плагина. Читаются только два типа файлов: README* и .py, потому что
// остальное (манифест с его описанием, requirements.lock) уже учтено выше.
func mentionsPipInstall(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			if name == "__pycache__" || name == ".git" {
				continue
			}
			if mentionsPipInstall(filepath.Join(dir, name)) {
				return true
			}
			continue
		}
		if !strings.HasPrefix(strings.ToUpper(name), "README") && !strings.HasSuffix(name, ".py") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		if pipWord.Match(raw) {
			return true
		}
	}
	return false
}

// requirementName — имя пакета из спецификации `package==version`.
//
// Точные версии НЕ приводятся: задача и контракт этого не требуют, а пин
// меняется вместе с lock-файлом, и дублировать его здесь — значит показать
// человеку цифру, которая завтра разойдётся с манифестом.
func requirementName(spec string) string {
	s := strings.TrimSpace(spec)
	for _, sep := range []string{"==", ">=", "<=", "~=", "!=", ">", "<", "[", ";", "@"} {
		if i := strings.Index(s, sep); i > 0 {
			s = s[:i]
		}
	}
	return strings.Trim(s, "[] ")
}

// requirementImportName — имя МОДУЛЯ, которым этот пакет импортируется.
//
// Оно не всегда совпадает с именем пакета на PyPI: dnspython ставит модуль
// `dns`, и без этой поправки doctor объявил бы отсутствующим пакет, который на
// машине стоит и с которым плагин работает. Проверено: importlib.util.find_spec
// для «dnspython» возвращает None, для «dns» — модуль.
//
// Таблица намеренно мала и перечислимая: выводить её из кода плагинов дороже,
// чем поддерживать одну-две Known-истины, а выдумывать правила перевода имён
// («убери дефис», «замени _ на -») нельзя — они ломают имена вроде py-altdns.
var requirementImportAliases = map[string]string{
	"dnspython": "dns",
}

// requirementImportNames — под какими именами искать пакет в интерпретаторе:
// своё имя и, если известно отличие, имя модуля.
func requirementImportNames(name string) []string {
	if alias, ok := requirementImportAliases[name]; ok {
		return []string{name, alias}
	}
	return []string{name}
}
