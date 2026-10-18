package plugin

// Механика allow-list: разбор конфига, строгая проверка формы записи,
// слияние со встроенным списком и целостность засева.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wykserdex/wedra/internal/registry"
)

// Запись allow-list обязана иметь вид id@sha256:<64 hex>. Каждая проверка
// нужна по своей причине, и все они ловят одну общую беду: опечатка, которая
// выглядит как запись, но никогда не сработает, — и плагин «почему-то» не
// доверен.
func TestSplitAllowEntryRejectsMalformed(t *testing.T) {
	cases := []struct {
		name  string
		entry string
		ok    bool
	}{
		{"корректная", "llm_openai@sha256:" + strings.Repeat("a", 64), true},
		{"без префикса sha256", "llm_openai@" + strings.Repeat("a", 64), false},
		{"короткий хэш", "llm_openai@sha256:abc", false},
		{"длинный хэш", "llm_openai@sha256:" + strings.Repeat("a", 65), false},
		{"не-hex", "llm_openai@sha256:" + strings.Repeat("z", 64), false},
		{"верхний регистр", "llm_openai@sha256:" + strings.Repeat("A", 64), false},
		{"без @", "llm_openai", false},
		{"пустой id", "@sha256:" + strings.Repeat("a", 64), false},
		{"пустая строка", "", false},
		{"только пробелы", "   ", false},
	}
	for _, c := range cases {
		_, _, err := SplitAllowEntry(c.entry)
		if c.ok && err != nil {
			t.Errorf("%s: %q должен разбираться, получено: %v", c.name, c.entry, err)
		}
		if !c.ok && err == nil {
			t.Errorf("%s: %q обязан быть отвергнут", c.name, c.entry)
		}
	}
}

func TestLoadTrustConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, TrustConfigFile)
	hashA := "sha256:" + strings.Repeat("a", 64)
	hashB := "sha256:" + strings.Repeat("b", 64)
	raw := "version: \"" + TrustConfigVersion + "\"\n" +
		"trusted_plugins:\n" +
		"  - \"my_plugin@" + hashA + "\"\n" +
		"  - \"other@" + hashB + "\"\n"
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	list, err := LoadTrustConfig(path)
	if err != nil {
		t.Fatalf("конфиг не прочитан: %v", err)
	}
	if list.Len() != 2 {
		t.Fatalf("в allow-list %d записей, ждали 2", list.Len())
	}
	if !list.Allows("my_plugin", hashA) || !list.Allows("other", hashB) {
		t.Fatal("записи конфига не попали в allow-list")
	}
	// Совпадение только при полном совпадении id И хэша.
	if list.Allows("my_plugin", hashB) {
		t.Error("чужой хэш для того же id принят как доверенный")
	}
	if list.Allows("unknown", hashA) {
		t.Error("чужой id с верным хэшем принят как доверенный")
	}
}

// Отсутствие конфига — не ошибка, а «никто не доверен сверх встроенного».
// Иначе первый же запуск в чистом каталоге падал бы с ошибкой разбора.
func TestLoadTrustConfigMissingFileIsEmpty(t *testing.T) {
	list, err := LoadTrustConfig(filepath.Join(t.TempDir(), "нет-такого.yaml"))
	if err != nil {
		t.Fatalf("отсутствие конфига обязано быть штатным случаем: %v", err)
	}
	if list.Len() != 0 {
		t.Fatalf("ожидался пустой allow-list, получено %d записей", list.Len())
	}
}

// Битый конфиг — ошибка, а не «пустой allow-list». Молчаливый откат здесь
// выглядел бы как «плагины внезапно перестали доверяться» без видимой причины.
func TestLoadTrustConfigRejectsBroken(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		"битый yaml":       "trusted_plugins: [oops\n",
		"неизвестное поле": "trustet_plugins:\n  - x\n",
		"плохая запись":    "trusted_plugins:\n  - \"без-хэша\"\n",
		"чужая версия":     "version: \"9.9\"\ntrusted_plugins: []\n",
		"два документа":    "version: \"" + TrustConfigVersion + "\"\n---\nversion: \"0.1\"\n",
	}
	for name, raw := range cases {
		path := filepath.Join(dir, "c-"+name+".yaml")
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadTrustConfig(path); err == nil {
			t.Errorf("%s: конфиг обязан быть отвергнут", name)
		}
	}
}

// Встроенный список и конфиг оператора СУММИРУЮТСЯ, а не заменяют друг друга.
//
// Это и есть та совместимость, ради которой всё затевалось: если бы конфиг
// заменял встроенный список, любая запись community-плагина в wedra-trust.yaml
// тихо выкинула бы из доверия все 23 официальных, и на хостах без изолятора
// (Windows, macOS) они перестали бы работать.
func TestEffectiveAllowListIsUnion(t *testing.T) {
	dir := t.TempDir()
	hashA := "sha256:" + strings.Repeat("a", 64)
	if err := os.WriteFile(filepath.Join(dir, TrustConfigFile),
		[]byte("trusted_plugins:\n  - \"my_plugin@"+hashA+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	list, err := EffectiveAllowList(filepath.Join(dir, TrustConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	if !list.Allows("my_plugin", hashA) {
		t.Error("запись конфига оператора потерялась")
	}
	builtin := BuiltinAllowList()
	for _, id := range builtin.IDs() {
		if !list.Allows(id, builtinHash(t, id)) {
			t.Errorf("встроенный плагин %q выпал из доверия при загрузке конфига", id)
		}
	}
}

// builtinHash — один из встроенных хэшей плагина (для проверки слияния).
func builtinHash(t *testing.T, id string) string {
	t.Helper()
	hashes := builtinTrusted[id]
	if len(hashes) == 0 {
		t.Fatalf("встроенный плагин %q без хэшей", id)
	}
	return hashes[0]
}

// Каждая копия BuiltinAllowList независима. Общая карта означала бы, что
// вызывающий, добавивший запись, поправил доверие для всех последующих.
func TestBuiltinAllowListIsIndependentCopy(t *testing.T) {
	first := BuiltinAllowList()
	first.Allow("intruder", "sha256:"+strings.Repeat("c", 64))
	second := BuiltinAllowList()
	if second.Allows("intruder", "sha256:"+strings.Repeat("c", 64)) {
		t.Fatal("правка одной копии allow-list затронула другую")
	}
}

// ЗАСЕВ СОВМЕСТИМОСТИ, часть 1: каждый плагин из registry.yaml присутствует во
// встроенном allow-list.
//
// После инверсии доверенным остался только allow-list, поэтому отсутствие
// плагина в засеве означало бы ровно то, ради чего инверсию делали: реестровый
// плагин стал бы untrusted и перестал бы работать на хостах без изолятора.
func TestBuiltinSeedHasEveryRegistryPlugin(t *testing.T) {
	names := registryPluginNames(t, repoRoot(t))
	if len(names) == 0 {
		t.Fatal("в реестре нет ни одной записи плагина — тест в vacuous")
	}
	seeds := BuiltinAllowList()
	for _, name := range names {
		if !seeds.Allows(name, builtinHash(t, name)) {
			t.Errorf("реестровый плагин %q не доверен: он стал бы untrusted "+
				"и сломался бы вне Linux", name)
		}
	}
}

// ЗАСЕВ СОВМЕСТИМОСТИ, часть 2: засев совпадает с пинами реестра.
//
// Проверку выполняет сам генератор в режиме -check: он разворачивает каждую
// запись по её пину и пересчитывает хэш. Дублировать эту логику в тесте нельзя
// — две реализации разъедутся, и тест продолжит «проверять» не то. Расхождение
// означало бы, что доверен не тот код, который реально уедет пользователю.
func TestGeneratedSeedMatchesRegistryPins(t *testing.T) {
	repo := repoRoot(t)
	cmd := exec.Command("go", "run", "./internal/plugin/cmd/genseed", "-repo", repo, "-check")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		if strings.Contains(string(out), "not a tree object") {
			t.Skipf("shallow clone — коммит из пина вне истории, пропуск: %s", string(out))
		}
		t.Fatalf("засев разошёлся с пинами реестра: %v\n%s\n"+
			"исправь: go run ./internal/plugin/cmd/genseed", err, out)
	}
}

// registryPluginNames — имена записей plugins из registry.yaml.
func registryPluginNames(t *testing.T, repo string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repo, "registry.yaml"))
	if err != nil {
		t.Skipf("registry.yaml не прочитан: %v", err)
	}
	handle, err := registry.Load(filepath.Join(repo, "registry.yaml"))
	if err != nil {
		t.Skipf("реестр не разобран: %v", err)
	}
	defer handle.Close()
	if len(handle.Registry.Plugins) == 0 {
		t.Skipf("реестр без плагинов (%d байт)", len(raw))
	}
	return handle.PluginNames()
}

// repoRoot — корень репозитория относительно каталога пакета.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// internal/plugin → корень на два уровня выше.
	root := filepath.Clean(filepath.Join(dir, "..", ".."))
	if _, err := os.Stat(filepath.Join(root, "registry.yaml")); err != nil {
		t.Skipf("не найден корень репозитория (%s): %v", root, err)
	}
	return root
}
