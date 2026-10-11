package cli

// Регрессия: `wedra pipeline install <пресет>` нормализует пресет через
// yaml.Marshal и писал YAML, который сам же затем не мог прочитать.
//
//	> wedra pipeline install when_demo --registry=./registry.yaml
//	  ■ пресет "when_demo" готов: examples/when_demo.yaml --yes
//	> wedra pipeline validate examples/when_demo.yaml
//	  YAML ошибка: YAML: when: path обязателен
//
// Причина была в асимметрии маршалинга и парсинга (omitempty у Step.When и
// MarshalYAML у Duration). Причина исправлена в internal/pipeline/model.go,
// но гейта, который бы это поймал, не было: install проверял структуру в
// памяти и печатал «пресет готов» без единой попытки прочитать собственный
// вывод.
//
// Тесты ниже закрывают оба конца:
//   - TestInstallPipelinePresetWritesFileCoreCanRead — на настоящем пресете из
//     реестра: установленный файл обязан читаться и валидироваться тем же
//     загрузчиком, которым его прочитает пользователь;
//   - TestInstallPipelinePresetRejectsUnreadableOutput — затвор проверяется
//     напрямую: маршалинг подменяется на несимметричный парсингу, и install
//     обязан отказаться, ничего не публикуя.
//
// Оба работают без сети: реестр и пресет лежат во временном каталоге.

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/wykserdex/wedra/internal/pipeline"
)

// registryPresetFixture — пресет со всеми полями, на которых маршалинг ядра
// уже расходился с парсингом: when (пустой блок уезжал в файл), timeout и
// retry.delay (Duration без MarshalYAML уезжали картой). Плагины — встроенные,
// поэтому тесту не нужен ни plugins/, ни сеть.
const registryPresetFixture = `format_version: "0.2"
pipeline:
  name: rt
  input:
    n: 5
  steps:
    - id: a
      plugin: core/human_gate
      timeout: 7s
      form:
        - field: input.n
          editable: true
      actions: [accept]

    - id: b
      plugin: core/human_gate
      when: { path: steps.a.n, op: gte, value: 10 }
      on_error: retry
      retry: { attempts: 3, delay: 2s, backoff: exponential }
      form:
        - field: input.n
          editable: false
      actions: [accept]
`

// writeLocalRegistry — оффлайн-реестр в отдельном каталоге: source указывает на
// локальный каталог с пресетом, поэтому pluginSourceDir берёт файл оттуда и до
// git clone дело не доходит.
func writeLocalRegistry(t *testing.T) (registryPath, presetName string) {
	t.Helper()
	source := t.TempDir()
	if err := os.MkdirAll(filepath.Join(source, "examples"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "examples", "rt.yaml"), []byte(registryPresetFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	body := "version: \"0.1\"\npresets:\n  rt:\n    source: " + strconv.Quote(source) +
		"\n    path: examples/rt.yaml\n"
	registryPath = filepath.Join(dir, "registry.yaml")
	if err := os.WriteFile(registryPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return registryPath, "rt"
}

func TestInstallPipelinePresetWritesFileCoreCanRead(t *testing.T) {
	useWorkingDir(t, t.TempDir())
	registryPath, name := writeLocalRegistry(t)

	raw, fallback, prov, err := fetchPreset(name, registryPath, "")
	if err != nil {
		t.Fatalf("пресет из реестра не прочитан: %v", err)
	}
	result, err := installPipelinePreset(raw, fallback, registryPath, prov)
	if err != nil {
		t.Fatalf("установка пресета: %v", err)
	}

	// Ровно то, что делает `wedra pipeline validate <file>`.
	installed, err := pipeline.LoadPipelineFile(result.OutFile)
	if err != nil {
		t.Fatalf("ядро не читает собственный установленный файл: %v", err)
	}
	if len(installed.Pipeline.Steps) != 2 {
		t.Fatalf("установлено %d шагов, ждём 2", len(installed.Pipeline.Steps))
	}

	// Не только «читается», но и не потерял то, что маршалинг терял.
	first := installed.Pipeline.Steps[0]
	if first.Timeout.Duration.String() != "7s" {
		t.Fatalf("timeout шага a = %v, ждём 7s (Duration должна писаться скаляром)", first.Timeout.Duration)
	}
	second := installed.Pipeline.Steps[1]
	if !second.When.IsSet() || second.When.Path != "steps.a.n" || second.When.Op != "gte" || second.When.Value != 10 {
		t.Fatalf("when шага b потерян при установке: %+v", second.When)
	}
	if second.Retry == nil || second.Retry.Attempts != 3 || second.Retry.Delay.Duration.String() != "2s" {
		t.Fatalf("retry шага b потерян при установке: %+v", second.Retry)
	}
}

func TestInstallPipelinePresetRejectsUnreadableOutput(t *testing.T) {
	// Затвор проверяется маршаллингом, который заведомо ломает файл: пустой блок
	// when — ровно тот вывод, который уехал в релиз. Если затвор снять, этот
	// тест станет зелёным и install продолжит печатать «пресет готов».
	useWorkingDir(t, t.TempDir())
	registryPath, name := writeLocalRegistry(t)
	raw, fallback, prov, err := fetchPreset(name, registryPath, "")
	if err != nil {
		t.Fatalf("пресет из реестра не прочитан: %v", err)
	}

	restore := presetMarshal
	presetMarshal = func(pf *pipeline.PipelineFile) ([]byte, error) {
		clean, err := yaml.Marshal(pf)
		if err != nil {
			return nil, err
		}
		// Симметричный парсингу, но нечитаемый вывод: when без path.
		return []byte(strings.Replace(string(clean), "steps.a.n", "", 1)), nil
	}
	t.Cleanup(func() { presetMarshal = restore })

	if _, err := installPipelinePreset(raw, fallback, registryPath, prov); err == nil {
		t.Fatal("install сообщил об успехе, хотя записал нечитаемый пресет")
	} else if !strings.Contains(err.Error(), "не читается ядром") {
		t.Fatalf("невнятный отказ: %v", err)
	}

	// Плохой файл не публикуется: ни в examples/, ни в провенансе.
	if _, err := os.Stat(filepath.Join("examples", "rt.yaml")); !os.IsNotExist(err) {
		t.Fatalf("нечитаемый пресет стал виден пользователю: %v", err)
	}
	if _, err := os.Stat(filepath.Join("examples", "rt.yaml"+presetProvenanceExt)); !os.IsNotExist(err) {
		t.Fatalf("провенанс написан на нечитаемый пресет: %v", err)
	}
	entries, err := os.ReadDir("examples")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Fatalf("в examples/ остался мусор после отказа: %s", e.Name())
	}
}
