package install

// Тесты автоустановки зависимостей при установке плагина.
//
// Смысл изменения: «из коробки» и «работает после кнопки» — разные вещи.
// Механизм pip в проекте был один и вызывался только из GUI по кнопке, поэтому
// плагин с объявленными зависимостями после установки оставался нерабочим.
// Теперь установка ставит их сама.
//
// pip здесь не запускается: вызов подменён. Иначе тест ходил бы в сеть и
// зависел бы от чужой доступности, а падение PyPI читалось бы как поломка
// установки.

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/wykserdex/wedra/internal/registry"
)

const manifestWithDeps = `id: safe
version: 0.1.0
platform_api: "0.1"
runtime:
  type: python
  entry: main.py
  requires: [pydantic==2.12.3]
input:
  text:
    from: input.text
    type: string
output:
  value:
    type: string
permissions:
  network: []
  filesystem: none
  secrets: []
`

// writeDepsFixture — реестр с одним плагином, у которого объявлена
// зависимость. requirements.lock обязателен: без него манифест не проходит
// валидацию, и тест проверял бы отказ по другой причине.
func writeDepsFixture(t *testing.T) (registryDir string) {
	t.Helper()
	sourceDir := t.TempDir()
	pluginDir := filepath.Join(sourceDir, "plugin")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "plugin.yaml"), []byte(manifestWithDeps), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "main.py"), []byte("pass\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "requirements.lock"), []byte("pydantic==2.12.3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	registryDir = t.TempDir()
	body := "version: \"0.1\"\nplugins:\n  safe:\n    source: " + strconv.Quote(sourceDir) +
		"\n    path: plugin\n    version: main\n"
	if err := os.WriteFile(filepath.Join(registryDir, "registry.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return registryDir
}

func stubPip(t *testing.T, err error) *[]string {
	t.Helper()
	prev := pipInstall
	var got []string
	pipInstall = func(specs []string) (string, error) {
		got = append(got, specs...)
		return "Successfully installed pydantic-2.12.3", err
	}
	t.Cleanup(func() { pipInstall = prev })
	return &got
}

// ГЛАВНОЕ: установка плагина с объявленными зависимостями ставит их сама.
// До изменения установка заканчивалась строкой «+ safe (main) → …», и человек
// уходил в doctor за командо�� pip install, а потом в консоль за кнопкой.
func TestRunInstallsDeclaredDependencies(t *testing.T) {
	registryDir := writeDepsFixture(t)
	got := stubPip(t, nil)

	dest := t.TempDir()
	var log bytes.Buffer
	res, err := Run(&log, Request{
		Name: "safe", RegistrySrc: filepath.Join(registryDir, "registry.yaml"), Dest: dest,
	})
	if err != nil {
		t.Fatalf("установка: %v\n%s", err, log.String())
	}
	if len(*got) != 1 || (*got)[0] != "pydantic==2.12.3" {
		t.Fatalf("в pip ушло %v, ждали ровно [pydantic==2.12.3]", *got)
	}
	if len(res.DepsInstalled) != 1 {
		t.Errorf("DepsInstalled=%v, человек не увидит, что поставилось", res.DepsInstalled)
	}
	if res.DepsError != "" {
		t.Errorf("DepsError=%q при успешном pip", res.DepsError)
	}
	// Человек должен видеть в логе, что зависимости ставятся: молчаливый pip
	// выглядит как «ничего не произошло».
	if !strings.Contains(log.String(), "pydantic==2.12.3") {
		t.Errorf("в логе нет упоминания зависимости:\n%s", log.String())
	}
}

// Плагин без объявленных зависимостей не должен дёргать pip вообще: лишний
// вызов интерпретатора на каждый из 58 плагинов, которым pip не нужен, — это
// ровно та цена, которую платят все ради одного.
func TestRunSkipsPipWhenNoDependencies(t *testing.T) {
	sourceDir := t.TempDir()
	pluginDir := filepath.Join(sourceDir, "plugin")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "plugin.yaml"), []byte(validManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "main.py"), []byte("pass\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	registryDir := t.TempDir()
	body := "version: \"0.1\"\nplugins:\n  safe:\n    source: " + strconv.Quote(sourceDir) +
		"\n    path: plugin\n    version: main\n"
	if err := os.WriteFile(filepath.Join(registryDir, "registry.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got := stubPip(t, nil)

	if _, err := Run(bytes.NewBuffer(nil), Request{
		Name: "safe", RegistrySrc: filepath.Join(registryDir, "registry.yaml"), Dest: t.TempDir(),
	}); err != nil {
		t.Fatalf("установка: %v", err)
	}
	if len(*got) != 0 {
		t.Errorf("pip вызван без объявленных зависимостей: %v", *got)
	}
}

// Отказ pip НЕ откатывает установленный плагин: каталог уже опубликован и
// валиден, а отсутствие зависимостей — это состояние окружения, а не
// неправильная установка. Но об этом обязано быть сказано: молчаливо
// «установилось» с неработающим плагином — худший из вариантов.
func TestRunReportsDepsFailureWithoutRollingBack(t *testing.T) {
	registryDir := writeDepsFixture(t)
	stubPip(t, os.ErrPermission)

	dest := t.TempDir()
	res, err := Run(bytes.NewBuffer(nil), Request{
		Name: "safe", RegistrySrc: filepath.Join(registryDir, "registry.yaml"), Dest: dest,
	})
	if err != nil {
		t.Fatalf("установка должна завершиться, отказ pip — не её отказ: %v", err)
	}
	if res.DepsError == "" {
		t.Error("DepsError пуст: отказ pip не замечен")
	}
	if _, statErr := os.Stat(filepath.Join(dest, "safe", "plugin.yaml")); statErr != nil {
		t.Errorf("плагин не должен откатываться из-за pip: %v", statErr)
	}
}

// Отказ от автоустановки — осознанный выбор человека: без сети или в CI, где
// зависимости ставят заранее.
func TestRunNoDepsSkipsPip(t *testing.T) {
	registryDir := writeDepsFixture(t)
	got := stubPip(t, nil)

	var log bytes.Buffer
	res, err := Run(&log, Request{
		Name: "safe", RegistrySrc: filepath.Join(registryDir, "registry.yaml"),
		Dest: t.TempDir(), NoDeps: true,
	})
	if err != nil {
		t.Fatalf("установка: %v", err)
	}
	if len(*got) != 0 {
		t.Errorf("pip вызван при NoDeps: %v", *got)
	}
	if res.DepsInstalled != nil && len(res.DepsInstalled) > 0 {
		t.Errorf("DepsInstalled=%v при NoDeps", res.DepsInstalled)
	}
}

var _ = registry.SplitRef
