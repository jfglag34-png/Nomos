package doctor

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/wykserdex/wedra/internal/plugin"
)

// doctorFixture кладёт плагин с заданными манифестом и кодом и возвращает
// путь к каталогу plugins.
//
// Своя фикстура, а не plugins/ из репозитория: тест не должен зависеть от
// витрины (она меняется) и не должен видеть плагины, установленные на машине
// того, кто тест запускает.
func doctorFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// doctorManifestText — манифест минимального валидного плагина.
//
// Описание берётся в одинарных кавычках: реальные манифесты пишут его свёрнутым
// блоком (`description: >`), а в кавычках строка с двоеточием остаётся строкой.
// Без кавычек «Обёртка: pip install X» YAML считает началом отображения, и
// манифест молча не парсится — а плагин пропадает из отчёта.
func doctorManifestText(id, desc string, requires []string) string {
	req := "[]"
	if len(requires) > 0 {
		quoted := make([]string, 0, len(requires))
		for _, r := range requires {
			quoted = append(quoted, `"`+r+`"`)
		}
		req = "[" + strings.Join(quoted, ", ") + "]"
	}
	safe := strings.ReplaceAll(desc, "'", "''")
	return "id: " + id + `
version: 0.1.0
platform_api: "^0.1"
description: '` + safe + `'
author: "test"

runtime:
  type: python
  entry: main.py
  requires: ` + req + `

input:
  value:
    from: input.value
    type: string
output:
  value:
    type: string
permissions:
  network: []
  filesystem: none
  secrets: []
`
}

// doctorOne — отчёт ровно на один плагин, с внятной ошибкой вместо паника.
func doctorOne(t *testing.T, root string) Plugin {
	t.Helper()
	rep := Run(root)
	if len(rep.Plugins) != 1 {
		t.Fatalf("в отчёте плагинов %d, ждали 1 (каталог %s)", len(rep.Plugins), root)
	}
	return rep.Plugins[0]
}

// stubIndexLookup подменяет проверку индекса на заданный ответ на все имена.
// Тесты не ходят в сеть: иначе они зависели бы от чужой доступности, а падение
// «PyPI недоступен» выглядело бы как поломка doctor.
func stubIndexLookup(t *testing.T, state indexState) {
	t.Helper()
	prev := indexLookup
	indexLookup = func(string) indexState { return state }
	t.Cleanup(func() { indexLookup = prev })
}

// Имя, которого заведомо нет на PyPI: с префиксом, который не может совпасть
// с настоящим пакетом. Донар при этом НЕ установлен локально — иначе проверка
// прошла бы не по той ветке.
const absentDonor = "wedra-nonexistent-donor-xyz"

// Дефект, найденный измерением: проба интерпретатора отвечает на вопрос
// «установлен ли пакет здесь», а не «существует ли он на PyPI». Ветка
// «не установлен» трактовала это как «устанавливаем» и выдавала
// `pip install recon-ng` — команду, которая не может сработать, потому что
// такого пакета на индексе нет. Проверено на живых манифестах: doctor так
// советовал по recon_ng и spiderfoot.
func TestDoctorDoesNotAdvisePipInstallForPackageAbsentFromIndex(t *testing.T) {
	stubIndexLookup(t, indexAbsent)
	root := doctorFixture(t, map[string]string{
		"absent/plugin.yaml": doctorManifestText("absent", "Ставится отдельно: pip install "+absentDonor, nil),
		"absent/main.py":     "import json\nprint('{}')\n",
	})
	p := doctorOne(t, root)

	// Проверяется ФОРМА совета, а не подстрока: текст про «пакета нет на PyPI»
	// упоминает `pip install` в отрицании, и Contains поймал бы его как
	// успех. Копируемая команда начинается с `pip install`, и ничего больше
	// начинаться не должна.
	if strings.HasPrefix(p.Install, "pip install") {
		t.Errorf("doctor посоветовал pip install для пакета, которого нет на PyPI: %q", p.Install)
	}
	if !p.DonorAbsentFromIndex {
		t.Errorf("отсутствие пакета на индексе не отмечено: %+v", p)
	}
	// Отсутствие на индексе — не повод считать плагин готовым: он и правда не
	// работает. Проверяем отдельно, чтобы «не советовать install» не
	// превратилось в «всё хорошо».
	if p.Status == StatusReady {
		t.Errorf("плагин с ненайденным донором помечен готовым: %+v", p)
	}
	if p.Install == "" {
		t.Errorf("совет пуст: человеку не сказано, что делать. %+v", p)
	}
}

// «Не знаю» и «нет на индексе» — разные вещи, и обе обязаны молчать вместо
// того, чтобы выдавать confident-команду. Недоступность сети не должна
// выглядеть как разрешение советовать наугад.
func TestDoctorDoesNotAdvisePipInstallWhenIndexUnknown(t *testing.T) {
	stubIndexLookup(t, indexUnknown)
	root := doctorFixture(t, map[string]string{
		"unknown/plugin.yaml": doctorManifestText("unknown", "Ставится отдельно: pip install "+absentDonor, nil),
		"unknown/main.py":     "import json\nprint('{}')\n",
	})
	p := doctorOne(t, root)

	if strings.HasPrefix(p.Install, "pip install") {
		t.Errorf("совет выдан при непроверяемом индексе: %q", p.Install)
	}
	if !p.DonorIndexUnknown {
		t.Errorf("непроверяемый индекс не отмечен: %+v", p)
	}
}

// Обратная сторона правки: настоящий пакет с индекса советовать перестать не
// надо. Без этого теста «никогда не пиши pip install» проходит идеально и при
// этом ломает весь смысл команды.
func TestDoctorStillAdvisesPipInstallForRealPackage(t *testing.T) {
	stubIndexLookup(t, indexPresent)
	root := doctorFixture(t, map[string]string{
		"present/plugin.yaml": doctorManifestText("present", "Ставится отдельно: pip install "+absentDonor, nil),
		"present/main.py":     "import json\nprint('{}')\n",
	})
	p := doctorOne(t, root)

	if p.Install != "pip install "+absentDonor {
		t.Errorf("для существующего пакета нужен ровно pip install: %q", p.Install)
	}
	if p.DonorAbsentFromIndex || p.DonorIndexUnknown {
		t.Errorf("существующий пакет помечен отсутствующим или неизвестным: %+v", p)
	}
}

// Нормальный случай: плагин на чистой стандартной библиотеке и без донора
// обязан быть готов. Это регрессия на «doctor считает готовым всё подряд» и
// наоборот — на «doctor флагует штатные плагины».
func TestDoctorReadyWhenNothingIsNeeded(t *testing.T) {
	root := doctorFixture(t, map[string]string{
		"clean/plugin.yaml": doctorManifestText("clean", "Плагин на stdlib, донора нет", nil),
		"clean/main.py":     "import json, sys\nprint(json.dumps({'status': 'ok', 'output': {'value': 'x'}}))\n",
	})
	rep := Run(root)

	if rep.Total != 1 {
		t.Fatalf("найдено плагинов %d, ждали 1 — фикстура не прочитана", rep.Total)
	}
	if rep.Ready != 1 {
		t.Fatalf("готовых %d из 1: статус %q (%+v)", rep.Ready, rep.Plugins[0].Status, rep.Plugins[0])
	}
	if !rep.OK {
		t.Errorf("OK=false при полностью готовом окружении: %s", rep.Summary)
	}
	if rep.Plugins[0].Donor != "" {
		t.Errorf("донер выдуман там, где его нет: %q", rep.Plugins[0].Donor)
	}
	if !strings.Contains(rep.Summary, "готовы все 1 плагинов") {
		t.Errorf("итоговая строка не похожа на «всё готово»: %q", rep.Summary)
	}
}

// Случай с недостающей зависимостью: runtime.requires объявлен, но имён,
// которых наверняка нет на машине, в нём быть не должно — значит проверка
// «отсутствует» обязана сработать. Ничего не устанавливаем.
func TestDoctorReportsMissingDependency(t *testing.T) {
	const absent = "wedra_absent_pkg_doctor_probe"
	root := doctorFixture(t, map[string]string{
		"needy/plugin.yaml": doctorManifestText("needy", "Плагин с зависимостью",
			[]string{absent + "==9.9.9"}),
		"needy/main.py": "import json\nprint(json.dumps({'status': 'ok', 'output': {'value': 'x'}}))\n",
	})
	rep := Run(root)

	if rep.NeedsDep != 1 {
		t.Fatalf("нужна зависимость для %d плагинов, ждали 1 (итог: %s)", rep.NeedsDep, rep.Summary)
	}
	p := doctorOne(t, root)
	if p.Status != StatusNeedsDependency {
		t.Errorf("статус = %q, ждали %q", p.Status, StatusNeedsDependency)
	}
	if len(p.MissingRequires) != 1 || !strings.Contains(p.MissingRequires[0], absent) {
		t.Errorf("missing_requires = %v, ждали один элемент с %s", p.MissingRequires, absent)
	}
	// Готовая команда обязана быть пригодна к копированию.
	if !strings.Contains(p.Install, "pip install") || !strings.Contains(p.Install, absent) {
		t.Errorf("команда установки не готова к копированию: %q", p.Install)
	}
	if rep.OK {
		t.Error("OK=true при отсутствующей зависимости")
	}
}

// Случай с отсутствующим внешним инструментом: донор назван в описании, его
// нет в PATH. Проверяем через подмену PATH на пустой каталог, чтобы результат
// не зависел от того, что установлено у машины.
func TestDoctorReportsMissingExternalTool(t *testing.T) {
	// Индекс подменён: тест проверяет «пакета нет ЗДЕСЬ», а «есть ли он на
	// PyPI» — другой вопрос с другим ответом. Без подмены тест ходил бы в сеть,
	// и падение индекса читалось бы как поломка doctor.
	stubIndexLookup(t, indexPresent)
	root := doctorFixture(t, map[string]string{
		"donor/plugin.yaml": doctorManifestText("donor",
			"Обёртка над CLI. Ставится отдельно: pip install wedra-absent-tool.", nil),
		"donor/main.py": "import json\nprint(json.dumps({'status': 'ok', 'output': {'value': 'x'}}))\n",
	})
	// PATH оставляем с интерпретатором, но без донора: если убрать и его, то
	// doctor честно скажет «не знаю» вместо «не установлено», и тест проверял
	// бы не то. Интерпретатор нужен, чтобы отличить «пакета нет» от «не знаю».
	interp, err := doctorRealInterpreter()
	if err != nil {
		t.Skipf("на этой машине нет интерпретатора: %v", err)
	}
	t.Setenv("PATH", filepath.Dir(interp))

	rep := Run(root)

	if rep.Total != 1 {
		t.Fatalf("найдено плагинов %d, ждали 1", rep.Total)
	}
	p := doctorOne(t, root)
	if p.Donor != "wedra-absent-tool" {
		t.Fatalf("донер = %q, ждали wedra-absent-tool", p.Donor)
	}
	if p.Status != StatusNeedsExternalTool {
		t.Errorf("статус = %q, ждали %q", p.Status, StatusNeedsExternalTool)
	}
	if p.Install != "pip install wedra-absent-tool" {
		t.Errorf("команда = %q", p.Install)
	}
	if rep.NeedsDonor != 1 {
		t.Errorf("needs_external_tool = %d, ждали 1", rep.NeedsDonor)
	}
}

// Дефект 1. Пакет УСТАНОВЛЕН, но его консольного скрипта нет в PATH — pip
// кладёт скрипты в Scripts\, которого в PATH обычно нет. Предлагать
// «pip install X» здесь нельзя: человек выполнит команду и ничего не изменится.
func TestDoctorDoesNotOfferToInstallAlreadyInstalledPackage(t *testing.T) {
	interp, err := doctorRealInterpreter()
	if err != nil {
		t.Skipf("на этой машине нет интерпретатора: %v", err)
	}
	// Пустой PATH без интерпретатора не годится (см. выше), поэтому ищем
	// пакет, который заведомо есть в этой среде и точно не лежит в PATH.
	pkg, ok := doctorKnownInstalledPackage(t)
	if !ok {
		t.Skip("нет подходящего установленного пакета для проверки")
	}
	t.Setenv("PATH", filepath.Dir(interp))

	root := doctorFixture(t, map[string]string{
		"installed/plugin.yaml": doctorManifestText("installed",
			"Обёртка. Ставится отдельно: pip install "+pkg+".", nil),
		"installed/main.py": "import json\nprint(json.dumps({'status': 'ok', 'output': {'value': 'x'}}))\n",
	})
	rep := Run(root)
	p := doctorOne(t, root)

	if !p.DonorInstalledNoScript {
		t.Skipf("пакет %s установлен и его скрипт нашёлся в PATH — состояние «(c)», тест неприменим", pkg)
	}
	if !strings.Contains(p.Install, "установлен") {
		t.Errorf("совет = %q, ждали «пакет уже установлен», а не «pip install»", p.Install)
	}
	if strings.HasPrefix(p.Install, "pip install") {
		t.Errorf("совет предлагает установить уже установленный пакет: %q", p.Install)
	}
	// Куда смотреть — самое полезное в совете.
	if rep.Python.ScriptsDir == "" || !strings.Contains(p.Install, rep.Python.ScriptsDir) {
		t.Errorf("совет не называет каталог Scripts (%q): %q", rep.Python.ScriptsDir, p.Install)
	}
}

// doctorKnownInstalledPackage находит пакет, который есть в этой среде, но
// чей консольный скрипт гарантированно не в PATH. Ничего не ставится.
func doctorKnownInstalledPackage(t *testing.T) (string, bool) {
	t.Helper()
	info, err := plugin.PythonPackages(interpreterOrSkip(t), []string{"pip", "setuptools", "wheel", "cffi", "pycparser"})
	if err != nil {
		t.Skipf("проба не отработала: %v", err)
	}
	pi, ok := info["pip"]
	if !ok || !pi.Installed {
		return "", false
	}
	// Нужен пакет, который УСТАНОВЛЕН, но у которого нет ни одного
	// console_script: тогда его имя заведомо не появится в PATH как команда.
	for _, cand := range []string{"setuptools", "wheel", "cffi", "pycparser"} {
		ci, cok := info[cand]
		if cok && ci.Installed && len(ci.Scripts) == 0 {
			return cand, true
		}
	}
	return "", false
}

func interpreterOrSkip(t *testing.T) string {
	t.Helper()
	p, err := doctorRealInterpreter()
	if err != nil {
		t.Skipf("нет интерпретатора: %v", err)
	}
	return p
}

// Донор может называться в коде иначе, чем пакет: scoutsuite ставит бинарь
// scout. Если такое имя есть в PATH, плагин готов — иначе doctor ругался бы на
// плагин, который работает.
func TestDoctorAcceptsDonorFoundUnderCodeName(t *testing.T) {
	// Сначала узнаём настоящий интерпретатор: PATH будет подменён, а он нужен
	// для проверки requires.
	interp, err := doctorRealInterpreter()
	if err != nil {
		t.Skipf("на этой машине нет интерпретатора: %v", err)
	}
	binDir := t.TempDir()
	stub := filepath.Join(binDir, "scout")
	if runtimeIsWindows() {
		stub += ".bat"
	}
	if err := os.WriteFile(stub, []byte("@echo off\r\nexit /b 0\r\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	root := doctorFixture(t, map[string]string{
		"scoutpkg/plugin.yaml": doctorManifestText("scoutpkg",
			"Обёртка: pip install scoutsuite", nil),
		"scoutpkg/main.py": "import json, shutil\n" +
			"cmd = shutil.which(\"scout\")\n" +
			"print(json.dumps({'status': 'ok', 'output': {'value': str(cmd)}}))\n",
	})
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+filepath.Dir(interp))

	rep := Run(root)
	p := doctorOne(t, root)
	if !p.DonorFound {
		t.Fatalf("донор scout не найден, хотя положен в PATH: %+v", p)
	}
	if !rep.OK {
		t.Errorf("OK=false, хотя единственный плагин запускается: %s", rep.Summary)
	}
	if p.DonorFoundName != "scout" {
		t.Errorf("найден под именем %q, ждали scout", p.DonorFoundName)
	}
	if p.Status != StatusReady {
		t.Errorf("статус = %q, ждали %q: плагин запускается, ругаться не на что", p.Status, StatusReady)
	}
}

// Имя пакета на PyPI и имя модуля не обязаны совпадать: dnspython ставит
// модуль `dns`. Без этой поправки doctor объявляет отсутствующим пакет, который
// стоит. Реальный кейс репозитория — official/syntax_mx_checker.
func TestDoctorAcceptsPackageWhoseModuleDiffers(t *testing.T) {
	if _, ok := requirementImportAliases["dnspython"]; !ok {
		t.Fatal("поправка на dnspython→dns пропала: она нужна, иначе syntax_mx_checker читается как сломанный")
	}
	names := requirementImportNames("dnspython")
	if len(names) != 2 || names[1] != "dns" {
		t.Fatalf("requirementImportNames(dnspython) = %v, ждали [dnspython dns]", names)
	}
	// Обычный пакет даёт одно имя.
	if got := requirementImportNames("holehe"); len(got) != 1 || got[0] != "holehe" {
		t.Errorf("requirementImportNames(holehe) = %v", got)
	}
}

// `pip install -r requirements.txt` и `pip install git+https://…` не называют
// пакета. Подставлять вместо них имя — значит выдумать нерабочую команду.
func TestDoctorIgnoresPipFlagsAndURLs(t *testing.T) {
	cases := []struct{ desc, donor string }{
		{"Ставится: pip install -r requirements.txt.", ""},
		{"Установка: pip install git+https://github.com/x/y.git", ""},
		{"Ставится отдельно: pip install arjun.", "arjun"},
	}
	for _, c := range cases {
		got, ok := donorFromDescription(c.desc)
		if ok != (c.donor != "") {
			t.Errorf("donorFromDescription(%q): ok=%v, ждали %v", c.desc, ok, c.donor != "")
		}
		if got != c.donor {
			t.Errorf("donorFromDescription(%q) = %q, ждали %q", c.desc, got, c.donor)
		}
	}
}

// Адрес установки не должен превращаться в имя пакета.
//
// ДО правки: обрезание описания по «,:;» шло раньше проверок VCS/URL и
// оставляло от ссылки ровно схему, а схема — валидное имя пакета:
//
//	pip install https://github.com/x/y-1.0.tar.gz?a=b  → донор «https»
//	pip install git://github.com/x/y.git                → донор «git»
//	pip install file:///opt/tools/y                     → донор «file»
//	pip install https://example.com/y.tar.gz#egg=x      → донор «https»
//
// Ни один манифест в репозитории так не пишет, поэтому дыры не было видно в
// выводе, но она была ложной по построению: тот, кто напишет адрес в
// описании, получил бы человеку совет «pip install https».
func TestDoctorRejectsURLInstallTargets(t *testing.T) {
	cases := []struct{ desc, before string }{
		{"pip install https://github.com/x/y-1.0.tar.gz?a=b", "https"},
		{"pip install https://example.com/y-1.0.tar.gz#egg=x", "https"},
		{"pip install git://github.com/x/y.git", "git"},
		{"pip install file:///opt/tools/y", "file"},
		{"pip install git+https://github.com/x/y.git", ""},
		{"pip install git+ssh://git@github.com/x/y.git#egg=y", ""},
		{"pip install hg+http://example.com/y", ""},
		{"pip install svn+ssh://example.com/y", ""},
		{"pip install http://example.com/y.tar.gz", ""},
		{"pip install ftp://example.com/y.tar.gz", ""},
		{"pip install git@github.com:x/y.git#egg=y", ""},
		{"Ставится: pip install 'https://example.com/y.tar.gz'.", ""},
		{"Ставится: pip install (https://example.com/y.tar.gz).", ""},
		{"Установка: pip install https://example.com/y.tar.gz#egg=y.", ""},
	}
	for _, c := range cases {
		got, ok := donorFromDescription(c.desc)
		if ok {
			t.Errorf("donorFromDescription(%q) = %q, ждали отказ (до правки давала %q)",
				c.desc, got, c.before)
			continue
		}
		if got != "" {
			t.Errorf("donorFromDescription(%q) = %q при ok=false, а имя при отказе должно быть пустым",
				c.desc, got)
		}
	}
}

// Обратная сторона правки: честные пины и чистка мусора из описаний обязаны
// остаться ровно такими же, какими были ДО неё. Значения здесь не выдуманы,
// а сняты с того же кода до правки — если правило «отсекать ссылку» начнёт
// рубить что-то ещё, упадёт этот тест, а не тот, что про адреса.
//
// Ключевой случай — `vt-py):`: там двоеточие тоже часть мусора, и отличить
// его от двоеточия в схеме можно только по `//` после него. Поэтому «режем
// по первому `,:;`» и нельзя было починить перестановкой: резать пришлось бы
// раньше, а раньше — значит ломать мусор.
func TestDoctorURLFixKeepsPinsAndGarbageCleanup(t *testing.T) {
	cases := []struct{ desc, want string }{
		// Честные пины (community/pefile, community/detect_secrets).
		{"Пин: pip install pefile==2024.8.26", "pefile"},
		{"Пин: pip install detect-secrets==1.5.0", "detect-secrets"},
		{"Ставится: pip install 'exifread==3.5.1'.", "exifread"},
		{"Ставится: pip install maigret==0.6.6, если нет.", "maigret"},
		{"Ставится: pip install we-dr-marker==1.0;python_version<'3.9'", "we-dr-marker"},
		// Мусор из описаний.
		{"Донор — vt-py (import vt, REST API v3, pip install vt-py): плагин идёт", "vt-py"},
		{"Это Go-программа, а не pip-пакет: `pip install webanalyze` на PyPI нет", "webanalyze"},
		{"Ставится: pip install 'detect-secrets'.", "detect-secrets"},
		{"Ставится: pip install (binwalk).", "binwalk"},
		{"Ставится: pip install probe, если нет.", "probe"},
		{"Ставится отдельно: pip install arjun.", "arjun"},
	}
	for _, c := range cases {
		got, ok := donorFromDescription(c.desc)
		if !ok {
			t.Errorf("donorFromDescription(%q) не нашёл донора", c.desc)
			continue
		}
		if got != c.want {
			t.Errorf("donorFromDescription(%q) = %q, ждали %q", c.desc, got, c.want)
		}
	}
}

// Сквозная проверка по отчёту, а не по одной функции: плагин, чья цель
// установки — адрес, обязан попасть в unverified_donors («наличие НЕ
// проверено»), а НЕ получить выдуманного донора и совет «pip install https».
//
// Это и есть разница между «до» и «после» для человека: раньше такой плагин
// считался проверенным и получал неработающую команду установки.
func TestDoctorURLInstallTargetIsUnverifiedNotAFakeDonor(t *testing.T) {
	const desc = "Установка донора отдельно: pip install https://github.com/x/y-1.0.tar.gz?a=b"
	root := doctorFixture(t, map[string]string{
		"url/plugin.yaml": doctorManifestText("url", desc, nil),
		"url/main.py":     "import json\nprint(json.dumps({'status': 'ok', 'output': {'value': 'x'}}))\n",
		"url/README.md":   "Установка донора:\n\n    pip install https://github.com/x/y-1.0.tar.gz?a=b\n",
	})
	rep := Run(root)
	p := doctorOne(t, root)

	if p.Donor != "" {
		t.Errorf("donor = %q, а адрес установки донором не является", p.Donor)
	}
	for _, c := range p.DonorCandidates {
		if c == "https" || c == "http" || c == "git" || c == "file" {
			t.Errorf("в кандидатах %q — это схема адреса, а не имя бинаря: %v", c, p.DonorCandidates)
		}
	}
	if strings.Contains(p.Install, "https") {
		t.Errorf("совет установки %q обещает поставить пакет https", p.Install)
	}
	if p.Status != StatusReady {
		t.Errorf("статус = %q, ждали %q: «не проверено» — это не «нужен инструмент»",
			p.Status, StatusReady)
	}
	found := false
	for _, id := range rep.UnverifiedDonors {
		if id == "url" {
			found = true
		}
	}
	if !found {
		t.Errorf("плагин не попал в unverified_donors: %v — донора из описания взять неоткуда, и врать нельзя",
			rep.UnverifiedDonors)
	}
}

// Версии из пина не должны попадать в имя пакета: иначе проба ищет модуль
// «holehe==1.61», которого не бывает, и находит его только у отсутствующего.
func TestRequirementNameStripsPin(t *testing.T) {
	cases := map[string]string{
		"holehe==1.61":     "holehe",
		"dnspython==2.8.0": "dnspython",
		"holehe":           "holehe",
		"pkg>=1.0":         "pkg",
		"":                 "",
	}
	for in, want := range cases {
		if got := requirementName(in); got != want {
			t.Errorf("requirementName(%q) = %q, ждали %q", in, got, want)
		}
	}
}

// Пустой каталог плагинов — не «всё готово», а отсутствие витрины. Итоговая
// строка обязана называть причину, иначе человек решит, что у него всё в
// порядке.
func TestDoctorEmptyPluginsDirExplainsItself(t *testing.T) {
	rep := Run(t.TempDir())
	if rep.Total != 0 {
		t.Fatalf("Total = %d при пустом каталоге", rep.Total)
	}
	if rep.OK {
		t.Error("OK=true при пустом каталоге плагинов")
	}
	if !strings.Contains(rep.Summary, "не найдены") {
		t.Errorf("итог не объясняет, что плагинов нет: %q", rep.Summary)
	}
}

// JSON-вывод обязан разбираться и нести те же счётчики, что и человеческий:
// иначе автоматизация и человек видят разное.
func TestDoctorJSONShape(t *testing.T) {
	root := doctorFixture(t, map[string]string{
		"a/plugin.yaml": doctorManifestText("a", "Плагин A на stdlib", nil),
		"a/main.py":     "import json\nprint('{}')\n",
	})
	rep := Run(root)
	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]interface{}
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"ok", "python", "plugins_dir", "total", "ready", "summary", "plugins"} {
		if _, ok := back[key]; !ok {
			t.Errorf("в JSON нет поля %q", key)
		}
	}
	list, ok := back["plugins"].([]interface{})
	if !ok || len(list) != 1 {
		t.Fatalf("plugins в JSON = %v, ждали массив из 1", back["plugins"])
	}
	entry := list[0].(map[string]interface{})
	if entry["status"] != StatusReady {
		t.Errorf("status в JSON = %v, ждали %q", entry["status"], StatusReady)
	}
}

// Согласование числительного: «1 ждёт», а не «1 ждут».
func TestDoctorSummaryPlural(t *testing.T) {
	cases := []struct {
		n          int
		wantSubstr string
	}{
		{1, "1 ждёт"},
		{2, "2 ждут"},
		{5, "5 ждут"},
		{11, "11 ждут"},
		{21, "21 ждёт"},
	}
	for _, c := range cases {
		got := plural(c.n, "ждёт", "ждут", "ждут")
		if got != c.wantSubstr {
			t.Errorf("plural(%d) = %q, ждали %q", c.n, got, c.wantSubstr)
		}
	}
}

// doctorRealInterpreter — путь к интерпретатору до подмены PATH: тесты,
// подставляющие пустой PATH, всё равно должны находить python тем же кодом,
// каким это делает ран.
func doctorRealInterpreter() (string, error) {
	info, err := plugin.PythonInfo()
	if err != nil {
		return "", err
	}
	return info.Path, nil
}

func runtimeIsWindows() bool { return os.PathListSeparator == ';' }

// Дефект 2. Если «внешний инструмент» из описания совпадает с пакетом из
// runtime.requires того же плагина, это НЕ внешний инструмент, а та же самая
// pip-зависимость. Ровно этот случай в репозитории — community/exifread:
// в описании `pip install exifread`, в манифесте `requires: [exifread==3.5.1]`.
func TestDoctorDonorMatchingRequiresIsNotExternalTool(t *testing.T) {
	interp, err := doctorRealInterpreter()
	if err != nil {
		t.Skipf("на этой машине нет интерпретатора: %v", err)
	}
	t.Setenv("PATH", filepath.Dir(interp))

	// Имя донора из описания и имя пакета из requires — ОДНО И ТО ЖЕ, иначе
	// проверка классификации ничего не проверяет. У community/exifread именно
	// так: `pip install exifread` в описании и `exifread==3.5.1` в requires.
	root := doctorFixture(t, map[string]string{
		"exif/plugin.yaml": doctorManifestText("exif",
			"Обёртка над библиотекой. Ставится отдельно: pip install wedra-absent-exif-pkg.",
			[]string{"wedra-absent-exif-pkg==1.0"}),
		"exif/main.py": "import json\nprint(json.dumps({'status': 'ok', 'output': {'value': 'x'}}))\n",
	})
	rep := Run(root)
	p := doctorOne(t, root)

	if p.DonorKind != DonorKindSameAsRequires {
		t.Errorf("donor_kind = %q, ждали %q: донор совпадает с пакетом из requires",
			p.DonorKind, DonorKindSameAsRequires)
	}
	if p.Status != StatusNeedsDependency {
		t.Errorf("статус = %q, ждали %q: отсутствующий пакет из requires — это needs_dependency",
			p.Status, StatusNeedsDependency)
	}
	if rep.NeedsDonor != 0 {
		t.Errorf("needs_external_tool = %d, ждали 0: совет «установи инструмент» тут путает", rep.NeedsDonor)
	}
}

// Дефект 3. Мусор из описания манифеста не должен попадать в имя пакета:
// реальные случаи в репозитории — «pip install vt-py):» и
// «`pip install webanalyze` на PyPI нет».
func TestDoctorStripsGarbageFromDonorName(t *testing.T) {
	cases := []struct {
		desc string
		want string
	}{
		{"Донор — vt-py (import vt, REST API v3, pip install vt-py): плагин идёт по паттерну C",
			"vt-py"},
		{"Это Go-программа, а не pip-пакет: `pip install webanalyze` на PyPI нет",
			"webanalyze"},
		{"Ставится отдельно: pip install arjun.", "arjun"},
		{"Ставится: pip install 'detect-secrets'.", "detect-secrets"},
		{"Ставится: pip install (binwalk).", "binwalk"},
		{"Ставится: pip install probe, если нет.", "probe"},
	}
	for _, c := range cases {
		got, ok := donorFromDescription(c.desc)
		if !ok {
			t.Errorf("donorFromDescription(%q) не нашёл донора", c.desc)
			continue
		}
		if got != c.want {
			t.Errorf("donorFromDescription(%q) = %q, ждали %q", c.desc, got, c.want)
		}
	}
}

// Пин `package==version` в описании — это имя пакета плюс версия, а не
// «непонятное имя». community/pefile и community/detect_secrets держат пин
// прямо в описании («Пин: pip install pefile==2024.8.26»), и раньше такой хвост
// ронял проверку имени: плагин попадал в unverified_donors с надписью «наличие
// инструмента НЕ проверено», хотя пакет объявлен в runtime.requires и doctor
// его прекрасно видит.
func TestDoctorReadsPinnedDonorName(t *testing.T) {
	cases := []struct {
		desc string
		want string
	}{
		{"Пин: pip install pefile==2024.8.26", "pefile"},
		{"Пин: pip install detect-secrets==1.5.0", "detect-secrets"},
		{"Ставится: pip install 'exifread==3.5.1'.", "exifread"},
		{"Ставится: pip install maigret==0.6.6, если нет.", "maigret"},
		{"Ставится: pip install holehe==1.61.", "holehe"},
	}
	for _, c := range cases {
		got, ok := donorFromDescription(c.desc)
		if !ok {
			t.Errorf("donorFromDescription(%q) не нашёл донора: версия в пине — это не мусор", c.desc)
			continue
		}
		if got != c.want {
			t.Errorf("donorFromDescription(%q) = %q, ждали %q", c.desc, got, c.want)
		}
	}
}

// Обратная сторона строгости: отбрасывание `==…` не должно превращаться в
// «обрежем всё до равных знаков и назовём это именем». Всё, что не является
// пинoм вида `имя==версия`, обязано быть отвергнуто целиком — включая случаи,
// где левая часть сама по себе не имя пакета (VCS-цель, путь, флаг).
func TestDoctorRejectsNonPinsAroundEquals(t *testing.T) {
	cases := []struct {
		desc string
		why  string
	}{
		{"Ставится: pip install pefile==", "пин без версии"},
		{"Ставится: pip install ==1.0", "пин без имени"},
		{"Ставится: pip install pefile==latest", "версия не начинается с цифры"},
		{"Ставится: pip install pefile==1.0=2", "вторые равные знаки"},
		{"Ставится: pip install 2024.8.26==1.0", "слева версия, а не имя"},
		{"Ставится: pip install git+https://github.com/r0oth3x49/ghauri.git==1.0", "VCS-цель с пином"},
		{"Ставится: pip install -r requirements.txt==1.0", "флаг с пином"},
		{"Ставится: pip install /opt/tools/pefile==1.0", "путь с пином"},
		{"Ставится: pip install pefile[exe]==1.0", "extras слева от пина"},
	}
	for _, c := range cases {
		if got, ok := donorFromDescription(c.desc); ok {
			t.Errorf("donorFromDescription(%q) = %q, ждали отказ (%s)", c.desc, got, c.why)
		}
	}
}

// Маркер окружения (`pkg==1.0;python_version<'3.9'`) режется ПРЕЖДЕ разбора
// пина — правилом про запятую/двоеточие/точку с запятой, которое существовало
// и раньше. Имя оттуда всё равно верное, поэтому это не «мусор проскочил», а
// отдельный, более ранний разбор: проверяем, чтобы правка пина его не сломала.
func TestDoctorPinWithEnvironmentMarkerKeepsName(t *testing.T) {
	got, ok := donorFromDescription("Ставится: pip install we-dr-marker==1.0;python_version<'3.9'")
	if !ok {
		t.Fatal("маркер окружения не должен оставлять плагин без донора")
	}
	if got != "we-dr-marker" {
		t.Errorf("донор = %q, ждали %q", got, "we-dr-marker")
	}
}

// Валидатор имени пакета остаётся СТРОГИМ: он не должен начать принимать
// `name==version` — иначе чинить пришлось бы в нём, и тогда вниз по коду
// поехало бы неразобранное `pefile==2024.8.26` (в PATH искать нечего, в
// сравнение с requires не попало бы). Разбор пина — отдельный шаг.
func TestDoctorValidatorStillRejectsPin(t *testing.T) {
	for _, s := range []string{"pefile==2024.8.26", "detect-secrets==1.5.0", "pefile==", "==1.0", "pkg==1.0=2"} {
		if validPackageName(s) {
			t.Errorf("validPackageName(%q) = true, а это не имя пакета", s)
		}
	}
}

// Сквозной регресс на исходный симптом: плагин с пином в описании, тем же
// пином в runtime.requires и упоминанием установки в README обязан быть
// ПРОВЕРЕН, а не попасть в unverified_donors. Раньше он попадал именно туда:
// имя `pefile==2024.8.26` не проходило проверку, донор становился «неизвестным».
func TestDoctorPinnedDonorIsNotUnverified(t *testing.T) {
	const pkg = "wedra_absent_pinned_donor"
	root := doctorFixture(t, map[string]string{
		"pinned/plugin.yaml": doctorManifestText("pinned",
			"Плагин. Пин: pip install "+pkg+"==1.2.3", []string{pkg + "==1.2.3"}),
		"pinned/main.py":   "import json\nprint(json.dumps({'status': 'ok', 'output': {'value': 'x'}}))\n",
		"pinned/README.md": "Установка донора:\n\n    pip install " + pkg + "==1.2.3\n",
	})
	rep := Run(root)
	p := doctorOne(t, root)

	if p.Donor != pkg {
		t.Errorf("donor = %q, ждали %q: версия в пине — это не часть имени", p.Donor, pkg)
	}
	if p.DonorKind != DonorKindSameAsRequires {
		t.Errorf("donor_kind = %q, ждали %q", p.DonorKind, DonorKindSameAsRequires)
	}
	for _, id := range rep.UnverifiedDonors {
		if id == "pinned" {
			t.Errorf("pinned попал в unverified_donors: %v — донор проверен", rep.UnverifiedDonors)
		}
	}
}

func TestDoctorPinParsingTable(t *testing.T) {
	cases := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"pefile==2024.8.26", "pefile", true},
		{"detect-secrets==1.5.0", "detect-secrets", true},
		{"holehe==1.61", "holehe", true},
		{"pkg==1.0.*", "pkg", true},
		{"pkg==1.0+local", "pkg", true},
		{"pkg==1.0rc1", "pkg", true},
		{"pkg", "", false},
		{"pefile==", "", false},
		{"==1.0", "", false},
		{"pkg==latest", "", false},
		{"pkg==1.0;python_version<'3.9'", "", false},
		{"git+https://github.com/x/y.git==1.0", "", false},
		{"2024.8.26==1.0", "", false},
	}
	for _, c := range cases {
		got, ok := packageNameFromPin(c.in)
		if ok != c.wantOK || (ok && got != c.want) {
			t.Errorf("packageNameFromPin(%q) = (%q, %v), ждали (%q, %v)", c.in, got, ok, c.want, c.wantOK)
		}
	}
}

// Проверка на ВСЕХ манифестах репозитория: в команде установки не должно
// остаться ни одного символа, который в имени пакета не бывает. Это ровно
// дефект 3, только в масштабе витрины, а не на двух примерах.
func TestDoctorRealRepoDonorNamesHaveNoGarbage(t *testing.T) {
	// Каталог плагинов ищем от исходника теста: go test запускает пакет в
	// internal/doctor, и относительный "plugins" там не существует — проверка
	// молча прошла бы вхолостую.
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Skip("не удалось определить путь к исходнику")
	}
	root := filepath.Join(filepath.Dir(self), "..", "..", "plugins")
	manifests := scanManifests(root)
	if len(manifests) == 0 {
		t.Skipf("плагины не найдены в %s", root)
	}
	// Мусор, который реально встречался: закрывающая скобка, markdown-кавычка,
	// запятая, двоеточие, точка в конце, обратный слэш, пробел.
	bad := "()[]{}<>`\\,;: \t"
	checked := 0
	for _, m := range manifests {
		donor, ok := donorFromDescription(m.Description)
		if !ok {
			continue
		}
		checked++
		if strings.ContainsAny(donor, bad) {
			t.Errorf("%s: имя пакета %q содержит мусор", m.ID, donor)
		}
		if !validPackageName(donor) {
			t.Errorf("%s: %q не похоже на имя пакета", m.ID, donor)
		}
	}
	if checked == 0 {
		t.Fatalf("не нашлось ни одного донора в %d манифестах — проверка ваккуумная", len(manifests))
	}
	t.Logf("проверено имён пакетов: %d из %d манифестов", checked, len(manifests))
}

// Оговорка о границах проверки: плагин, у которого донора нет ни в описании,
// ни в README, обязан попасть в unverified_donors. Плагин с `pip install`
// в README теперь тоже проверяется (fallback donorFromFiles), поэтому
// readmeonly НЕ в списке, а really_unverified — да.
func TestDoctorListsUnverifiedDonors(t *testing.T) {
	root := doctorFixture(t, map[string]string{
		"checked/plugin.yaml":           doctorManifestText("checked", "Ставится: pip install some-tool", nil),
		"checked/main.py":               "import json\nprint('{}')\n",
		"readmeonly/plugin.yaml":        doctorManifestText("readmeonly", "Плагин без упоминания донора", nil),
		"readmeonly/main.py":            "import json\nprint('{}')\n",
		"readmeonly/README.md":          "Установка внешнего инструмента: `pip install other-tool`.\n",
		"really_unverified/plugin.yaml": doctorManifestText("really_unverified", "Плагин без упоминания донора", nil),
		"really_unverified/main.py":     "import json\nprint('{}')\n",
		"really_unverified/README.md":   "Установка: pip install https://example.com/pkg.tar.gz\n",
	})
	rep := Run(root)

	found := map[string]bool{}
	for _, id := range rep.UnverifiedDonors {
		found[id] = true
	}
	if found["readmeonly"] == true {
		t.Errorf("readmeonly теперь проверяется через README, не должен быть в unverified_donors: %v", rep.UnverifiedDonors)
	}
	if found["really_unverified"] != true {
		t.Errorf("really_unverified должен быть в unverified_donors (url-установка не парсится): %v", rep.UnverifiedDonors)
	}
	if found["checked"] {
		t.Errorf("checked донора проверил — в оговорке ему не место: %v", rep.UnverifiedDonors)
	}
}

// Оговорка обязана быть и в человеческом выводе: поле в --json не мешает
// человеку, который смотрит отчёт глазами. С fallback на README оговорка
// появляется только если есть плагин без pip install нигде.
func TestDoctorHumanOutputHasHonestyNote(t *testing.T) {
	root := doctorFixture(t, map[string]string{
		"checked/plugin.yaml":    doctorManifestText("checked", "Ставится: pip install some-tool", nil),
		"checked/main.py":        "import json\nprint('{}')\n",
		"unverified/plugin.yaml": doctorManifestText("unverified", "Плагин без упоминания донора", nil),
		"unverified/main.py":     "import json\nprint('{}')\n",
		"unverified/README.md":   "Установка: pip install https://example.com/pkg.tar.gz\n",
	})
	rep := Run(root)
	if len(rep.UnverifiedDonors) == 0 {
		t.Fatal("оговорка не набралась — проверять нечего")
	}
	var buf bytes.Buffer
	PrintHuman(&buf, rep)
	text := buf.String()
	if !strings.Contains(text, "Оговорка") || !strings.Contains(text, "НЕ проверено") {
		t.Errorf("в человеческом выводе нет оговорки о границах проверки:\n%s", text)
	}
	if !strings.Contains(text, "unverified") {
		t.Errorf("оговорка не перечисляет непроверенные плагины:\n%s", text)
	}
}
